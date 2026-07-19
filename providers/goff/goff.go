// Package goff implements a mamori provider for GO Feature Flag
// (github.com/thomaspoignant/go-feature-flag), the standalone, file-driven
// feature-flag engine (the embedded ffclient, not a remote relay/vendor SaaS).
//
// The scheme is "goff" and the ref grammar is:
//
//	goff://<flag-key>[#json-key]
//
// A ref resolves to the flag's evaluated variation for an evaluation context.
// By default the context is an anonymous user whose targeting key is "mamori"
// (override with WithTargetingKey). The evaluated variation is rendered to bytes
// by type:
//
//	bool   -> "true" / "false"
//	string -> the raw string
//	number -> its decimal string form
//	JSON   -> its JSON encoding (object or array)
//
//	Beta       bool   `source:"goff://new-checkout"`
//	Ratelimit  int    `source:"goff://api-ratelimit"`
//	Theme      string `source:"goff://ui-config#theme"`
//
// When a #json-key fragment is present the JSON variation is treated as a JSON
// object and the named field is selected with mamori.SelectKey, identically to
// every other mamori provider.
//
// A flag that does not exist resolves to an error satisfying
// errors.Is(err, mamori.ErrNotFound) (go-feature-flag reports a FLAG_NOT_FOUND
// error code / ERROR reason, which the provider detects). Flag values are
// configuration, not managed secrets, so resolved values are not marked
// Sensitive. Value.Version is mamori.VersionHash of the rendered bytes, so
// change detection is exact and cheap.
//
// GO Feature Flag loads its flag definitions from a retriever - a local file, an
// HTTP URL, S3, and so on - and reloads them on a polling interval. This
// provider therefore does NOT implement mamori.WatchableProvider: mamori polls
// Resolve, and go-feature-flag independently refreshes its in-memory cache, so a
// changed flag file is picked up without any native push mechanism.
package goff

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	ffclient "github.com/thomaspoignant/go-feature-flag"
	"github.com/thomaspoignant/go-feature-flag/modules/core/ffcontext"
	"github.com/thomaspoignant/go-feature-flag/modules/core/flag"
	"github.com/thomaspoignant/go-feature-flag/modules/core/model"
	"github.com/thomaspoignant/go-feature-flag/retriever"
	"github.com/thomaspoignant/go-feature-flag/retriever/fileretriever"
	"github.com/thomaspoignant/go-feature-flag/retriever/httpretriever"
	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "goff"

// defaultTargetingKey is the key of the anonymous evaluation context used when
// the caller does not set one with WithTargetingKey.
const defaultTargetingKey = "mamori"

// defaultPollingInterval is how often go-feature-flag reloads its flag
// definitions from the configured retriever. It matches the library default.
const defaultPollingInterval = 60 * time.Second

// configEnv is the environment variable read for the default flag-configuration
// source. A value beginning with http:// or https:// is treated as an HTTP
// retriever URL; anything else is treated as a local file path.
const configEnv = "GOFF_CONFIG"

// evaluator is the minimal subset of the go-feature-flag client the provider
// depends on. The real *ffclient.GoFeatureFlag returned by ffclient.New
// satisfies it directly, and tests inject an in-memory fake implementing the
// same shape so the conformance kit runs without any flag-configuration file.
type evaluator interface {
	// RawVariation evaluates flagKey for evalCtx, returning the raw variation
	// value together with its resolution metadata (variation type, reason, and
	// error code for a missing flag).
	RawVariation(flagKey string, evalCtx ffcontext.Context, defaultValue any) (model.RawVarResult, error)
}

// compile-time check that the real client satisfies evaluator.
var _ evaluator = (*ffclient.GoFeatureFlag)(nil)

// Provider resolves goff:// refs against a GO Feature Flag engine. It is safe
// for concurrent use. The underlying go-feature-flag client is built lazily on
// first Resolve from the configured retriever (WithConfigFile / WithConfigURL,
// or the GOFF_CONFIG environment variable) unless an evaluator is injected for
// testing.
type Provider struct {
	targetingKey    string
	configFile      string
	configURL       string
	pollingInterval time.Duration

	mu sync.Mutex
	ev evaluator // resolved client (injected or lazily built)
}

// Option configures a Provider.
type Option func(*Provider)

// WithTargetingKey sets the targeting (user) key of the anonymous evaluation
// context used for every resolution. It selects which variation percentage
// bucket / targeting rule a flag evaluates to. Default: "mamori".
func WithTargetingKey(key string) Option {
	return func(p *Provider) { p.targetingKey = key }
}

// WithConfigFile configures go-feature-flag to load its flag definitions from a
// local file (YAML, JSON, or TOML). It overrides the GOFF_CONFIG environment
// variable.
func WithConfigFile(path string) Option {
	return func(p *Provider) { p.configFile = path }
}

// WithConfigURL configures go-feature-flag to load its flag definitions from an
// HTTP(S) endpoint. It overrides the GOFF_CONFIG environment variable.
func WithConfigURL(url string) Option {
	return func(p *Provider) { p.configURL = url }
}

// WithPollingInterval overrides how often go-feature-flag reloads flag
// definitions from the retriever (default 60s). It only affects how quickly a
// changed flag file is observed; mamori polls Resolve independently.
func WithPollingInterval(d time.Duration) Option {
	return func(p *Provider) { p.pollingInterval = d }
}

