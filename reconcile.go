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

// deriveEntry holds one WithDerive hook alongside the field paths it declares
// having written. fn is stored as any (options is not generic, the same
// reason preApply and onChange are); writes stays on this non-generic side
// deliberately, because it is just strings, and it lets the reconciler read
// the declared paths without ever needing to know T (see typedDerive and
// typedDerives, reconciler.go).
type deriveEntry struct {
	fn     any
	writes []string
}

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
	stale        time.Duration    // 0 = disabled
	bootstrap    *bootstrapConfig // nil = disabled; see WithBootstrapCache
	backoffBase  time.Duration    // 0 = disabled; see WithBackoff
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

	// derives holds the WithDerive hooks in registration order. Each entry
	// pairs the hook (a func(*T) error typed per T and stored as any, for the
	// same reason preApply above is) with the field paths it declares having
	// written (see deriveEntry). They run after fields are decoded and BEFORE
	// validation, so a derived field is validated like any other, and before
	// the PreApply gate, so a rotation-safety hook proves the derived value
	// rather than the one it replaced.
	//
	// A slice rather than a single hook: unrelated derivations stay in separate
	// functions instead of accreting into one closure, and a field derived from
	// another derived field works with no new concept, because a later hook sees
	// an earlier one's output.
	derives []deriveEntry

	// admin server config, consumed only by Watch (see adminhttp.go). Load
	// accepts the same Option values but has no watcher to run a server
	// against, so it ignores all three.
	adminAddr string
	adminOpts []HandlerOption
	adminTLS  *tls.Config

	// change/error callbacks are typed per T, stored as any and asserted by
	// Watch[T]. onChange holds a func(Change[T]) and is asserted via the
	// shared typedOnChange[T] helper (reconciler.go), the same shape and
	// same loud-on-mismatch contract typedPreApply gives preApply above;
	// onError holds a func(error) and needs no such assertion, since it is
	// not generic in the first place.
	onChange any
	onError  func(error)
}

// defaultOptions returns the option set every Load and Watch starts from.
//
// backoffBase/backoffMax are deliberately absent, leaving the retry backoff
// window at zero: backoff is opt-in via WithBackoff. Giving them a nonzero
// default here would silently turn backoff on for every existing caller and
// change their retry cadence without them asking for it. See WithBackoff.
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
// flatten:"json|yaml|toml|env" payload into a nested struct. Hooks run after the
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
// Backoff is OFF by default: without this option a failing ref is retried on
// the plain WithPollInterval cadence. Choose the window deliberately, since
// turning it on changes how soon a just-failed backend gets retried.
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
// for a candidate a PreApply gate or a WithDerive hook rejected.
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

// WithDerive installs a hook that computes fields from already-resolved fields.
// It runs on every Load and every reconciled update, after values are decoded
// into the struct and before validation, so a value assembled from other fields
// is rebuilt whenever any of its inputs changes rather than going stale.
//
// The canonical case is a DSN assembled from a host, a user, and a rotating
// password. Built once in application code after Get, such a value is silently
// wrong the moment the password rotates; built here, it is rebuilt on every
// applied update and proven by any PreApply gate before Get serves it.
//
//	mamori.WithDerive(func(c *Config) error {
//	    c.DSN = secret.NewString((&url.URL{
//	        Scheme: "postgres",
//	        User:   url.UserPassword(c.User, c.Password.Reveal()),
//	        Host:   c.Host,
//	        Path:   "/" + c.DB,
//	    }).String())
//	    return nil
//	})
//
// Escaping and secret hygiene are the caller's: net/url escapes a password
// containing '@' or '/' correctly, and assigning into a secret.String keeps the
// assembled value redacted in fmt, JSON, and slog.
//
// Unlike PreApply, the hook takes no context.Context. PreApply does I/O to
// prove a credential; a derive is a pure transformation of an already-resolved
// struct, and the missing parameter is how the API says so.
//
// It must not call back into the same Watcher. Get is safe (a lock-free atomic
// load), but Pin, PinCurrent, Unpin, and Refresh are serviced by the very
// goroutine this hook occupies, so they would wait for themselves; mamori
// refuses them instead - see [ErrReentrantCall] for what each returns. There is
// no context here to bound the wait either. Issue the call from another
// goroutine, or let the next reconciliation carry it.
//
// Multiple calls run in registration order. Returning an error rejects the
// whole candidate configuration exactly as a validation failure does: Get keeps
// serving the last valid config and the error reaches OnError as a *DeriveError.
//
// writes declares the dotted field paths the hook writes (the same shape
// spec.Path uses, e.g. "Redis.DSN"), in any order. mamori cannot infer what an
// opaque Go function assigns, so the caller states it, and declaring it is what
// lets a derived field appear in ev.Changed and in Status()'s per-field report.
// It is variadic and optional: WithDerive(fn) still registers and runs the
// hook, it simply reports no writes.
//
// A declared path is validated for shape, not existence. An empty or
// whitespace-only path is rejected at Load/Watch time (see typedDerives), since
// silently ignoring one reintroduces the invisible-field problem writes exists
// to fix. A path that names no field on T is NOT rejected: mamori has no
// resolver for it outside the decode machinery this hook's opaqueness keeps it
// from running, so such a path simply never reports as written.
//
// A nil fn installs nothing and is silently dropped, the same clamp WithHistory
// and WithPreApplyTimeout apply.
func WithDerive[T any](fn func(*T) error, writes ...string) Option {
	return func(o *options) {
		if fn == nil {
			return
		}
		// Copy writes rather than storing the caller's slice. A variadic call
		// site spelled WithDerive(fn, paths...) passes its own backing array, so
		// storing it directly leaves the caller holding a live alias to the
		// declared paths. Mutating it after Watch would change what the
		// reconciler goroutine reads while it reads it, which is a data race as
		// well as a silent change to what the hook claims to write.
		o.derives = append(o.derives, deriveEntry{fn: fn, writes: append([]string(nil), writes...)})
	}
}

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
	cfg, _, _, err := loadValue[T](ctx, o)
	if err != nil {
		return zero, err
	}
	return cfg, nil
}

