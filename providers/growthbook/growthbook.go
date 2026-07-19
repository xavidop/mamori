// Package growthbook implements a mamori provider for GrowthBook feature flags.
//
// It registers the "growthbook" scheme. Refs take the form:
//
//	growthbook://<feature-key>[#json-key]
//
// where <feature-key> is the id of a feature in the GrowthBook project and the
// optional #json-key selects a field from a JSON-object feature value:
//
//	Banner     string `source:"growthbook://show_banner"`
//	NewUI      string `source:"growthbook://feature_flags#new_ui"`
//	MaxItems   int    `source:"growthbook://feature_flags#max_items"`
//
// The provider loads the current feature set (from the GrowthBook Features API,
// or from an offline features JSON) and evaluates the named feature with the
// GrowthBook Go SDK. The feature's evaluated value is returned encoded as bytes:
//
//	bool   -> "true" / "false"
//	string -> the raw string
//	number -> its decimal string form
//	object / array / null -> its JSON encoding
//
// A feature that is not present in the loaded feature set resolves to an error
// satisfying errors.Is(err, mamori.ErrNotFound), so mamori applies your default
// or optional handling.
//
// Authentication uses a GrowthBook SDK client key and (optionally) an API host,
// supplied with WithClientKey and WithAPIHost. For offline / air-gapped use,
// supply the raw features JSON with WithFeatures and no network access is
// performed. The underlying SDK client is created lazily on first Resolve, so
// registration never fails for lack of configuration.
//
// GrowthBook exposes a streaming (SSE) endpoint, but this provider deliberately
// does not implement native Watch: mamori polls it on the configured interval,
// re-reading the feature set each poll. This keeps the provider free of
// background goroutines.
package growthbook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	gb "github.com/growthbook/growthbook-golang"
	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "growthbook"

// evaluator loads the current GrowthBook feature set and evaluates a single
// feature. The real SDK-backed implementation (sdkEvaluator) satisfies it; tests
// inject an in-memory fake. This is the minimal surface the provider needs: a
// value plus whether the feature exists in the loaded set.
type evaluator interface {
	// evaluateFeature loads the current feature set (refreshing from the
	// GrowthBook API for API-backed providers) and evaluates the named feature.
	// found is false when the feature is not present in the loaded feature set.
	evaluateFeature(ctx context.Context, key string) (val any, found bool, err error)
}

// Provider resolves growthbook:// refs by evaluating GrowthBook features. It is
// safe for concurrent use.
type Provider struct {
	mu   sync.Mutex
	eval evaluator

	// Configuration used to build the default (SDK) evaluator lazily on first use.
	clientKey     string
	apiHost       string
	featuresJSON  string
	decryptionKey string
	httpClient    *http.Client
}

// Option configures a Provider.
type Option func(*Provider)

// WithClientKey sets the GrowthBook SDK client key used to fetch the feature set
// from the GrowthBook Features API. Required for API-backed operation (unless
// WithFeatures supplies an offline feature set).
func WithClientKey(key string) Option {
	return func(p *Provider) { p.clientKey = key }
}

// WithAPIHost sets the GrowthBook API host (e.g. "https://cdn.growthbook.io" for
// GrowthBook Cloud, or your self-hosted GrowthBook API URL). If unset, the SDK
// default host (https://cdn.growthbook.io) is used.
func WithAPIHost(host string) Option {
	return func(p *Provider) { p.apiHost = host }
}

// WithFeatures supplies a raw features JSON payload (the "features" object of a
// GrowthBook SDK payload) to evaluate offline. When set, the provider performs
// no network access: it never contacts the GrowthBook API. Use it for
// air-gapped deployments or to pin a known feature set.
func WithFeatures(featuresJSON string) Option {
	return func(p *Provider) { p.featuresJSON = featuresJSON }
}

// WithDecryptionKey sets the key used to decrypt an encrypted GrowthBook feature
// payload. When combined with WithFeatures, the supplied JSON is treated as an
// encrypted payload; when combined with API-backed operation, encrypted API
// responses are decrypted with this key.
func WithDecryptionKey(key string) Option {
	return func(p *Provider) { p.decryptionKey = key }
}

// WithHTTPClient injects the HTTP client used for GrowthBook API calls. Useful
// for custom transports, proxies, or tests against an httptest server.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) { p.httpClient = c }
}

// withEvaluator injects an evaluator directly, bypassing SDK construction. Tests
// use it to supply an in-memory fake.
func withEvaluator(e evaluator) Option {
	return func(p *Provider) { p.eval = e }
}

// New constructs a GrowthBook provider. By default the underlying SDK client is
// created lazily on first Resolve, so New never contacts the network and never
// fails for lack of configuration.
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, o := range opts {
		o(p)
	}
	return p
}

func init() { mamori.Register(New()) }

// Scheme returns "growthbook".
func (p *Provider) Scheme() string { return scheme }

// getEvaluator returns the backing evaluator, building the default SDK-backed
// evaluator lazily (and caching it) on first use.
func (p *Provider) getEvaluator(ctx context.Context) (evaluator, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.eval != nil {
		return p.eval, nil
	}
	e, err := p.buildEvaluator(ctx)
	if err != nil {
		return nil, err
	}
	p.eval = e
	return e, nil
}

