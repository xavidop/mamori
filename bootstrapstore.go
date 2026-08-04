package mamori

import (
	"fmt"
	"os"
	"time"
)

// bootstrapOrigin records what a load learned about the bootstrap cache, so
// Watch can seed the engine's reporting from it and Load can be told nothing at
// all. It is deliberately not part of any exported surface: what leaves the
// process is BootstrapStatus, which carries no bytes.
//
// restored distinguishes the case that matters. A snapshot that was merely
// written by this load is fresh by construction and says nothing about the
// health of the process; one that was RESTORED means the backend could not be
// reached and every value being served came off disk.
type bootstrapOrigin struct {
	present   bool
	restored  bool
	writtenAt time.Time
	// resolveErr is the failure that made the restore necessary. It is kept so
	// a later rejection of the restored configuration (a decode, derive,
	// validation or PreApply failure) can name the outage as well as the
	// rejection, which is the only combination that tells an operator why the
	// process will not start.
	resolveErr error
}

// bootstrapTransient reports whether a resolve failure means the backend could
// not be reached, which is the only situation a cached value is the best
// available answer for.
//
// KindNotFound, KindPermissionDenied, KindUnauthenticated and KindInvalid all
// mean the backend answered and said no: a deleted secret, a revoked
// credential, a changed policy, a malformed ref. Serving a cached copy of a
// value the backend has deliberately removed would defeat the removal, so those
// fail the start. KindUnknown joins them, because a provider that cannot
// classify its own failure has not established that the backend was unreachable,
// and guessing in the permissive direction is guessing about a revocation.
func bootstrapTransient(k Kind) bool {
	return k == KindUnavailable || k == KindRateLimited
}

// bootstrapRecords turns the resolved values behind an applied configuration
// into the records a snapshot stores.
//
// Only a field that was actually set is recorded. An optional field the backend
// had nothing for contributes no record, which is what lets a replay leave it at
// its zero value without needing a separate "this was absent" marker.
//
// The bytes are referenced rather than copied: sealSnapshot marshals them
// synchronously on this goroutine, and every extra copy of a secret is another
// page that has to be zeroed or swapped.
func bootstrapRecords(res []resolved) []snapshotRecord {
	recs := make([]snapshotRecord, 0, len(res))
	for _, r := range res {
		if !r.set {
			continue
		}
		ref := ""
		if len(r.spec.Refs) > 0 {
			ref = redactRef(r.spec.Refs[0])
		}
		recs = append(recs, snapshotRecord{
			Path:         r.spec.Path,
			Ref:          ref,
			Bytes:        r.value.Bytes,
			ValueVersion: r.value.Version,
			Sensitive:    r.value.Sensitive || r.spec.Sensitive,
			NotAfter:     r.value.NotAfter,
			Metadata:     r.value.Metadata,
		})
	}
	return recs
}

// writeBootstrapSnapshot seals res and replaces the snapshot file with it.
//
// It is called only where a configuration has already passed decode, every
// WithDerive hook, validation and the PreApply gate, so the file can never hold
// a value this process itself refused.
func writeBootstrapSnapshot(o *options, specs []fieldSpec, res []resolved, now time.Time) error {
	sealed, err := sealSnapshot(snapshot{
		Version:     snapshotFormatVersion,
		WrittenAt:   now.UTC(),
		Fingerprint: schemaFingerprint(specs),
		Records:     bootstrapRecords(res),
	}, o.bootstrap.key)
	if err != nil {
		return err
	}
	return writeSnapshotFile(o.bootstrap.path, sealed)
}

