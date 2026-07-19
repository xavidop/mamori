// Package unleash implements a mamori provider for Unleash
// (https://www.getunleash.io), the open-source feature-flag / feature-toggle
// platform.
//
// It registers the "unleash" scheme. Refs take the form:
//
//	unleash://<feature-toggle-name>[#variant|#payload]
//
// where <feature-toggle-name> is the name of an Unleash feature toggle. The
// optional URL fragment selects what is resolved:
//
//	Flag    bool   `source:"unleash://new-checkout"`          // enabled state: "true"/"false"
//	Variant string `source:"unleash://new-checkout#variant"`  // active variant name
//	Payload string `source:"unleash://new-checkout#payload"`  // active variant payload value
//
// Evaluation:
//
//   - With no fragment the provider returns the toggle's enabled state as the
//     string "true" or "false" (Client.IsEnabled).
//   - With #variant it returns the active variant's name (Client.GetVariant).
//   - With #payload it returns the active variant's payload value.
//
// Unleash feature toggles hold configuration/rollout state, not managed
// secrets, so resolved values are never marked Sensitive. Unleash exposes no
// per-toggle revision identifier through the client, so Value.Version is a
// content hash (mamori.VersionHash), which still gives mamori cheap, correct
// change detection.
//
// # Not found
//
// A ref that names a feature toggle the client does not know about resolves to
// an error satisfying errors.Is(err, mamori.ErrNotFound). Unleash's IsEnabled
// returns false (not an error) for unknown toggles, so the provider inspects the
// client's loaded feature repository (Client.ListFeatures) to distinguish a
// genuinely-missing toggle from one that exists and is disabled.
//
// # Authentication
//
// The provider connects to an Unleash server using three settings, supplied
// either explicitly via options or, when unset, read lazily from the
// environment at first use:
//
//   - the server URL: WithURL or UNLEASH_URL (e.g. https://unleash.example.com/api)
//   - an API token: WithToken or UNLEASH_API_TOKEN (sent as the Authorization header)
//   - an application name: WithAppName or UNLEASH_APP_NAME (defaults to "mamori")
//
// The underlying *unleash.Client is created lazily on first Resolve, so
// registering the provider never contacts the network and never fails for lack
// of configuration.
//
// # Lazy start and synchronization
//
// The Unleash client fetches feature toggles from the server in a background
// goroutine after it is created; it is not usable until that first fetch has
// completed. The provider therefore calls Client.WaitForReady() as part of its
// lazy construction, so the first Resolve blocks until the feature repository is
// populated rather than reporting spurious not-found results.
//
// # Watch
//
// The Unleash client refreshes its feature repository on an internal interval
// (WithRefreshInterval, 15s by default) but exposes no clean per-toggle push,
// so this provider is not watchable; mamori wraps it in its polling adapter
// automatically.
package unleash

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"

	unleash "github.com/Unleash/unleash-client-go/v4"
	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "unleash"

// defaultAppName is used when neither WithAppName nor UNLEASH_APP_NAME is set.
const defaultAppName = "mamori"

// featureClient is the minimal Unleash surface this provider depends on. The
// real *unleash.Client is adapted to it by sdkClient; tests inject an in-memory
// fake implementing the same interface, so the conformance kit runs without a
// live Unleash server.
type featureClient interface {
	// Exists reports whether a feature toggle named feature is known to the
	// client (present in its loaded feature repository). It is what lets the
	// provider distinguish a missing toggle (ErrNotFound) from a disabled one.
	Exists(feature string) bool
	// IsEnabled returns the toggle's evaluated enabled state.
	IsEnabled(feature string) bool
	// GetVariant returns the active variant's name and payload value for the
	// toggle.
	GetVariant(feature string) (name, payloadValue string)
	// Close releases the client's resources.
	Close() error
}

