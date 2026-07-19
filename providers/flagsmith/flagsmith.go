// Package flagsmith implements a mamori provider for Flagsmith
// (https://www.flagsmith.com), the open-source feature-flag and remote-config
// platform.
//
// It uses the official Flagsmith Go SDK
// (github.com/Flagsmith/flagsmith-go-client/v4) in its default remote-evaluation
// mode: each resolve fetches the environment's flags from the Flagsmith API and
// looks up a single feature by name.
//
// # Scheme
//
//	flagsmith://<feature-name>[#enabled]
//
// The path is the feature name. With no fragment the provider returns the
// feature's value (the Flagsmith feature_state_value) as bytes. The reserved
// fragment #enabled returns the feature's enabled state as the literal text
// "true" or "false":
//
//	Banner   string `source:"flagsmith://homepage_banner"`
//	NewFlow  bool   `source:"flagsmith://new_checkout_flow#enabled"`
//
// Any other #fragment selects a field from a JSON-object feature value, exactly
// like every other mamori provider (see mamori.SelectKey).
//
// # Authentication
//
// The provider authenticates with a Flagsmith environment key supplied either
// explicitly via WithEnvironmentKey or, when unset, from the
// FLAGSMITH_ENVIRONMENT_KEY environment variable read lazily at first resolve.
// WithBaseURL points the provider at a self-hosted Flagsmith API.
//
// # Versioning and sensitivity
//
// Flagsmith exposes no per-feature revision identifier over the flags API, so
// Value.Version is a content hash (mamori.VersionHash) of the returned bytes,
// which still gives mamori cheap, correct change detection. Feature flags are
// configuration, not secrets, so values are NOT marked Sensitive.
//
// # Watch
//
// Flagsmith's flags API has no native change-notification surface exposed here,
// so this provider is intentionally not watchable; mamori wraps it in its
// polling adapter automatically. (The SDK can also refresh its own cache in
// local-evaluation mode, which this provider does not enable.)
package flagsmith

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"

	flagsmith "github.com/Flagsmith/flagsmith-go-client/v4"
	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "flagsmith"

// enabledKey is the reserved #fragment that selects a feature's enabled state
// instead of its value.
const enabledKey = "enabled"

// featureState is the resolved state of a single Flagsmith feature: its value
// and whether it is enabled.
type featureState struct {
	value   any
	enabled bool
}

// flagSource is the minimal surface the provider needs from Flagsmith. The real
// SDK client satisfies it via sdkSource; tests inject an in-memory fake. An
// implementation MUST return an error satisfying errors.Is(err,
// mamori.ErrNotFound) when the named feature is absent.
type flagSource interface {
	getFeature(ctx context.Context, name string) (featureState, error)
}

// Provider resolves flagsmith:// refs against the Flagsmith API. It is safe for
// concurrent use.
type Provider struct {
	envKey  string
	baseURL string

	mu  sync.Mutex
	src flagSource // built lazily on first resolve, or injected for tests
}

// Option configures a Provider.
type Option func(*Provider)

// WithEnvironmentKey sets the Flagsmith environment key explicitly. When unset,
// the provider reads FLAGSMITH_ENVIRONMENT_KEY from the environment at first
// resolve.
func WithEnvironmentKey(key string) Option {
	return func(p *Provider) { p.envKey = key }
}

// WithBaseURL points the provider at a self-hosted Flagsmith API base URL (for
// example https://flagsmith.example.com/api/v1/). When unset the public
// Flagsmith SaaS API is used.
func WithBaseURL(url string) Option {
	return func(p *Provider) { p.baseURL = url }
}

// New constructs a Flagsmith provider. Without options it targets the public
// Flagsmith API and reads FLAGSMITH_ENVIRONMENT_KEY lazily at first resolve, so
// it is safe to register from init even when no key is present at process start.
//
// Users who need explicit configuration call
// mamori.WithProvider(flagsmith.New(flagsmith.WithEnvironmentKey("ser...."))).
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, o := range opts {
		o(p)
	}
	return p
}

