// Package configcat implements a mamori Provider backed by ConfigCat
// (https://configcat.com), a feature-flag and configuration service.
//
// It resolves refs of the form:
//
//	configcat://<setting-key>
//
// where <setting-key> is the key of a feature flag or setting as defined in the
// ConfigCat dashboard. The resolved Value is the evaluated setting rendered as
// text:
//
//	Enabled bool   `source:"configcat://isPOCFeatureEnabled"`
//	Ratio   string `source:"configcat://samplingRatio"`
//
// Booleans resolve to "true"/"false", strings resolve to their raw text, and
// numbers resolve to their decimal string form.
//
// # Authentication
//
// The provider authenticates with a ConfigCat SDK key supplied either explicitly
// via WithSDKKey or, when unset, from the CONFIGCAT_SDK_KEY environment variable
// read lazily when the underlying client is first built. This lets the provider
// register itself from init even before the environment is populated.
//
// # Versioning and sensitivity
//
// ConfigCat exposes no per-setting revision identifier that is stable across
// evaluations, so Value.Version is a content hash (mamori.VersionHash), which
// still gives mamori cheap, correct change detection. Feature-flag values are
// configuration, not secrets, so Value.Sensitive is false.
//
// # Watching
//
// The ConfigCat SDK auto-polls the CDN in the background (AutoPoll mode), but it
// exposes no push-style change notification that mamori can subscribe to. This
// provider therefore does NOT implement WatchableProvider; mamori wraps it in
// its polling adapter automatically. Configure the cadence with
// mamori.WithPollInterval (mamori side) and/or WithPollInterval (SDK side).
package configcat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	configcat "github.com/configcat/go-sdk/v9"

	"github.com/xavidop/mamori"
)

// Scheme is the URL scheme handled by this provider.
const Scheme = "configcat"

// envSDKKey is the environment variable read for ambient configuration.
const envSDKKey = "CONFIGCAT_SDK_KEY"

func init() { mamori.Register(New()) }

// settingClient is the minimal surface of the ConfigCat SDK that this provider
// depends on: list the available setting keys and evaluate one. The real SDK is
// adapted to it by sdkClient; tests inject an in-memory fake. Keeping the
// dependency this small is what lets the conformance suite run without a live
// backend.
type settingClient interface {
	// keys returns every setting key present in the currently loaded config.
	keys() []string
	// value returns the evaluated value for key (bool, string, int, or float64).
	// It is only called for keys known to be present.
	value(key string) any
	// close releases any resources (background polling goroutines) held.
	close()
}

// Provider resolves configcat:// refs against ConfigCat. It is safe for
// concurrent use. The underlying SDK client is built lazily on first Resolve and
// then reused, since the SDK maintains a background poll of the config.
type Provider struct {
	sdkKey       string
	pollInterval time.Duration

	mu  sync.Mutex
	cli settingClient // built lazily (or injected in tests)
}

// Option configures a Provider.
type Option func(*Provider)

// WithSDKKey sets the ConfigCat SDK key explicitly. When unset, the provider
// reads CONFIGCAT_SDK_KEY from the environment when the client is first built.
func WithSDKKey(key string) Option {
	return func(p *Provider) { p.sdkKey = key }
}

// WithPollInterval overrides how old the cached config may be before the SDK
// refreshes it in the background. Values less than 1 leave the SDK default (60s)
// in place. This is the SDK-side cadence; mamori's own poll interval (which
// drives re-resolution of your struct) is configured separately with
// mamori.WithPollInterval.
func WithPollInterval(d time.Duration) Option {
	return func(p *Provider) { p.pollInterval = d }
}

// New constructs a ConfigCat provider. Without options it reads CONFIGCAT_SDK_KEY
// lazily when the client is first needed, so it is safe to register from init
// even when no key is present at process start.
//
// Users who need explicit configuration register via:
//
//	mamori.WithProvider(configcat.New(configcat.WithSDKKey("configcat-sdk-1/...")))
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, o := range opts {
		o(p)
	}
	return p
}

// newWithClient builds a Provider around an already-constructed settingClient.
// It is the injection seam used by tests (and the conformance kit) to run
// against an in-memory fake instead of the live ConfigCat CDN.
func newWithClient(c settingClient) *Provider {
	return &Provider{cli: c}
}

// Scheme returns "configcat".
func (p *Provider) Scheme() string { return Scheme }