// persistBootstrap writes the snapshot and reports a failure instead of
// returning it, answering with the write time on success and the zero time
// otherwise.
//
// A write failure must never fail an update. The configuration is good: it
// resolved, decoded, derived, validated and passed the gate. Refusing it because
// a cache file could not be written would invert the whole point of the feature,
// turning a fallback meant to survive an outage into a new way to fail during
// one. So the failure goes to the logger, to the meter, and to OnError, which is
// the only channel a caller has for a problem that cannot be returned anywhere.
//
// emit is the caller's OnError delivery: the engine's emitErr on the reconciler
// goroutine (which arms the reentrancy guard), or a direct call on the load
// path, where no reconciler goroutine exists yet for a callback to reenter.
func persistBootstrap(o *options, specs []fieldSpec, res []resolved, now time.Time, emit func(error)) time.Time {
	if err := writeBootstrapSnapshot(o, specs, res, now); err != nil {
		o.meter.RecordBootstrapWriteFailed()
		o.log().Error("the bootstrap snapshot could not be written; this configuration was applied anyway, but a restart during an outage will have nothing to fall back to",
			errAttrs(err)...)
		if emit != nil {
			emit(err)
		}
		return time.Time{}
	}
	o.log().Debug("bootstrap snapshot written", logAttrCount, len(res))
	return now
}

// readBootstrapSnapshot reads and opens the snapshot file.
//
// The two failures it distinguishes are the two an operator acts on differently:
// there is no snapshot at all (nothing was ever written, or the volume it lived
// on did not survive the restart), or there is one that will not open (the key
// is wrong, or the file was altered).
func readBootstrapSnapshot(o *options) (snapshot, error) {
	b, err := os.ReadFile(o.bootstrap.path)
	if err != nil {
		return snapshot{}, fmt.Errorf("mamori: reading the bootstrap snapshot: %w: %w", ErrUnavailable, err)
	}
	return openSnapshot(b, o.bootstrap.key)
}

// restoreBootstrap answers a failed cold-start resolve with the values from the
// snapshot, or with an error explaining why it could not.
//
// resolveErr is the failure that brought us here, and every error returned wraps
// it alongside the cache's own failure. An operator debugging a boot that did
// not happen needs both halves: the backend was down, AND the fallback they were
// relying on is unusable. Reporting only the second reads as a cache bug;
// reporting only the first hides that the cache never helped.
func restoreBootstrap(o *options, specs []fieldSpec, resolveErr error, now time.Time) ([]resolved, time.Time, error) {
	if !bootstrapTransient(ErrorKind(resolveErr)) {
		return nil, time.Time{}, fmt.Errorf(
			"mamori: the backend answered and refused, so the bootstrap snapshot was deliberately not used: a %s failure names a value the backend still controls, and serving a cached copy of it would defeat the refusal: %w",
			ErrorKind(resolveErr), resolveErr)
	}

	s, err := readBootstrapSnapshot(o)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("mamori: the backend is unreachable and the bootstrap snapshot is unusable: %w: %w", resolveErr, err)
	}
	if fp := schemaFingerprint(specs); fp != s.Fingerprint {
		// Named as drift rather than left to fail downstream: replaying a
		// snapshot written for a different struct fails somewhere in decode or
		// validation with a message about a value, which sends the operator
		// looking at their backend during an outage instead of at the config
		// change they shipped.
		return nil, time.Time{}, fmt.Errorf("mamori: the backend is unreachable and the bootstrap snapshot was written for a different config struct, so it cannot be restored: %w: %w", resolveErr, ErrInvalid)
	}

	byPath := make(map[string]snapshotRecord, len(s.Records))
	for _, rec := range s.Records {
		// An expired lease is not restorable. NotAfter is the instant the
		// backend itself said this value stops working (Vault fills it from the
		// lease), so restoring it would hand the application a credential
		// guaranteed to fail while reporting a healthy boot. The honest answer
		// is that this process cannot start without reaching the backend.
		if !rec.NotAfter.IsZero() && !rec.NotAfter.After(now) {
			return nil, time.Time{}, fmt.Errorf("mamori: the backend is unreachable and the bootstrap snapshot's value for %s expired at %s: %w: %w",
				rec.Path, rec.NotAfter.UTC().Format(time.RFC3339), resolveErr, ErrInvalid)
		}
		byPath[rec.Path] = rec
	}

	out := make([]resolved, len(specs))
	for i, spec := range specs {
		out[i] = resolved{spec: spec}
		rec, ok := byPath[spec.Path]
		if !ok {
			// No record means the field was not set when the snapshot was
			// written, which for a field that reaches an applied config can only
			// mean optional-and-absent. The fingerprint already proved the two
			// structs agree, so this is not a missing record; it is a recorded
			// absence.
			continue
		}
		out[i].value = Value{
			Bytes:     rec.Bytes,
			Version:   rec.ValueVersion,
			Sensitive: rec.Sensitive || spec.Sensitive,
			NotAfter:  rec.NotAfter,
			Metadata:  rec.Metadata,
		}
		out[i].found = true
		out[i].set = true
	}
	return out, s.WrittenAt, nil
}

