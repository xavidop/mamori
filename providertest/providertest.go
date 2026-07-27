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
//	        Fail:   func(ctx context.Context, key string, err error) error { ... },
//	        Clear:  func(ctx context.Context, key string) error { ... },
//	    })
//	}
package providertest

import (
	"context"
	"errors"
	"strings"
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

	// WatchDeliversBaseline declares that Watch emits an Update carrying the
	// current value - a "baseline" - before any change notification, and, the
	// load-bearing half, that the backend subscription is already established
	// by the time that baseline is emitted.
	//
	// It exists because WatchEmitsOnMutate has to mutate the backend after the
	// watch is live, and nothing in the WatchableProvider interface says when
	// that happened. Watch may return its channel and subscribe afterwards in a
	// goroutine - mamori explicitly permits this ("a native-watch subscription
	// is asynchronous", reconciler.go) - so a mutation issued too early is
	// simply not seen by anybody. Unset, the kit copes by waiting a fixed
	// interval and hoping (see drainNonBlocking). Set, it waits for the
	// baseline instead, which is an actual happens-after signal: a provider
	// that subscribes before it reads-and-emits cannot produce that Update
	// without having already subscribed. The wait is then deterministic rather
	// than timing-dependent, and typically finishes in microseconds instead of
	// the fixed 200ms.
	//
	// Subscribe-then-read is the ordering a watch wants regardless of this kit
	// (see the redis provider: "Subscribe before emitting the baseline so no
	// notification is missed between the baseline GET and the start of the read
	// loop"), which is why declaring it is safe for a correct provider and only
	// awkward for an incorrect one. If your Watch reads the value first and
	// subscribes second, do not set this: that ordering drops any write landing
	// in between, and setting the field will surface that as a watch failure -
	// correctly, but the bug to fix is the ordering, not the declaration.
	//
	// The zero value keeps the previous behavior exactly, so leaving it unset
	// is always safe. Providers that emit no baseline at all (a pure
	// change-event stream, e.g. etcd or launchdarkly here) must leave it unset;
	// for those the kit has no signal to wait on and falls back to the fixed
	// interval. Declaring it when no baseline is coming fails the case with an
	// explicit message rather than hanging silently.
	WatchDeliversBaseline bool

	// EventuallyTimeout bounds how long watch/poll tests wait for a change to
	// propagate. Defaults to 5s.
	EventuallyTimeout time.Duration

	// Fail makes the backend return err for key on the next Resolve, until
	// Clear is called for that key. It powers the ErrorClassification case,
	// which verifies the provider preserves a classified error's errors.Is
	// chain rather than flattening it.
	//
	// Extending Fail to also affect any active Watch is reserved for future
	// use: no conformance case exercises the watch path today, so a provider
	// that only honors Fail on Resolve is fully conforming.
	//
	// The common real bug this catches is a provider that catches an error and
	// reformats it with fmt.Errorf("...: %v", err), which destroys the chain and
	// makes every failure report KindUnknown.
	//
	// Fail (and Clear) are required unless NoResolveErrors is set. A provider
	// that supplies neither fails ErrorClassification with a hard error rather
	// than being silently skipped.
	Fail func(ctx context.Context, key string, err error) error

	// Clear cancels a Fail for key. Required whenever Fail is set.
	//
	// Without it, a Fail injected for one key stays active for the rest of the
	// run: it leaks into later conformance cases, which then resolve that key
	// (or reuse the provider instance) and observe the injected failure instead
	// of their own expected behavior, corrupting their results. RunErrorClassification
	// hard-fails rather than skips when Fail is set but Clear is nil, precisely
	// so this leak cannot happen silently.
	Clear func(ctx context.Context, key string) error

	// NoResolveErrors declares that this provider's Resolve can never return a
	// classifiable error beyond ErrNotFound, because its client surfaces no
	// per-key error (existence is a bool or a sentinel value). It exempts the
	// provider from the ErrorClassification case, which has nothing to inject.
	// Set it deliberately, with a comment naming why: Fail and Clear are
	// required, and a provider that supplies neither Fail nor NoResolveErrors
	// is a hard error, so this exemption is explicit and greppable, never a
	// silent skip.
	NoResolveErrors bool

	// PointerRef builds a ref whose #fragment selects a value out of a JSON
	// payload, given a logical key and the fragment to use (including its
	// leading '#'). Supply it only when this provider's fragment slot IS a JSON
	// selector routed through mamori.SelectKey; leave it nil otherwise, and the
	// JSONPointerSelection case skips.
	//
	// It is a builder rather than a bool because Config.Ref is not fragment-free
	// by convention: several providers bake a fixed fragment into it (vault,
	// mongodb, firestore, and k8s all produce "...#value"), so the case cannot
	// simply append its own fragment to Ref's output. A builder lets each
	// provider say where its selector goes:
	//
	//	PointerRef: func(key, frag string) string {
	//	    return "vault://secret/" + key + frag
	//	}
	//
	// Leaving it nil is not a confession of a gap. A fragment means three
	// different things across this ecosystem: a JSON selector (most providers),
	// a backend-native key (a Kubernetes Secret data key, a Doppler secret
	// name), or nothing at all (providers/mamori never reads ref.Key by design;
	// a flag provider returning a bare bool has no payload to select from).
	// Only the first can be tested for pointer support, and the other two are
	// not defective for failing such a test.
	//
	// What this case DOES catch, for a provider that supplies it: hand-rolling a
	// top-level-only key lookup instead of calling mamori.SelectKey, which
	// passes every other case in this kit and fails only this one.
	PointerRef func(key, fragment string) string
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
	t.Run("JSONPointerSelection", func(t *testing.T) { testJSONPointerSelection(t, c) })
	t.Run("ErrorClassification", func(t *testing.T) { RunErrorClassification(t, c) })
	t.Run("ContextCancel", func(t *testing.T) { testContextCancel(t, c) })
	t.Run("ConcurrentResolve", func(t *testing.T) { testConcurrentResolve(t, c) })
	t.Run("VersionMonotonic", func(t *testing.T) { testVersionMonotonic(t, c) })
	t.Run("WatchEmitsOnMutate", func(t *testing.T) { RunWatch(t, c) })
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

// RunErrorClassification runs only the error-classification case. Run calls it;
// it is exported so the kit can test itself against a deliberately broken
// provider without running the whole suite.
func RunErrorClassification(tb testing.TB, c Config) {
	tb.Helper()
	if c.NoResolveErrors {
		tb.Skip("providertest: provider declares NoResolveErrors; no per-key error surface to classify")
		return
	}
	if c.Fail == nil {
		tb.Fatal("providertest: Config.Fail and Clear are required unless NoResolveErrors is set")
		return
	}
	if c.Clear == nil {
		tb.Fatal("providertest: Config.Clear is required whenever Config.Fail is set")
		return
	}

	ctx := context.Background()
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"PermissionDenied", mamori.ErrPermissionDenied, mamori.KindPermissionDenied},
		{"Unauthenticated", mamori.ErrUnauthenticated, mamori.KindUnauthenticated},
		{"Unavailable", mamori.ErrUnavailable, mamori.KindUnavailable},
		{"RateLimited", mamori.ErrRateLimited, mamori.KindRateLimited},
		{"Invalid", mamori.ErrInvalid, mamori.KindInvalid},
	}

	for _, tc := range cases {
		key := c.key("classify-" + strings.ToLower(tc.name) + "-" + uniq())
		if err := c.Seed(ctx, key, "seeded-value"); err != nil {
			tb.Fatalf("Seed(%q): %v", key, err)
			return
		}
		if err := c.Fail(ctx, key, tc.err); err != nil {
			tb.Fatalf("Fail(%q): %v", key, err)
			return
		}

		p := c.New()
		ref, err := mamori.ParseRef(c.Ref(key))
		if err != nil {
			tb.Fatalf("Ref(%q) produced an unparseable ref: %v", key, err)
			return
		}
		_, resolveErr := p.Resolve(ctx, ref)

		if cerr := c.Clear(ctx, key); cerr != nil {
			tb.Fatalf("Clear(%q): %v", key, cerr)
			return
		}

		if resolveErr == nil {
			tb.Fatalf("%s: Resolve returned a nil error while the backend was failing", tc.name)
			return
		}
		if got := mamori.ErrorKind(resolveErr); got != tc.want {
			tb.Fatalf("%s: ErrorKind(err) = %q, want %q. The provider most likely "+
				"formatted the error with %%v instead of %%w, which breaks the "+
				"errors.Is chain. Underlying error: %v",
				tc.name, got, tc.want, resolveErr)
			return
		}
	}
}

