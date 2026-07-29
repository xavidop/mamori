package mamori

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

// FieldChange records one field that changed between two snapshots.
type FieldChange struct {
	Path       string // dotted field path, e.g. "Redis.Password"
	OldVersion string
	NewVersion string
}

// Change is delivered to OnChange when a reconciled, re-validated update is
// applied. Old and New are immutable full snapshots; Fields lists what changed.
type Change[T any] struct {
	Old    T
	New    T
	Fields []FieldChange
}

// Changed reports whether the field at the given dotted path is among the changed
// fields in this event.
func (c Change[T]) Changed(path string) bool {
	for _, f := range c.Fields {
		if f.Path == path {
			return true
		}
	}
	return false
}

// OnChange installs the callback invoked (on a single, serialized goroutine) for
// each applied update. It is typed to the same T passed to Watch.
func OnChange[T any](fn func(Change[T])) Option {
	return func(o *options) { o.onChange = fn }
}

// Watcher holds the reconciled configuration of type T and manages the
// background watch goroutines. Obtain one from Watch and always Close it.
type Watcher[T any] struct {
	cfg    atomic.Pointer[T]
	cancel context.CancelFunc
	wg     sync.WaitGroup

	closeOnce sync.Once

	// report is the most recent snapshot published by the reconciler goroutine.
	// Status only ever Load()s this pointer; it never reads engine-owned state
	// directly, since the reconciler goroutine may be mid-mutation of those maps
	// on its own goroutine at the moment Status is called.
	report atomic.Pointer[Report]
	// snapshots is the bounded, retained snapshot history published by the
	// reconciler goroutine after each applied flush (see recordSnapshot).
	// History Load()s this pointer the same lock-free way Status Load()s
	// report: the reconciler goroutine may be mid-mutation of engine-owned
	// state at the moment a caller reads it, so callers only ever see a
	// pointer to an already-complete, never-mutated-in-place slice.
	snapshots atomic.Pointer[[]Snapshot[T]]
	// stale and clock are copied from options at construction so Status can
	// recompute Age/Stale without reaching into the engine (which would race
	// with the reconciler goroutine).
	stale time.Duration
	clock Clock

	// control carries Pin/PinCurrent/Unpin/Refresh commands to the reconciler
	// goroutine; see pin.go and engine.handlePin. It is unbuffered: sendPin's
	// send only completes once the reconciler goroutine has taken the
	// command off the channel and is committed to running handlePin, which is
	// what lets Close rely on a command already in flight being answered even
	// after cancel() has been called.
	control chan pinCmd
	// ctx is the reconciler's own context (wctx from Watch, the child of the
	// caller's parent ctx created via context.WithCancel). sendPin selects on
	// ctx.Done() so it never blocks forever once the reconciler goroutine has
	// exited, for ANY reason: Close cancels ctx via w.cancel(), and the
	// parent ctx passed to Watch being cancelled independently of Close also
	// cancels ctx, since it is a child context. Either path leaves control
	// with no receiver (loop has returned), and ctx.Done() is what gives
	// sendPin a way out in both cases, not just the Close one. Done() closes
	// synchronously as part of the cancel call itself (context.CancelFunc
	// does not return until its Done channel, and every child's, is already
	// closed), so a sendPin call blocked on control unblocks the moment
	// Close (or an external cancel of the parent ctx) happens, with no
	// separate "closed" signal needed to get the same guarantee.
	ctx context.Context

	// inCallback holds the ID of the goroutine currently running one of this
	// watcher's INLINE callbacks, and 0 whenever none is - which is always, for
	// a watcher that installed neither. Two arm it, and they are exactly the two
	// this package runs on the reconciler goroutine itself: flush's PreApply
	// gate (via runPreApply's mark parameter) and emitErr's OnError. So it names
	// the reconciler goroutine and no other; sendPinCtx is the only reader.
	//
	// OnChange is deliberately absent. It runs on the dispatch goroutine, which
	// receives Change events from a queue rather than occupying the reconciler,
	// so an OnChange callback calling Pin or Refresh is an ordinary caller whose
	// command is serviced in the ordinary way - marking it would refuse correct
	// code. See armReentrancy (preapply.go) for the rest of that reasoning, and
	// for why the disarm restores the previous value rather than storing 0.
	//
	// It is a goroutine ID rather than a bool because the two questions are not
	// the same one. "Is a callback running" is true for every caller in the
	// process while one runs, and refusing all of them would break correct code
	// that merely overlapped a rotation. "Is the caller the goroutine that is
	// inside the callback" is true only for the caller that is genuinely waiting
	// on itself, which is exactly the set that can never be answered.
	inCallback atomic.Uint64

	// adminServer and adminAddrVal are set only when WithAdminHTTP was given to
	// Watch (see adminhttp.go); both are nil/unset otherwise. adminServer is
	// used by Close to shut the server down; adminAddrVal backs AdminAddr.
	adminServer  *http.Server
	adminAddrVal net.Addr
}

// AdminAddr returns the address the admin HTTP server is bound to, or nil if
// WithAdminHTTP was not passed to Watch. This is the way to discover which
// port was actually chosen when binding to port 0 (letting the OS pick a
// free port), for example to register it with service discovery or to point
// a test client at it.
func (w *Watcher[T]) AdminAddr() net.Addr { return w.adminAddrVal }

// Get returns the current configuration snapshot. It is lock-free and always
// returns the last valid configuration (never a partially-updated or
// validation-failing one).
func (w *Watcher[T]) Get() T { return *w.cfg.Load() }

// Close cancels all provider watches, drains the callback queue, shuts down
// the admin HTTP server if one was started, and returns. By the time Close
// returns, the admin port (if any) is free: Close waits for the server's
// graceful Shutdown, bounded by adminShutdownGrace, before waiting on wg.
func (w *Watcher[T]) Close() error {
	w.closeOnce.Do(func() {
		// Canceling unblocks any sendPin call already waiting to deliver a
		// command: cancel() does not return until w.ctx.Done() (and every
		// child context derived from it) is closed, so sendPin's own
		// <-w.ctx.Done() select branch (see pin.go) is guaranteed a way out
		// by the time this call returns, with no separate ordering to get
		// right here.
		w.cancel()
		shutdownAdminHTTP(w.adminServer)
		w.wg.Wait()
	})
	return nil
}

// srcUpdate is an Update tagged with the spec and chain-position indices it
// belongs to: (idx, pos) identifies exactly which position of which field's
// precedence chain produced it, so loop can update that position's state
// (see srcState) and recompute the field's current winner.
type srcUpdate struct {
	idx int // index into engine.specs
	pos int // index into specs[idx].Refs and engine.sources[idx]
	up  Update
}

// srcState is the latest observed outcome of one position in a field's
// precedence chain, as delivered by that position's own watch source
// (native or polling). It mirrors the three outcomes resolveChain
// distinguishes for a live resolve (resolve.go): a value, ErrNotFound, or
// any other error. seen is false until this position's watch source has
// delivered its first update, or (for a chain) until seedChainSources has
// run; recomputeWinner treats "not seen" the same as "not found", since
// every watch source in this package is silent for a key that does not
// exist - see recomputeWinner's doc comment for why that is safe.
type srcState struct {
	seen  bool
	value Value // meaningful only when seen && err == nil
	err   error // meaningful only when seen && err != nil; may wrap ErrNotFound
}

// typedPreApply asserts o.preApply back to a func(context.Context, Change[T])
// error, the concrete type PreApply[T] actually stored (options is not
// generic, so the field is held as any). It returns (nil, nil) when no hook
// was installed at all, which is the common case.
//
// A hook written against some other T would make a bare comma-ok assertion
// yield nil, and a nil gate is an OPEN gate: the hook would never be called,
// no error would ever be emitted, and every rotation (or, on the initial load,
// the very first configuration) would be applied unverified while the caller
// believed it had been proven to work first. For onChange the same kind of
// mismatch costs a dropped notification and the application keeps serving
// correct configuration; for a gate whose only job is refusing credentials
// that do not work, silence is the worst available outcome. So this returns a
// loud error instead, wrapping ErrInvalid and naming both types, and is shared
// by every caller that can observe this mismatch (Watch, and loadValue for
// both Load and Watch's initial resolve) so none of them can drift into
// tolerating it via their own bare assertion.
func typedPreApply[T any](o *options) (func(context.Context, Change[T]) error, error) {
	if o.preApply == nil {
		return nil, nil
	}
	fn, ok := o.preApply.(func(context.Context, Change[T]) error)
	if !ok {
		var want func(context.Context, Change[T]) error
		return nil, fmt.Errorf("mamori: PreApply hook has type %T, want %T: %w", o.preApply, want, ErrInvalid)
	}
	return fn, nil
}

