package mamori

import (
	"context"
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
}

// Get returns the current configuration snapshot. It is lock-free and always
// returns the last valid configuration (never a partially-updated or
// validation-failing one).
func (w *Watcher[T]) Get() T { return *w.cfg.Load() }

// Close cancels all provider watches, drains the callback queue, and returns.
func (w *Watcher[T]) Close() error {
	w.closeOnce.Do(func() {
		w.cancel()
		w.wg.Wait()
	})
	return nil
}

// srcUpdate is an Update tagged with the spec index it belongs to.
type srcUpdate struct {
	idx int
	up  Update
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
	w := &Watcher[T]{cancel: cancel}
	w.cfg.Store(&cfg)

	e := &engine[T]{
		w:        w,
		o:        o,
		specs:    specs,
		observed: make(map[string]Value, len(specs)),
		applied:  make(map[string]string, len(specs)),
		lastOK:   make(map[string]time.Time, len(specs)),
		onChange: onChange,
		lastGood: cfg,
	}
	now := o.clock.Now()
	for _, r := range initial {
		if r.set {
			e.observed[r.spec.Path] = r.value
			e.applied[r.spec.Path] = r.value.Version
			e.lastOK[r.spec.Path] = now
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

	// updated only by the reconciler goroutine:
	observed map[string]Value    // latest value seen per path (always advances)
	applied  map[string]string   // version per path at last successful apply
	lastOK   map[string]time.Time
	lastGood T

	dispatch chan Change[T]
}

func (e *engine[T]) start(ctx context.Context) {
	updates := make(chan srcUpdate)
	e.dispatch = make(chan Change[T], e.o.queueDepth)

	// Per-ref watch sources -> forwarders -> updates channel.
	var fwd sync.WaitGroup
	for i := range e.specs {
		spec := e.specs[i]
		p, ok := e.o.provider(spec.Ref.Scheme)
		if !ok {
			continue
		}
		var src <-chan Update
		if wp, isW := p.(WatchableProvider); isW {
			ch, werr := wp.Watch(ctx, spec.Ref)
			if werr != nil {
				// Fall back to polling if native watch cannot start.
				src = pollWatch(ctx, p, spec.Ref, e.o)
			} else {
				src = ch
			}
		} else {
			src = pollWatch(ctx, p, spec.Ref, e.o)
		}

		fwd.Add(1)
		idx := i
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
					case updates <- srcUpdate{idx: idx, up: up}:
					case <-ctx.Done():
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()
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
			if u.up.Err != nil {
				e.handleErr(spec, u.up.Err)
				continue
			}
			val := u.up.Value
			if spec.Sensitive {
				val.Sensitive = true
			}
			e.lastOK[spec.Path] = e.o.clock.Now()
			if cur, had := e.observed[spec.Path]; had && !cur.changed(val) {
				continue // no real change
			}
			e.observed[spec.Path] = val
			fieldWindow := e.debounceFor(spec)
			if len(pending) == 0 {
				pendingSince = e.o.clock.Now()
				window = fieldWindow
			} else if fieldWindow < window {
				window = fieldWindow
			}
			pending[spec.Path] = struct{}{}
			arm()

		case <-timerC:
			e.flush(pending)
			pending = map[string]struct{}{}
			disarm()
			if updates == nil {
				return
			}
		}
	}
}

// debounceFor returns the coalescing window for a spec, honoring a per-field
// ?debounce= override.
func (e *engine[T]) debounceFor(spec fieldSpec) time.Duration {
	if v := spec.Ref.Opt("debounce"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		// bare number = milliseconds, or "0"
		if v == "0" {
			return 0
		}
	}
	return e.o.debounce
}

// flush builds a candidate from all observed values, validates, and either
// applies it atomically (emitting a Change) or rejects it (emitting OnError).
func (e *engine[T]) flush(pending map[string]struct{}) {
	if len(pending) == 0 {
		return
	}
	var cand T
	dst := reflect.ValueOf(&cand).Elem()
	for _, spec := range e.specs {
		v, ok := e.observed[spec.Path]
		if !ok {
			continue
		}
		if err := setField(dst, spec, v.Bytes); err != nil {
			e.emitErr(&ValidationError{Err: err})
			return
		}
	}
	if err := e.o.validator.Validate(cand); err != nil {
		e.emitErr(&ValidationError{Err: err})
		return
	}

	// Compute diff vs last applied versions.
	var fields []FieldChange
	for _, spec := range e.specs {
		v, ok := e.observed[spec.Path]
		if !ok {
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
	if len(fields) == 0 {
		return
	}

	old := e.lastGood
	e.w.cfg.Store(&cand)
	e.lastGood = cand
	for _, f := range fields {
		e.o.meter.RecordRefresh(schemeForPath(e.specs, f.Path))
	}
	e.enqueue(Change[T]{Old: old, New: cand, Fields: fields})
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
			default:
			}
		}
	}
}

func (e *engine[T]) handleErr(spec fieldSpec, err error) {
	e.o.meter.RecordWatchError(spec.Ref.Scheme)
	pe := &ProviderError{Scheme: spec.Ref.Scheme, Ref: spec.Ref.Raw, Err: err}
	if e.o.stale > 0 {
		if last, ok := e.lastOK[spec.Path]; ok && e.o.clock.Now().Sub(last) > e.o.stale {
			e.emitErr(&StaleError{Ref: spec.Ref.Raw, Err: err})
			return
		}
	}
	e.emitErr(pe)
}

func (e *engine[T]) emitErr(err error) {
	if e.o.onError != nil {
		e.o.onError(err)
	}
}

func schemeForPath(specs []fieldSpec, path string) string {
	for _, s := range specs {
		if s.Path == path {
			return s.Ref.Scheme
		}
	}
	return ""
}
