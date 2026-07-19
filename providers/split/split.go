// Package split implements a mamori provider for Split, the feature-flag and
// experimentation platform now part of Harness FME (https://www.split.io).
//
// It registers the "split" scheme. Refs take the form:
//
//	split://<feature-flag-name>[?key=<traffic-key>]
//
// where <feature-flag-name> is the name of a Split feature flag. The provider
// evaluates the flag and returns its treatment as a plain string ("on", "off",
// or any named treatment you configured in Split):
//
//	Checkout string `source:"split://new-checkout"`
//	Layout   string `source:"split://homepage-layout?key=user-42"`
//
// # Traffic key
//
// Split evaluates a flag on behalf of a traffic key (the identifier of the user,
// account, or entity the flag is rolled out to). The key is chosen as follows:
//
//   - the ref's ?key=<traffic-key> option, when present, otherwise
//   - the provider's default key (WithKey), which defaults to "mamori".
//
// A stable, non-empty key is important: Split's percentage rollouts and
// targeting rules are computed from a hash of the traffic key, so the same key
// always yields the same treatment for a given flag configuration.
//
// # Evaluation and not found
//
// The provider calls the Split client's Treatment method. Split returns the
// special treatment "control" when the flag does not exist, has been archived,
// or the client is not yet ready. The provider maps a "control" (or empty)
// result to an error satisfying errors.Is(err, mamori.ErrNotFound), so mamori's
// default / optional handling applies exactly as it does for every other
// provider.
//
// Feature flags hold rollout/configuration state, not managed secrets, so
// resolved values are never marked Sensitive. Split exposes no per-flag revision
// identifier through the treatment API, so Value.Version is a content hash
// (mamori.VersionHash), which still gives mamori cheap, correct change
// detection: the version changes exactly when the evaluated treatment changes.
//
// # Authentication
//
// The provider needs a Split SDK key (server-side API key), supplied either
// explicitly via WithAPIKey or, when unset, read lazily from SPLIT_API_KEY at
// first use. The underlying Split client is created lazily on first Resolve, so
// registering the provider never contacts the network and never fails for lack
// of configuration.
//
// # Lazy start and readiness
//
// The Split SDK downloads flag definitions from Split's servers in a background
// goroutine after the factory is created; a client is not usable until that
// first sync completes, and until then every Treatment call returns "control".
// To avoid reporting spurious not-found results, the provider blocks on the
// client's BlockUntilReady during its lazy construction (bounded by
// WithReadyTimeout, 10s by default). If the client fails to become ready within
// that window, the first Resolve returns that initialization error - not a
// not-found - so a misconfigured SDK key or an unreachable Split backend is
// surfaced rather than masked.
//
// # Watch
//
// The Split SDK refreshes flag definitions on an internal interval but exposes
// no clean per-flag push notification, so this provider is not watchable;
// mamori wraps it in its polling adapter automatically.
package split

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	splitclient "github.com/splitio/go-client/v6/splitio/client"
	splitconf "github.com/splitio/go-client/v6/splitio/conf"
	"github.com/splitio/go-toolkit/v5/logging"
	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "split"

// defaultKey is the traffic key used when neither the ref's ?key option nor
// WithKey supplies one.
const defaultKey = "mamori"

// controlTreatment is the sentinel treatment Split returns when a flag is
// missing/archived or the client is not ready. It is part of the documented
// Split contract and is mapped to mamori.ErrNotFound.
const controlTreatment = "control"

// defaultReadyTimeout bounds how long the lazy client build waits for the SDK's
// first flag sync (BlockUntilReady).
const defaultReadyTimeout = 10 * time.Second

// treatmentClient is the minimal Split surface this provider depends on. The
// real *splitclient.SplitClient is adapted to it by sdkClient; tests inject an
// in-memory fake implementing the same interface, so the conformance kit runs
// without a live Split backend.
type treatmentClient interface {
	// Treatment evaluates feature on behalf of the traffic key and returns the
	// resulting treatment string ("on", "off", a named treatment, or "control").
	Treatment(key, feature string) string
	// Destroy releases the client's resources and flushes any pending data.
	Destroy()
}

// Provider resolves split:// refs against a Split backend. It is safe for
// concurrent use. The underlying client is built lazily on first use unless one
// is injected via WithClient (or a fake via withClient in tests).
type Provider struct {
	apiKey       string
	key          string
	readyTimeout time.Duration
	config       *splitconf.SplitSdkConfig

	mu     sync.Mutex
	client treatmentClient
	// newClient builds the backing client on first use. Overridable in tests.
	newClient func(ctx context.Context) (treatmentClient, error)
}

// Option configures a Provider.
type Option func(*Provider)

// WithAPIKey sets the Split SDK key (server-side API key). When empty the
// provider reads SPLIT_API_KEY from the environment at first use.
func WithAPIKey(apiKey string) Option {
	return func(p *Provider) { p.apiKey = apiKey }
}

// WithKey sets the default traffic key used to evaluate flags when a ref does
// not carry its own ?key option. When empty the default is "mamori".
func WithKey(key string) Option {
	return func(p *Provider) { p.key = key }
}

// WithReadyTimeout bounds how long the lazy client build waits for the Split
// SDK's first flag sync to complete. When zero the default (10s) is used.
func WithReadyTimeout(d time.Duration) Option {
	return func(p *Provider) { p.readyTimeout = d }
}

// WithConfig injects a custom *conf.SplitSdkConfig used when the provider builds
// the real Split client lazily (e.g. to enable localhost mode, a sync proxy, or
// a custom logger). When nil a sensible default is used (in-memory standalone,
// logging silenced). Ignored when a client is injected via WithClient.
func WithConfig(cfg *splitconf.SplitSdkConfig) Option {
	return func(p *Provider) { p.config = cfg }
}