// withEvaluator injects a bare evaluator. Unexported: used by tests to supply an
// in-memory fake and by callers who build the client themselves.
func withEvaluator(ev evaluator) Option {
	return func(p *Provider) { p.ev = ev }
}

// New constructs a GO Feature Flag provider. The go-feature-flag client is
// created lazily on first Resolve, so New never fails and never reads a config
// file or contacts an HTTP endpoint.
func New(opts ...Option) *Provider {
	p := &Provider{
		targetingKey:    defaultTargetingKey,
		pollingInterval: defaultPollingInterval,
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.targetingKey == "" {
		p.targetingKey = defaultTargetingKey
	}
	return p
}

// init registers a lazily-initialized provider so `import _` wiring works from
// the ambient GOFF_CONFIG environment variable. Users who need explicit config
// call mamori.WithProvider(goff.New(goff.WithConfigFile("flags.yaml"))).
func init() { mamori.Register(New()) }

// Scheme returns "goff".
func (p *Provider) Scheme() string { return scheme }

// client returns the evaluator, building the real go-feature-flag client lazily
// from the configured retriever on first use. Concurrent callers share one
// client. The client starts a background poller that reloads the flag
// definitions on the polling interval for the lifetime of the process.
func (p *Provider) client() (evaluator, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ev != nil {
		return p.ev, nil
	}
	r, err := p.retriever()
	if err != nil {
		return nil, err
	}
	c, err := ffclient.New(ffclient.Config{
		PollingInterval: p.pollingInterval,
		Retriever:       r,
	})
	if err != nil {
		return nil, fmt.Errorf("goff: init client: %w", err)
	}
	p.ev = c
	return p.ev, nil
}

// retriever selects the flag-definition source from the explicit options or the
// GOFF_CONFIG environment variable.
func (p *Provider) retriever() (retriever.Retriever, error) {
	switch {
	case p.configURL != "":
		return &httpretriever.Retriever{URL: p.configURL}, nil
	case p.configFile != "":
		return &fileretriever.Retriever{Path: p.configFile}, nil
	}
	if env := strings.TrimSpace(os.Getenv(configEnv)); env != "" {
		if strings.HasPrefix(env, "http://") || strings.HasPrefix(env, "https://") {
			return &httpretriever.Retriever{URL: env}, nil
		}
		return &fileretriever.Retriever{Path: env}, nil
	}
	return nil, fmt.Errorf("goff: no flag configuration source; set %s or use WithConfigFile/WithConfigURL", configEnv)
}

// Resolve evaluates ref's flag for the provider's anonymous evaluation context.
// A flag that does not exist yields an error satisfying
// errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	if ref.Path == "" {
		return mamori.Value{}, fmt.Errorf("goff: empty flag key in ref %q", ref.Raw)
	}
	ev, err := p.client()
	if err != nil {
		return mamori.Value{}, err
	}

	evalCtx := ffcontext.NewAnonymousEvaluationContext(p.targetingKey)
	res, err := ev.RawVariation(ref.Path, evalCtx, nil)
	if isNotFound(res, err) {
		return mamori.Value{}, fmt.Errorf("goff: flag %q: %w", ref.Path, mamori.ErrNotFound)
	}
	if err != nil {
		return mamori.Value{}, fmt.Errorf("goff: evaluate flag %q: %w", ref.Path, err)
	}
	if res.Failed {
		return mamori.Value{}, fmt.Errorf("goff: flag %q evaluation failed (reason=%s, errorCode=%s): %s",
			ref.Path, res.Reason, res.ErrorCode, res.ErrorDetails)
	}

	b, err := render(res.Value)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("goff: flag %q: %w", ref.Path, err)
	}
	if ref.Key != "" {
		sel, err := mamori.SelectKey(b, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		b = sel
	}

	md := map[string]string{"variationType": res.VariationType, "reason": string(res.Reason)}
	if res.Version != "" {
		md["flagVersion"] = res.Version
	}
	return mamori.Value{
		Bytes:     b,
		Version:   mamori.VersionHash(b),
		Sensitive: false,
		Metadata:  md,
	}, nil
}

// isNotFound reports whether a RawVariation call indicates that the flag does
// not exist. go-feature-flag signals this with a FLAG_NOT_FOUND error code; as a
// fallback the returned error message is inspected for the same condition.
func isNotFound(res model.RawVarResult, err error) bool {
	if res.ErrorCode == flag.ErrorCodeFlagNotFound {
		return true
	}
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "was not found") || strings.Contains(msg, "does not exist") {
			return true
		}
	}
	return false
}

// render converts an evaluated variation value into its mamori byte form:
// booleans become "true"/"false", strings pass through unchanged, numbers become
// their decimal string, and everything else (JSON objects and arrays) becomes
// its JSON encoding.
func render(v any) ([]byte, error) {
	switch val := v.(type) {
	case nil:
		return nil, fmt.Errorf("evaluated to a nil value")
	case bool:
		return []byte(strconv.FormatBool(val)), nil
	case string:
		return []byte(val), nil
	case float64:
		return []byte(strconv.FormatFloat(val, 'f', -1, 64)), nil
	case float32:
		return []byte(strconv.FormatFloat(float64(val), 'f', -1, 32)), nil
	case int:
		return []byte(strconv.Itoa(val)), nil
	case int64:
		return []byte(strconv.FormatInt(val, 10)), nil
	case json.Number:
		return []byte(val), nil
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return nil, fmt.Errorf("encode variation of type %T: %w", v, err)
		}
		return b, nil
	}
}
