package mamori

import (
	"context"
	"crypto/tls"
	"log/slog"
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
	backoffBase  time.Duration // 0 = disabled; see WithBackoff
	backoffMax   time.Duration
	meter        Meter
	tracer       Tracer
	logger       *slog.Logger
	historyN     int               // snapshots retained beyond the current one; 0 = current only
	refVars      map[string]string // ${VAR} expansion for source tags; nil means none

	// preApply is the gate run before a candidate snapshot becomes current,
	// typed per T and stored as any for the same reason onChange below is;
	// Watch[T] and loadValue (reconcile.go) each assert it back to a concrete
	// type via the shared typedPreApply[T] helper - loadValue is the only
	// assertion Load ever gets, since Load has no Watch-side check of its own.
	// preApplyTimeout bounds it (see WithPreApplyTimeout for why that bound is
	// mandatory).
	preApply        any
	preApplyTimeout time.Duration

	// admin server config, consumed only by Watch (see adminhttp.go). Load
	// accepts the same Option values but has no watcher to run a server
	// against, so it ignores all three.
	adminAddr string
	adminOpts []HandlerOption
	adminTLS  *tls.Config

	// change/error callbacks are typed per T, stored as any and asserted by
	// Watch[T]. onChange holds a func(Change[T]); onError holds a func(error).
	onChange any
	onError  func(error)
}

