// Package launchdarkly implements a mamori provider that resolves configuration
// values from LaunchDarkly feature flags.
//
// The scheme is "launchdarkly" and the ref grammar is:
//
//	launchdarkly://<flag-key>[#json-key]
//
// Each ref names a LaunchDarkly feature flag. The provider evaluates the flag
// for a single evaluation context (a non-anonymous context whose key defaults to
// "mamori", overridable with WithContextKey) using the SDK's JSON "detail"
// evaluation, so it obtains both the flag value and the evaluation reason.
//
//	KillSwitch  bool   `source:"launchdarkly://new-checkout-enabled"`
//	Timeout     string `source:"launchdarkly://api-config#timeout"`
//
// The flag value is converted to bytes as follows: a boolean becomes "true" or
// "false", a string becomes its raw text, a number becomes its shortest decimal
// form, and a JSON object or array becomes its JSON encoding. When a #json-key
// fragment is present the (object) value is decoded and the named field selected
// with mamori.SelectKey, identically to every other provider.
//
// LaunchDarkly holds configuration and flags, not managed secrets, so resolved
// values are not marked Sensitive. Because feature flags have no server-side
// revision identifier exposed to the SDK, Value.Version is a content hash of the
// resolved bytes (mamori.VersionHash), which changes whenever the value changes.
//
// Not-found: when the evaluation reason is an ERROR of kind FLAG_NOT_FOUND (the
// flag does not exist in the environment), Resolve returns an error satisfying
// errors.Is(err, mamori.ErrNotFound).
//
// The provider implements mamori.WatchableProvider using the SDK's native flag
// tracker. Watch subscribes with GetFlagTracker().AddFlagValueChangeListener,
// which streams an event whenever the flag's value for the evaluation context
// changes; each event is turned into an Update. On context cancellation the
// listener is removed and the Update channel is closed, so no goroutine leaks.
//
// Authentication uses a LaunchDarkly server-side SDK key, taken from
// WithSDKKey or the LAUNCHDARKLY_SDK_KEY environment variable. The underlying
// client is created lazily on first Resolve/Watch and connects to LaunchDarkly
// (streaming) at that point; New never contacts the network.
package launchdarkly

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/launchdarkly/go-sdk-common/v3/ldcontext"
	"github.com/launchdarkly/go-sdk-common/v3/ldreason"
	"github.com/launchdarkly/go-sdk-common/v3/ldvalue"
	ldclient "github.com/launchdarkly/go-server-sdk/v7"
	"github.com/launchdarkly/go-server-sdk/v7/interfaces"

	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "launchdarkly"

// defaultContextKey is the key of the evaluation context used when none is set
// via WithContextKey. A stable, non-anonymous key gives deterministic
// evaluations for configuration-style (non-targeted) flags.
const defaultContextKey = "mamori"

// initTimeout bounds how long the lazy client waits to connect to LaunchDarkly
// on first use before proceeding in the background.
const initTimeout = 10 * time.Second

// ldEvaluator is the minimal subset of the LaunchDarkly SDK the provider
// depends on: evaluate a flag with its reason, and subscribe/unsubscribe to
// value-change notifications for a flag. The real *ldclient.LDClient is adapted
// to this shape by realClient, and tests inject an in-memory fake implementing
// the same shape (including value-change streaming), so the conformance kit runs
// with no live LaunchDarkly connection.
type ldEvaluator interface {
	// JSONVariationDetail evaluates flagKey for ctx, returning the value and an
	// EvaluationDetail whose Reason distinguishes a missing flag.
	JSONVariationDetail(flagKey string, ctx ldcontext.Context, defaultVal ldvalue.Value) (ldvalue.Value, ldreason.EvaluationDetail, error)
	// AddFlagValueChangeListener subscribes to value changes for flagKey and ctx,
	// returning a channel that receives an event whenever the value changes.
	AddFlagValueChangeListener(flagKey string, ctx ldcontext.Context, defaultVal ldvalue.Value) <-chan interfaces.FlagValueChangeEvent
	// RemoveFlagValueChangeListener unsubscribes a channel previously returned by
	// AddFlagValueChangeListener.
	RemoveFlagValueChangeListener(listener <-chan interfaces.FlagValueChangeEvent)
	// Close releases the underlying client's resources.
	Close() error
}