// WithClient injects a pre-built *splitclient.SplitClient, bypassing lazy
// construction. Use it when you build the Split factory yourself. The provided
// client must already be ready; callers typically invoke BlockUntilReady on the
// factory (or client) before handing it over.
func WithClient(c *splitclient.SplitClient) Option {
	return func(p *Provider) {
		if c != nil {
			p.client = &sdkClient{c: c}
		}
	}
}

// withClient injects a bare treatmentClient. Unexported: used by tests to supply
// an in-memory fake.
func withClient(c treatmentClient) Option {
	return func(p *Provider) { p.client = c }
}

// New constructs a Split provider. By default the underlying client is created
// lazily on first Resolve using WithAPIKey/SPLIT_API_KEY, so New never contacts
// the network and never fails for lack of configuration.
//
// Users who need explicit configuration call
// mamori.WithProvider(split.New(split.WithAPIKey("..."), split.WithKey("user-42"))).
func New(opts ...Option) *Provider {
	p := &Provider{key: defaultKey, readyTimeout: defaultReadyTimeout}
	p.newClient = func(ctx context.Context) (treatmentClient, error) {
		return p.buildClient()
	}
	for _, o := range opts {
		o(p)
	}
	if p.key == "" {
		p.key = defaultKey
	}
	if p.readyTimeout <= 0 {
		p.readyTimeout = defaultReadyTimeout
	}
	return p
}

// init registers a lazily-initialized provider so `import _` wiring works from
// ambient environment configuration (SPLIT_API_KEY). Users who need explicit
// config call mamori.WithProvider(split.New(...)).
func init() { mamori.Register(New()) }

// Scheme returns "split".
func (p *Provider) Scheme() string { return scheme }

// buildClient constructs the real Split client from the provider's configuration
// (falling back to SPLIT_API_KEY) and blocks until the SDK has completed its
// first flag sync, so the first Resolve does not see an unready client and
// report spurious not-found results. It is only reached on the live path; tests
// inject a fake via withClient/WithClient.
func (p *Provider) buildClient() (treatmentClient, error) {
	apiKey := p.apiKey
	if apiKey == "" {
		apiKey = os.Getenv("SPLIT_API_KEY")
	}
	if apiKey == "" {
		return nil, errors.New("mamori/split: no SDK key; set SPLIT_API_KEY or use split.WithAPIKey")
	}

	cfg := p.config
	if cfg == nil {
		cfg = splitconf.Default()
		// The SPI forbids logging payloads and the SDK's chatter is noise at this
		// layer, so silence it. Callers who want SDK logs supply their own config
		// via WithConfig.
		cfg.LoggerConfig.LogLevel = logging.LevelNone
	}

	factory, err := splitclient.NewSplitFactory(apiKey, cfg)
	if err != nil {
		return nil, fmt.Errorf("mamori/split: creating factory: %w", err)
	}

	// The client is not usable until its first flag sync has completed; until
	// then every Treatment returns "control". Block for readiness so the first
	// Resolve does not misreport missing flags. A timeout here means the SDK
	// could not initialize (bad key, unreachable backend); surface it rather than
	// letting every flag look not-found.
	c := factory.Client()
	timeoutSecs := int(p.readyTimeout / time.Second)
	if timeoutSecs < 1 {
		timeoutSecs = 1
	}
	if err := c.BlockUntilReady(timeoutSecs); err != nil {
		factory.Destroy()
		return nil, fmt.Errorf("mamori/split: client not ready: %w", err)
	}
	return &sdkClient{c: c}, nil
}

// getClient returns the backing client, creating it lazily on first use.
// Concurrent callers share one client.
func (p *Provider) getClient(ctx context.Context) (treatmentClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	if p.newClient == nil {
		return nil, errors.New("mamori/split: no client and no client factory configured")
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
	p.client.Destroy()
	p.client = nil
	return nil
}

// Resolve evaluates the feature flag named by ref.Path for the traffic key
// (ref ?key option, else the provider's default key) and returns the resulting
// treatment string. A flag that Split does not know about - or a client that is
// not ready - evaluates to "control", which the provider maps to an error
// satisfying errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	feature := ref.Path
	if feature == "" {
		return mamori.Value{}, fmt.Errorf("mamori/split: ref %q requires a feature flag name", ref.Raw)
	}

	key := ref.Opt("key")
	if key == "" {
		key = p.key
	}
	if key == "" {
		key = defaultKey
	}

	c, err := p.getClient(ctx)
	if err != nil {
		return mamori.Value{}, err
	}

	treatment := c.Treatment(key, feature)
	if treatment == "" || treatment == controlTreatment {
		return mamori.Value{}, fmt.Errorf("mamori/split: feature flag %q evaluates to control (missing/archived or client not ready): %w", feature, mamori.ErrNotFound)
	}

	b := []byte(treatment)
	return mamori.Value{
		Bytes:     b,
		Version:   mamori.VersionHash(b),
		Sensitive: false,
		Metadata: map[string]string{
			"flag": feature,
			"key":  key,
		},
	}, nil
}

// sdkClient adapts a *splitclient.SplitClient to the treatmentClient interface.
type sdkClient struct{ c *splitclient.SplitClient }

func (s *sdkClient) Treatment(key, feature string) string {
	return s.c.Treatment(key, feature, nil)
}

func (s *sdkClient) Destroy() { s.c.Destroy() }

// Interface compliance checks.
var (
	_ mamori.Provider = (*Provider)(nil)
	_ treatmentClient = (*sdkClient)(nil)
)