// Watch performs an initial, fail-fast Load of T and then keeps it reconciled at
// runtime, delivering validated, diff-aware updates to OnChange. It returns after
// the initial configuration is resolved (OnChange fires only on subsequent
// changes).
func Watch[T any](ctx context.Context, opts ...Option) (*Watcher[T], error) {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}

	// Checked here, before loadValue, so that a mismatched hook is caught
	// before any provider round trip is spent resolving fields - see
	// typedPreApply's doc comment for why the mismatch is fatal rather than
	// tolerated. loadValue itself is called just below (for the initial Load)
	// and now also runs this same check as part of gating that Load's result
	// (decision D7); asserting it here again is what buys Watch this earlier,
	// round-trip-free failure on top of that, not a substitute for it. This
	// call is also where preApply, below, comes from: it is the value stored
	// into the engine (e.preApply) for every flush after the initial one, not
	// merely a discarded probe.
	preApply, err := typedPreApply[T](o)
	if err != nil {
		return nil, err
	}

	cfg, initial, err := loadValue[T](ctx, o)
	if err != nil {
		return nil, err
	}

	var onChange func(Change[T])
	if o.onChange != nil {
		onChange, _ = o.onChange.(func(Change[T]))
	}

	specs := make([]fieldSpec, len(initial))
	for i, r := range initial {
		specs[i] = r.spec
	}

	wctx, cancel := context.WithCancel(ctx)
	w := &Watcher[T]{cancel: cancel, stale: o.stale, clock: o.clock}
	w.ctx = wctx
	w.control = make(chan pinCmd)
	w.cfg.Store(&cfg)

	e := &engine[T]{
		w:         w,
		o:         o,
		specs:     specs,
		observed:  make(map[string]Value, len(specs)),
		applied:   make(map[string]string, len(specs)),
		lastOK:    make(map[string]time.Time, len(specs)),
		lastErr:   make(map[string]error, len(specs)),
		blocked:   make(map[string]struct{}),
		sources:   make([][]srcState, len(specs)),
		onChange:  onChange,
		preApply:  preApply,
		lastGood:  cfg,
		version:   1, // initial snapshot
		controlCh: w.control,
	}
	// Allocate (but do not yet populate) per-position state for every field,
	// sized to its own chain length, so recomputeWinner/buildReport never
	// index a nil slice even before start's chain seeding runs (see
	// seedChainSources). A single-ref field's lone element starts unseen,
	// exactly as the pre-chain engine's single tracked value started
	// unknown until the first watch update arrived.
	for i, s := range specs {
		e.sources[i] = make([]srcState, len(s.Refs))
	}
	now := o.clock.Now()
	for _, r := range initial {
		if r.set {
			e.observed[r.spec.Path] = r.value
			e.applied[r.spec.Path] = r.value.Version
			e.lastOK[r.spec.Path] = now
		}
	}
	w.report.Store(e.buildReport())

	// Seed the initial snapshot (version 1) and publish it the same lock-free
	// way recordSnapshot republishes on every subsequent applied flush, so
	// History never observes a nil pointer between Watch returning and the
	// first change landing. Publish a copy, not &e.history itself: e.history
	// is mutated in place by the reconciler goroutine (recordSnapshot appends
	// to and reslices it) once e.start below launches it, so storing the
	// field's own address would let a caller's already-loaded pointer
	// dereference into memory the reconciler goroutine is concurrently
	// writing. recordSnapshot avoids that same trap by always storing a
	// freshly made slice, never &e.history.
	e.history = []Snapshot[T]{{Version: e.version, At: now, Config: cfg, applied: copyStringMap(e.applied)}}
	initialSnapshots := append([]Snapshot[T](nil), e.history...)
	w.snapshots.Store(&initialSnapshots)

	// Bind the admin listener, if requested, before any background goroutine
	// starts: a bind failure must fail Watch outright (mirroring the fail-fast
	// initial Load above) rather than leave the caller believing the endpoint
	// is up. Binding first also means a failure here has nothing to unwind
	// beyond canceling wctx, since e.start has not run yet.
	if o.adminAddr != "" {
		if err := startAdminHTTP[T](w, o); err != nil {
			cancel()
			return nil, err
		}
	}

	e.start(wctx)
	return w, nil
}

// engine runs the reconciliation loop for a single Watcher.
type engine[T any] struct {
	w        *Watcher[T]
	o        *options
	specs    []fieldSpec
	onChange func(Change[T])
	// preApply gates a candidate before it becomes current (see flush). It is
	// typed here the same way onChange above is, though Watch refuses a
	// mismatched hook rather than tolerating the nil this would otherwise
	// leave (see the assertion there). It is nil only when the caller
	// installed no hook, and the gate then costs a flush that has changes one
	// nil check inside runPreApply plus the Change[T] literal flush builds
	// anyway - arguments are evaluated before the call, so those two copies of
	// T are paid hook or no hook.
	preApply func(context.Context, Change[T]) error

	// updated only by the reconciler goroutine:
	observed map[string]Value  // latest value seen per path (always advances)
	applied  map[string]string // version per path at last successful apply
	lastOK   map[string]time.Time
	lastErr  map[string]error // most recent error per path, cleared on success
	lastGood T
	version  uint64 // monotonic snapshot version; starts at 1, bumped by flush

	// sources holds the latest observed state of every position in every
	// field's precedence chain, indexed sources[specIdx][refPos]; see
	// srcState and recomputeWinner. A single-ref field's sources[i] is
	// always a length-1 slice whose lone element carries exactly the state
	// the pre-chain engine tracked directly (a value, a not-found, or an
	// error), so recomputeWinner reduces to today's single-source behavior
	// for it.
	sources [][]srcState
	// blocked holds the paths currently rejecting every flush attempt
	// because a chain position with onfail:"fail" is in a terminal,
	// non-not-found error state; see loop's onFailFail case and
	// buildCandidate.
	blocked map[string]struct{}

	// history is the bounded, newest-last snapshot ring; see recordSnapshot.
	// Only the reconciler goroutine ever reads or writes it directly. Callers
	// observe it only through w.snapshots, never through this field.
	history []Snapshot[T]

	// controlCh is w.control, wired in at construction. handlePin is the only
	// place that acts on what arrives here.
	controlCh chan pinCmd
	// pinned, pinnedVersion, pinnedConfig, and pinnedApplied are the
	// reconciler-owned pin state, mutated only by handlePin. pinnedApplied is
	// the applied-version map AS OF the pinned snapshot: for PinCurrent that
	// is a copy of the live applied map at Pin time (the served snapshot IS
	// current), and for Pin(version) it is the pinned snapshot's own applied
	// map (Snapshot.applied), which may be older than the live applied map.
	// diffApplied compares it against the live applied map at Unpin time to
	// produce the single coalesced Change for everything that changed while
	// pinned, however many flushes that took.
	pinned        bool
	pinnedVersion uint64
	pinnedConfig  T
	pinnedApplied map[string]string

	dispatch chan Change[T]
}