// realClient adapts *ldclient.LDClient to ldEvaluator. The flag-tracker methods
// live on the value returned by GetFlagTracker, so they are delegated here.
type realClient struct {
	c *ldclient.LDClient
}

var _ ldEvaluator = realClient{}

func (r realClient) JSONVariationDetail(flagKey string, ctx ldcontext.Context, defaultVal ldvalue.Value) (ldvalue.Value, ldreason.EvaluationDetail, error) {
	return r.c.JSONVariationDetail(flagKey, ctx, defaultVal)
}

func (r realClient) AddFlagValueChangeListener(flagKey string, ctx ldcontext.Context, defaultVal ldvalue.Value) <-chan interfaces.FlagValueChangeEvent {
	return r.c.GetFlagTracker().AddFlagValueChangeListener(flagKey, ctx, defaultVal)
}

func (r realClient) RemoveFlagValueChangeListener(listener <-chan interfaces.FlagValueChangeEvent) {
	r.c.GetFlagTracker().RemoveFlagValueChangeListener(listener)
}

func (r realClient) Close() error { return r.c.Close() }

// Provider resolves launchdarkly:// refs by evaluating LaunchDarkly feature
// flags. It is safe for concurrent use. The underlying SDK client is built
// lazily on first use from the configured SDK key (WithSDKKey or the
// LAUNCHDARKLY_SDK_KEY environment variable) unless a client is injected.
type Provider struct {
	sdkKey     string
	contextKey string

	mu  sync.Mutex
	cli ldEvaluator // resolved client (injected or lazily built)
}

// Option configures a Provider.
type Option func(*Provider)

// WithSDKKey sets the LaunchDarkly server-side SDK key. It overrides the
// LAUNCHDARKLY_SDK_KEY environment variable.
func WithSDKKey(key string) Option {
	return func(p *Provider) { p.sdkKey = key }
}

// WithContextKey sets the key of the evaluation context used for flag
// evaluations. It defaults to "mamori". The context is non-anonymous.
func WithContextKey(key string) Option {
	return func(p *Provider) {
		if key != "" {
			p.contextKey = key
		}
	}
}

// WithClient injects a pre-configured *ldclient.LDClient, bypassing lazy
// construction. Use it when you build the LaunchDarkly client yourself (custom
// configuration, relay proxy, big segments, ...).
func WithClient(c *ldclient.LDClient) Option {
	return func(p *Provider) {
		if c != nil {
			p.cli = realClient{c: c}
		}
	}
}

// withClient injects a bare ldEvaluator. Unexported: used by tests to supply an
// in-memory fake.
func withClient(c ldEvaluator) Option {
	return func(p *Provider) { p.cli = c }
}

// New constructs a LaunchDarkly provider. The client is created lazily on first
// Resolve/Watch, so New never fails and never contacts LaunchDarkly.
func New(opts ...Option) *Provider {
	p := &Provider{contextKey: defaultContextKey}
	for _, opt := range opts {
		opt(p)
	}
	if p.contextKey == "" {
		p.contextKey = defaultContextKey
	}
	return p
}

// init registers a lazily-initialized provider so `import _` wiring works from
// the ambient LAUNCHDARKLY_SDK_KEY configuration. Users who need explicit config
// call mamori.WithProvider(launchdarkly.New(launchdarkly.WithSDKKey("..."))).
func init() { mamori.Register(New()) }

// Scheme returns "launchdarkly".
func (p *Provider) Scheme() string { return scheme }

// evalContext builds the evaluation context used for flag evaluations: a
// non-anonymous context whose key is the configured context key.
func (p *Provider) evalContext() ldcontext.Context {
	return ldcontext.New(p.contextKey)
}

