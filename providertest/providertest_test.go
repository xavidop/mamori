package providertest_test

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"

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