func (e *engine[T]) start(ctx context.Context) {
	updates := make(chan srcUpdate)
	e.dispatch = make(chan Change[T], e.o.queueDepth)

	// Seed per-source state for every chain (len(Refs) > 1) field before any
	// watch source is created, so recomputeWinner already knows which
	// leading positions are absent and which position won, matching what
	// the initial Load just proved (see seedChainSources's doc comment for
	// why this matters). A single-ref field is left fully unseen, exactly
	// as before chains existed: there is no lower-priority position for it
	// to race against, so no seeding round trip is spent on it.
	for i := range e.specs {
		spec := e.specs[i]
		if len(spec.Refs) > 1 {
			e.sources[i] = e.seedChainSources(ctx, spec)
		}
	}
	// Republish now: for a chain this makes the very first report Watch
	// hands back already name the winning ref, rather than only converging
	// to it once the winning position's own watch source reports in.
	e.w.report.Store(e.buildReport())

	// Per-ref watch sources -> forwarders -> updates channel. Every position
	// of every field's chain gets its own watch source and forwarder
	// goroutine; loop's recomputeWinner is what turns these many
	// per-position streams back into one winning value per field. A
	// single-ref field's chain has exactly one position, so this reduces to
	// today's one-source-per-spec setup.
	var fwd sync.WaitGroup
	for i := range e.specs {
		spec := e.specs[i]
		for pos, ref := range spec.Refs {
			p, ok := e.o.provider(ref.Scheme)
			if !ok {
				// No provider for this position's scheme: it can never
				// resolve, so it gets no watch source (matching how the
				// pre-chain engine skipped the whole spec in this
				// situation). Its seeded/zero state stops recomputeWinner's
				// walk there for good, exactly like resolveRef classifies
				// the same situation on the live-resolve path.
				continue
			}
			// watchRef (watchref.go) is the extracted native-watch-or-poll
			// selection this loop used to inline directly; it is called here
			// with e.o, the engine's own already-built *options, so this
			// remains byte-identical to the pre-extraction behavior - see
			// watchRef's doc comment for why that is the form start uses
			// rather than the public, ...Option-taking WatchRef.
			src := watchRef(ctx, p, ref, e.o)

			fwd.Add(1)
			specIdx, refPos := i, pos
			e.w.wg.Add(1)
			go func() {
				defer e.w.wg.Done()
				defer fwd.Done()
				for {
					select {
					case up, open := <-src:
						if !open {
							return
						}
						select {
						case updates <- srcUpdate{idx: specIdx, pos: refPos, up: up}:
						case <-ctx.Done():
							return
						}
					case <-ctx.Done():
						return
					}
				}
			}()
		}
	}

	// Close updates once all forwarders exit.
	go func() {
		fwd.Wait()
		close(updates)
	}()

	// Dispatch goroutine: serial delivery of Change events.
	e.w.wg.Add(1)
	go func() {
		defer e.w.wg.Done()
		for ev := range e.dispatch {
			if e.onChange != nil {
				e.onChange(ev)
			}
		}
	}()

	// Reconciler goroutine.
	e.w.wg.Add(1)
	go func() {
		defer e.w.wg.Done()
		defer close(e.dispatch)
		e.loop(ctx, updates)
	}()
}

func (e *engine[T]) loop(ctx context.Context, updates <-chan srcUpdate) {
	pending := map[string]struct{}{}
	var timer *Timer
	var timerC <-chan time.Time
	var pendingSince time.Time
	var window time.Duration

	disarm := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
			timerC = nil
		}
	}

	arm := func() {
		fireAt := pendingSince.Add(window)
		d := fireAt.Sub(e.o.clock.Now())
		if d < 0 {
			d = 0
		}
		disarm()
		timer = e.o.clock.NewTimer(d)
		timerC = timer.C
	}

	// addPending folds spec into the pending batch, widening pendingSince/
	// window exactly as markChanged always has: the first field to join an
	// empty batch sets pendingSince to now and window to its own debounce,
	// and every field after that only ever shrinks window, never re-bases
	// pendingSince. Factored out of markChanged so unblock (below) can put a
	// field back into the batch without pretending a new value was just
	// observed.
	addPending := func(spec fieldSpec) {
		fieldWindow := e.debounceFor(spec)
		if len(pending) == 0 {
			pendingSince = e.o.clock.Now()
			window = fieldWindow
		} else if fieldWindow < window {
			window = fieldWindow
		}
		pending[spec.Path] = struct{}{}
	}

	// markChanged is the single place a field's observed value is set from a
	// winning outcome: exactly today's single-source change-detection and
	// pending/debounce bookkeeping (compare against what was last observed;
	// if different, record it, fold this field's debounce window into the
	// pending batch, and (re)arm the timer), just called from more than one
	// place now that a chain can produce a new observed value two ways: a
	// position winning outright, or a terminal error resolving to the
	// field's default (onfail:"default").
	markChanged := func(spec fieldSpec, val Value) {
		if cur, had := e.observed[spec.Path]; had && !cur.changed(val) {
			return
		}
		e.observed[spec.Path] = val
		addPending(spec)
		arm()
	}

	// unblock clears path from e.blocked and, only on an actual block ->
	// unblock TRANSITION (path was really in e.blocked, not a no-op delete
	// of a path that was never there), re-arms a flush for every field whose
	// observed version has not yet been applied (e.observed[f].Version !=
	// e.applied[f], the same comparison buildCandidate's own diff uses).
	//
	// This closes a gap shared by the three places that lift a block (the
	// cherr == nil branch below, handleChainNotFound, and the
	// onFailUseDefault branch below): buildCandidate rejects EVERY field's
	// flush while anything is blocked (the onfail:"fail" contract), and the
	// pending batch is unconditionally cleared when the timer fires and
	// finds the flush rejected (see loop's timerC case), win or lose. If the
	// field that unblocks recovers to the exact value/version it held
	// before the block (the common transient-outage case: availability
	// returned, the value itself never changed), markChanged's own
	// change-detection means no new timer gets armed for it. Without this,
	// any OTHER field whose change was held back by the block would stay
	// stranded at its pre-block value in Get(), with no timer pending and
	// no error surfaced, until some unrelated later flush happened to sweep
	// it up alongside a further, unrelated change.
	//
	// Re-adding a field here is always safe: addPending on an
	// already-pending path is a harmless re-insert (idempotent map write,
	// window only ever shrinks), and a field whose observed version already
	// equals its applied version has nothing left to flush for it and is
	// filtered out by the equality check below before addPending is ever
	// called for it - so this can never manufacture a spurious Change for a
	// field that did not actually change.
	unblock := func(path string) {
		if _, wasBlocked := e.blocked[path]; !wasBlocked {
			return
		}
		delete(e.blocked, path)
		rearmed := false
		for _, spec := range e.specs {
			v, has := e.observed[spec.Path]
			if !has {
				continue
			}
			if e.applied[spec.Path] == v.Version {
				continue
			}
			addPending(spec)
			rearmed = true
		}
		if rearmed {
			arm()
		}
	}

	for {
		select {
		case <-ctx.Done():
			disarm()
			return

		case u, ok := <-updates:
			if !ok {
				updates = nil
				if len(pending) == 0 {
					disarm()
					return
				}
				continue
			}
			spec := e.specs[u.idx]
			e.recordSourceUpdate(u.idx, u.pos, u.up, spec.Sensitive)

			// Any position updating can change which position wins the
			// field's precedence chain, so the winner is recomputed from
			// scratch over all of the chain's currently-known per-source
			// state, exactly the same precedence rules resolveChain applies
			// to a live resolve (resolve.go). For a one-element chain this
			// always walks straight to that one element.
			val, pos, cherr := e.recomputeWinner(u.idx)
			switch {
			case cherr == nil:
				e.lastOK[spec.Path] = e.o.clock.Now()
				delete(e.lastErr, spec.Path) // successful observe clears any prior error
				unblock(spec.Path)           // a resolving field can never stay blocked
				markChanged(spec, val)

			case errors.Is(cherr, ErrNotFound):
				// Every position in the chain is absent (or has not
				// reported in yet - see recomputeWinner): this is not-found
				// handling (default:/optional:), which is orthogonal to
				// OnFail - onfail governs only a non-not-found terminal
				// error (case 3), never genuine absence (case 4).
				e.handleChainNotFound(spec, cherr, unblock)

			default:
				// A position stopped the walk with a non-not-found error
				// (case 3): apply the field's OnFail policy, uniformly for
				// any chain length - exactly as applyOnFail (resolve.go)
				// applies it on the Load path. onFailKeepLast is the
				// zero-value default for a field with no `onfail` tag, so an
				// untagged single-ref field still behaves exactly like
				// today's pre-chain runtime error handling (the single-ref
				// invariant). Only a single-ref field that explicitly sets
				// onfail:"fail" or onfail:"default" changes behavior here,
				// and it changes to match what the tag says and what Load
				// already does for that same field - Watch and Load no
				// longer disagree about what an explicit onfail tag means.
				onFail := spec.OnFail
				ref := spec.Refs[0]
				if pos >= 0 {
					ref = spec.Refs[pos]
				}
				switch onFail {
				case onFailUseDefault:
					// Masks the error behind the field's default, silently
					// (no OnError), exactly as applyOnFail's onFailUseDefault
					// does for the one-shot Load path: the whole point of the
					// explicit onfail:"default" opt-in is that it fully
					// masks the error, not just partially.
					unblock(spec.Path)
					e.lastOK[spec.Path] = e.o.clock.Now()
					// No "resolve recovered" log here: decode.go's walkSpecs
					// rejects onfail:"default" without a default: tag, so
					// HasDefault is always true for this field, and every
					// failure it hits is masked right here rather than through
					// reportTerminalError - e.lastErr[spec.Path] can never
					// have been set for it, so there is never a real error to
					// recover from.
					delete(e.lastErr, spec.Path)
					markChanged(spec, Value{Bytes: []byte(spec.Default), Sensitive: spec.Sensitive, Version: "default"})
				case onFailFail:
					// Rejects the whole candidate snapshot rather than
					// applying a partial or stale update: mark this field
					// blocked so buildCandidate refuses every flush attempt
					// (any field, not just this one) until it clears.
					e.blocked[spec.Path] = struct{}{}
					e.reportTerminalError(spec, ref, cherr)
				default: // onFailKeepLast
					delete(e.blocked, spec.Path)
					e.reportTerminalError(spec, ref, cherr)
				}
			}
			e.w.report.Store(e.buildReport())

		case <-timerC:
			// Discarded deliberately: a rejection is already on its way to
			// OnError (flush's own doc comment), and there is no caller here to
			// hand it to. Refresh is the one path with somewhere to return it.
			_ = e.flush(ctx, pending)
			pending = map[string]struct{}{}
			disarm()
			e.w.report.Store(e.buildReport())
			if updates == nil {
				return
			}

		case cmd := <-e.controlCh:
			// handlePin runs entirely on this goroutine, the same one that
			// calls flush and enqueue, so Unpin's config application and its
			// single coalesced Change stay serialized with every other
			// applied update - and so does a Refresh, which re-resolves and
			// flushes here rather than racing the reconciliation it duplicates.
			// It publishes its own report before returning.
			e.handlePin(ctx, cmd)
		}
	}
}

