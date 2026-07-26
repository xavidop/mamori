package server

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/xavidop/mamori"
)

// This file is the fan-out core: it turns the operator-declared binding
// table (bindings.go) into live values, watched from upstream exactly once
// per binding no matter how many concurrent callers read them.
//
// Provider resolution. Core's own Load/Watch resolve a ref's provider with
// options.provider(scheme) (reconcile.go): check the explicit,
// per-call providers map (populated by mamori.WithProvider) first, then fall
// back to the process-wide registry (mamori.Register / the unexported
// providerFor). Only the first half of that is reachable from this package:
// providerFor is unexported, so a package outside mamori has no way to look
// a scheme up in the global registry directly, and mamori.WatchRef itself
// takes the Provider as an explicit argument rather than resolving one from
// its Option list (WatchRef's opts only ever affect clock/pollInterval/
// jitter - see watchref.go's doc comment). So this package keeps its own
// scheme->Provider map (Server.providers, populated by WithProvider in
// server.go) and does its own lookup before calling mamori.WatchRef, which
// is exactly core's explicit-override path with the registry fallback
// omitted - not a different mechanism, the same one with the one
// unreachable half left out. This also happens to be the more conservative
// choice for a config server: every provider a binding can resolve through
// is one the operator named explicitly, not whatever happened to
// self-register via some imported package's init().

// bindingSnapshot is the state one binding's upstream watch publishes. A
// single mamori.WatchRef goroutine owns writing it (via resolverState.apply);
// arbitrarily many lookup callers read it concurrently.
//
// The value and its error/kind are bundled into one struct, swapped in as a
// whole with a single atomic.Pointer.Store, rather than kept as separate
// atomics for value/err/kind. That mirrors how the core reconciler publishes
// its own Report (Watcher.report atomic.Pointer[Report] in reconciler.go)
// and for the same reason: with separate atomics a lookup could load a value
// from one update and a kind from a different, later or earlier one, which
// is exactly the "fresh value paired with a stale error" (or vice versa)
// inconsistency the whole point of last-known-good tracking is to avoid.
type bindingSnapshot struct {
	// hasValue is true once the binding has resolved successfully at least
	// once. It only ever goes false->true, never back: value is always the
	// most recent SUCCESSFUL resolve, even while err/kind (below) describe a
	// later, still-ongoing failure. This is the last-known-good value.
	hasValue bool
	value    mamori.Value

	// err and kind describe the upstream's CURRENT state: nil/"" once the
	// most recent update was itself a success (apply clears both on
	// success), set when it was not. kind is computed with mamori.ErrorKind
	// once, at update time, rather than by every lookup call re-walking
	// err's chain.
	err  error
	kind mamori.Kind

	// resolvedAt is when value was last replaced by a SUCCESSFUL resolve. It
	// is carried forward untouched by a failing update, exactly as value is,
	// so it always dates the bytes actually being served rather than the last
	// attempt to fetch them. It is surfaced on the wire (see newValueBody)
	// because with several replicas each watching upstream on its own
	// schedule, "when did this value last come from upstream" is the only way
	// a client can tell that the replica it just reached is behind one it
	// already spoke to.
	resolvedAt time.Time

	// settled is false only for the initial errPendingResolve placeholder and
	// true once any real outcome has landed, success or failure. It backs
	// readiness (see Server.readiness): a replica whose bindings are still
	// unsettled has a cold cache and must not be sent traffic, while a
	// binding that settled on an ERROR is not a reason to hold the replica
	// out of rotation, since every other replica would report that same
	// upstream error.
	settled bool
}

// resolverState is start's per-binding runtime record: the parsed Binding it
// watches, and the atomic snapshot its own watch goroutine publishes to.
type resolverState struct {
	binding Binding

	snapshot atomic.Pointer[bindingSnapshot]
}

