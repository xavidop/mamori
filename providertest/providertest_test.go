package providertest_test

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeBackend is a correct in-memory provider used to self-test the conformance
// kit: it must pass every check.
type fakeBackend struct {
	mu       sync.Mutex
	data     map[string]string
	version  map[string]int
	watchers map[string][]chan mamori.Update
}

func newFake() *fakeBackend {
	return &fakeBackend{
		data:     map[string]string{},
		version:  map[string]int{},
		watchers: map[string][]chan mamori.Update{},
	}
}

func (f *fakeBackend) Scheme() string { return "fake" }

func (f *fakeBackend) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[ref.Path]
	if !ok {
		return mamori.Value{}, mamori.ErrNotFound
	}
	return mamori.Value{Bytes: []byte(v), Version: strconv.Itoa(f.version[ref.Path])}, nil
}

func (f *fakeBackend) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	ch := make(chan mamori.Update, 4)
	f.mu.Lock()
	f.watchers[ref.Path] = append(f.watchers[ref.Path], ch)
	if v, ok := f.data[ref.Path]; ok {
		ch <- mamori.Update{Value: mamori.Value{Bytes: []byte(v), Version: strconv.Itoa(f.version[ref.Path])}}
	}
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		cur := f.watchers[ref.Path]
		for i, c := range cur {
			if c == ch {
				f.watchers[ref.Path] = append(cur[:i], cur[i+1:]...)
				break
			}
		}
		close(ch)
	}()
	return ch, nil
}

func (f *fakeBackend) set(key, val string) {
	f.mu.Lock()
	f.data[key] = val
	f.version[key]++
	chans := append([]chan mamori.Update(nil), f.watchers[key]...)
	ver := strconv.Itoa(f.version[key])
	f.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- mamori.Update{Value: mamori.Value{Bytes: []byte(val), Version: ver}}:
		default:
		}
	}
}

func TestConformanceKitPassesForCorrectProvider(t *testing.T) {
	backend := newFake()
	providertest.Run(t, providertest.Config{
		New:    func() mamori.Provider { return backend },
		Ref:    func(key string) string { return "fake://" + key },
		Seed:   func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
		Mutate: func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
		// fakeBackend has no per-key error injection; classification itself is
		// exercised separately below by classifyingProvider.
		NoResolveErrors: true,
	})
}

// --- Watch: subscription timing ---
//
// WatchEmitsOnMutate has to mutate the backend after the watch is live, and
// WatchableProvider says nothing about when Watch's subscription takes effect.
// asyncSubscriber makes that window explicit and adjustable so the kit can be
// tested against each shape a real provider can have, rather than against the
// timing a particular machine happens to produce.
//
// subscribeDelay is how long after Watch returns the channel before the
// subscription actually exists. Writes during that window reach no watcher.
// emitBaseline says whether the provider, once subscribed, reads the current
// value and emits it - the ordering Config.WatchDeliversBaseline describes.
type asyncSubscriber struct {
	mu             sync.Mutex
	data           map[string]string
	version        map[string]int
	watchers       map[string][]chan mamori.Update
	subscribeDelay time.Duration
	emitBaseline   bool
}

func newAsync(delay time.Duration, baseline bool) *asyncSubscriber {
	return &asyncSubscriber{
		data: map[string]string{}, version: map[string]int{},
		watchers:       map[string][]chan mamori.Update{},
		subscribeDelay: delay, emitBaseline: baseline,
	}
}

func (f *asyncSubscriber) Scheme() string { return "async" }

func (f *asyncSubscriber) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[ref.Path]
	if !ok {
		return mamori.Value{}, mamori.ErrNotFound
	}
	return mamori.Value{Bytes: []byte(v), Version: strconv.Itoa(f.version[ref.Path])}, nil
}

func (f *asyncSubscriber) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	ch := make(chan mamori.Update, 4)
	go func() {
		// Not subscribed yet. mamori permits exactly this: "a native-watch
		// subscription is asynchronous" (reconciler.go, seedChainSources).
		select {
		case <-time.After(f.subscribeDelay):
		case <-ctx.Done():
			close(ch)
			return
		}
		f.mu.Lock()
		f.watchers[ref.Path] = append(f.watchers[ref.Path], ch)
		if f.emitBaseline {
			// Read AFTER subscribing. This ordering is the whole point: the
			// read observes any write the subscription was too late for.
			if v, ok := f.data[ref.Path]; ok {
				ch <- mamori.Update{Value: mamori.Value{Bytes: []byte(v), Version: strconv.Itoa(f.version[ref.Path])}}
			}
		}
		f.mu.Unlock()

		<-ctx.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		cur := f.watchers[ref.Path]
		for i, c := range cur {
			if c == ch {
				f.watchers[ref.Path] = append(cur[:i], cur[i+1:]...)
				break
			}
		}
		close(ch)
	}()
	return ch, nil
}

