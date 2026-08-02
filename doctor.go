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
// not populate T's fields or validate them: a field that resolves but fails
// validation is Load's concern, not a reachability check's. It does run a ref's
// ?decode= pipeline, which happens inside resolution rather than after it (see
// resolveRef), so a value whose declared encoding does not match what the
// backend holds is reported here as a failed field with LastKind "invalid" -
// exactly as it would fail at Load, which is the point of a preflight check.
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
	res := make([]resolved, 0, len(specs))
	healthy := true
	// sourcesComplete tracks whether every spec ended the probe loop holding
	// the value Load would have built it from - a resolved value, its default,
	// or (for an optional field) the zero value Load itself leaves behind. It
	// is deliberately NOT the same question as healthy: fieldUnhealthy counts
	// only the terminal kinds, so a field failing with KindUnavailable,
	// KindRateLimited, or KindUnknown leaves healthy true while contributing
	// no entry to res at all. Gating the derive hooks on healthy therefore fed
	// them a zero value for such a field and published a hash of it; see
	// doctorDerivedFields for why any hash computed from a stand-in value is
	// worse than none.
	sourcesComplete := true
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

		switch {
		case rerr == nil:
			res = append(res, resolved{spec: spec, value: val, found: true, set: true})
		case errors.Is(rerr, ErrNotFound) && spec.HasDefault:
			// Mirror applyDefault (resolve.go) exactly: an absent field
			// covered by a default is reported healthy above, so the hooks must
			// see the default rather than the zero value, or the version this
			// publishes would not match what Load computes.
			res = append(res, resolved{
				spec:  spec,
				value: Value{Bytes: []byte(spec.Default), Sensitive: spec.Sensitive, Version: "default"},
				found: false,
				set:   true,
			})
		case errors.Is(rerr, ErrNotFound) && spec.Optional:
			// applyDefault's optional branch sets nothing, so Load leaves this
			// field at its zero value too. The hooks seeing zero here is not a
			// stand-in for a value that failed to arrive: it is exactly what
			// production computes from, so this spec still counts as complete.
		case spec.OnFail == onFailUseDefault:
			// The error is terminal and not not-found (the branches above took
			// every not-found case), which is precisely what applyOnFail
			// (resolve.go) turns into the field's default for an explicit
			// onfail:"default". Mirror it, or the hooks would derive from zero
			// here while Load derives from the default and the two versions
			// would disagree. walkSpecs rejects onfail:"default" without a
			// default: tag, so spec.Default is always the real value.
			res = append(res, resolved{
				spec:  spec,
				value: Value{Bytes: []byte(spec.Default), Sensitive: spec.Sensitive, Version: "default"},
				found: false,
				set:   true,
			})
		default:
			// Nothing to build this field from that Load would agree with.
			sourcesComplete = false
		}
	}

	derivedFields, derivedHealthy := doctorDerivedFields[T](o, specs, res, sourcesComplete)
	fields = append(fields, derivedFields...)
	healthy = healthy && derivedHealthy

	return Report{
		Fields:      fields,
		Snapshot:    0, // Doctor is a one-shot probe, not a running snapshot
		Live:        0,
		Healthy:     healthy,
		GeneratedAt: now,
	}, nil
}