// apply folds one mamori.Update from the binding's watch into its snapshot.
// A success replaces the last-known-good value and clears any error/kind. A
// failure replaces only err/kind, carrying forward whatever value (and
// hasValue) the previous snapshot already held - this is the entire
// mechanism behind "a binding whose upstream is failing serves its
// last-known-good value": the value simply is never overwritten by a failed
// update.
func (rs *resolverState) apply(up mamori.Update) {
	if up.Err == nil {
		rs.snapshot.Store(&bindingSnapshot{
			hasValue:   true,
			value:      up.Value,
			resolvedAt: time.Now(),
			settled:    true,
		})
		return
	}
	next := &bindingSnapshot{err: up.Err, kind: mamori.ErrorKind(up.Err), settled: true}
	if prev := rs.snapshot.Load(); prev != nil && prev.hasValue {
		next.hasValue = true
		next.value = prev.value
		// resolvedAt travels with the value it dates, so a stale-but-served
		// value keeps reporting when it was actually fetched, not now.
		next.resolvedAt = prev.resolvedAt
	}
	rs.snapshot.Store(next)
}

// errNoProvider reports that a binding's ref scheme has no Provider
// registered via WithProvider. It carries no mamori sentinel - "the operator
// never wired up a provider for this scheme" is a configuration mistake, not
// one of the backend-classified failure kinds (permission denied, rate
// limited, ...) mamori.Kind exists to distinguish - so mamori.ErrorKind
// honestly reports it as KindUnknown, same as errPendingResolve below.
func errNoProvider(scheme string) error {
	return fmt.Errorf("mamori/server: no provider registered for scheme %q (see WithProvider)", scheme)
}

// errPendingResolve is the placeholder every binding's snapshot starts at,
// before its watch has delivered a first update. It exists so lookup never
// has to special-case a nil snapshot (start populates every binding's
// snapshot with this before returning, so a nil load can only ever mean "not
// a bound name"): a lookup that lands in the brief window between start
// registering a binding and its first mamori.Update arriving reports this,
// honestly classified as KindUnknown, rather than a misleading empty
// success.
var errPendingResolve = fmt.Errorf("mamori/server: binding not yet resolved")

// errAlreadyStarted is start's answer to being called a second time. start
// is meant to run once, when a later task's Serve begins serving; calling it
// twice would launch a second set of per-binding watch goroutines racing the
// first to publish into the same resolverStates; refusing the second call
// outright is cheaper and clearer than trying to make that race safe.
var errAlreadyStarted = fmt.Errorf("mamori/server: start called more than once")

// errClosed is start's (and, transitively, Serve's) answer to being called
// on a Server that Close has already made its teardown transition against.
// See closed's doc comment in server.go for why this needs its own sentinel
// rather than start silently doing nothing: a Server must tolerate Close
// being called before Serve/start ever ran at all (an error path, or a
// signal handler racing `go s.Serve(ctx)`), and without this check that
// ordering would let a LATER Serve bind real listeners and spawn real
// goroutines that the already-spent Close could never reach again.
var errClosed = fmt.Errorf("mamori/server: server is closed")