// Resolve evaluates the setting named by the ref and returns its value as text.
// A key that is not present in the loaded config resolves to an error satisfying
// errors.Is(err, mamori.ErrNotFound); the SDK's default value is never returned
// for a missing key.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}

	key := settingKey(ref)
	if key == "" {
		return mamori.Value{}, fmt.Errorf("configcat: ref %q has no setting key", ref.Raw)
	}

	cli, err := p.ensureClient(ctx)
	if err != nil {
		return mamori.Value{}, err
	}

	// Not-found is decided by the config's key set, never by the SDK's default
	// value: an absent key is a real not-found, not a flag that happens to
	// evaluate to a default.
	if !contains(cli.keys(), key) {
		return mamori.Value{}, fmt.Errorf("configcat: setting %q not found in config: %w", key, mamori.ErrNotFound)
	}

	text, err := stringify(cli.value(key))
	if err != nil {
		return mamori.Value{}, fmt.Errorf("configcat: setting %q: %w", key, err)
	}
	b := []byte(text)

	// Honor the shared #key convention: when the caller selects a field and the
	// setting's string value is a JSON object, extract that field identically to
	// every other provider. With no #key this returns the bytes unchanged.
	b, err = mamori.SelectKey(b, ref.Key)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("configcat: setting %q: %w", key, err)
	}

	return mamori.Value{
		Bytes:     b,
		Version:   mamori.VersionHash(b),
		Sensitive: false,
		Metadata:  map[string]string{"key": key},
	}, nil
}

// Close releases the underlying SDK client and its background polling goroutine.
// It is safe to call more than once and on a provider that never resolved.
func (p *Provider) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cli != nil {
		p.cli.close()
		p.cli = nil
	}
}

// ensureClient returns the underlying client, building the real SDK client on
// first use. Construction is guarded so concurrent Resolves share one client.
func (p *Provider) ensureClient(ctx context.Context) (settingClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cli != nil {
		return p.cli, nil
	}
	key := p.effectiveSDKKey()
	if key == "" {
		return nil, fmt.Errorf("configcat: no SDK key; set %s or use configcat.WithSDKKey", envSDKKey)
	}
	cfg := configcat.Config{
		SDKKey:      key,
		PollingMode: configcat.AutoPoll,
	}
	if p.pollInterval > 0 {
		cfg.PollInterval = p.pollInterval
	}
	c, err := newSDKClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	p.cli = c
	return c, nil
}

// effectiveSDKKey returns the configured key, or CONFIGCAT_SDK_KEY read lazily.
func (p *Provider) effectiveSDKKey() string {
	if p.sdkKey != "" {
		return p.sdkKey
	}
	return os.Getenv(envSDKKey)
}

// settingKey extracts the ConfigCat setting key from a ref. The whole path is
// the key; leading/trailing slashes (e.g. from configcat:///key) are trimmed.
func settingKey(ref mamori.Ref) string {
	return strings.Trim(ref.Path, "/")
}

// stringify renders an evaluated ConfigCat value as the text mamori stores.
// Booleans become "true"/"false", strings pass through, and numbers use their
// decimal form.
func stringify(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case nil:
		return "", errors.New("evaluated to a nil value")
	default:
		return "", fmt.Errorf("unsupported value type %T", v)
	}
}

// contains reports whether s is an element of list.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- Real SDK adapter --------------------------------------------------------

// newSDKClient builds the live ConfigCat client and waits for its first config
// fetch (bounded by ctx) so GetAllKeys reflects the real config on the first
// Resolve. It is a package var so tests can stub the live path if needed.
var newSDKClient = func(ctx context.Context, cfg configcat.Config) (settingClient, error) {
	c := configcat.NewCustomClient(cfg)
	select {
	case <-c.Ready():
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	}
	return &sdkClient{c: c}, nil
}

// sdkClient adapts *configcat.Client to the settingClient interface.
type sdkClient struct{ c *configcat.Client }

func (s *sdkClient) keys() []string { return s.c.GetAllKeys() }

func (s *sdkClient) value(key string) any {
	// A nil User uses the client's default targeting context, which is correct
	// for machine-scoped configuration resolution.
	return s.c.Snapshot(nil).GetValue(key)
}

func (s *sdkClient) close() { s.c.Close() }

// Ensure Provider satisfies the core interface but NOT WatchableProvider: the
// ConfigCat SDK offers no push notification, so mamori polls this provider.
var _ mamori.Provider = (*Provider)(nil)