// defaultOptions returns the option set every Load and Watch starts from.
//
// backoffBase/backoffMax are deliberately absent, leaving the retry backoff
// window at zero: backoff is opt-in via WithBackoff. This used to name 1s/1m
// here while nothing in the engine read either field, so those numbers never
// described real behavior; adopting them once the option was implemented would
// have changed the retry cadence of every existing caller. See WithBackoff.
func defaultOptions() *options {
	return &options{
		providers:       map[string]Provider{},
		validator:       defaultValidator(),
		clock:           SystemClock(),
		pollInterval:    defaultPollInterval,
		jitter:          defaultJitter,
		debounce:        defaultDebounce,
		queueDepth:      defaultQueueDepth,
		meter:           noopMeter{},
		tracer:          noopTracer{},
		logger:          slog.New(slog.DiscardHandler),
		preApplyTimeout: defaultPreApplyTimeout,
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

// WithHistory retains the n most recent snapshots in addition to the current
// one, readable via Watcher.History and pinnable via Watcher.Pin. It defaults
// to 0. A negative n clamps to 0.
//
// Retained snapshots hold full copies of T, including any secret material that
// has since been rotated. Enabling history extends the in-memory lifetime of
// old secrets; enable it deliberately.
func WithHistory(n int) Option {
	return func(o *options) {
		if n < 0 {
			n = 0
		}
		o.historyN = n
	}
}

// WithBackoff enables per-ref exponential backoff on resolve failure for
// polled refs. After a ref fails to resolve, its next attempt is delayed by
// base instead of the poll interval; each further consecutive failure doubles
// that delay, held at max once it gets there. Any successful round trip with
// the backend resets it, and the ref returns to the normal poll interval.
//
// Backoff is OFF by default. Without this option a failing ref is retried on
// the WithPollInterval cadence, exactly as it always has been - which is the
// point: this option set two fields nothing read until it was implemented, so
// no existing caller can have been relying on backoff, and switching it on for
// everyone would have made a just-failed backend get retried far sooner than
// its operators had ever seen. Choose the window deliberately.
//
// Normalization: a base of zero or less disables backoff, so WithBackoff(0, 0)
// turns it back off. A max below base is raised to base, which gives
// WithBackoff(d, 0) the meaning "retry every d while failing" rather than
// unbounded exponential growth.
//
// Three behaviors are worth knowing before choosing a window.
//
// It does not apply to providers with a native watch. A WatchableProvider
// (Kubernetes informers, Consul blocking queries, Postgres LISTEN/NOTIFY, the
// mamori:// SSE client, ...) owns its own stream and its own reconnection
// cadence; mamori polls nothing on its behalf, so there is no attempt for this
// option to delay. Reconnect behavior for those is provider-internal and
// documented per provider. The single exception is a native watch that fails
// to START: mamori falls back to the polling adapter for that ref, and backoff
// governs it from then on like any other polled ref.
//
// A not-found is not a failure. ErrNotFound means the backend answered and the
// ref is absent, which is ordinary default:/optional: territory; it ends a
// streak rather than extending one, so a ref that gets provisioned after the
// process starts is still discovered on the normal poll interval.
//
// It interacts with WithStale. Staleness is escalated to a *StaleError on the
// first failed attempt after maxAge elapses, and backoff is what pushes that
// attempt out, so a large max delays the OnError signal by up to one backoff
// step. Watcher.Status and Watcher.Health are unaffected: they recompute Age
// and Stale at read time from the last success, so a readiness probe still
// turns unhealthy at exactly maxAge. Keep max well under the WithStale
// threshold if the OnError timing matters to you.
func WithBackoff(base, max time.Duration) Option {
	return func(o *options) { o.backoffBase, o.backoffMax = base, max }
}

// WithMeter installs a metrics sink (see the x/otel module for an OTel adapter).
func WithMeter(m Meter) Option { return func(o *options) { o.meter = m } }

// WithTracer installs a tracing sink (see the x/otel module for an OTel adapter).
func WithTracer(t Tracer) Option { return func(o *options) { o.tracer = t } }

// OnError installs a callback for runtime resolve/validation/stale errors, and
// for a candidate a PreApply gate rejected.
//
// Unlike OnChange, it runs INLINE on the reconciler goroutine rather than on the
// dispatch queue: errors are delivered, never dropped, which the drop-oldest
// queue OnChange uses could not promise. Two things follow, and both are the
// caller's to design around:
//
// It blocks reconciliation for as long as it runs. Log, count, notify - do not
// do I/O with no deadline here, and do not wait on something that is itself
// waiting on this watcher.
//
// It must not call back into the same Watcher. Get is safe (it is a lock-free
// atomic load), but Pin, PinCurrent, Unpin and Refresh are commands serviced by
// the very goroutine this callback is occupying, so they would wait for
// themselves. mamori detects that and refuses the call - see [ErrReentrantCall],
// which spells out what each one returns. "The reload was rejected, retry it" is
// the tempting thing to write here; issue it from another goroutine, or let the
// next reconciliation do it.
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

	// Checked before any provider round trip, for the same reason Watch checks
	// it before calling loadValue at all (see typedPreApply's doc comment):
	// a hook typed for the wrong T is a caller bug that should fail loudly and
	// immediately, not after fields have already been resolved. This duplicates
	// Watch's own check for the Watch path (harmless: same nil-or-error result
	// either time), but it is the ONLY check for Load, which has no earlier
	// point of its own to catch this.
	hook, err := typedPreApply[T](o)
	if err != nil {
		return cfg, nil, err
	}

	t := reflect.TypeOf(cfg)
	specs, err := fieldSpecs(t, o.refVars)
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

	// Gate the initial configuration too (decision D7): a hook that verifies a
	// credential should verify the first one, so a credential that does not
	// work fails at startup (Watch) or on the spot (Load) rather than at the
	// first rotation. Old is the zero value of T here, since nothing was
	// serving yet.
	//
	// Fields is populated, not left nil, and it is populated by applying the
	// engine's own diff rule (buildCandidate, reconciler.go) against the true
	// prior state at this point in time: e.applied does not exist yet (Watch
	// only seeds it after loadValue returns), so the prior version for every
	// field is the empty string, and buildCandidate already treats a missing
	// applied entry and an explicit "" identically (see flush's own comment on
	// that equivalence). Applying that same rule here yields one FieldChange
	// per resolved field, each with OldVersion "" - exactly what buildCandidate
	// would compute one instant later, were e.applied queried before Watch
	// seeds it. This is what makes ev.Changed(path) true for every field set on
	// this load, which is what lets a hook written the documented way (guard on
	// ev.Changed before doing the I/O) verify the initial configuration at all
	// - the entire point of D7. See TestPreApplyInitialLoadPopulatesFields.
	//
	// This is the only place either Load or Watch's initial resolve runs the
	// gate: Watch stores this call's result directly into the engine's
	// lastGood/cfg without a further gate of its own (see Watch in
	// reconciler.go), so gating here costs exactly one hook invocation for the
	// initial configuration, not two.
	// Guarded on the hook, because nothing else ever reads this slice: it exists
	// solely to populate the Change handed to the gate. Without the guard every
	// Load and every Watch allocates one FieldChange per resolved field for a
	// hook that is not there - which is the common case, since PreApply is
	// opt-in.
	var fields []FieldChange
	if hook != nil {
		for _, r := range res {
			if r.set {
				fields = append(fields, FieldChange{Path: r.spec.Path, NewVersion: r.value.Version})
			}
		}
	}
	// The reentrancy mark is nil here, and that is not an oversight: this gate
	// runs on the CALLER's goroutine, inside Load or inside Watch before it has
	// constructed a Watcher at all. There is no reconciler goroutine yet and no
	// control channel to send on, so there is nothing a hook could reenter -
	// a hook reaching for the Watcher this load is producing can only find the
	// nil it has not been assigned yet. Only flush's gate, which runs on the
	// reconciler goroutine itself, needs the mark (see reconciler.go).
	if err := runPreApply(ctx, hook, o.preApplyTimeout, Change[T]{New: cfg, Fields: fields}, nil); err != nil {
		return cfg, nil, err
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