// debounceFor returns the coalescing window for a spec, honoring a per-ref
// ?debounce= override. A chain may carry the option on more than one
// position (a caller could reasonably tag whichever ref they are tuning);
// the smallest of them wins, matching the existing rule that a batch
// touching multiple fields uses the tightest of their windows. For a
// one-element chain this only ever looks at spec.Refs[0], so it is
// byte-identical to before chains existed.
func (e *engine[T]) debounceFor(spec fieldSpec) time.Duration {
	best := e.o.debounce
	has := false
	for _, ref := range spec.Refs {
		d, ok := refDebounceOverride(ref)
		if !ok {
			continue
		}
		if !has || d < best {
			best = d
			has = true
		}
	}
	return best
}

// refDebounceOverride returns ref's ?debounce= option as a duration, and
// whether it specified one at all. It is the single-ref parsing rule
// debounceFor already used, factored out so it can be applied per ref of a
// chain: try it as a Go duration string first, falling back to the bare "0"
// spelling (a duration string requires a unit, so "0" alone does not parse
// on its own). Any other unparseable value is treated as not set at all,
// exactly as before.
func refDebounceOverride(ref Ref) (time.Duration, bool) {
	v := ref.Opt("debounce")
	if v == "" {
		return 0, false
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d, true
	}
	if v == "0" {
		return 0, true
	}
	return 0, false
}

// recordSourceUpdate stores up as the latest state of one position in a
// field's precedence chain (see srcState), applying the ref's ?decode=
// pipeline and then the field's Sensitive flag to a delivered value exactly as
// the pre-chain single-source path always did. It is called on every
// srcUpdate, before the chain's winner is recomputed.
//
// This is the single funnel every watch-delivered value passes through -
// start's per-position forwarders feed it from a native WatchableProvider and
// from the polling adapter alike (see watchRef) - which is why the decode
// belongs here rather than in either of those. Without it a field with
// ?decode= would resolve correctly at Load (resolveRef decodes) and then hand
// the application raw, undecoded bytes from its first update onward: a bug
// that passes every startup-only test and first appears in production at the
// moment a secret rotates.
//
// Decoding here, rather than later at the winner or at flush, means it happens
// exactly once and before the value becomes engine state, so recomputeWinner,
// buildCandidate, and the published snapshot all agree on the same decoded
// bytes. (buildReport is unaffected either way: it reads only Version, never
// the bytes - and Version is exactly what applyDecode leaves untouched.)
func (e *engine[T]) recordSourceUpdate(specIdx, pos int, up Update, sensitive bool) {
	st := &e.sources[specIdx][pos]
	st.seen = true
	if up.Err != nil {
		st.err = up.Err
		st.value = Value{}
		return
	}
	// A decode failure is recorded exactly like a transient resolve failure
	// delivered by the source itself: this position carries the error and no
	// value, recomputeWinner stops the chain walk here (ErrInvalid is not
	// ErrNotFound, so it does not fall through to a lower-precedence source),
	// and loop applies the field's OnFail policy - keeplast by default, which
	// leaves Get() serving the last good snapshot. Storing Value{} rather than
	// the raw bytes is what makes "keep the last good value" true: a stored
	// raw value could win a later recompute and reach the application
	// undecoded, which is the exact outcome this whole path exists to prevent.
	dec, err := applyDecode(e.specs[specIdx].Refs[pos], up.Value)
	if err != nil {
		st.err = err
		st.value = Value{}
		return
	}
	val := dec
	if sensitive {
		val.Sensitive = true
	}
	st.value = val
	st.err = nil
}

// recomputeWinner determines specIdx's current winning outcome by walking
// its chain over already-observed per-source state (see srcState), applying
// the identical precedence rules resolveChain (resolve.go) applies to a live
// resolve: the first position holding a value wins; a not-found position
// falls through to the next; any other error stops the walk. A position
// that has never reported in (seen == false) is treated exactly like a
// not-found position: every watch source in this package, native or
// polling (see pollWatch and mamoritest.Provider.Watch), is silent for a key
// that does not exist, so "nothing observed yet" and "confirmed absent" are
// indistinguishable here and both mean the same thing: keep looking further
// down the chain. seedChainSources is what keeps this safe for a genuinely
// slow-to-report leading position (see its doc comment).
//
// On success, pos is the winning ref's index. On a stopped walk (a
// non-not-found error), pos is the index of the ref that stopped it, so a
// caller can attribute the error to the right scheme/ref - unlike
// resolveChain's own contract, which reports -1 there because none of its
// callers need the index. When every position falls through, pos is -1 and
// err is the terminal not-found: for a one-element chain this is literally
// that position's own last-observed error, preserving its exact
// text/wrapping byte-for-byte (the single-ref invariant), and for a genuine
// chain it is the plain ErrNotFound sentinel, mirroring how resolveChain
// synthesizes the same case (it has no single natural attribution either).
func (e *engine[T]) recomputeWinner(specIdx int) (val Value, pos int, err error) {
	states := e.sources[specIdx]
	for i, st := range states {
		if !st.seen {
			continue
		}
		if st.err != nil {
			if errors.Is(st.err, ErrNotFound) {
				continue
			}
			return Value{}, i, st.err
		}
		return st.value, i, nil
	}
	if len(states) == 1 && states[0].err != nil {
		return Value{}, 0, states[0].err
	}
	return Value{}, -1, ErrNotFound
}