// Provider resolves unleash:// refs against an Unleash server. It is safe for
// concurrent use. The underlying client is built lazily on first use unless one
// is injected via WithClient.
type Provider struct {
	url     string
	token   string
	appName string

	mu     sync.Mutex
	client featureClient
	// newClient builds the backing client on first use. Overridable in tests.
	newClient func(ctx context.Context) (featureClient, error)
}

// Option configures a Provider.
type Option func(*Provider)

// WithURL sets the Unleash server API URL, e.g.
// "https://unleash.example.com/api". When empty the provider reads UNLEASH_URL
// from the environment at first use.
func WithURL(url string) Option {
	return func(p *Provider) { p.url = url }
}

// WithToken sets the Unleash API token (client or frontend token). It is sent
// as the Authorization header. When empty the provider reads UNLEASH_API_TOKEN
// from the environment at first use.
func WithToken(token string) Option {
	return func(p *Provider) { p.token = token }
}

// WithAppName sets the application name reported to Unleash (used for metrics
// and registration). When empty the provider reads UNLEASH_APP_NAME from the
// environment, falling back to "mamori".
func WithAppName(appName string) Option {
	return func(p *Provider) { p.appName = appName }
}

// WithClient injects a pre-built *unleash.Client, bypassing lazy construction.
// Use it when you build the client yourself (custom strategies, storage,
// listener, or HTTP client). The provided client must already be initialized;
// callers typically invoke client.WaitForReady() before handing it over.
func WithClient(c *unleash.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.client = &sdkClient{c: c}
		}
	}
}

// withClient injects a bare featureClient. Unexported: used by tests to supply
// an in-memory fake.
func withClient(c featureClient) Option {
	return func(p *Provider) { p.client = c }
}

// New constructs an Unleash provider. By default the underlying client is
// created lazily on first Resolve using WithURL/UNLEASH_URL,
// WithToken/UNLEASH_API_TOKEN and WithAppName/UNLEASH_APP_NAME, so New never
// contacts the network and never fails for lack of configuration.
//
// Users who need explicit configuration call
// mamori.WithProvider(unleash.New(unleash.WithURL("https://unleash.example.com/api"), unleash.WithToken("..."))).
func New(opts ...Option) *Provider {
	p := &Provider{appName: defaultAppName}
	p.newClient = func(ctx context.Context) (featureClient, error) {
		return p.buildClient()
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// init registers a lazily-initialized provider so `import _` wiring works from
// ambient environment configuration. Users who need explicit config call
// mamori.WithProvider(unleash.New(...)).
func init() { mamori.Register(New()) }

// Scheme returns "unleash".
func (p *Provider) Scheme() string { return scheme }

// buildClient constructs the real Unleash client from the provider's
// configuration (falling back to the environment) and blocks until it has
// loaded feature toggles from the server. It is only reached on the live path;
// tests inject a fake via withClient/WithClient.
func (p *Provider) buildClient() (featureClient, error) {
	url := p.url
	if url == "" {
		url = os.Getenv("UNLEASH_URL")
	}
	if url == "" {
		return nil, errors.New("mamori/unleash: no server URL; set UNLEASH_URL or use unleash.WithURL")
	}
	token := p.token
	if token == "" {
		token = os.Getenv("UNLEASH_API_TOKEN")
	}
	appName := p.appName
	if appName == "" {
		appName = os.Getenv("UNLEASH_APP_NAME")
	}
	if appName == "" {
		appName = defaultAppName
	}

	headers := http.Header{}
	if token != "" {
		headers.Set("Authorization", token)
	}

	c, err := unleash.NewClient(
		unleash.WithAppName(appName),
		unleash.WithUrl(url),
		unleash.WithCustomHeaders(headers),
		// A silent listener drains the client's channels so its background
		// goroutines never block; it deliberately logs nothing (the SPI forbids
		// logging payloads, and toggle names are noise at this layer).
		unleash.WithListener(silentListener{}),
	)
	if err != nil {
		return nil, fmt.Errorf("mamori/unleash: creating client: %w", err)
	}
	// The client is not usable until its first fetch of feature toggles has
	// completed; block for it so the first Resolve does not see an empty
	// repository and report spurious not-found results.
	c.WaitForReady()
	return &sdkClient{c: c}, nil
}

// getClient returns the backing client, creating it lazily on first use.
// Concurrent callers share one client.
func (p *Provider) getClient(ctx context.Context) (featureClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	if p.newClient == nil {
		return nil, errors.New("mamori/unleash: no client and no client factory configured")
	}
	c, err := p.newClient(ctx)
	if err != nil {
		return nil, err
	}
	p.client = c
	return c, nil
}

// Close releases the backing client, if one has been created.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client == nil {
		return nil
	}
	err := p.client.Close()
	p.client = nil
	return err
}