// conn returns the SDK client, building it lazily from the configured SDK key on
// first use. Concurrent callers share one client.
func (p *Provider) conn() (ldEvaluator, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cli != nil {
		return p.cli, nil
	}
	key := p.sdkKey
	if key == "" {
		key = os.Getenv("LAUNCHDARKLY_SDK_KEY")
	}
	if key == "" {
		return nil, fmt.Errorf("launchdarkly: no SDK key configured (set LAUNCHDARKLY_SDK_KEY or use launchdarkly.WithSDKKey)")
	}
	c, err := ldclient.MakeClient(key, initTimeout)
	if c == nil {
		return nil, fmt.Errorf("launchdarkly: create client: %w", err)
	}
	// A non-nil error alongside a non-nil client means initialization timed out
	// or failed; the client keeps retrying in the background and evaluations fall
	// back to defaults until it connects. We keep the usable client and let the
	// not-found / evaluation semantics surface the state per resolve.
	p.cli = realClient{c: c}
	return p.cli, nil
}

// Resolve evaluates the flag named by ref for the provider's evaluation context.
// A flag that does not exist yields an error satisfying
// errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	cli, err := p.conn()
	if err != nil {
		return mamori.Value{}, err
	}
	val, detail, evalErr := cli.JSONVariationDetail(ref.Path, p.evalContext(), ldvalue.Null())
	if isFlagNotFound(detail.Reason) {
		return mamori.Value{}, fmt.Errorf("launchdarkly: flag %q: %w", ref.Path, mamori.ErrNotFound)
	}
	if evalErr != nil {
		return mamori.Value{}, fmt.Errorf("launchdarkly: evaluate flag %q: %w", ref.Path, evalErr)
	}
	return valueFor(val, ref)
}

// Watch implements mamori.WatchableProvider using LaunchDarkly's native flag
// tracker. It subscribes to value-change events for the flag and evaluation
// context and emits an Update for each change. The listener is removed and the
// Update channel closed when ctx is cancelled, so the goroutine exits with no
// leak.
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	cli, err := p.conn()
	if err != nil {
		return nil, err
	}
	events := cli.AddFlagValueChangeListener(ref.Path, p.evalContext(), ldvalue.Null())

	ch := make(chan mamori.Update, 1)
	go func() {
		defer close(ch)
		defer cli.RemoveFlagValueChangeListener(events)

		emit := func(u mamori.Update) bool {
			select {
			case ch <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}

		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				v, verr := valueFor(ev.NewValue, ref)
				if !emit(mamori.Update{Value: v, Err: verr}) {
					return
				}
			}
		}
	}()
	return ch, nil
}

// isFlagNotFound reports whether reason indicates the flag does not exist: an
// ERROR reason whose error kind is FLAG_NOT_FOUND.
func isFlagNotFound(reason ldreason.EvaluationReason) bool {
	return reason.GetKind() == ldreason.EvalReasonError &&
		reason.GetErrorKind() == ldreason.EvalErrorFlagNotFound
}

// valueFor converts an evaluated flag value into a mamori.Value, applying
// #json-key selection when requested and hashing the bytes for the version.
func valueFor(v ldvalue.Value, ref mamori.Ref) (mamori.Value, error) {
	b := flagValueToBytes(v)
	if ref.Key != "" {
		sel, err := mamori.SelectKey(b, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		b = sel
	}
	return mamori.Value{
		Bytes:     b,
		Version:   mamori.VersionHash(b),
		Sensitive: false,
	}, nil
}

// flagValueToBytes converts a LaunchDarkly flag value to its byte form: a bool
// becomes "true"/"false", a string its raw text, a number its shortest decimal
// form, and any other type (object, array, null) its JSON encoding.
func flagValueToBytes(v ldvalue.Value) []byte {
	switch v.Type() {
	case ldvalue.BoolType:
		if v.BoolValue() {
			return []byte("true")
		}
		return []byte("false")
	case ldvalue.StringType:
		return []byte(v.StringValue())
	case ldvalue.NumberType:
		if v.IsInt() {
			return []byte(strconv.Itoa(v.IntValue()))
		}
		return []byte(strconv.FormatFloat(v.Float64Value(), 'g', -1, 64))
	default:
		// Object, Array, Null, Raw: use the JSON encoding.
		return []byte(v.JSONString())
	}
}