// loadValue is the shared load path used by Load and Watch's initial resolve. It
// returns the built config, the per-spec resolved values (for change detection
// in Watch), and what it learned about the bootstrap cache.
//
// The bootstrapOrigin is nil unless WithBootstrapCache is configured. It is the
// only way Watch can tell that the values it is about to seed the engine with
// came off disk rather than from a backend, which is what Report, Health and
// Doctor then say out loud. Load discards it: a one-shot Load has no reporting
// surface to put it on, and it returns to a caller who is about to use the
// configuration and nothing else.
func loadValue[T any](ctx context.Context, o *options) (T, []resolved, *bootstrapOrigin, error) {
	var cfg T

	// A misconfigured bootstrap cache fails before any provider round trip, the
	// same reason the hook assertions below do. WithBootstrapCache cannot return
	// an error from an Option, so it parks one here; surfacing it only when the
	// cache is first needed would mean the process learns its fallback was never
	// viable at the exact moment it is relying on it.
	if o.bootstrap != nil && o.bootstrap.err != nil {
		return cfg, nil, nil, o.bootstrap.err
	}

	// Checked before any provider round trip, for the same reason Watch checks
	// it before calling loadValue at all (see typedPreApply's doc comment):
	// a hook typed for the wrong T is a caller bug that should fail loudly and
	// immediately, not after fields have already been resolved. This duplicates
	// Watch's own check for the Watch path (harmless: same nil-or-error result
	// either time), but it is the ONLY check for Load, which has no earlier
	// point of its own to catch this.
	hook, err := typedPreApply[T](o)
	if err != nil {
		return cfg, nil, nil, err
	}
	// Checked here too, for the identical reason: a mismatched WithDerive hook
	// is the same kind of caller bug as a mismatched PreApply one, and must
	// fail before resolveAll spends a round trip, not after. Running this check
	// only after resolveAll and buildInto would let a failing resolve mask the
	// mismatch entirely and cost a full round of provider round trips first.
	// See typedDerives's doc comment.
	derives, err := typedDerives[T](o)
	if err != nil {
		return cfg, nil, nil, err
	}

	t := reflect.TypeOf(cfg)
	specs, err := fieldSpecs(t, o.refVars)
	if err != nil {
		return cfg, nil, nil, err
	}

	// The ordinary resolve runs first, always. The snapshot is a fallback, never
	// a fast path: a healthy process must never serve from disk, or the cache
	// would start masking a backend that is up but wrong, and nobody would learn
	// about it until the values on disk expired.
	var origin *bootstrapOrigin
	res, err := resolveAll(ctx, specs, o)
	if err != nil {
		if o.bootstrap == nil {
			return cfg, nil, nil, err
		}
		restored, writtenAt, berr := restoreBootstrap(o, specs, err, o.clock.Now())
		if berr != nil {
			return cfg, nil, nil, berr
		}
		o.log().Warn("the configuration backend is unreachable; booting from the bootstrap snapshot",
			append([]any{"snapshot_written_at", writtenAt.UTC().Format(time.RFC3339)}, errAttrs(err)...)...)
		res = restored
		origin = &bootstrapOrigin{present: true, restored: true, writtenAt: writtenAt, resolveErr: err}
	}

	if err := buildInto(reflect.ValueOf(&cfg).Elem(), res, o.decodeHooks); err != nil {
		return cfg, nil, nil, bootstrapReplayErr(origin, err)
	}
	// Derives run here, after decode and before validation, so a derived field
	// is validated on its derived value rather than the zero value it held a
	// moment ago. See WithDerive for why this position and not after. The type
	// assertion itself already happened above, before resolveAll; this loop
	// only invokes the hooks.
	for _, d := range derives {
		if err := d.fn(&cfg); err != nil {
			return cfg, nil, nil, bootstrapReplayErr(origin, &DeriveError{Err: err})
		}
	}
	// Validation runs on a restored snapshot exactly as it does on a live
	// resolve, and that is the property that makes the cache safe rather than a
	// way to smuggle a configuration past the gate. A snapshot only ever holds
	// values that passed this same check when they were written, so a failure
	// here means the rules changed, not that the file did.
	if err := o.validator.Validate(cfg); err != nil {
		return cfg, nil, nil, bootstrapReplayErr(origin, &ValidationError{Err: err})
	}

	// Gate the initial configuration too (decision D7): a hook that verifies a
	// credential should verify the first one, so a credential that does not
	// work fails at startup (Watch) or on the spot (Load) rather than at the
	// first rotation. Old is the zero value of T here, since nothing was
	// serving yet.
	//
	// Fields is populated, not left nil, using the same diff rule buildCandidate
	// applies (reconciler.go) against the true prior state at this point: no
	// field has an applied version yet, and buildCandidate already treats a
	// missing applied entry the same as an explicit "". That yields one
	// FieldChange per resolved field, each with OldVersion "" - exactly what
	// buildCandidate would compute an instant later - which is what makes
	// ev.Changed(path) true for every field on this load, the property a
	// PreApply hook needs to verify the initial configuration at all.
	//
	// This is the only place either Load or Watch's initial resolve runs the
	// gate: Watch stores this call's result directly into the engine's
	// lastGood/cfg without a further gate of its own, so gating here costs
	// exactly one hook invocation for the initial configuration, not two.
	// Guarded on the hook being non-nil, since nothing else reads this slice:
	// without the guard every Load and every Watch would allocate one
	// FieldChange per resolved field for a hook that is not there, the common
	// case since PreApply is opt-in.
	var fields []FieldChange
	if hook != nil {
		for _, r := range res {
			if r.set {
				fields = append(fields, FieldChange{Path: r.spec.Path, NewVersion: r.value.Version})
			}
		}
		// A declared derive write path carries no ref and no Version, so the
		// loop above never sees it; append its own diff here, comparing the
		// zero value of T (nothing was serving before this load) against cfg,
		// which already carries whatever the derive loop just above wrote to
		// it. See derivedFieldChanges for why this is the identical comparison
		// buildCandidate and diffApplied perform for a reconciled update, not a
		// second implementation of it.
		var zero T
		fields = append(fields, derivedFieldChanges(zero, cfg, derives, specs)...)
	}
	// The reentrancy mark is nil here, and that is not an oversight: this gate
	// runs on the CALLER's goroutine, inside Load or inside Watch before it has
	// constructed a Watcher at all. There is no reconciler goroutine yet and no
	// control channel to send on, so there is nothing a hook could reenter -
	// a hook reaching for the Watcher this load is producing can only find the
	// nil it has not been assigned yet. Only flush's gate, which runs on the
	// reconciler goroutine itself, needs the mark (see reconciler.go).
	if err := runPreApply(ctx, hook, o.preApplyTimeout, Change[T]{New: cfg, Fields: fields}, nil); err != nil {
		return cfg, nil, nil, bootstrapReplayErr(origin, err)
	}

	// The snapshot is written for a configuration that has passed decode, every
	// derive hook, validation and the gate, so it can never hold something this
	// process itself refused. A restore is deliberately excluded: rewriting the
	// file with what was just read from it would reset its age and quietly hand
	// back the staleness bound BootstrapMaxAge exists to enforce.
	if o.bootstrap != nil && origin == nil {
		origin = &bootstrapOrigin{}
		if at := persistBootstrap(o, specs, res, o.clock.Now(), o.onError); !at.IsZero() {
			origin.present, origin.writtenAt = true, at
		}
	}

	return cfg, res, origin, nil
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