// Resolve evaluates the feature toggle named by ref.Path. With no fragment it
// returns the toggle's enabled state as "true"/"false"; with #variant the
// active variant name; with #payload the active variant payload value. A toggle
// the client does not know about returns an error satisfying
// errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	name := ref.Path
	if name == "" {
		return mamori.Value{}, fmt.Errorf("mamori/unleash: ref %q requires a feature toggle name", ref.Raw)
	}

	c, err := p.getClient(ctx)
	if err != nil {
		return mamori.Value{}, err
	}

	// Unleash's IsEnabled/GetVariant return defaults (not errors) for unknown
	// toggles, so existence must be checked explicitly against the loaded
	// feature repository to honor the not-found contract.
	if !c.Exists(name) {
		return mamori.Value{}, fmt.Errorf("mamori/unleash: feature toggle %q not found: %w", name, mamori.ErrNotFound)
	}

	var out, kind string
	switch ref.Key {
	case "":
		out = strconv.FormatBool(c.IsEnabled(name))
		kind = "enabled"
	case "variant":
		vname, _ := c.GetVariant(name)
		out = vname
		kind = "variant"
	case "payload":
		_, pval := c.GetVariant(name)
		out = pval
		kind = "payload"
	default:
		return mamori.Value{}, fmt.Errorf("mamori/unleash: ref %q has unsupported fragment %q (use #variant or #payload)", ref.Raw, ref.Key)
	}

	b := []byte(out)
	return mamori.Value{
		Bytes:     b,
		Version:   mamori.VersionHash(b),
		Sensitive: false,
		Metadata: map[string]string{
			"toggle": name,
			"kind":   kind,
		},
	}, nil
}

// sdkClient adapts a *unleash.Client to the featureClient interface.
type sdkClient struct{ c *unleash.Client }

func (s *sdkClient) Exists(feature string) bool {
	for _, f := range s.c.ListFeatures() {
		if f.Name == feature {
			return true
		}
	}
	return false
}

func (s *sdkClient) IsEnabled(feature string) bool { return s.c.IsEnabled(feature) }

func (s *sdkClient) GetVariant(feature string) (name, payloadValue string) {
	v := s.c.GetVariant(feature)
	if v == nil {
		return "", ""
	}
	return v.Name, v.Payload.Value
}

func (s *sdkClient) Close() error { return s.c.Close() }

// silentListener implements the Unleash client's listener interfaces (error,
// repository, and metrics) as no-ops so the client's background channels are
// drained without emitting any log output.
type silentListener struct{}

func (silentListener) OnError(error)                        {}
func (silentListener) OnWarning(error)                      {}
func (silentListener) OnReady()                             {}
func (silentListener) OnUpdate()                            {}
func (silentListener) OnCount(string, bool)                 {}
func (silentListener) OnSent(unleash.MetricsData)           {}
func (silentListener) OnRegistered(unleash.ClientData)      {}
func (silentListener) OnImpression(unleash.ImpressionEvent) {}

// Interface compliance checks.
var (
	_ mamori.Provider = (*Provider)(nil)
	_ featureClient   = (*sdkClient)(nil)
)