// seedChainSources performs one synchronous walk of spec's chain, resolving
// each position in turn exactly as resolveChain (resolve.go) does, so the
// engine's per-source state starts out already knowing what the initial
// Load just proved: which leading positions are absent and which position
// actually won.
//
// Without this, the position that won at Load time might not yet have
// delivered its own watch baseline (a native-watch subscription is
// asynchronous; even a polling source's first tick, though issued
// immediately, is not instantaneous) by the time a LOWER-priority
// position's baseline arrives on the shared updates channel, and
// recomputeWinner would then have no way to tell "genuinely absent" apart
// from "hasn't reported in yet" for the higher position, transiently
// letting the lower one win.
//
// It is called only for a genuine chain (len(spec.Refs) > 1): a single-ref
// field has no lower position to race against, so it is left fully
// unseeded, with no extra provider round trip, matching today's behavior
// exactly. This costs one additional resolve per position up to whichever
// one stops the walk, paid once at Watch startup, only for chains - the same
// documented tradeoff resolveAll already accepts for chain resolution
// generally (see its doc comment: "correct now beats clever later").
//
// Errors are stored raw, via the provider's own Resolve rather than
// resolveRef, so they carry no extra wrapping here: recomputeWinner's
// callers wrap them with the right ref's Scheme exactly once, the same way a
// live watch update's raw error is wrapped, so a chain's seeded state and
// its later-observed state can never format an error differently depending
// on which one happened to win.
//
// Calling Resolve directly does mean this bypasses resolveRef's ?decode=
// handling, so it applies the pipeline itself - it is a fourth place a Value
// becomes state that can reach the application, alongside resolveRef,
// resolveBatchScheme, and recordSourceUpdate. Skipping it here would not be
// merely cosmetic, and it goes wrong in two different ways depending on
// whether the winning position's provider supplies a Version:
//
//   - Version-less provider (Value.changed falls back to comparing bytes):
//     the seeded RAW bytes differ from the decoded bytes Load just stored, so
//     no rotation is needed at all. Any other position reporting in first at
//     startup triggers a recompute that publishes the raw bytes, and the
//     application sees a Change carrying an undecoded value. It heals when the
//     winning position's own watch baseline arrives (raw vs decoded bytes
//     differ again), so this one is a startup flap - but a flap the
//     application has already been handed and may have acted on.
//
//   - Versioned provider: it takes a rotation landing between Watch's initial
//     Load and this seed. The seeded state then carries the RAW bytes under
//     the NEW version, another position's first update publishes them, and the
//     winning position's own baseline - same new version, correctly decoded -
//     is discarded by markChanged as unchanged. This is the worse case,
//     because nothing corrects it until the next revision change at that
//     position. On a 90-day rotation schedule that is a quarter of serving
//     undecoded bytes. It is not permanent - the following rotation produces a
//     version the stale raw value does not match, and the field heals - but
//     "eventually, at the next rotation" is not a recovery story worth having.
func (e *engine[T]) seedChainSources(ctx context.Context, spec fieldSpec) []srcState {
	st := make([]srcState, len(spec.Refs))
	for i, ref := range spec.Refs {
		p, ok := e.o.provider(ref.Scheme)
		if !ok {
			// No provider registered for this position's scheme: it can
			// never resolve, matching resolveRef's own classification of
			// the same situation (ErrInvalid, a non-not-found error that
			// stops the walk here for good).
			st[i] = srcState{seen: true, err: ErrInvalid}
			break
		}
		val, err := p.Resolve(ctx, ref)
		if err == nil {
			// A decode failure is a non-not-found error, so it falls into the
			// same branch below as a permission denial: this position is
			// seeded as terminal and the walk stops, rather than sliding down
			// to a lower-precedence source on the strength of a
			// misconfigured coding.
			val, err = applyDecode(ref, val)
		}
		if err == nil {
			if spec.Sensitive {
				val.Sensitive = true
			}
			st[i] = srcState{seen: true, value: val}
			break
		}
		st[i] = srcState{seen: true, err: err}
		if errors.Is(err, ErrNotFound) {
			continue
		}
		break
	}
	return st
}

// handleChainNotFound applies a field's not-found handling when
// recomputeWinner reports every position in its chain absent (or not yet
// reporting in - see recomputeWinner's doc comment). This is unconditional
// on OnFail: resolveChain's case 4 (Task 2) and this runtime counterpart
// both treat "every position not found" as ordinary absence, governed only
// by default:/optional, never by onfail (which exists only for a
// non-not-found terminal error).
//
// unblock is loop's own block -> unblock helper (see its doc comment where
// it is constructed): a field with onfail:"fail" that was blocked on a
// terminal error can still arrive here if its chain's winner later resolves
// to not-found instead of a recovered value (e.g. a lower-priority position
// clears while the higher one stays absent), so this path needs the same
// re-arm-on-transition treatment a value recovery gets, not a bare
// delete(e.blocked, ...) that could silently strand another field's
// held-back change.
func (e *engine[T]) handleChainNotFound(spec fieldSpec, err error, unblock func(string)) {
	unblock(spec.Path) // absence is never a blocking condition
	if errors.Is(err, ErrNotFound) && (spec.HasDefault || spec.Optional) {
		// Absent but covered by a default or optional: this is exactly what
		// pollWatch already swallows silently for polling-driven fields, and
		// what Doctor already classifies as healthy for a one-shot probe.
		// Native-watch providers are the only path that can otherwise
		// surface ErrNotFound as a health-affecting event, so treat it the
		// same way here: not a watch error, not a lastErr, not an OnError
		// delivery.
		//
		// Clear any prior error for this path so a field that previously
		// failed and now returns a tolerated not-found becomes healthy
		// again. The field's last-observed value is left untouched;
		// re-applying the default at runtime is a separate concern.
		if _, had := e.lastErr[spec.Path]; had {
			e.o.log().Info("resolve recovered", logAttrField, spec.Path)
		}
		delete(e.lastErr, spec.Path)
		return
	}
	e.reportTerminalError(spec, spec.Refs[0], err)
}

// reportTerminalError delivers a chain's terminal error to OnError and
// records it for Status/Health, exactly as the pre-chain single-ref path
// already did (including the WithStale escalation), except ref names the
// position that actually produced the error rather than always
// spec.Refs[0], so a chain's diagnostics point at the source that is
// actually broken. For a one-element chain ref is always spec.Refs[0], so
// this reduces to byte-identical output.
func (e *engine[T]) reportTerminalError(spec fieldSpec, ref Ref, err error) {
	e.o.meter.RecordWatchError(ref.Scheme)
	e.o.log().Warn("watch error",
		append([]any{logAttrScheme, ref.Scheme, logAttrRef, redactRef(ref)}, errAttrs(err)...)...)
	pe := &ProviderError{Scheme: ref.Scheme, Ref: redactRef(ref), Err: err}
	if e.o.stale > 0 {
		if last, ok := e.lastOK[spec.Path]; ok && e.o.clock.Now().Sub(last) > e.o.stale {
			se := &StaleError{Ref: redactRef(ref), Err: err}
			e.o.log().Warn("value is stale",
				logAttrField, spec.Path, logAttrRef, redactRef(ref),
				logAttrErr, se.Error())
			e.lastErr[spec.Path] = se
			e.emitErr(se)
			return
		}
	}
	e.lastErr[spec.Path] = pe
	e.emitErr(pe)
}

// buildCandidate builds a candidate config from every observed value,
// validates it, and diffs it against the versions last applied, advancing
// e.applied to match. It is the single source of truth for what counts as a
// change, shared by flush's pinned and unpinned branches below so the two can
// never disagree about it, and reused again (via the applied/pinnedApplied
// comparison in diffApplied) to compute Unpin's single coalesced diff.
//
// There is nothing for the caller to apply in two distinct situations, and the
// returns distinguish them because Refresh has to tell them apart:
//
// A non-nil err means the candidate was REFUSED - it could not be built, it
// failed validation, or a blocked field forbids applying anything at all. The
// refusal has already been delivered to OnError by whoever detected it (this
// function for the validation cases, reportTerminalError for the blocked one),
// so err is returned for a caller that needs the reason, not as a second
// notification: callers must not emit it again. A rejected candidate is exactly
// the same failure whether or not the watcher happens to be pinned at the
// moment, which is why the emission lives here rather than in either of flush's
// branches and must not be skipped just because Get is currently frozen.
//
// A nil err with no fields means the candidate built and validated cleanly but
// changed nothing versus what was last applied, which is silently a no-op. That
// is a success, and Refresh reports it as one.
func (e *engine[T]) buildCandidate() (cand T, fields []FieldChange, err error) {
	if len(e.blocked) > 0 {
		// A chain position with onfail:"fail" is in a terminal,
		// non-not-found error state (see loop's onFailFail case): reject
		// the whole candidate rather than applying a partial update built
		// around a source the field's own policy says must not be
		// tolerated. This blocks every field's flush, not just the failing
		// one, matching onfail:"fail"'s contract. The underlying error was
		// already delivered to OnError at the point it was detected
		// (reportTerminalError), so this returns it rather than emitting a
		// second time for the same condition.
		return cand, nil, e.blockedErr()
	}
	dst := reflect.ValueOf(&cand).Elem()
	for _, spec := range e.specs {
		v, has := e.observed[spec.Path]
		if !has {
			continue
		}
		if err := setField(dst, spec, v.Bytes, e.o.decodeHooks); err != nil {
			ve := &ValidationError{Err: err}
			e.emitErr(ve)
			return cand, nil, ve
		}
	}
	if err := e.o.validator.Validate(cand); err != nil {
		ve := &ValidationError{Err: err}
		e.o.log().Error("candidate rejected by validation; continuing to serve the previous config",
			errAttrs(err)...)
		e.emitErr(ve)
		return cand, nil, ve
	}

	for _, spec := range e.specs {
		v, has := e.observed[spec.Path]
		if !has {
			continue
		}
		if e.applied[spec.Path] != v.Version {
			fields = append(fields, FieldChange{
				Path:       spec.Path,
				OldVersion: e.applied[spec.Path],
				NewVersion: v.Version,
			})
			e.applied[spec.Path] = v.Version
		}
	}
	return cand, fields, nil
}

