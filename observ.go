package mamori

import (
	"context"
	"time"
)

// Meter is the minimal metrics sink mamori emits to. It is deliberately tiny so
// the core module takes no OpenTelemetry dependency; the x/otel module provides
// an adapter that implements this interface on top of an OTel meter. Pass one
// with WithMeter. All methods must be safe for concurrent use.
type Meter interface {
	// RecordResolve reports a provider resolve: its scheme, duration, and error
	// (nil on success).
	RecordResolve(scheme string, dur time.Duration, err error)
	// RecordRefresh reports that a watched value changed and was reconciled.
	// scheme is always a real provider scheme (env, aws-sm, vault, ...), never
	// empty and never a placeholder: a WithDerive-declared field changing
	// never calls this, because it has no ref and therefore no scheme to
	// report, and a derive rebuild is not a "watched value" changing in the
	// first place - it is a pure recomputation from fields that were already
	// watched (and already recorded their own RecordRefresh when they
	// changed). See isDerivedPath (reconciler.go), which flush uses to skip
	// exactly this case rather than recording it under a fabricated scheme
	// label.
	RecordRefresh(scheme string)
	// RecordWatchError reports a provider watch-channel error.
	RecordWatchError(scheme string)
	// RecordStale reports that a value has not refreshed within the WithStale
	// threshold.
	RecordStale(scheme string)
	// RecordChangeDropped reports that a change event was discarded because the
	// OnChange dispatch queue was full. A non-zero rate means handlers are not
	// keeping up and callers are missing changes.
	RecordChangeDropped()
	// RecordApplyRejected reports that a candidate configuration was refused
	// and the previous one is still being served.
	RecordApplyRejected(reason RejectReason)
}

// BootstrapMeter is an optional interface for metrics sinks that also count
// WithBootstrapCache snapshot-write failures. mamori type-asserts for it where
// a write fails (see persistBootstrap, bootstrapstore.go) and records nothing
// when the configured Meter does not implement it.
//
// It is separate from Meter because Meter is implemented by callers: a seventh
// method on it would stop every hand-written sink from compiling, for a counter
// that only processes using the bootstrap cache can ever emit. The x/otel and
// x/prom bridges implement it, so nothing has to be done to get this counter
// when using either of them.
type BootstrapMeter interface {
	Meter
	// RecordBootstrapWriteFailed reports that the WithBootstrapCache snapshot
	// could not be written after an otherwise good configuration was applied.
	//
	// It is a counter rather than an error return because such a failure must
	// never reject the configuration behind it (see persistBootstrap). Nothing
	// observable breaks the moment it happens, which is exactly why it needs a
	// metric: the damage appears later, at a restart that finds no fallback. A
	// non-zero rate means the process is running without the safety net it was
	// configured to have.
	RecordBootstrapWriteFailed()
}

// RejectReason names why a candidate configuration was refused. It is a closed
// set of three so an adapter can use it as a metric label without unbounded
// cardinality, which a free-form string would invite.
type RejectReason string

const (
	// RejectValidation means the candidate failed the configured Validator.
	RejectValidation RejectReason = "validation"
	// RejectPreApply means a PreApply hook refused the change.
	RejectPreApply RejectReason = "preapply"
	// RejectDerive means a WithDerive hook returned an error.
	RejectDerive RejectReason = "derive"
)

// Tracer is the minimal tracing sink mamori emits to (see Meter for the no-dep
// rationale). Pass one with WithTracer.
type Tracer interface {
	// StartResolve begins a span for a resolve and returns a derived context plus
	// a finish function to be called with the resolve error (nil on success).
	StartResolve(ctx context.Context, scheme, ref string) (context.Context, func(err error))
}

// noopMeter / noopTracer are the defaults, used when no observer is configured.
//
// noopMeter deliberately stops at Meter and does not implement BootstrapMeter:
// a no-op counter would be indistinguishable from no counter, and leaving it off
// means the default configuration exercises the same type assertion a caller's
// own Meter takes.
type noopMeter struct{}

func (noopMeter) RecordResolve(string, time.Duration, error) {}
func (noopMeter) RecordRefresh(string)                       {}
func (noopMeter) RecordWatchError(string)                    {}
func (noopMeter) RecordStale(string)                         {}
func (noopMeter) RecordChangeDropped()                       {}
func (noopMeter) RecordApplyRejected(RejectReason)           {}

type noopTracer struct{}

func (noopTracer) StartResolve(ctx context.Context, _, _ string) (context.Context, func(error)) {
	return ctx, func(error) {}
}