func (f *asyncSubscriber) set(key, val string) {
	f.mu.Lock()
	f.data[key] = val
	f.version[key]++
	chans := append([]chan mamori.Update(nil), f.watchers[key]...)
	ver := strconv.Itoa(f.version[key])
	f.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- mamori.Update{Value: mamori.Value{Bytes: []byte(val), Version: ver}}:
		default:
		}
	}
}

func asyncConfig(b *asyncSubscriber) providertest.Config {
	return providertest.Config{
		New:               func() mamori.Provider { return b },
		Ref:               func(key string) string { return "async://" + key },
		Seed:              func(_ context.Context, key, val string) error { b.set(key, val); return nil },
		Mutate:            func(_ context.Context, key, val string) error { b.set(key, val); return nil },
		NoResolveErrors:   true,
		EventuallyTimeout: 3 * time.Second,
	}
}

// The case WatchDeliversBaseline exists for: the subscription takes far longer
// than the fixed fallback wait, so no sleep the kit could pick would be right,
// but waiting for the baseline is exact. Without the declaration this same
// provider only passes because its post-subscribe read happens to recover the
// missed write - see TestWatchLateSubscribeSurvivesOnlyViaBaselineRead.
func TestWatchDeliversBaselineWaitsForLateSubscription(t *testing.T) {
	cfg := asyncConfig(newAsync(600*time.Millisecond, true))
	cfg.WatchDeliversBaseline = true
	providertest.RunWatch(t, cfg)
}

// The declared path must not pay the fixed fallback wait, which is the concrete
// cost the declaration removes and the part most likely to be quietly undone.
//
// The threshold is chosen against drainNonBlocking's own floor rather than
// against a stopwatch: that wait cannot return until 200ms after the last
// Update it saw, so a provider emitting its baseline immediately takes ~200ms
// through the fallback and microseconds through awaitBaseline. 150ms sits
// between the two with room on both sides. If RunWatch is ever changed to drain
// unconditionally, this is the test that says so.
func TestWatchDeliversBaselineSkipsTheFixedWait(t *testing.T) {
	cfg := asyncConfig(newAsync(0, true))
	cfg.WatchDeliversBaseline = true
	start := time.Now()
	providertest.RunWatch(t, cfg)
	if elapsed := time.Since(start); elapsed >= 150*time.Millisecond {
		t.Fatalf("RunWatch took %v with WatchDeliversBaseline set; it must wait for the "+
			"baseline rather than for the fixed fallback interval", elapsed)
	}
}

// Declaring a baseline that never arrives must fail with a message naming the
// declaration, not hang until the suite's timeout. A provider whose watch
// simply failed to start reaches the kit the same way, and the message covers
// both.
func TestWatchFailsWhenDeclaredBaselineNeverArrives(t *testing.T) {
	cfg := asyncConfig(newAsync(10*time.Millisecond, false))
	cfg.WatchDeliversBaseline = true
	cfg.EventuallyTimeout = 250 * time.Millisecond
	rec := &recordingTB{TB: t}
	providertest.RunWatch(rec, cfg)
	if !rec.failed {
		t.Fatal("RunWatch passed a provider that declares WatchDeliversBaseline but emits none; it must fail")
	}
}

// The counterweight to the test above, and the honest statement of what the
// declaration is worth. Undeclared, this provider passes - but not because the
// fixed 200ms wait covered a 600ms subscription. It passes because its baseline
// read runs after the subscription and therefore picks up the mutation the
// subscription was too late to be notified of. If this ever starts failing, the
// fake's read-after-subscribe ordering was broken, not the kit's timing.
func TestWatchLateSubscribeSurvivesOnlyViaBaselineRead(t *testing.T) {
	providertest.RunWatch(t, asyncConfig(newAsync(600*time.Millisecond, true)))
}