// bootstrapReplayErr explains a configuration that was restored from the
// snapshot and then rejected by the ordinary decode, derive, validate or
// PreApply path.
//
// Validation deliberately still runs on restored values (that is what makes the
// cache a fallback rather than a way past the gate), so this failure is real and
// the process must not start. It wraps the outage as well as the rejection
// because the two together are the whole story: the backend is down, and the
// snapshot that was supposed to cover for it holds a configuration this build
// will not accept.
func bootstrapReplayErr(origin *bootstrapOrigin, err error) error {
	if origin == nil || !origin.restored || err == nil {
		return err
	}
	return fmt.Errorf("mamori: the backend is unreachable and the configuration restored from the bootstrap snapshot was rejected: %w: %w", origin.resolveErr, err)
}

// seedBootstrapOrigin records that this watcher booted from the snapshot, and
// which of its fields are still carrying a value only the backend could have
// changed.
//
// A field restored to its `default:` tag value is excluded. That value did not
// come from a backend, cannot have been rotated behind this process's back, and
// is byte-for-byte what a live resolve would have produced for the same absent
// ref, so counting it would leave a config whose fields are all defaults waiting
// forever for a confirmation the backend has no way to give.
func (e *engine[T]) seedBootstrapOrigin(origin *bootstrapOrigin, restored []resolved) {
	e.bootstrapPresent = origin.present
	e.bootstrapRestored = true
	e.bootstrapWrittenAt = origin.writtenAt
	unconfirmed := make(map[string]struct{}, len(restored))
	for _, r := range restored {
		if !r.set || r.value.Version == defaultValueVersion {
			continue
		}
		unconfirmed[r.spec.Path] = struct{}{}
	}
	if len(unconfirmed) == 0 {
		return
	}
	e.bootstrapUnconfirmed = unconfirmed
	e.bootstrapServing = true
}

// markBootstrapReconciled records that path is no longer at risk of serving a
// value the backend may have rotated without this process noticing, and stops
// reporting the snapshot as the source once no path is.
//
// That risk, not the provenance of the bytes, is what BootstrapMaxAge exists to
// bound, so three outcomes end it:
//
//   - the field resolved from its backend, which would have shown a rotation;
//   - the backend answered that it is absent and a default: or optional: tag
//     covers that, which would equally have shown one (see handleChainNotFound,
//     whose tolerated branch deliberately leaves the last observed value in
//     place, and which mamori treats identically for a process that never used
//     a snapshot at all);
//   - the field's own onfail:"default" policy replaced the value outright, so
//     nothing from the snapshot is being served for it any more.
//
// Without this a pod that booted during a two-minute outage and has resolved
// normally for the twenty hours since would eventually fail its own readiness
// probe over a file it no longer depends on.
//
// The fresh snapshot written when the last path clears is not bookkeeping. The
// file on disk is at least as old as the restore that just ended, and nothing
// else would rewrite it until the configuration next changes, so a process that
// recovered from an outage would otherwise carry an ever-older fallback into the
// next one.
func (e *engine[T]) markBootstrapReconciled(path string) {
	if e.bootstrapUnconfirmed == nil {
		return
	}
	delete(e.bootstrapUnconfirmed, path)
	if len(e.bootstrapUnconfirmed) > 0 {
		return
	}
	e.bootstrapUnconfirmed = nil
	e.bootstrapServing = false
	e.o.log().Info("every restored field has been reconciled against its backend; no longer serving from the bootstrap snapshot")
	e.persistBootstrap()
}