// newWithSource builds a provider backed by an already-constructed flag source.
// It is the injection seam used by tests and the conformance kit.
func newWithSource(src flagSource) *Provider {
	return &Provider{src: src}
}

func init() { mamori.Register(New()) }

// Scheme returns "flagsmith".
func (p *Provider) Scheme() string { return scheme }

// source returns the flag source, building the real Flagsmith-backed one on
// first use. Constructing the SDK client performs no network I/O and (in the
// default remote-evaluation mode used here) starts no background goroutines.
func (p *Provider) source() (flagSource, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.src != nil {
		return p.src, nil
	}
	key := p.envKey
	if key == "" {
		key = os.Getenv("FLAGSMITH_ENVIRONMENT_KEY")
	}
	if key == "" {
		return nil, errors.New("mamori/flagsmith: no environment key; set FLAGSMITH_ENVIRONMENT_KEY or use flagsmith.WithEnvironmentKey")
	}
	var opts []flagsmith.Option
	if p.baseURL != "" {
		opts = append(opts, flagsmith.WithBaseURL(p.baseURL))
	}
	p.src = &sdkSource{client: flagsmith.NewClient(key, opts...)}
	return p.src, nil
}

// Resolve fetches the feature named by ref.Path. With no key it returns the
// feature value; with #enabled it returns the enabled state; with any other key
// it selects a field from a JSON-object value. A missing feature is reported as
// ErrNotFound.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	name := ref.Path
	if name == "" {
		return mamori.Value{}, fmt.Errorf("mamori/flagsmith: ref %q requires a feature name", ref.Raw)
	}

	src, err := p.source()
	if err != nil {
		return mamori.Value{}, err
	}

	st, err := src.getFeature(ctx, name)
	if err != nil {
		return mamori.Value{}, err // ErrNotFound (and real errors) propagate unchanged
	}

	var b []byte
	switch ref.Key {
	case "":
		b = valueToBytes(st.value)
	case enabledKey:
		b = []byte(strconv.FormatBool(st.enabled))
	default:
		// Honor the shared SPI rule: select a field from a JSON-object value.
		b, err = mamori.SelectKey(valueToBytes(st.value), ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}

	return mamori.Value{
		Bytes:     b,
		Version:   mamori.VersionHash(b),
		Sensitive: false,
		Metadata: map[string]string{
			"feature": name,
		},
	}, nil
}

// valueToBytes renders a Flagsmith feature value (string, number, bool, JSON, or
// null) as bytes. Strings are returned unquoted; other types are JSON-encoded so
// that, for example, a boolean becomes "true" and a number its decimal text.
func valueToBytes(v any) []byte {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return []byte(t)
	case []byte:
		return t
	default:
		if b, err := json.Marshal(t); err == nil {
			return b
		}
		return []byte(fmt.Sprintf("%v", t))
	}
}

// sdkSource adapts the real Flagsmith SDK client to flagSource.
type sdkSource struct {
	client *flagsmith.Client
}

// getFeature fetches the environment's flags and looks up name. A network or API
// failure is returned as-is; an absent feature is reported as ErrNotFound.
func (s *sdkSource) getFeature(ctx context.Context, name string) (featureState, error) {
	flags, err := s.client.GetEnvironmentFlags(ctx)
	if err != nil {
		return featureState{}, fmt.Errorf("mamori/flagsmith: fetching environment flags: %w", err)
	}
	flag, err := flags.GetFlag(name)
	// GetFlag errors only when the feature is absent (this provider sets no
	// default-flag handler). IsDefault guards the same case defensively.
	if err != nil || flag.IsDefault {
		return featureState{}, fmt.Errorf("mamori/flagsmith: feature %q not found: %w", name, mamori.ErrNotFound)
	}
	return featureState{value: flag.Value, enabled: flag.Enabled}, nil
}