// start begins upstream watching: one mamori.WatchRef per binding, through
// the Provider registered for that binding's ref scheme (see WithProvider in
// server.go and this file's package-level doc comment for how that provider
// is found). It is unexported and separate from New deliberately - see
// New's and the package doc's comments in server.go - because starting
// goroutines is an action with a lifecycle (Close, below, must stop them),
// which does not belong inside a constructor that a caller may invoke
// speculatively, discard the error and result of, or call from a context
// with no natural place to hold a *Server for a Close later. The later
// transports task (Serve, coordinate with Task 5) is expected to be start's
// one caller, running it before accepting any connection, so that every
// binding has at least a pending-resolve snapshot (see errPendingResolve)
// by the time a request could possibly arrive.
//
// A binding whose scheme has no registered provider is not treated as a
// construction-time error: it gets a snapshot fixed at errNoProvider and no
// watch goroutine of its own (there is nothing to watch), so lookup on it
// fails clearly and every OTHER binding still starts and resolves normally,
// rather than one misconfigured binding taking down the whole server. This
// mirrors how a required struct field with no matching provider fails at
// Load, not at Watch's construction.
//
// Every watch goroutine start spawns is tracked by s.resolveWG and exits
// when ctx is cancelled - modeled directly on how the core reconciler's
// per-position watch goroutines are tracked by Watcher.wg and exit on ctx.Done
// (see the per-ref loop in engine.start, reconciler.go). Close cancels the
// context derived from ctx here and waits on exactly that WaitGroup, so a
// goleak check after Close sees nothing left running.
//
// start runs its entire body under resolveMu, not just the resolveCancel
// assignment: this is what makes it safe against a Close arriving
// concurrently from a completely different goroutine (see resolveMu's doc
// comment in server.go). Either start acquires the lock first and Close then
// observes a fully-populated resolveCancel plus every resolveWG.Add already
// made (so cancel+Wait tears down exactly what was spawned, nothing more,
// nothing not-yet-counted), or Close acquires the lock first, sets closed,
// and start then sees closed already true and refuses outright (errClosed,
// below) - spawning nothing at all. There is no window in between where a
// goroutine could be spawned uncounted or a resolveCancel left unreadable.
func (s *Server) start(ctx context.Context) error {
	s.resolveMu.Lock()
	defer s.resolveMu.Unlock()

	if s.closed {
		return errClosed
	}
	if s.resolveCancel != nil {
		return errAlreadyStarted
	}
	rctx, cancel := context.WithCancel(ctx)
	s.resolveCancel = cancel

	// servingCtx is the OTHER context this call creates: a child of the same
	// ctx Serve was given, independently cancelable via servingCancel, that
	// every accepted HTTP request's r.Context() derives from (newHTTPServer's
	// BaseContext, transport.go) - see servingCtx's own doc comment in
	// server.go for why this exists at all (a streaming handler like
	// handleWatch otherwise only ever unblocks when the client disconnects,
	// never when this Server shuts down) and why creating it here, under the
	// same resolveMu-guarded, closed-checked call as resolveCancel, matters:
	// it needs the exact same protection against a concurrent Close racing
	// start - either start wins the race and Close's teardown (below) finds
	// a non-nil servingCancel to cancel, or Close wins first, start sees
	// closed already true and returns errClosed without ever setting
	// servingCtx/servingCancel at all, so there is no window where a
	// listener could be built with a BaseContext this Server's own Close
	// could never reach.
	sctx, scancel := context.WithCancel(ctx)
	s.servingCtx = sctx
	s.servingCancel = scancel

	resolved := make(map[string]*resolverState, len(s.bindings))
	for name, b := range s.bindings {
		rs := &resolverState{binding: b}
		rs.snapshot.Store(&bindingSnapshot{err: errPendingResolve, kind: mamori.ErrorKind(errPendingResolve)})
		resolved[name] = rs

		p, ok := s.providers[b.Ref.Scheme]
		if !ok {
			// settled: this binding has no watch goroutine and can never
			// resolve, so it is already in its final state. Leaving it
			// unsettled would keep readiness at "priming" forever and hold
			// the whole replica out of rotation for one binding, which is
			// precisely what the surrounding not-a-construction-error
			// decision exists to avoid.
			rs.snapshot.Store(&bindingSnapshot{
				err:     errNoProvider(b.Ref.Scheme),
				kind:    mamori.ErrorKind(errNoProvider(b.Ref.Scheme)),
				settled: true,
			})
			continue
		}

		ch := mamori.WatchRef(rctx, p, b.Ref)

		s.resolveWG.Add(1)
		go func(rs *resolverState, ch <-chan mamori.Update) {
			defer s.resolveWG.Done()
			for {
				select {
				case up, open := <-ch:
					if !open {
						return
					}
					rs.apply(up)
				case <-rctx.Done():
					return
				}
			}
		}(rs, ch)
	}
	s.resolved = resolved
	return nil
}

// lookup returns binding name's current state, for the wire protocol
// handler to turn into a response: value is the last-known-good value
// (fresh if kind is empty, stale-but-serving if not), kind is the upstream's
// current error classification (empty when healthy), hasValue is the
// snapshot's own authoritative record of whether that binding has EVER
// resolved successfully, and found reports whether name is a bound name at
// all.
//
// hasValue is deliberately returned as its own bool rather than left for a
// caller to infer from value's shape (e.g. "len(Bytes)!=0 || Version!=""):
// a binding's genuine last-known-good value can itself be zero-length bytes
// with no Version (mamori.Value's own doc comment: Version is optional, a
// provider may never set one), which is indistinguishable, by shape alone,
// from the zero Value this function returns when hasValue is false. Only
// resolve.go's bindingSnapshot.hasValue - set exactly once, the first time
// apply sees a successful Update, and never cleared afterward - knows which
// case it actually is, so that is what gets returned here instead of asking
// the caller to guess.
//
// kind != "" with hasValue true means "serving stale, upstream is currently
// erroring" - the handler is expected to still return value, annotated with
// kind in response metadata, even if value happens to be the zero Value.
// kind != "" with hasValue false means the binding has never resolved
// successfully; the handler is expected to treat that as a hard failure,
// using kind (and, via a later plumbing task, the underlying error) to
// explain why, rather than returning the zero value as if it were real.
func (s *Server) lookup(name string) (value mamori.Value, kind mamori.Kind, hasValue bool, found bool) {
	v, _, k, has, ok := s.lookupFresh(name)
	return v, k, has, ok
}