// persistBootstrap rewrites the snapshot from everything currently observed.
//
// The values are read from e.observed rather than from the candidate that was
// just applied, because e.observed is where a resolved Value lives with its
// version, expiry and sensitivity intact; the candidate is a decoded T, which is
// exactly the shape that cannot be persisted (see snapshot).
func (e *engine[T]) persistBootstrap() {
	if e.o.bootstrap == nil {
		return
	}
	if at := persistBootstrap(e.o, e.specs, e.observedAsResolved(), e.o.clock.Now(), e.emitErr); !at.IsZero() {
		e.bootstrapPresent, e.bootstrapWrittenAt = true, at
	}
}

// observedAsResolved views the engine's per-path observed values as the
// []resolved shape the snapshot writer shares with the load path, so both write
// the same records from the same code.
func (e *engine[T]) observedAsResolved() []resolved {
	out := make([]resolved, 0, len(e.specs))
	for _, spec := range e.specs {
		v, ok := e.observed[spec.Path]
		if !ok {
			continue // optional and absent: recorded as an absence, see restoreBootstrap
		}
		out = append(out, resolved{spec: spec, value: v, found: true, set: true})
	}
	return out
}

// bootstrapStatus renders the engine's bootstrap state for a Report, or nil when
// the option is not configured.
func (e *engine[T]) bootstrapStatus(now time.Time) *BootstrapStatus {
	if e.o.bootstrap == nil {
		return nil
	}
	bs := &BootstrapStatus{
		Present:   e.bootstrapPresent,
		Restored:  e.bootstrapRestored,
		WrittenAt: e.bootstrapWrittenAt,
		// True whenever a snapshot exists at all here: this process either wrote
		// the file itself or verified the fingerprint before restoring it, so
		// there is no third case a running watcher can be in.
		FingerprintMatch: e.bootstrapPresent,
	}
	if !e.bootstrapWrittenAt.IsZero() {
		bs.Age = now.Sub(e.bootstrapWrittenAt)
	}
	if !bs.Present {
		bs.Problem = "no snapshot has been written; a restart during an outage will have nothing to fall back to"
	}
	return bs
}

// bootstrapSource names where the configuration this engine is serving came
// from, for a Report.
func (e *engine[T]) bootstrapSource() ConfigSource {
	if e.bootstrapServing {
		return SourceBootstrapCache
	}
	return SourceBackend
}

// bootstrapStatusFor inspects the snapshot file without restoring it, for
// Doctor: does one exist, how old is it, and does it still match this build's
// config struct.
//
// It reports a problem as text rather than returning an error because Doctor's
// contract is that field-level trouble lands in the Report, not in its return
// value. The text names the failure and never the contents.
func bootstrapStatusFor(o *options, specs []fieldSpec, now time.Time) *BootstrapStatus {
	if o.bootstrap == nil {
		return nil
	}
	bs := &BootstrapStatus{}
	if o.bootstrap.err != nil {
		bs.Problem = o.bootstrap.err.Error()
		return bs
	}
	s, err := readBootstrapSnapshot(o)
	if err != nil {
		bs.Problem = err.Error()
		return bs
	}
	bs.Present = true
	bs.WrittenAt = s.WrittenAt
	bs.Age = now.Sub(s.WrittenAt)
	bs.FingerprintMatch = schemaFingerprint(specs) == s.Fingerprint
	if !bs.FingerprintMatch {
		bs.Problem = "the snapshot was written for a different config struct and cannot be restored"
	}
	return bs
}
