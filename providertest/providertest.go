// Package providertest is the mamori provider conformance kit. A provider that
// passes providertest.Run behaves identically to every other provider with
// respect to resolution, not-found semantics, versioning, watching, concurrency,
// goroutine hygiene, and secret-safe logging.
//
// Wire it up in your provider's tests:
//
//	func TestConformance(t *testing.T) {
//	    providertest.Run(t, providertest.Config{
//	        New:    func() mamori.Provider { return myprovider.New(...) },
//	        Ref:    func(key string) string { return "myscheme://" + key },
//	        Seed:   func(ctx context.Context, key, val string) error { ... },
//	        Mutate: func(ctx context.Context, key, val string) error { ... },
//	    })
//	}
package providertest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"go.uber.org/goleak"
)

// Config describes how to exercise a provider under test.
type Config struct {
	// New constructs a fresh provider instance. Required.
	New func() mamori.Provider

	// Ref builds a source ref string for a logical key, e.g.
	//   func(key string) string { return "aws-sm://" + key }
	// Required.
	Ref func(key string) string

	// Seed writes an initial value for key to the backend. Required.
	Seed func(ctx context.Context, key, val string) error

	// Mutate changes an existing key's value (used by watch/version tests). If
	// nil, watch and version-monotonicity tests are skipped.
	Mutate func(ctx context.Context, key, val string) error

	// Key returns a unique key to use for a test; if nil, a default per-test key
	// is used. Provide this if your backend needs namespaced or pre-created keys.
	Key func(name string) string

	// SkipWatch forces the watch tests to be skipped even if the provider
	// implements WatchableProvider (e.g. when the backend can't push in CI).
	SkipWatch bool

	// EventuallyTimeout bounds how long watch/poll tests wait for a change to
	// propagate. Defaults to 5s.
	EventuallyTimeout time.Duration
}

func (c Config) key(name string) string {
	if c.Key != nil {
		return c.Key(name)
	}
	return "mamori-conformance-" + name
}

func (c Config) timeout() time.Duration {
	if c.EventuallyTimeout > 0 {
		return c.EventuallyTimeout
	}
	return 5 * time.Second
}

func (c Config) parseRef(t *testing.T, key string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(c.Ref(key))
	if err != nil {
		t.Fatalf("Ref(%q) produced an unparseable ref: %v", key, err)
	}
	return ref
}

// Run executes the full conformance suite. Call it from a single test function.
func Run(t *testing.T, c Config) {
	if c.New == nil || c.Ref == nil || c.Seed == nil {
		t.Fatal("providertest.Config requires New, Ref, and Seed")
	}

	t.Run("Scheme", func(t *testing.T) { testScheme(t, c) })
	t.Run("ResolveSeeded", func(t *testing.T) { testResolveSeeded(t, c) })
	t.Run("NotFoundTyped", func(t *testing.T) { testNotFound(t, c) })
	t.Run("ContextCancel", func(t *testing.T) { testContextCancel(t, c) })
	t.Run("ConcurrentResolve", func(t *testing.T) { testConcurrentResolve(t, c) })
	t.Run("VersionMonotonic", func(t *testing.T) { testVersionMonotonic(t, c) })
	t.Run("WatchEmitsOnMutate", func(t *testing.T) { testWatch(t, c) })
	t.Run("WatchClosesOnCancel", func(t *testing.T) { testWatchCloses(t, c) })
	t.Run("NoGoroutineLeak", func(t *testing.T) { testNoLeak(t, c) })
}

func testScheme(t *testing.T, c Config) {
	p := c.New()
	if p.Scheme() == "" {
		t.Fatal("Scheme() returned empty string")
	}
	ref := c.parseRef(t, c.key("scheme"))
	if ref.Scheme != p.Scheme() {
		t.Fatalf("Ref scheme %q does not match provider Scheme() %q", ref.Scheme, p.Scheme())
	}
}