// freshness dates a served value and says whether it is being served from
// last-known-good while upstream is currently failing. It is what the wire's
// resolved_at / stale fields are built from; see bindingSnapshot.resolvedAt
// for why a multi-replica deployment needs it.
type freshness struct {
	resolvedAt time.Time
	stale      bool
}

// lookupFresh is lookup plus the freshness metadata. The two are separate
// entry points so the many call sites that only need the value keep reading
// as they did, while the handlers that serialize a response can date it.
func (s *Server) lookupFresh(name string) (value mamori.Value, f freshness, kind mamori.Kind, hasValue bool, found bool) {
	rs, ok := s.resolved[name]
	if !ok {
		return mamori.Value{}, freshness{}, "", false, false
	}
	snap := rs.snapshot.Load()
	if snap == nil {
		// Unreachable in practice - start always stores a placeholder before
		// returning - but reported the same way an unresolved binding with an
		// active error would be, rather than panicking on a nil dereference,
		// should that invariant ever slip.
		return mamori.Value{}, freshness{}, mamori.ErrorKind(errPendingResolve), false, true
	}
	if !snap.hasValue {
		return mamori.Value{}, freshness{}, snap.kind, false, true
	}
	// A value present alongside a live error is by definition last-known-good:
	// apply carried it forward past a failing update.
	f = freshness{resolvedAt: snap.resolvedAt, stale: snap.err != nil}
	return snap.value, f, snap.kind, true, true
}

// readiness reports whether this replica is fit to receive traffic, and is
// the whole of what GET /v1/readyz answers.
//
// Draining wins over everything: Close flips it before any listener stops
// accepting, which is what gives a load balancer a window to take this
// replica out of rotation before its connections start failing.
//
// Otherwise a replica is ready once every binding has SETTLED (see
// bindingSnapshot.settled): it has a cold cache until then, and answering
// requests from a cold cache is exactly the failure mode that makes naive
// horizontal scaling unsafe. Note the deliberate asymmetry: a binding that
// settled on an error still counts as ready, because that error is upstream's
// answer and every other replica would give it too. Holding the whole replica
// out for it would turn one broken binding into a total outage, the same
// reasoning start applies when it refuses to let one unresolvable binding
// fail the whole server.
func (s *Server) readiness() (state string, ready bool) {
	if s.draining.Load() {
		return readyStateDraining, false
	}
	// resolved is written once by start and only read afterward, so the
	// per-binding atomic snapshots are the only shared state to load here.
	for _, rs := range s.resolved {
		snap := rs.snapshot.Load()
		if snap == nil || !snap.settled {
			return readyStatePriming, false
		}
	}
	return readyStateReady, true
}

