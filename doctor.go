package mamori

import (
	"context"
	"errors"
	"reflect"
)

// Doctor resolves every field of T exactly once and returns a Report describing
// what succeeded and what failed, without starting a watcher. It accepts the
// same Options as Load and Watch, so it exercises the caller's real provider
// wiring, middleware, and Prefix rewriting: run it in a build-tagged CI test to
// catch a misconfigured ref before it ships.
//
// The returned error is non-nil only when T itself cannot be walked as a config
// struct. Individual field failures are recorded in the Report, not returned, so
// a caller sees every problem at once rather than only the first. Doctor does
// not decode or validate; a field that resolves but fails validation is Load's
// concern, not a reachability check's.
//
// Report.Snapshot and Report.Live are always 0, signaling a one-shot probe
// rather than a running watcher's snapshot (whose version starts at 1).
func Doctor[T any](ctx context.Context, opts ...Option) (Report, error) {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}
	var cfg T
	specs, err := fieldSpecs(reflect.TypeOf(cfg), o.refVars)
	if err != nil {
		return Report{}, err
	}

	now := o.clock.Now()
	fields := make([]FieldStatus, 0, len(specs))
	healthy := true
	for _, spec := range specs {
		val, idx, rerr := probeField(ctx, spec, o)
		fs := FieldStatus{
			Path:      spec.Path,
			Sensitive: spec.Sensitive,
		}
		var pe *ProviderError
		switch {
		case rerr == nil:
			// The winning ref reports the outcome: for a single-ref field
			// idx is always 0, matching today's behavior exactly; for a
			// chain it is whichever ref actually produced the value.
			fs.Scheme = spec.Refs[idx].Scheme
			fs.Ref = redactRef(spec.Refs[idx])
		case errors.As(rerr, &pe):
			// resolveChain's terminal error is always a *ProviderError
			// tagged with whichever ref stopped the walk (or, when every ref
			// was not-found, refs[0]); report that ref rather than always
			// refs[0], so a chain's Doctor output names the source that
			// actually failed.
			fs.Scheme = pe.Scheme
			fs.Ref = pe.Ref
		default:
			fs.Scheme = spec.Refs[0].Scheme
			fs.Ref = redactRef(spec.Refs[0])
		}
		switch {
		case rerr == nil:
			fs.Version = val.Version
			fs.LastOK = now
		case errors.Is(rerr, ErrNotFound) && (spec.HasDefault || spec.Optional):
			// Absent but covered by a default or optional: this resolves in
			// practice, so it is healthy even though the provider itself has
			// nothing for this ref.
			fs.LastOK = now
		default:
			fs.LastError = rerr.Error()
			fs.LastKind = ErrorKind(rerr)
		}
		if fieldUnhealthy(fs) {
			healthy = false
		}
		fields = append(fields, fs)
	}
	return Report{
		Fields:      fields,
		Snapshot:    0, // Doctor is a one-shot probe, not a running snapshot
		Live:        0,
		Healthy:     healthy,
		GeneratedAt: now,
	}, nil
}

// probeField walks spec's precedence chain through resolveChain, returning
// the winning value and its index, or the terminal error that stopped the
// walk, without applying default:/optional:/onfail: handling or aborting. It
// is Doctor's non-fail-fast counterpart to resolveOne: resolveOne turns a
// resolveChain outcome into a decoded field value (applying policy along the
// way), while probeField reports the raw outcome so a caller can classify it
// itself. Doctor uses that raw outcome to decide per-field health without
// applying policy at all - a field a policy would repair (default, optional)
// is still reported as reachable-or-not on its own merits.
//
// probeField (via resolveChain -> resolveRef) always resolves one ref at a
// time and never groups refs by scheme through BatchProvider.ResolveBatch.
// Every in-repo BatchProvider (aws-sm, aws-ps) also implements a fully
// working single Resolve that hits the same underlying per-item API;
// ResolveBatch exists purely to cut round trips when many refs of the same
// scheme are resolved together, not to change what a single ref resolves to.
// The middleware wrapper in package middleware goes further and implements
// ResolveBatch by looping over single Resolve calls itself. Single-resolve is
// therefore semantically faithful to how Load resolves the same ref, and it
// keeps Doctor's per-field bookkeeping (one FieldStatus per spec)
// straightforward, so grouping by scheme is unnecessary complexity here.
func probeField(ctx context.Context, spec fieldSpec, o *options) (Value, int, error) {
	return resolveChain(ctx, spec.Refs, o)
}