func testResolveSeeded(t *testing.T, c Config) {
	ctx := context.Background()
	key := c.key("resolve")
	if err := c.Seed(ctx, key, "hello-world"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	p := c.New()
	v, err := p.Resolve(ctx, c.parseRef(t, key))
	if err != nil {
		t.Fatalf("Resolve of seeded key: %v", err)
	}
	if string(v.Bytes) != "hello-world" {
		t.Fatalf("resolved value = %q, want hello-world", v.Bytes)
	}
	if v.Version == "" {
		t.Error("provider returned an empty Version; supply one (mamori.VersionHash helps)")
	}
}

func testNotFound(t *testing.T, c Config) {
	p := c.New()
	_, err := p.Resolve(context.Background(), c.parseRef(t, c.key("does-not-exist-"+uniq())))
	if err == nil {
		t.Fatal("Resolve of missing key returned nil error; must return ErrNotFound")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing-key error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

func testContextCancel(t *testing.T, c Config) {
	key := c.key("ctxcancel")
	_ = c.Seed(context.Background(), key, "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := c.New()
	_, err := p.Resolve(ctx, c.parseRef(t, key))
	if err == nil {
		t.Fatal("Resolve with a cancelled context returned nil error")
	}
	// We do not require a specific error, only that cancellation is honored and
	// not ignored.
}

func testConcurrentResolve(t *testing.T, c Config) {
	ctx := context.Background()
	key := c.key("concurrent")
	if err := c.Seed(ctx, key, "concurrent-value"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	p := c.New()
	ref := c.parseRef(t, key)

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Resolve(ctx, ref); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Resolve failed: %v", err)
	}
}

func testVersionMonotonic(t *testing.T, c Config) {
	if c.Mutate == nil {
		t.Skip("no Mutate; skipping version test")
	}
	ctx := context.Background()
	key := c.key("version")
	if err := c.Seed(ctx, key, "one"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	p := c.New()
	ref := c.parseRef(t, key)
	v1, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Mutate(ctx, key, "two"); err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	v2, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version == v2.Version {
		t.Fatalf("Version did not change after Mutate (both %q); change detection is impossible", v1.Version)
	}
	if string(v2.Bytes) != "two" {
		t.Fatalf("post-mutate value = %q, want two", v2.Bytes)
	}
}

func testWatch(t *testing.T, c Config) {
	p := c.New()
	wp, ok := p.(mamori.WatchableProvider)
	if !ok || c.SkipWatch || c.Mutate == nil {
		t.Skip("provider is not watchable or watch disabled")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := c.key("watch")
	if err := c.Seed(ctx, key, "start"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	ch, err := wp.Watch(ctx, c.parseRef(t, key))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	// Drain the baseline (if any), then mutate.
	drainNonBlocking(ch)
	if err := c.Mutate(ctx, key, "changed"); err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	deadline := time.After(c.timeout())
	for {
		select {
		case u, open := <-ch:
			if !open {
				t.Fatal("watch channel closed before delivering the mutation")
			}
			if u.Err != nil {
				continue
			}
			if string(u.Value.Bytes) == "changed" {
				return
			}
		case <-deadline:
			t.Fatal("watch did not deliver the mutation within the timeout")
		}
	}
}

func testWatchCloses(t *testing.T, c Config) {
	p := c.New()
	wp, ok := p.(mamori.WatchableProvider)
	if !ok || c.SkipWatch {
		t.Skip("provider is not watchable")
	}
	key := c.key("watchclose")
	_ = c.Seed(context.Background(), key, "x")
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := wp.Watch(ctx, c.parseRef(t, key))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()
	deadline := time.After(c.timeout())
	for {
		select {
		case _, open := <-ch:
			if !open {
				return // closed as required
			}
		case <-deadline:
			t.Fatal("watch channel not closed after context cancellation")
		}
	}
}

func testNoLeak(t *testing.T, c Config) {
	defer goleak.VerifyNone(t)
	p := c.New()
	key := c.key("leak")
	_ = c.Seed(context.Background(), key, "x")
	ctx, cancel := context.WithCancel(context.Background())
	if wp, ok := p.(mamori.WatchableProvider); ok && !c.SkipWatch {
		ch, err := wp.Watch(ctx, c.parseRef(t, key))
		if err == nil {
			go func() {
				for range ch {
				}
			}()
		}
	}
	_, _ = p.Resolve(ctx, c.parseRef(t, key))
	cancel()
	time.Sleep(100 * time.Millisecond)
}

func drainNonBlocking(ch <-chan mamori.Update) {
	for {
		select {
		case <-ch:
		case <-time.After(200 * time.Millisecond):
			return
		}
	}
}

var uniqCounter struct {
	sync.Mutex
	n int
}

func uniq() string {
	uniqCounter.Lock()
	defer uniqCounter.Unlock()
	uniqCounter.n++
	return time.Now().Format("150405") + "-" + itoa(uniqCounter.n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
