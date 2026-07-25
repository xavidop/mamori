package mamori

// buildReport constructs an immutable Report from the engine's current state.
// It is called only by the reconciler goroutine, so it reads the engine maps
// without locking: they are never written by any other goroutine. Age and
// Stale are computed as of build time; Status recomputes both again at read
// time so a report that has gone unrefreshed does not look fresher than it
// is.
func (e *engine[T]) buildReport() *Report {
	now := e.o.clock.Now()
	fields := make([]FieldStatus, 0, len(e.specs))
	healthy := true
	for i, spec := range e.specs {
		// Report the WINNING ref of the chain (see recomputeWinner), not
		// always spec.Refs[0], so a chain's Status/Report names the source
		// actually in effect - matching how Doctor already reports the
		// winning ref for the same field (doctor.go). For a one-element
		// chain this is always spec.Refs[0], byte-identical to before
		// chains existed.
		ref := spec.Refs[0]
		if _, pos, _ := e.recomputeWinner(i); pos >= 0 {
			ref = spec.Refs[pos]
		}
		fs := FieldStatus{
			Path:      spec.Path,
			Scheme:    ref.Scheme,
			Ref:       redactRef(ref),
			Sensitive: spec.Sensitive,
		}
		if v, ok := e.observed[spec.Path]; ok {
			fs.Version = v.Version
		}
		if last, ok := e.lastOK[spec.Path]; ok {
			fs.LastOK = last
			fs.Age = now.Sub(last)
			if e.o.stale > 0 && fs.Age > e.o.stale {
				fs.Stale = true
			}
		}
		if err := e.lastErr[spec.Path]; err != nil {
			fs.LastError = err.Error()
			fs.LastKind = ErrorKind(err)
		}
		if fieldUnhealthy(fs) {
			healthy = false
		}
		fields = append(fields, fs)
	}
	// served is the version Get actually returns: the pinned version while
	// pinned (Snapshot freezes there even as Live keeps climbing), otherwise
	// the same as Live.
	served := e.version
	if e.pinned {
		served = e.pinnedVersion
	}
	return &Report{
		Fields:      fields,
		Snapshot:    served,
		Live:        e.version,
		Pinned:      e.pinned,
		Healthy:     healthy,
		GeneratedAt: now,
	}
}

// fieldUnhealthy is the single source of truth for what makes one field
// unhealthy, shared by buildReport, Status, and Health (and Doctor, later).
// KindNotFound, KindPermissionDenied, KindUnauthenticated, and KindInvalid are
// terminal: they will not clear without human action, so a field carrying one
// is unhealthy immediately. Everything else (including no error at all) is
// judged only by staleness, since KindUnavailable and KindRateLimited are
// expected to self-heal on the next successful resolve.
func fieldUnhealthy(fs FieldStatus) bool {
	switch fs.LastKind {
	case KindNotFound, KindPermissionDenied, KindUnauthenticated, KindInvalid:
		return true
	}
	return fs.Stale
}

// Status returns a point-in-time report of the watcher's per-field health. It
// is lock-free: it only ever Load()s the report pointer most recently
// published by the reconciler goroutine and works on a copy, never touching
// the engine's maps directly. That matters because the reconciler goroutine
// may be mid-mutation of those maps on its own goroutine at the exact moment
// Status is called from a caller's goroutine.
//
// Age and Stale are recomputed against the watcher's clock (not the stored
// report's build time) so a watcher that has gone quiet does not keep
// reporting the age it had at the last reconcile.
func (w *Watcher[T]) Status() Report {
	rep := w.report.Load()
	if rep == nil {
		return Report{}
	}
	out := *rep
	out.Fields = make([]FieldStatus, len(rep.Fields))
	copy(out.Fields, rep.Fields)

	now := w.clock.Now()
	out.GeneratedAt = now
	out.Healthy = true
	for i := range out.Fields {
		f := &out.Fields[i]
		if !f.LastOK.IsZero() {
			f.Age = now.Sub(f.LastOK)
			f.Stale = w.stale > 0 && f.Age > w.stale
		}
		if fieldUnhealthy(*f) {
			out.Healthy = false
		}
	}
	return out
}

// Health returns nil when every field is fresh and no field carries a
// terminal error kind. It wraps the offending fields in a HealthError
// otherwise, so a caller can log which fields are broken instead of a bare
// "unhealthy". Intended for use as a readiness probe.
func (w *Watcher[T]) Health() error {
	rep := w.Status()
	var bad []FieldStatus
	for _, f := range rep.Fields {
		if fieldUnhealthy(f) {
			bad = append(bad, f)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return &HealthError{Fields: bad}
}