// blockedErr is the reason a blocked field gives for refusing every candidate:
// the terminal error reportTerminalError recorded for the first blocked path in
// spec order. Spec order rather than map order because a caller comparing two
// refusals should not see them differ by iteration luck.
//
// The two maps move together - loop's onFailFail case sets e.blocked[path] and
// calls reportTerminalError, which sets e.lastErr[path], and every path that
// clears one clears the other - so the fallback is unreachable today. It exists
// because the alternative to an unreachable fallback here is returning nil,
// which flush would forward to Refresh as "applied cleanly" for a snapshot that
// was in fact refused: a silent lie is a far worse failure than a vague error.
func (e *engine[T]) blockedErr() error {
	for _, spec := range e.specs {
		if _, isBlocked := e.blocked[spec.Path]; !isBlocked {
			continue
		}
		if err := e.lastErr[spec.Path]; err != nil {
			return err
		}
		return fmt.Errorf("mamori: %s is failing and its onfail:\"fail\" policy rejects every update: %w", spec.Path, ErrInvalid)
	}
	return nil
}

// flush builds a candidate from all observed values, validates it, puts it
// past the PreApply gate, and then either applies it (emitting a Change) or
// rejects it (emitting OnError). Building, validating, gating, advancing the
// version, and recording history all happen whether or not the watcher is
// pinned: pinning only changes what happens after that. While pinned, the
// candidate is not stored to cfg and no Change is enqueued, so Get stays
// frozen and OnChange stays silent; Unpin later applies the newest such
// candidate and emits one Change coalescing everything that changed while
// pinned (see handlePin's unpin case).
//
// ctx is the reconciler's own context (loop's, which is Watch's wctx). It is
// threaded in rather than held on the engine because the gate is the first
// thing here that can block: cancelling the watcher has to release a hook
// waiting on an unresponsive backend, rather than leaving Close to wait out
// the hook's full budget.
//
// The returned error is the candidate's rejection reason, or nil when a
// snapshot was applied AND when there was nothing to apply - the two outcomes
// that both mean "what Get returns is current". It exists for Refresh, which
// has to answer that question to its caller (see refresh.go); the debounce-timer
// call site ignores it, because everything it could report has already gone to
// OnError by the time it returns. Nothing is emitted here that was not emitted
// before this function grew a return value.
func (e *engine[T]) flush(ctx context.Context, pending map[string]struct{}) error {
	if len(pending) == 0 {
		return nil
	}
	cand, fields, err := e.buildCandidate()
	if err != nil || len(fields) == 0 {
		return err
	}

	// Gate the candidate before anything observable changes. This runs after
	// buildCandidate, because validation is pure and cheap and rejects far more
	// candidates than a live check ever will, and before the version bump, the
	// history record and the swap, so a network round trip is only ever spent
	// on a candidate whose shape is already known good and a refusal costs
	// exactly what a validation failure costs: one OnError delivery, and
	// nothing else moves.
	//
	// It runs on the pinned branch too, on purpose. A pin freezes what Get
	// returns; it does not stop Live advancing, and Unpin applies the newest
	// such candidate wholesale without any gating of its own (see handlePin's
	// unpin case). Gating only while unpinned would make Unpin the one path
	// that can publish a snapshot nothing ever verified. Note that Old is
	// therefore the live snapshot this candidate supersedes, which while
	// pinned is not the one Get is currently serving.
	//
	// The rollback is not defensive coding, and it is not removable.
	// buildCandidate advanced e.applied to the new version for every field in
	// fields, as a side effect, before returning. Leaving it advanced past a
	// value that was refused breaks the engine in two ways at once:
	//
	// The next flush would diff the rejected version against itself, come up
	// empty, and return ok == false. The rejected value would never be retried,
	// no further error would ever be emitted, and Get would serve the
	// superseded configuration indefinitely. (Status reports the watcher
	// healthy through a refusal either way: emitErr does not set e.lastErr, so
	// buildReport sees nothing wrong, exactly as it does not for a validation
	// failure. What the rollback restores is the retry, and with it an error on
	// every subsequent attempt; without it even the errors stop, which is what
	// makes the staleness silent rather than merely undesired.)
	//
	// And the refused value is not withdrawn from e.observed, so it stays in
	// every candidate built afterwards. Hidden from the diff, it rides into the
	// next unrelated field's flush past a hook written the way PreApply's own
	// documentation recommends - verify only what Changed - and reaches Get
	// having been verified by nothing.
	//
	// Each FieldChange carries the OldVersion this needs, which is why the diff
	// itself undoes the mutation rather than a separate copy of the map taken
	// beforehand. Restoring an OldVersion of "" writes an empty entry where a
	// field may have had no entry at all; every reader of e.applied compares it
	// with ==, and a missing key reads as "" too, so the two are
	// indistinguishable.
	if err := runPreApply(ctx, e.preApply, e.o.preApplyTimeout, Change[T]{
		Old:    e.lastGood,
		New:    cand,
		Fields: fields,
	}, &e.w.inCallback); err != nil {
		for _, f := range fields {
			e.applied[f.Path] = f.OldVersion
		}
		e.o.log().Warn("change rejected by PreApply; continuing to serve the previous config",
			append([]any{logAttrCount, len(fields)}, errAttrs(err)...)...)
		e.emitErr(err)
		return err
	}

	old := e.lastGood
	e.version++
	e.lastGood = cand
	e.recordSnapshot(cand, fields)

	if e.pinned {
		// Live (e.version) and history advance so Status can show the
		// divergence and Unpin has something newer to apply, but Get and
		// OnChange stay exactly as they were: no cfg.Store, no enqueue.
		//
		// RecordRefresh still fires here, though: it documents "a watched
		// value changed and was reconciled" (see observ.go), and that is
		// exactly what just happened to fields, pin state notwithstanding.
		// Skipping it while pinned would under-report the refresh counter
		// for however long the pin is held. This does not double-count
		// against Unpin: Unpin applies an already-reconciled snapshot to Get
		// and emits the coalesced Change, it does not reconcile anything
		// itself, so it must not (and does not) call RecordRefresh again for
		// these same fields.
		for _, f := range fields {
			e.o.meter.RecordRefresh(e.schemeForPath(f.Path))
		}
		e.w.report.Store(e.buildReport())
		return nil
	}

	e.w.cfg.Store(&cand)
	for _, f := range fields {
		e.o.meter.RecordRefresh(e.schemeForPath(f.Path))
	}
	e.o.log().Info("config change applied", logAttrCount, len(fields))
	for _, f := range fields {
		e.o.log().Debug("field updated",
			logAttrField, f.Path, logAttrVersion, f.NewVersion)
	}
	e.enqueue(Change[T]{Old: old, New: cand, Fields: fields})
	e.w.report.Store(e.buildReport())
	return nil
}

