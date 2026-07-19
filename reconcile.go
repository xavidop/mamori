package mamori

import (
	"context"
	"reflect"
	"time"

	"github.com/go-viper/mapstructure/v2"
)

// Default option values.
const (
	defaultPollInterval = 30 * time.Second
	defaultDebounce     = 500 * time.Millisecond
	defaultQueueDepth   = 16
	defaultJitter       = 0.2
)

// options holds all configuration for a Load or Watch call.
type options struct {
	providers    map[string]Provider // explicit providers, override the registry
	validator    Validator
	decodeHooks  []mapstructure.DecodeHookFunc // user hooks, applied on the flatten path
	clock        Clock
	pollInterval time.Duration
	jitter       float64
	debounce     time.Duration
	queueDepth   int
	stale        time.Duration // 0 = disabled
	backoffBase  time.Duration
	backoffMax   time.Duration
	meter        Meter
	tracer       Tracer

	// change/error callbacks are typed per T, stored as any and asserted by
	// Watch[T]. onChange holds a func(Change[T]); onError holds a func(error).
	onChange any
	onError  func(error)
}

func defaultOptions() *options {
	return &options{
		providers:    map[string]Provider{},
		validator:    defaultValidator(),
		clock:        SystemClock(),
		pollInterval: defaultPollInterval,
		jitter:       defaultJitter,
		debounce:     defaultDebounce,
		queueDepth:   defaultQueueDepth,
		backoffBase:  time.Second,
		backoffMax:   time.Minute,
		meter:        noopMeter{},
		tracer:       noopTracer{},
	}
}

// Option configures Load and Watch.
type Option func(*options)

// WithProvider registers a provider for this call only, taking precedence over
// the global registry for its scheme.
func WithProvider(p Provider) Option {
	return func(o *options) { o.providers[p.Scheme()] = p }
}

// WithValidator overrides the default (go-playground/validator) validator.
func WithValidator(v Validator) Option { return func(o *options) { o.validator = v } }

// WithDecodeHook adds a mapstructure decode hook applied when decoding a
// flatten:"json|yaml|env" payload into a nested struct. Hooks run after the
// built-in secret/duration hook, in the order registered, so you can convert
// custom field types (a time.Time layout, a net.IP, an enum, ...).
func WithDecodeHook(h mapstructure.DecodeHookFunc) Option {
	return func(o *options) { o.decodeHooks = append(o.decodeHooks, h) }
}

// WithClock overrides the clock, primarily for deterministic tests.
func WithClock(c Clock) Option { return func(o *options) { o.clock = c } }

// WithPollInterval sets the fallback poll interval for non-watchable providers.
func WithPollInterval(d time.Duration) Option {
	return func(o *options) { o.pollInterval = d }
}

// WithJitter sets the poll jitter fraction (0..1); a value of 0.2 randomizes each
// interval by ±20% to avoid thundering herds.
func WithJitter(f float64) Option { return func(o *options) { o.jitter = f } }

// WithDebounce sets the coalescing window for change events (default 500ms). A
// per-field `?debounce=` ref option overrides this for that field.
func WithDebounce(d time.Duration) Option { return func(o *options) { o.debounce = d } }

// WithQueueDepth bounds the OnChange dispatch queue; when full, the oldest event
// is dropped (default 16).
func WithQueueDepth(n int) Option { return func(o *options) { o.queueDepth = n } }

// WithStale escalates staleness to a hard error: if a ref cannot be refreshed
// for longer than maxAge, OnError receives a *StaleError.
func WithStale(maxAge time.Duration) Option { return func(o *options) { o.stale = maxAge } }

// WithBackoff configures per-ref exponential backoff on resolve failure.
func WithBackoff(base, max time.Duration) Option {
	return func(o *options) { o.backoffBase, o.backoffMax = base, max }
}

// WithMeter installs a metrics sink (see the x/otel module for an OTel adapter).
func WithMeter(m Meter) Option { return func(o *options) { o.meter = m } }

// WithTracer installs a tracing sink (see the x/otel module for an OTel adapter).
func WithTracer(t Tracer) Option { return func(o *options) { o.tracer = t } }

// OnError installs a callback for runtime resolve/validation/stale errors.
func OnError(fn func(error)) Option { return func(o *options) { o.onError = fn } }

// provider resolves the provider for a scheme, preferring explicit providers
// over the global registry.
func (o *options) provider(scheme string) (Provider, bool) {
	if p, ok := o.providers[scheme]; ok {
		return p, true
	}
	return providerFor(scheme)
}

// Load resolves all refs of T once, applies defaults, validates, and returns the
// typed config. It fails fast: on any resolve or validation error it returns a
// non-nil error and the zero value of T; partial config is never returned.
func Load[T any](ctx context.Context, opts ...Option) (T, error) {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}
	var zero T
	cfg, _, err := loadValue[T](ctx, o)
	if err != nil {
		return zero, err
	}
	return cfg, nil
}

// loadValue is the shared load path used by Load and Watch's initial resolve. It
// returns the built config and the per-spec resolved values (for change
// detection in Watch).
func loadValue[T any](ctx context.Context, o *options) (T, []resolved, error) {
	var cfg T
	t := reflect.TypeOf(cfg)
	specs, err := fieldSpecs(t)
	if err != nil {
		return cfg, nil, err
	}
	res, err := resolveAll(ctx, specs, o)
	if err != nil {
		return cfg, nil, err
	}
	if err := buildInto(reflect.ValueOf(&cfg).Elem(), res, o.decodeHooks); err != nil {
		return cfg, nil, err
	}
	if err := o.validator.Validate(cfg); err != nil {
		return cfg, nil, &ValidationError{Err: err}
	}
	return cfg, res, nil
}

// buildInto decodes all resolved values into the struct value dst.
func buildInto(dst reflect.Value, res []resolved, hooks []mapstructure.DecodeHookFunc) error {
	for _, r := range res {
		if !r.set {
			continue // optional + not found: leave zero value
		}
		if err := setField(dst, r.spec, r.value.Bytes, hooks); err != nil {
			return err
		}
	}
	return nil
}