// Close shuts this Server down completely: every listener Serve bound
// (transport.go's closeTransports - gracefully, bounded by shutdownGrace,
// unlinking any Unix socket file afterward), then every binding's upstream
// watch (cancel, then wg.Wait, mirroring Watcher.Close in reconciler.go).
// Transports are torn down first so that no HTTP handler goroutine can still
// be calling lookup (resolve.go) while the resolver goroutines it reads from
// are being canceled underneath it.
//
// It is idempotent and safe to call even if Serve/start was never called at
// all: closeTransports is a no-op with no listeners bound, and resolveCancel
// is nil with nothing to cancel or wait for - so a caller can unconditionally
// defer Close right after New, regardless of whether Serve ever actually
// ran, and regardless of how many times Close itself is called (a deferred
// Close plus an explicit one on an error path, say) - a second call finds
// closed already true and returns immediately without double-tearing-down
// anything.
//
// Idempotency is implemented with the closed bool guarded by resolveMu (see
// closed's doc comment in server.go), not a sync.Once: a sync.Once cannot
// distinguish "my one teardown ran against a Server that had something to
// tear down" from "my one teardown ran against a Server with NOTHING to tear
// down yet, because Close fired before Serve/start ever got a chance to
// start anything" - both consume the Once identically, so a Close-before-
// Serve would permanently spend it, and a LATER Serve on that same Server
// would then bind real listeners and spawn real goroutines that no
// subsequent Close could ever reach again. closed fixes this from the other
// end instead: start (above) and Serve (transport.go) both check closed,
// under this same resolveMu, before creating anything, and refuse with
// errClosed if it is already set - so Close-before-Serve makes the later
// Serve a no-op error rather than an unreachable teardown, and Close itself
// stays a plain idempotent flag-guarded transition.
//
// A Close that instead arrives WHILE Serve is mid-setup - concurrent with a
// listener bind, say - can still observe closed as false and find nothing
// yet to tear down, the same way a Close-before-Serve does. Serve handles
// that case from its own end: once it finishes creating everything it is
// going to create, it checks isClosed one more time and, if Close raced in
// and beat it, tears down what it just built itself via teardown below (see
// Serve's doc comment in transport.go) - teardown is safe to invoke from
// both places, even concurrently, since closeTransports, cancel, and
// resolveWG.Wait are each safe to call more than once.
func (s *Server) Close() error {
	s.resolveMu.Lock()
	if s.closed {
		s.resolveMu.Unlock()
		return nil
	}
	s.closed = true
	s.resolveMu.Unlock()

	// Announce the shutdown before causing it. Flipping draining makes
	// GET /v1/readyz answer 503 while every listener is still accepting
	// normally, which is the signal a load balancer needs to stop routing
	// here; DrainGrace (opt-in, zero by default) then holds this replica in
	// that state long enough for the balancer to actually notice. Without the
	// wait, teardown stops accepting immediately and whatever the balancer
	// sends in the interim is refused rather than drained.
	s.draining.Store(true)
	if s.drainGrace > 0 {
		time.Sleep(s.drainGrace)
	}

	s.teardown()
	return nil
}

// isClosed reports whether Close has already made its idempotent closed
// transition, under the same resolveMu that guards closed and resolveCancel.
// Serve (transport.go) calls this once it finishes binding every configured
// listener, to detect a Close that raced in and fired before there was
// anything to tear down yet - see Close's own doc comment above for why that
// ordering is possible and how Serve is expected to respond (teardown,
// below).
func (s *Server) isClosed() bool {
	s.resolveMu.Lock()
	defer s.resolveMu.Unlock()
	return s.closed
}

// teardown does Close's actual shutdown work: FIRST cancel servingCtx (so
// every in-flight request derived from it - every handleWatch loop among
// them, see servingCtx's doc comment in server.go - unblocks on its own
// <-ctx.Done() immediately, rather than only when its connection happens to
// close), THEN shut down every listener Serve bound (closeTransports,
// transport.go, whose Shutdown calls now have nothing left to wait out but
// ordinary bookkeeping), THEN stop every binding's upstream watch (cancel
// the context start derived, then wait for resolveWG). It is split out from
// Close's idempotent closed transition so that Serve (transport.go) can
// invoke this exact same teardown itself, unconditionally, when its own
// post-setup isClosed check finds that Close already ran (and, being
// idempotent, will never run again) - see Close's doc comment above. Every
// step here is safe to overlap or repeat: a context.CancelFunc, closeTransports,
// and resolveWG.Wait are each safe to call more than once, including
// concurrently with themselves - so both callers' invocations composing all
// of this together stays safe too.
func (s *Server) teardown() {
	s.resolveMu.Lock()
	servingCancel := s.servingCancel
	resolveCancel := s.resolveCancel
	s.resolveMu.Unlock()

	if servingCancel != nil {
		servingCancel()
	}

	s.closeTransports()

	if resolveCancel == nil {
		return
	}
	resolveCancel()
	s.resolveWG.Wait()
}