// handlePin executes one control-channel command. It runs only on the
// reconciler goroutine (see loop's controlCh case), which is what lets Unpin
// apply the newest config to Get and enqueue its single coalesced Change
// without racing flush's own cfg.Store/enqueue for a concurrently-landing
// update: the two can never run at the same time because they are the same
// goroutine.
//
// The report is published (e.w.report.Store) before cmd.reply is sent, on
// purpose, not after: sendPin's caller unblocks the instant it receives the
// reply, and Go's channel send happens-before its receive, so publishing
// first is what guarantees a caller's very next call (Pinned, Status) can
// never observe stale pin state. Sending the reply first and publishing
// after would leave a real window where PinCurrent has returned but Pinned
// still reports not-pinned.
//
// Every branch replies exactly once, and the structure is what guarantees it
// rather than four (now five) separate promises: each case only assigns to
// reply, and the single send below every case is the only send in the function.
// A command that went unanswered would leave its caller blocked until the
// watcher closes - Pin and Unpin have no context to escape on at all, and
// Refresh's is the caller's, not a deadline this could rely on - so "reply
// exactly once" is enforced here by there being exactly one reply statement.
//
// ctx is the reconciler's own context, threaded in for the refresh case: it
// re-resolves through providers and runs the PreApply gate, both of which must
// be released by a watcher shutdown rather than run to completion after Close.
func (e *engine[T]) handlePin(ctx context.Context, cmd pinCmd) {
	var reply pinReply
	switch cmd.kind {
	case pinCurrent:
		// PinCurrent means "freeze at whatever Get returns right now," which
		// is the currently-SERVED snapshot, not necessarily the live one: if
		// already pinned, Get is currently serving the existing pinned
		// snapshot (so re-pinning current must be a no-op and Get must not
		// move); only when not already pinned is the served snapshot the
		// live one, since that is what Get returns in that case. Using
		// e.version/e.lastGood unconditionally here was the bug: re-pinning
		// while already pinned at an older version would silently jump the
		// pin forward to the live version while Get (never Store'd) stayed
		// at the old config, desyncing Get from Status/Pinned.
		servedVersion := e.version
		servedConfig := e.lastGood
		servedApplied := e.applied
		if e.pinned {
			servedVersion = e.pinnedVersion
			servedConfig = e.pinnedConfig
			servedApplied = e.pinnedApplied
		}
		e.pinned = true
		e.pinnedVersion = servedVersion
		e.pinnedConfig = servedConfig
		e.pinnedApplied = copyStringMap(servedApplied)
		// Store the served config unconditionally: harmless when not
		// already pinned (Get already returns this), necessary when
		// re-pinning while already pinned so Get/Status/Pinned stay
		// consistent regardless of prior state.
		e.w.cfg.Store(&servedConfig)
		reply = pinReply{version: servedVersion}

	case pinAt:
		snap, found := e.findSnapshot(cmd.version)
		if !found {
			reply = pinReply{err: ErrNoSuchSnapshot}
			break
		}
		e.pinned = true
		e.pinnedVersion = cmd.version
		e.pinnedConfig = snap.Config
		// The baseline for Unpin's diff must be the applied-version map AS
		// OF the pinned snapshot, not the engine's current live applied map:
		// when pinning an older, still-retained version, the live map has
		// already advanced past it, and using it here would make Unpin
		// under-report which fields changed while pinned.
		e.pinnedApplied = copyStringMap(snap.applied)
		e.w.cfg.Store(&snap.Config)
		reply = pinReply{version: cmd.version}

	case unpin:
		if !e.pinned {
			break
		}
		e.pinned = false
		old := e.pinnedConfig
		newCfg := e.lastGood
		e.w.cfg.Store(&newCfg)
		fields := e.diffApplied(e.pinnedApplied, e.applied)
		if len(fields) > 0 {
			e.enqueue(Change[T]{Old: old, New: newCfg, Fields: fields})
		}
		e.pinnedApplied = nil

	case refresh:
		reply = pinReply{err: e.refreshNow(ctx)}
	}
	e.w.report.Store(e.buildReport())
	cmd.reply <- reply
}

// refreshNow re-resolves every field and flushes the result, returning what the
// flush made of it: nil when a snapshot was applied or nothing changed, and the
// rejection reason otherwise. It is Refresh's whole implementation on the
// reconciler side (see refresh.go), and it runs on the reconciler goroutine, so
// it can touch engine state directly - the same state loop's own update
// handling touches, and never at the same time as it.
//
// It resolves through seedChainSources, which is the walk start already performs
// for a chain at startup: each position in turn, applying the ref's ?decode=
// pipeline, stopping at the first that holds a value or fails
// non-not-foundly - exactly resolveChain's precedence rules, expressed as the
// per-position state recomputeWinner consumes. Reusing it is what keeps a
// refreshed field indistinguishable from a watch-delivered one; a third resolve
// path would be a third place for decode handling, sensitivity and chain
// precedence to drift apart, and this package has already paid for that once
// (see recordSourceUpdate's doc comment on the ?decode= bug).
//
// It is called for every field, single-ref or chain, unlike start's seeding
// which is only worth its round trips for a chain. That is the point of a
// forced refresh: the single-ref case is exactly the case an operator is
// reaching for when a secret has just rotated and nothing has pushed it yet.
//
// Two costs come with reusing it here, and neither is the one start pays.
//
// This occupies a LIVE reconciler for the whole walk, which start's seeding
// never does: start runs before loop exists, so it delays only a caller already
// waiting synchronously inside Watch. For as long as this runs, pin commands go
// unserviced, watch updates back up on the unbuffered updates channel, the
// debounce timer cannot be observed firing, and no new Report is published. It
// is also every field, not just the chains start seeds. ctx is the watcher's, so
// Close releases a resolve in flight; a provider that ignores its context stalls
// reconciliation for as long as it hangs. Refresh's caller context does not help
// with that - it bounds the caller's wait, not the work (see refresh.go) - which
// is why the watcher's context, not the caller's, is what reaches the providers.
//
// And these round trips go straight to Provider.Resolve, bypassing resolveRef,
// so they are invisible to tracer.StartResolve and meter.RecordResolve, and they
// do not group through BatchProvider: N single-ref fields sharing one batch
// scheme cost N round trips here where a Load would have cost one. That is
// inherited from seedChainSources, where it is paid once per chain at startup;
// this path is operator-triggered and covers every field, so it is a
// deliberately accepted cost rather than an unnoticed one. Routing a refresh
// through resolveAll instead would fix both, and would cost the per-position
// state that makes a refreshed field indistinguishable from a watched one -
// which is the property this whole function is built on.
func (e *engine[T]) refreshNow(ctx context.Context) error {
	// loop's unblock re-arms a flush for every field its block had left
	// unapplied; here a plain delete is enough, because the single flush below
	// already considers every field. Hoisted out of the loop so
	// handleChainNotFound, which takes it as its unblock callback, is handed one
	// func value rather than a fresh closure per field.
	clearBlock := func(path string) { delete(e.blocked, path) }

	// observe is markChanged (loop) minus the debounce bookkeeping: a refresh
	// flushes now, so there is no window to coalesce into and no timer to arm.
	// The change check is not merely an optimization - storing a value the
	// engine considers unchanged would put new bytes behind an unchanged
	// Version, where buildCandidate's diff cannot see them and some later,
	// unrelated flush would publish them having passed no gate.
	observe := func(path string, val Value) {
		if cur, had := e.observed[path]; had && !cur.changed(val) {
			return
		}
		e.observed[path] = val
	}

	for i := range e.specs {
		spec := e.specs[i]
		fresh := e.seedChainSources(ctx, spec)

		// Merge, do not replace. seedChainSources marks seen only the
		// positions its walk actually reached, so assigning the slice wholesale
		// would ERASE what a lower position's own watch baseline had already
		// taught this engine (recomputeWinner reads unseen as not-found), and
		// nothing would re-teach it: that position's watch source delivered its
		// baseline once and stays silent until the value next changes. A
		// refresh would then quietly disarm a chain's fallback - the higher
		// position disappearing later would resolve to not-found rather than
		// falling through to the value the lower one is still holding.
		//
		// Nothing that could change the winner is skipped by merging: the walk
		// visits every position down to the one that stops it, and positions
		// below a stopping position cannot win while it stands.
		for pos := range fresh {
			if !fresh[pos].seen {
				continue
			}
			e.sources[i][pos] = fresh[pos]
		}

		// Classify the winner exactly as loop's update case does, case for case
		// and including the OnFail policy, so a value that arrived by refresh
		// and the same value arriving by watch leave the engine in the same
		// state - down to which errors were emitted and which fields count as
		// healthy.
		val, pos, cherr := e.recomputeWinner(i)
		switch {
		case cherr == nil:
			e.lastOK[spec.Path] = e.o.clock.Now()
			delete(e.lastErr, spec.Path) // a successful resolve clears any prior error
			clearBlock(spec.Path)        // and a resolving field can never stay blocked
			observe(spec.Path, val)

		case errors.Is(cherr, ErrNotFound):
			// Every position absent: ordinary default:/optional handling,
			// never governed by onfail. Shared with loop rather than restated,
			// because "which absences are tolerated" is precisely the kind of
			// rule two copies would drift on.
			e.handleChainNotFound(spec, cherr, clearBlock)

		default:
			ref := spec.Refs[0]
			if pos >= 0 {
				ref = spec.Refs[pos]
			}
			switch spec.OnFail {
			case onFailUseDefault:
				// Masks the error behind the field's default, silently, as
				// applyOnFail and loop both do for this explicit opt-in.
				clearBlock(spec.Path)
				e.lastOK[spec.Path] = e.o.clock.Now()
				delete(e.lastErr, spec.Path)
				observe(spec.Path, Value{Bytes: []byte(spec.Default), Sensitive: spec.Sensitive, Version: "default"})
			case onFailFail:
				// Blocks every field's flush, this one included, until it
				// clears; buildCandidate is what enforces that, and it is what
				// turns this into Refresh's returned error rather than a
				// partial apply.
				e.blocked[spec.Path] = struct{}{}
				e.reportTerminalError(spec, ref, cherr)
			default: // onFailKeepLast
				clearBlock(spec.Path)
				e.reportTerminalError(spec, ref, cherr)
			}
		}
	}

	// flush treats pending only as "is there anything to consider" - the
	// candidate itself is built from every observed value and diffed against
	// every applied version (see buildCandidate) - so naming every field here
	// is both what makes the flush happen and an honest description of its
	// scope. An empty struct has nothing to refresh and nothing to report.
	pending := make(map[string]struct{}, len(e.specs))
	for _, spec := range e.specs {
		pending[spec.Path] = struct{}{}
	}
	return e.flush(ctx, pending)
}