// testJSONPointerSelection verifies a provider whose #fragment is a JSON
// selector routes it through mamori.SelectKey, so nested selection behaves
// identically everywhere. A provider that hand-rolls a top-level-only key
// lookup passes every other case in this kit and fails only this one.
//
// It skips when Config.PointerRef is nil, because a fragment is not a JSON
// selector in every provider: it is a backend-native key in some and means
// nothing in others. See PointerRef's doc comment.
func testJSONPointerSelection(t *testing.T, c Config) {
	t.Helper()
	if c.PointerRef == nil {
		t.Skip("providertest: provider supplies no PointerRef; its #fragment is not a JSON selector")
		return
	}
	ctx := context.Background()
	p := c.New()
	key := c.key("jsonpointer")

	const payload = `{"outer":{"inner":"deep"},"list":[{"n":"zero"},{"n":"one"}],"dotted.key":"literal"}`
	if err := c.Seed(ctx, key, payload); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	ok := []struct{ frag, want string }{
		{"#/outer/inner", "deep"},
		{"#/list/1/n", "one"},
		{"#dotted.key", "literal"}, // literal fragment, not a path
	}
	for _, tc := range ok {
		ref, err := mamori.ParseRef(c.PointerRef(key, tc.frag))
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", tc.frag, err)
		}
		v, err := p.Resolve(ctx, ref)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", tc.frag, err)
		}
		if string(v.Bytes) != tc.want {
			t.Errorf("Resolve(%q) = %q, want %q", tc.frag, v.Bytes, tc.want)
		}
	}

	bad := []struct {
		frag string
		want error
	}{
		{"#/outer/absent", mamori.ErrNotFound},
		{"#/list/9", mamori.ErrNotFound},
		{"#/outer/inner/deeper", mamori.ErrInvalid},
	}
	for _, tc := range bad {
		ref, err := mamori.ParseRef(c.PointerRef(key, tc.frag))
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", tc.frag, err)
		}
		if _, err := p.Resolve(ctx, ref); !errors.Is(err, tc.want) {
			t.Errorf("Resolve(%q) err = %v, want %v", tc.frag, err, tc.want)
		}
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