// The residual gap, pinned as a test rather than left as a comment: a provider
// that subscribes asynchronously and emits no baseline gives the kit nothing to
// synchronize on, and the fallback wait is a guess that a slow enough
// subscription defeats. This asserts the kit FAILS such a provider today. That
// is a real limitation, and the fix belongs in the provider - subscribe before
// returning from Watch, or emit a baseline after subscribing and declare it.
//
// If a future change closes this gap, this test is what will notice: it will
// start failing, and it should then be inverted rather than deleted.
func TestWatchCannotCoverAsyncSubscribeWithoutBaseline(t *testing.T) {
	cfg := asyncConfig(newAsync(600*time.Millisecond, false))
	cfg.EventuallyTimeout = 250 * time.Millisecond
	rec := &recordingTB{TB: t}
	providertest.RunWatch(rec, cfg)
	if !rec.failed {
		t.Fatal("RunWatch passed an async-subscribe provider with no baseline; " +
			"if the kit gained a way to cover that shape, invert this test rather than deleting it")
	}
}

// classifyingProvider is a minimal provider whose backend can be told to fail
// with a specific error. It models a well-behaved provider: it wraps with %w.
type classifyingProvider struct {
	mu     sync.Mutex
	values map[string]string
	fails  map[string]error
}

func newClassifyingProvider() *classifyingProvider {
	return &classifyingProvider{values: map[string]string{}, fails: map[string]error{}}
}

func (p *classifyingProvider) Scheme() string { return "classify" }

func (p *classifyingProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err, ok := p.fails[ref.Path]; ok {
		return mamori.Value{}, fmt.Errorf("classify backend: %w", err)
	}
	v, ok := p.values[ref.Path]
	if !ok {
		return mamori.Value{}, fmt.Errorf("%w: %s", mamori.ErrNotFound, ref.Path)
	}
	return mamori.Value{Bytes: []byte(v), Version: mamori.VersionHash([]byte(v))}, nil
}

func (p *classifyingProvider) set(key, val string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.values[key] = val
}

func (p *classifyingProvider) fail(key string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fails[key] = err
}

func (p *classifyingProvider) clear(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.fails, key)
}

// flatteningProvider is the bug the conformance case must catch: it formats the
// sentinel with %v, destroying the errors.Is chain.
type flatteningProvider struct{ *classifyingProvider }

func (p flatteningProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err, ok := p.fails[ref.Path]; ok {
		return mamori.Value{}, fmt.Errorf("classify backend: %v", err)
	}
	v, ok := p.values[ref.Path]
	if !ok {
		return mamori.Value{}, fmt.Errorf("%w: %s", mamori.ErrNotFound, ref.Path)
	}
	return mamori.Value{Bytes: []byte(v), Version: mamori.VersionHash([]byte(v))}, nil
}

func TestErrorClassificationPassesForWrappingProvider(t *testing.T) {
	backend := newClassifyingProvider()
	providertest.Run(t, providertest.Config{
		New:    func() mamori.Provider { return backend },
		Ref:    func(key string) string { return "classify://" + key },
		Seed:   func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
		Mutate: func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
		Fail:   func(_ context.Context, key string, err error) error { backend.fail(key, err); return nil },
		Clear:  func(_ context.Context, key string) error { backend.clear(key); return nil },
	})
}

func TestErrorClassificationFailsForFlatteningProvider(t *testing.T) {
	backend := newClassifyingProvider()
	flat := flatteningProvider{backend}

	// Run the case against a recording TB rather than t, so the expected failure
	// does not fail this test.
	fake := &recordingTB{TB: t}
	providertest.RunErrorClassification(fake, providertest.Config{
		New:   func() mamori.Provider { return flat },
		Ref:   func(key string) string { return "classify://" + key },
		Seed:  func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
		Fail:  func(_ context.Context, key string, err error) error { backend.fail(key, err); return nil },
		Clear: func(_ context.Context, key string) error { backend.clear(key); return nil },
	})

	if !fake.failed {
		t.Fatal("ErrorClassification passed a provider that flattens errors with fmt's v verb; it must fail")
	}
}

func TestErrorClassificationFailsWithoutFailHookOrNoResolveErrors(t *testing.T) {
	backend := newClassifyingProvider()
	fake := &recordingTB{TB: t}
	providertest.RunErrorClassification(fake, providertest.Config{
		New:  func() mamori.Provider { return backend },
		Ref:  func(key string) string { return "classify://" + key },
		Seed: func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
		// no Fail hook, no NoResolveErrors: Fail/Clear are required unless a
		// provider explicitly opts out via NoResolveErrors.
	})
	if fake.skipped {
		t.Fatal("ErrorClassification skipped when neither Fail nor NoResolveErrors was supplied; it must fail")
	}
	if !fake.failed {
		t.Fatal("ErrorClassification did not fail when neither Fail nor NoResolveErrors was supplied")
	}
}