// findSnapshot searches the retained history (including the current
// snapshot) for the given version, the same set History() would return. It is
// the lookup behind Pin(version): a version not found here means the caller
// needs a larger WithHistory to reach that far back.
func (e *engine[T]) findSnapshot(version uint64) (Snapshot[T], bool) {
	for _, s := range e.history {
		if s.Version == version {
			return s, true
		}
	}
	return Snapshot[T]{}, false
}

// diffApplied returns a FieldChange for every path whose applied version in
// after differs from before, in spec order for a deterministic result. It is
// the counterpart, at Unpin time, to the diff buildCandidate performs on
// every flush: buildCandidate advances e.applied one flush at a time
// (pinned or not), and diffApplied collapses however many of those steps
// happened while pinned into the single accumulated diff Unpin emits, so
// several Sets while pinned still produce exactly one Change.
func (e *engine[T]) diffApplied(before, after map[string]string) []FieldChange {
	var fields []FieldChange
	for _, spec := range e.specs {
		b, a := before[spec.Path], after[spec.Path]
		if b != a {
			fields = append(fields, FieldChange{Path: spec.Path, OldVersion: b, NewVersion: a})
		}
	}
	return fields
}

// copyStringMap returns an independent copy of m, used to snapshot
// e.applied at Pin time so later mutations of the live map (further flushes
// while pinned) cannot retroactively change what Pin time looked like.
func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// recordSnapshot appends the applied config to the bounded history and
// republishes it. Called only by the reconciler goroutine.
//
// It always stores a freshly made published slice, never &e.history: the
// engine mutates e.history in place on every call (append, and occasionally
// a reslice to enforce the bound below), so publishing that field's own
// address would let a caller who already loaded the old pointer race with
// this goroutine's next mutation. Copying element-by-element into published
// also means the bound reslice on e.history below can never corrupt a
// snapshot slice already handed out by History: published does not alias
// e.history's backing array, so nothing about e.history's later reslicing or
// further appends is observable through a pointer already stored and read by
// a caller.
func (e *engine[T]) recordSnapshot(cfg T, fields []FieldChange) {
	// e.applied has already been advanced (by buildCandidate, in flush,
	// before recordSnapshot is called) to reflect every field applied up to
	// and including this version, so copying it here captures exactly the
	// cumulative per-field applied-version map as of this snapshot. Pin(v)
	// later reads this back as the correct baseline for Unpin's diff.
	snap := Snapshot[T]{Version: e.version, At: e.o.clock.Now(), Config: cfg, Fields: fields, applied: copyStringMap(e.applied)}
	e.history = append(e.history, snap)
	// Retain historyN prior snapshots plus the current one.
	if max := e.o.historyN + 1; len(e.history) > max {
		e.history = e.history[len(e.history)-max:]
	}
	published := make([]Snapshot[T], len(e.history))
	for i, s := range e.history {
		published[len(e.history)-1-i] = s // newest first
	}
	e.w.snapshots.Store(&published)
}

// enqueue delivers ev to the dispatch queue, dropping the oldest event if the
// queue is full (bounded, drop-oldest policy).
func (e *engine[T]) enqueue(ev Change[T]) {
	for {
		select {
		case e.dispatch <- ev:
			return
		default:
			select {
			case <-e.dispatch: // drop oldest, retry
				e.o.log().Warn("change event dropped, dispatch queue full; the OnChange handler is not keeping up",
					logAttrCount, cap(e.dispatch))
			default:
			}
		}
	}
}

// emitErr delivers err to the OnError callback, INLINE on the reconciler
// goroutine - it is not queued the way OnChange is, because an error has no
// snapshot to coalesce with and dropping the oldest one (what enqueue does when
// the dispatch queue is full) would lose exactly the diagnostic the caller
// installed the callback to see.
//
// That inline delivery puts an OnError callback in the same position a PreApply
// hook is in, so it is marked the same way. A callback that calls Pin,
// PinCurrent, Unpin or Refresh on this watcher is asking the goroutine it is
// currently occupying to service a command, and before this mark existed on this
// path that blocked until Close, silently, with no reconciliation in between -
// verified, not theorized: see TestRefreshFromInsideOnErrorFailsFast and
// TestPinFromInsideOnErrorFailsFast, both of which time out rather than fail
// without the arming below. "The reload was rejected, retry it" is a natural
// thing to write in an OnError callback and Refresh is how you would write it,
// which is what makes this the more tempting of the two callbacks to get wrong.
//
// The mark is armed only when a callback actually exists, and this is an error
// path rather than a per-flush one - buildCandidate's validation failures,
// reportTerminalError, and flush's gate rejection - so the goroutineID lookup
// (a runtime.Stack walk, see goroutineID) is never paid on the path a healthy
// watcher takes. A watcher with no OnError installed pays nothing at all.
func (e *engine[T]) emitErr(err error) {
	if e.o.onError == nil {
		return
	}
	defer armReentrancy(&e.w.inCallback)()
	e.o.onError(err)
}

// schemeForPath returns the scheme of the ref that most recently determined
// path's observed value or terminal error: the winning ref on a successful
// resolution, or the ref that stopped the chain's walk on a non-not-found
// error (the only terminal-error outcome that can still change the observed
// value - onfail:"default" - and therefore reach flush's refresh metric). It
// re-walks the field's already-recorded per-source state
// (recomputeWinner), a cheap, side-effect-free read, rather than caching the
// winning index separately, so this can never drift from the value flush is
// recording a refresh for. For a one-element chain this is always
// spec.Refs[0], matching today's behavior exactly.
func (e *engine[T]) schemeForPath(path string) string {
	for i, s := range e.specs {
		if s.Path != path {
			continue
		}
		if _, pos, _ := e.recomputeWinner(i); pos >= 0 {
			return s.Refs[pos].Scheme
		}
		return s.Refs[0].Scheme
	}
	return ""
}