// doctorDerivedFields runs the registered WithDerive hooks against the values
// the probe loop already resolved and returns one FieldStatus per declared
// write path. It is Doctor's counterpart to the Derived append in buildReport
// (report.go), and uses the same hasSpecPath / fieldByPath / CanInterface gates
// so the two can never disagree about which paths produce a row. The one
// exception is a set of hooks that cannot be typed to T at all, where those
// gates cannot be evaluated and would hide a config that fails at startup: see
// deriveConfigErrorFields.
//
// The hooks run only when every sourced field came out of the probe loop
// holding the value Load would have built it from (sourcesComplete, above). A
// hook fed a zero value because its input never resolved would produce a
// version that looks real, does not match what production computes, and is
// worse than no version: those rows report blocked instead. Note that this is
// not the report's healthy flag: a field failing with a self-healing kind
// (unavailable, rate-limited, unknown) leaves healthy true and still has no
// value to feed a hook. mamori cannot inspect a closure to learn which fields
// it reads, so blocked is all-or-nothing across derived fields rather than
// per-input.
//
// Running the hooks means Doctor executes caller code during a preflight.
// WithDerive documents a hook as a pure transformation and nothing enforces it,
// so this is a deliberate, documented trade: probing a derived field is not
// possible without evaluating the function that produces it.
func doctorDerivedFields[T any](o *options, specs []fieldSpec, res []resolved, sourcesComplete bool) ([]FieldStatus, bool) {
	derives, err := typedDerives[T](o)
	if err != nil {
		return deriveConfigErrorFields[T](o, err), false
	}
	if len(derives) == 0 {
		return nil, true
	}
	var cfg T
	var deriveErr error
	if sourcesComplete {
		if err := buildInto(reflect.ValueOf(&cfg).Elem(), res, o.decodeHooks); err != nil {
			deriveErr = err
		} else {
			for _, d := range derives {
				if err := d.fn(&cfg); err != nil {
					deriveErr = &DeriveError{Err: err}
					break
				}
			}
		}
	}
	cv := reflect.ValueOf(cfg)
	seen := make(map[string]struct{})
	healthy := true
	var out []FieldStatus
	for _, d := range derives {
		for _, p := range d.writes {
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			if hasSpecPath(specs, p) {
				continue
			}
			v, ok := fieldByPath(cv, p)
			if !ok || !v.CanInterface() {
				continue
			}
			fs := FieldStatus{
				Path:      p,
				Derived:   true,
				Sensitive: v.Type() == secretStringType || v.Type() == secretBytesType,
			}
			switch {
			case !sourcesComplete:
				fs.LastError = "not evaluated: a source field is unreachable"
			case deriveErr != nil:
				fs.LastError = deriveErr.Error()
				fs.LastKind = KindInvalid
			default:
				fs.Version = derivedVersion(v)
			}
			if fieldUnhealthy(fs) {
				healthy = false
			}
			out = append(out, fs)
		}
	}
	return out, healthy
}

// deriveConfigErrorFields turns a typedDerives rejection into one unhealthy
// FieldStatus per declared write path, carrying KindInvalid and the rejection's
// own text.
//
// The rejection means Load and Watch, given these same Options, fail outright
// with ErrInvalid: a hook typed for another config, or one declaring an empty
// write path. Doctor cannot return that as its error - its contract is that the
// returned error covers only an unwalkable T - and returning nothing would make
// the preflight whose entire purpose is catching a misconfiguration before it
// ships report green on one that breaks at startup. Silent and healthy is the
// one outcome a preflight must never produce, so the rows carry it instead.
//
// The rows are built from the raw registration (o.derives) rather than from T,
// because the assertion that would give them a typed hook is the thing that
// just failed. That is also why they skip the hasSpecPath and fieldByPath gates
// the healthy path applies: those answer "does this path already have a row, or
// name a real field on T", and neither question can hide a configuration that
// cannot start. A write path that also carries a source tag therefore gets a
// second, Derived row here, saying why the whole load is rejected.
//
// A hook registered with no write paths at all produces no row, since a row is
// keyed by write path and it declared none. The report is still unhealthy: the
// caller sees a failing preflight with nothing to point at, which is the same
// invisibility WithDerive's declared writes exist to fix and is strictly better
// than a green report on a config that cannot load.
// Sensitive is read from T's own field type rather than left false, matching
// what buildReport (report.go) does for a healthy derived row. The hooks could
// not be typed, so there is no config to inspect, but the zero T still carries
// the declared path's type, which is all Sensitive depends on. Without this a
// derived secret would flip the CLI's SENSITIVE column to false purely because
// the run happened to fail this way.
func deriveConfigErrorFields[T any](o *options, err error) []FieldStatus {
	var zero T
	zv := reflect.ValueOf(zero)
	seen := make(map[string]struct{})
	var out []FieldStatus
	for _, d := range o.derives {
		for _, p := range d.writes {
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			fs := FieldStatus{
				Path:      p,
				Derived:   true,
				LastError: err.Error(),
				LastKind:  KindInvalid,
			}
			if f, ok := fieldByPath(zv, p); ok {
				fs.Sensitive = f.Type() == secretStringType || f.Type() == secretBytesType
			}
			out = append(out, fs)
		}
	}
	return out
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
// BatchProvider embeds Provider, so every provider implementing it also
// implements a fully working single Resolve against the same underlying
// data; ResolveBatch exists purely to cut round trips when many refs of the
// same scheme are resolved together, not to change what a single ref
// resolves to. Single-resolve is therefore semantically faithful to how Load
// resolves the same ref, and it keeps Doctor's per-field bookkeeping (one
// FieldStatus per spec) straightforward, so grouping by scheme is
// unnecessary complexity here.
func probeField(ctx context.Context, spec fieldSpec, o *options) (Value, int, error) {
	return resolveChain(ctx, spec.Refs, o)
}