func TestErrorClassificationSkippedWithNoResolveErrors(t *testing.T) {
	backend := newClassifyingProvider()
	fake := &recordingTB{TB: t}
	providertest.RunErrorClassification(fake, providertest.Config{
		New:  func() mamori.Provider { return backend },
		Ref:  func(key string) string { return "classify://" + key },
		Seed: func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
		// no Fail hook, but NoResolveErrors declares this is intentional.
		NoResolveErrors: true,
	})
	if fake.failed {
		t.Fatal("ErrorClassification failed even though NoResolveErrors was set; it must skip")
	}
	if !fake.skipped {
		t.Fatal("ErrorClassification did not skip when NoResolveErrors was set")
	}
}

func TestErrorClassificationRequiresClearWhenFailIsSet(t *testing.T) {
	backend := newClassifyingProvider()
	fake := &recordingTB{TB: t}
	providertest.RunErrorClassification(fake, providertest.Config{
		New:  func() mamori.Provider { return backend },
		Ref:  func(key string) string { return "classify://" + key },
		Seed: func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
		Fail: func(_ context.Context, key string, err error) error { backend.fail(key, err); return nil },
		// no Clear hook
	})
	if !fake.failed {
		t.Fatal("ErrorClassification did not fail when Fail was supplied without Clear")
	}
	if fake.skipped {
		t.Fatal("ErrorClassification skipped instead of failing when Fail was supplied without Clear")
	}
}

// recordingTB captures whether the suite failed or skipped, without failing the
// enclosing test. It embeds testing.TB (which has an unexported method, so it
// must be embedded rather than reimplemented) solely to satisfy the interface;
// every method RunErrorClassification actually calls is overridden below to
// record state and return normally. Unlike the real *testing.T, none of these
// panic or otherwise stop execution, which is why RunErrorClassification places
// an explicit return after every Fatalf/Fatal/Skip call: that return is what
// keeps the function's control flow correct when driven by this fake.
type recordingTB struct {
	testing.TB
	failed  bool
	skipped bool
}

func (r *recordingTB) Errorf(format string, args ...any) { r.failed = true }
func (r *recordingTB) Fatalf(format string, args ...any) { r.failed = true }
func (r *recordingTB) Fatal(args ...any)                 { r.failed = true }
func (r *recordingTB) Error(args ...any)                 { r.failed = true }
func (r *recordingTB) Skip(args ...any)                  { r.skipped = true }
func (r *recordingTB) SkipNow()                          { r.skipped = true }
func (r *recordingTB) Helper()                           {}

// --- NoGoroutineLeak ---
//
// Every test below leaks a goroutine deliberately and permanently for the
// life of this test binary process. That is harmless only because each one
// is declared after every other test in this file that checks for goroutine
// leaks of its own (TestConformanceKitPassesForCorrectProvider included,
// which runs the full suite via Run) - Go runs a file's tests in declaration
// order, so nothing earlier ever observes what these leak.

// leakyProvider starts a goroutine on every Resolve that never exits, so it
// can prove RunNoGoroutineLeak's check actually detects a leak rather than
// being dead code that always passes.
type leakyProvider struct{}

func (leakyProvider) Scheme() string { return "leaky" }

func (leakyProvider) Resolve(context.Context, mamori.Ref) (mamori.Value, error) {
	go func() { select {} }() //nolint:staticcheck // deliberately permanent, see the section comment above
	return mamori.Value{Bytes: []byte("x")}, nil
}

func TestNoGoroutineLeakFailsForLeakyProvider(t *testing.T) {
	fake := &recordingTB{TB: t}
	providertest.RunNoGoroutineLeak(fake, providertest.Config{
		New:  func() mamori.Provider { return leakyProvider{} },
		Ref:  func(key string) string { return "leaky://" + key },
		Seed: func(context.Context, string, string) error { return nil },
	})
	if !fake.failed {
		t.Fatal("NoGoroutineLeak passed a provider that leaks a goroutine on every Resolve; it must fail")
	}
}

func TestNoGoroutineLeakPassesForCleanProvider(t *testing.T) {
	backend := newFake()
	fake := &recordingTB{TB: t}
	providertest.RunNoGoroutineLeak(fake, providertest.Config{
		New:  func() mamori.Provider { return backend },
		Ref:  func(key string) string { return "fake://" + key },
		Seed: func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
	})
	if fake.failed {
		t.Fatal("NoGoroutineLeak failed for a provider that leaks nothing")
	}
}