// RunWatch runs only the watch-emits-on-mutate case. Run calls it; it is
// exported for the same reason RunErrorClassification is, so the kit can test
// itself against deliberately misbehaving providers - one that subscribes late,
// one that declares a baseline it never sends - without running the whole suite
// and without those expected failures failing the enclosing test.
//
// As in RunErrorClassification, every Fatal/Fatalf/Skip is followed by an
// explicit return: driven by a recording testing.TB rather than a real
// *testing.T, none of them stop execution on their own.
func RunWatch(tb testing.TB, c Config) {
	tb.Helper()
	p := c.New()
	wp, ok := p.(mamori.WatchableProvider)
	if !ok || c.SkipWatch || c.Mutate == nil {
		tb.Skip("provider is not watchable or watch disabled")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := c.key("watch")
	if err := c.Seed(ctx, key, "start"); err != nil {
		tb.Fatalf("Seed: %v", err)
		return
	}
	ref, err := mamori.ParseRef(c.Ref(key))
	if err != nil {
		tb.Fatalf("Ref(%q) produced an unparseable ref: %v", key, err)
		return
	}
	ch, werr := wp.Watch(ctx, ref)
	if werr != nil {
		tb.Fatalf("Watch: %v", werr)
		return
	}

	// Get past whatever the subscription itself emits, so the mutation below is
	// the next thing on the channel and, more importantly, so the subscription
	// is actually live when it happens.
	if c.WatchDeliversBaseline {
		if !awaitBaseline(ch, c.timeout()) {
			tb.Fatalf("Config.WatchDeliversBaseline is set, but Watch delivered no Update "+
				"within %v. Either this provider emits no baseline at subscribe time, in "+
				"which case unset the field, or its watch never started.", c.timeout())
			return
		}
	} else {
		drainNonBlocking(ch)
	}

	if err := c.Mutate(ctx, key, "changed"); err != nil {
		tb.Fatalf("Mutate: %v", err)
		return
	}
	deadline := time.After(c.timeout())
	for {
		select {
		case u, open := <-ch:
			if !open {
				tb.Fatal("watch channel closed before delivering the mutation")
				return
			}
			if u.Err != nil {
				continue
			}
			if string(u.Value.Bytes) == "changed" {
				return
			}
		case <-deadline:
			tb.Fatal("watch did not deliver the mutation within the timeout")
			return
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

// awaitBaseline blocks for the first Update a Watch delivers - the baseline a
// provider emits for the current value at subscribe time - and reports whether
// one arrived before timeout. A closed channel counts as no baseline.
//
// This is the deterministic half of the watch case, and it works only because
// of what Config.WatchDeliversBaseline promises: that the subscription is
// established before the baseline is emitted. Given that ordering, receiving
// the baseline is a genuine happens-after signal for "the subscription is
// live", so the mutation that follows cannot be issued into a window where
// nobody is listening. No sleep is involved and none is needed.
//
// Having taken the baseline, it consumes anything else already queued but does
// not wait for more. Some providers emit several Updates around subscribe time
// (a reconnect that re-emits, say). Left sitting in a small channel buffer,
// those cost nothing for a provider that blocks on a full channel, but a
// provider that drops sends into a full buffer would lose the mutation
// notification to them - which is the one job drainNonBlocking did that was
// never about timing.
func awaitBaseline(ch <-chan mamori.Update, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case _, open := <-ch:
		if !open {
			return false
		}
	case <-timer.C:
		return false
	}
	for {
		select {
		case _, open := <-ch:
			if !open {
				return true
			}
		default:
			return true
		}
	}
}

// drainNonBlocking consumes Updates until the channel has been quiet for 200ms.
// It is the fallback for a provider that has not set
// Config.WatchDeliversBaseline, and it is worth being precise about what it
// does and does not buy, because the answer is less than it looks.
//
// What it reliably does: empty the channel, so a provider that drops sends into
// a full buffer still has room for the mutation notification.
//
// What it only appears to do: make an asynchronous subscription safe. The 200ms
// is not a synchronization point with anything - it is a guess that whatever
// goroutine Watch spawned has subscribed by now. It is enough for providers
// whose subscription completes quickly and useless for one that takes longer,
// and the kit cannot tell those apart. Two shapes explain why it has held up in
// practice anyway. A provider that subscribes first and then reads-and-emits
// the current value recovers on its own without help from any wait, because the
// baseline read happens after the subscription and therefore observes a
// mutation that the subscription missed; those providers should set
// WatchDeliversBaseline and skip the wait entirely. A provider that subscribes
// synchronously inside Watch was never at risk to begin with.
//
// The residual gap, which no amount of waiting closes: a provider that
// subscribes asynchronously and emits no baseline offers the kit nothing to
// synchronize on, so the wait is the only option and it is a guess. The fix
// there belongs in the provider - subscribe before returning from Watch, or
// emit a baseline after subscribing - not in a longer sleep here.
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