// buildEvaluator constructs the default SDK-backed evaluator from the provider's
// configuration. Callers must hold p.mu.
func (p *Provider) buildEvaluator(ctx context.Context) (evaluator, error) {
	// Offline mode: features supplied directly, no API access.
	if p.featuresJSON != "" {
		opts := []gb.ClientOption{}
		if p.decryptionKey != "" {
			opts = append(opts,
				gb.WithDecryptionKey(p.decryptionKey),
				gb.WithEncryptedJsonFeatures(p.featuresJSON),
			)
		} else {
			opts = append(opts, gb.WithJsonFeatures(p.featuresJSON))
		}
		client, err := gb.NewClient(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("growthbook: building offline client: %w", err)
		}
		// refresh: false - the offline feature set never changes and there is no
		// API to poll.
		return &sdkEvaluator{client: client, refresh: false}, nil
	}

	if p.clientKey == "" {
		return nil, fmt.Errorf("growthbook: no client key; set one with WithClientKey (and optionally WithAPIHost), or supply an offline feature set with WithFeatures")
	}

	opts := []gb.ClientOption{gb.WithClientKey(p.clientKey)}
	if p.apiHost != "" {
		opts = append(opts, gb.WithApiHost(p.apiHost))
	}
	if p.httpClient != nil {
		opts = append(opts, gb.WithHttpClient(p.httpClient))
	}
	if p.decryptionKey != "" {
		opts = append(opts, gb.WithDecryptionKey(p.decryptionKey))
	}
	client, err := gb.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("growthbook: building API client: %w", err)
	}
	// refresh: true - re-fetch the feature set from the API on each Resolve so
	// each mamori poll observes the current values.
	return &sdkEvaluator{client: client, refresh: true}, nil
}

// Close releases resources held by a lazily-built SDK client, if any. It is safe
// to call multiple times.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if se, ok := p.eval.(*sdkEvaluator); ok && se.client != nil {
		return se.client.Close()
	}
	return nil
}

// Resolve loads the current feature set and returns the evaluated value of the
// feature named by ref.Path. When ref.Key is set, the JSON payload field is
// selected. A feature that is not present in the loaded set returns an error
// satisfying errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	if ref.Path == "" {
		return mamori.Value{}, fmt.Errorf("growthbook: ref %q must be of the form growthbook://<feature-key>[#json-key]", ref.Raw)
	}

	ev, err := p.getEvaluator(ctx)
	if err != nil {
		return mamori.Value{}, err
	}

	val, found, err := ev.evaluateFeature(ctx, ref.Path)
	if err != nil {
		return mamori.Value{}, err
	}
	if !found {
		return mamori.Value{}, fmt.Errorf("growthbook: feature %q not found in feature set: %w", ref.Path, mamori.ErrNotFound)
	}

	data, err := encodeFeatureValue(val)
	if err != nil {
		return mamori.Value{}, err
	}

	// Version is derived from the full evaluated feature value, so it changes
	// whenever the feature's value changes (mamori.VersionHash gives cheap change
	// detection without a byte comparison). The backend has no per-feature native
	// revision to use instead.
	ver := mamori.VersionHash(data)

	if ref.Key != "" {
		data, err = mamori.SelectKey(data, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}

	return mamori.Value{
		Bytes:     data,
		Version:   ver,
		Sensitive: false,
	}, nil
}

// encodeFeatureValue renders an evaluated GrowthBook feature value as bytes:
// booleans as "true"/"false", strings raw, numbers as their decimal string form,
// and any other JSON value (object, array, null) as its JSON encoding.
func encodeFeatureValue(v any) ([]byte, error) {
	switch x := v.(type) {
	case string:
		return []byte(x), nil
	case bool:
		return strconv.AppendBool(nil, x), nil
	case float64:
		return []byte(strconv.FormatFloat(x, 'f', -1, 64)), nil
	case float32:
		return []byte(strconv.FormatFloat(float64(x), 'f', -1, 32)), nil
	case json.Number:
		return []byte(x.String()), nil
	case int:
		return []byte(strconv.Itoa(x)), nil
	case int64:
		return []byte(strconv.FormatInt(x, 10)), nil
	default:
		// Objects, arrays, null, and any other value: canonical JSON encoding,
		// which #json-key selection then operates on.
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("growthbook: encoding feature value: %w", err)
		}
		return b, nil
	}
}

// sdkEvaluator adapts the GrowthBook Go SDK client to the evaluator interface.
type sdkEvaluator struct {
	client *gb.Client
	// refresh re-fetches the feature set from the GrowthBook API before each
	// evaluation. It is false for an offline (WithFeatures) client, which has a
	// fixed feature set and no API to poll.
	refresh bool
}

var _ evaluator = (*sdkEvaluator)(nil)

func (e *sdkEvaluator) evaluateFeature(ctx context.Context, key string) (any, bool, error) {
	if e.refresh {
		if err := e.client.RefreshFeatures(ctx); err != nil {
			return nil, false, fmt.Errorf("growthbook: refreshing features: %w", err)
		}
	}
	res := e.client.EvalFeature(ctx, key)
	if res == nil || res.Source == gb.UnknownFeatureResultSource {
		return nil, false, nil
	}
	return res.Value, true, nil
}
