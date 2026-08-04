package firebasertdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
	"github.com/xavidop/mamori/providertest"
	"golang.org/x/oauth2"
)

// fakeBackend is an in-memory implementation of backend used by the conformance
// kit and unit tests. It models a Realtime Database path space: writes bump a
// per-database counter that becomes the entry ETag (native change detection), and
// every write pushes a "put" event to any open stream watching that path.
type fakeBackend struct {
	mu      sync.Mutex
	data    map[string]fakeEntry
	counter int
	streams map[*fakeStream]struct{}
	fails   map[string]error
}

type fakeEntry struct {
	val  []byte
	etag string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		data:    map[string]fakeEntry{},
		streams: map[*fakeStream]struct{}{},
		fails:   map[string]error{},
	}
}

// set writes val (verbatim, as the JSON representation at the path) for path,
// bumps the ETag, and wakes any stream watching path with a "put" event.
func (f *fakeBackend) set(path, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counter++
	f.data[path] = fakeEntry{val: []byte(val), etag: etagFor(f.counter)}
	for s := range f.streams {
		if s.path == path {
			select {
			case s.events <- "put":
			default: // buffer full: safe to drop, the provider re-resolves to latest
			}
		}
	}
}

// del removes path and pushes a "put" event (a delete surfaces as a put of null).
func (f *fakeBackend) del(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counter++
	delete(f.data, path)
	for s := range f.streams {
		if s.path == path {
			select {
			case s.events <- "put":
			default:
			}
		}
	}
}

// fail makes the next Get for path return err, until clear(path) is called. It
// keys identically to set (the raw database path), so a path seeded with set
// and then failed with fail targets the same entry. It powers the
// providertest ErrorClassification case and TestResolveClassifiesNonNotFoundError.
func (f *fakeBackend) fail(path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[path] = err
}

// clear cancels a previously injected fail(path, err).
func (f *fakeBackend) clear(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fails, path)
}

func etagFor(n int) string { return "etag-" + itoa(n) }

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

func (f *fakeBackend) Get(ctx context.Context, path string) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.fails[path]; ok {
		return nil, "", err
	}
	e, ok := f.data[path]
	if !ok {
		return nil, "", nil // absent -> provider maps to ErrNotFound
	}
	return append([]byte(nil), e.val...), e.etag, nil
}

func (f *fakeBackend) Stream(ctx context.Context, path string) (changeStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := &fakeStream{
		path:   path,
		events: make(chan string, 16),
		ctx:    ctx,
		done:   make(chan struct{}),
		be:     f,
	}
	f.mu.Lock()
	f.streams[s] = struct{}{}
	f.mu.Unlock()
	return s, nil
}

func (f *fakeBackend) removeStream(s *fakeStream) {
	f.mu.Lock()
	delete(f.streams, s)
	f.mu.Unlock()
}

// fakeStream is an in-memory changeStream whose Recv blocks until the next pushed
// event, ctx cancellation, or Close.
type fakeStream struct {
	path      string
	events    chan string
	ctx       context.Context
	done      chan struct{}
	be        *fakeBackend
	closeOnce sync.Once
}

func (s *fakeStream) Recv() (string, []byte, error) {
	select {
	case ev := <-s.events:
		return ev, nil, nil
	case <-s.ctx.Done():
		return "", nil, s.ctx.Err()
	case <-s.done:
		return "", nil, io.EOF
	}
}

func (s *fakeStream) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.be.removeStream(s)
	})
	return nil
}

// compile-time check that the fake satisfies the provider's backend contract.
var _ backend = (*fakeBackend)(nil)

func mustRef(t *testing.T, s string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return ref
}

func TestConformance(t *testing.T) {
	fake := newFakeBackend()
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider {
			return New(withBackend(fake), WithReconnectBackoff(200*time.Millisecond))
		},
		Ref:        func(key string) string { return "firebase-rtdb://" + key },
		PointerRef: func(key, frag string) string { return "firebase-rtdb://" + key + frag },
		// firebasertdb.go: the stream is opened first, then "Emit the current
		// value as a baseline once, after the stream is open".
		WatchDeliversBaseline: true,
		Seed:                  func(_ context.Context, key, val string) error { fake.set(key, val); return nil },
		Mutate: func(_ context.Context, key, val string) error {
			fake.set(key, val)
			return nil
		},
		Fail: func(_ context.Context, key string, err error) error {
			fake.fail(key, err)
			return nil
		},
		Clear: func(_ context.Context, key string) error {
			fake.clear(key)
			return nil
		},
		EventuallyTimeout: 3 * time.Second,
	})
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != "firebase-rtdb" {
		t.Fatalf("Scheme() = %q, want firebase-rtdb", got)
	}
}

func TestRegistered(t *testing.T) {
	found := false
	for _, s := range mamori.RegisteredSchemes() {
		if s == "firebase-rtdb" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("firebase-rtdb not registered by init()")
	}
}

func TestResolveValueAndVersion(t *testing.T) {
	fake := newFakeBackend()
	fake.set("config/service/log_level", "info")
	p := New(withBackend(fake))

	ref := mustRef(t, "firebase-rtdb://config/service/log_level")
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "info" {
		t.Fatalf("Bytes = %q, want info", v.Bytes)
	}
	if v.Version == "" {
		t.Fatal("Version is empty; want the ETag")
	}
	if v.Sensitive {
		t.Fatal("Realtime Database values must not be marked Sensitive")
	}

	// A write bumps the ETag, so the version must change.
	fake.set("config/service/log_level", "debug")
	v2, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve after mutate: %v", err)
	}
	if v2.Version == v.Version {
		t.Fatalf("Version did not change after write (both %q)", v.Version)
	}
	if string(v2.Bytes) != "debug" {
		t.Fatalf("Bytes = %q, want debug", v2.Bytes)
	}
}

func TestResolveScalarStringUnquoted(t *testing.T) {
	fake := newFakeBackend()
	// A JSON string leaf, as the Admin SDK returns it (quoted).
	fake.set("config/service/name", `"orders"`)
	p := New(withBackend(fake))

	v, err := p.Resolve(context.Background(), mustRef(t, "firebase-rtdb://config/service/name"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "orders" {
		t.Fatalf("Bytes = %q, want orders (unquoted)", v.Bytes)
	}
}

func TestResolveNonStringLeaf(t *testing.T) {
	fake := newFakeBackend()
	fake.set("config/service/max", "42")
	fake.set("config/service/enabled", "true")
	p := New(withBackend(fake))

	n, err := p.Resolve(context.Background(), mustRef(t, "firebase-rtdb://config/service/max"))
	if err != nil {
		t.Fatalf("Resolve number: %v", err)
	}
	if string(n.Bytes) != "42" {
		t.Fatalf("Bytes = %q, want 42", n.Bytes)
	}
	b, err := p.Resolve(context.Background(), mustRef(t, "firebase-rtdb://config/service/enabled"))
	if err != nil {
		t.Fatalf("Resolve bool: %v", err)
	}
	if string(b.Bytes) != "true" {
		t.Fatalf("Bytes = %q, want true", b.Bytes)
	}
}

func TestResolveJSONKey(t *testing.T) {
	fake := newFakeBackend()
	fake.set("config/service/db", `{"host":"db.internal","port":5432,"password":"s3cr3t"}`)
	p := New(withBackend(fake))

	got := func(key string) string {
		t.Helper()
		v, err := p.Resolve(context.Background(), mustRef(t, "firebase-rtdb://config/service/db#"+key))
		if err != nil {
			t.Fatalf("Resolve #%s: %v", key, err)
		}
		return string(v.Bytes)
	}
	if got("host") != "db.internal" {
		t.Fatalf("host = %q", got("host"))
	}
	if got("port") != "5432" {
		t.Fatalf("port = %q, want 5432", got("port"))
	}
	if got("password") != "s3cr3t" {
		t.Fatalf("password = %q", got("password"))
	}

	// A missing json key is a typed not-found.
	_, err := p.Resolve(context.Background(), mustRef(t, "firebase-rtdb://config/service/db#nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing json key err = %v, want ErrNotFound", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(withBackend(newFakeBackend()))
	_, err := p.Resolve(context.Background(), mustRef(t, "firebase-rtdb://config/absent"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestResolveClassifiesNonNotFoundError proves that a non-not-found backend
// error survives Resolve's error wrapping intact enough for errors.Is /
// mamori.ErrorKind to recognize it. This provider has no SDK error taxonomy
// to classify (no classifyFirebaseRTDB function exists), so unlike providers
// with a classifier, this test injects a real mamori sentinel
// (mamori.ErrPermissionDenied) directly through the fake, rather than a
// backend-shaped error a classifier would map. It survives only because
// Resolve wraps the backend error with %w, not %v; that is the entire value
// of wiring Fail here.
func TestResolveClassifiesNonNotFoundError(t *testing.T) {
	fake := newFakeBackend()
	fake.set("config/service/db", `"s3cr3t"`)
	fake.fail("config/service/db", mamori.ErrPermissionDenied)
	p := New(withBackend(fake))

	_, err := p.Resolve(context.Background(), mustRef(t, "firebase-rtdb://config/service/db"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; Resolve may not wrap the backend error with %%w", got, mamori.KindPermissionDenied)
	}
}

func TestResolveContextCancelled(t *testing.T) {
	fake := newFakeBackend()
	fake.set("k", "v")
	p := New(withBackend(fake))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustRef(t, "firebase-rtdb://k")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestLazyBackendFactory(t *testing.T) {
	fake := newFakeBackend()
	fake.set("k", "lazy")
	calls := 0
	p := New()
	p.newBackend = func(context.Context, string, string, httpcore.SSEConfig) (backend, error) {
		calls++
		return fake, nil
	}

	if calls != 0 {
		t.Fatalf("factory called %d times before first Resolve, want 0", calls)
	}
	if _, err := p.Resolve(context.Background(), mustRef(t, "firebase-rtdb://k")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := p.Resolve(context.Background(), mustRef(t, "firebase-rtdb://k")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if calls != 1 {
		t.Fatalf("factory called %d times, want 1 (lazy, cached)", calls)
	}
}

func TestWatchEmitsBaselineAndChange(t *testing.T) {
	fake := newFakeBackend()
	fake.set("watched", "v1")
	p := New(withBackend(fake), WithReconnectBackoff(200*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "firebase-rtdb://watched"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Baseline.
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("baseline err: %v", u.Err)
		}
		if string(u.Value.Bytes) != "v1" {
			t.Fatalf("baseline = %q, want v1", u.Value.Bytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no baseline emitted")
	}

	// A change pushed on the stream must produce an Update.
	fake.set("watched", "v2")
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("change err: %v", u.Err)
		}
		if string(u.Value.Bytes) != "v2" {
			t.Fatalf("change = %q, want v2", u.Value.Bytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mutation not delivered")
	}
}

func TestWatchDeleteSurfacesNotFound(t *testing.T) {
	fake := newFakeBackend()
	fake.set("watched", "v1")
	p := New(withBackend(fake), WithReconnectBackoff(200*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "firebase-rtdb://watched"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	// Drain the baseline.
	<-ch

	fake.del("watched")
	deadline := time.After(2 * time.Second)
	for {
		select {
		case u := <-ch:
			if errors.Is(u.Err, mamori.ErrNotFound) {
				return // delete delivered as a not-found Update, watch still open
			}
		case <-deadline:
			t.Fatal("delete not delivered as a not-found update")
		}
	}
}

func TestWatchClosesOnCancel(t *testing.T) {
	fake := newFakeBackend()
	fake.set("k", "v")
	p := New(withBackend(fake), WithReconnectBackoff(time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Watch(ctx, mustRef(t, "firebase-rtdb://k"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed after cancel")
		}
	}
}

// scriptedBackend counts Stream attempts and hands out whatever stream the test
// asks for. It exists to observe the RATE of reconnects, which the fake used by
// the other watch tests cannot: that one hands out streams that stay open.
type scriptedBackend struct {
	mu        sync.Mutex
	attempts  int
	newStream func() changeStream
	// streamErr, when set, makes every Stream call fail outright instead of
	// returning a connection: a database that is refusing connections rather
	// than one that answers and then breaks.
	streamErr error
}

func (b *scriptedBackend) Get(ctx context.Context, path string) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return []byte(`"v"`), "etag-1", nil
}

func (b *scriptedBackend) Stream(ctx context.Context, path string) (changeStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.attempts++
	err := b.streamErr
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return b.newStream(), nil
}

func (b *scriptedBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

// failingStream is a connection that opens and then fails on its first frame,
// which is exactly what a node larger than the frame ceiling produces: the
// Realtime Database answers every stream with a put of the whole node.
type failingStream struct{}

func (failingStream) Recv() (string, []byte, error) {
	return "", nil, httpcore.ErrSSEFrameTooLong
}
func (failingStream) Close() error { return nil }

// workingStream delivers one put and then ends cleanly, the way an idle proxy or
// a restarting server drops a connection that was doing its job.
type workingStream struct{ sent bool }

func (s *workingStream) Recv() (string, []byte, error) {
	if !s.sent {
		s.sent = true
		return "put", nil, nil
	}
	return "", nil, io.EOF
}
func (s *workingStream) Close() error { return nil }

// drain consumes ch until it closes, so a watch under test is never blocked on
// an unread channel.
func drain(ch <-chan mamori.Update) {
	go func() {
		for range ch { //nolint:revive // draining is the point
		}
	}()
}

func TestWatchBacksOffWhenEveryStreamFailsImmediately(t *testing.T) {
	// The failure that has no natural end: the node is bigger than the frame
	// ceiling, so every connection breaks on its first frame. With a fixed
	// reconnect wait this is a permanent hot loop at the configured rate. The
	// wait must grow instead, and it must NOT be reset by the fact that the
	// connection itself succeeded, since it always does.
	be := &scriptedBackend{newStream: func() changeStream { return failingStream{} }}
	p := New(withBackend(be), WithReconnectBackoff(5*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "firebase-rtdb://watched"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	drain(ch)

	time.Sleep(300 * time.Millisecond)
	cancel()

	// Doubling from a 5ms floor, jittered to [d/2, d], puts the eighth attempt
	// at 2.5+5+10+20+40+80+160 = 317.5ms at the earliest, so this window holds
	// at most seven. A wait that never grew would fit sixty or more.
	if got := be.count(); got > 15 {
		t.Fatalf("%d reconnect attempts in 300ms with a 5ms floor: the backoff is not growing", got)
	} else if got < 2 {
		t.Fatalf("%d reconnect attempts in 300ms: the watch is not retrying at all", got)
	}
}

func TestWatchBacksOffWhenTheStreamWillNotOpen(t *testing.T) {
	// The other unbounded failure, and a separate branch of the loop: the
	// database refuses the connection outright, so there is no stream to consume
	// and the retry decision is made before consume is ever reached. A database
	// that is down must not be dialled at the floor rate for as long as the
	// watch lives either.
	be := &scriptedBackend{
		newStream: func() changeStream { return failingStream{} },
		streamErr: errors.New("connection refused"),
	}
	p := New(withBackend(be), WithReconnectBackoff(5*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "firebase-rtdb://watched"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	drain(ch)

	time.Sleep(300 * time.Millisecond)
	cancel()

	// Same arithmetic as above: an eighth attempt needs 317.5ms at the least.
	if got := be.count(); got > 15 {
		t.Fatalf("%d dial attempts in 300ms with a 5ms floor: the backoff is not growing", got)
	} else if got < 2 {
		t.Fatalf("%d dial attempts in 300ms: the watch is not retrying at all", got)
	}
}

func TestWatchResetsBackoffAfterAStreamDelivers(t *testing.T) {
	// The other half of the same rule. A connection that delivered an event and
	// then dropped says nothing bad about the server's health, so the next
	// attempt must go back to the floor rather than inheriting a wait grown from
	// earlier failures. Without the reset these same 300ms hold about seven.
	be := &scriptedBackend{newStream: func() changeStream { return &workingStream{} }}
	p := New(withBackend(be), WithReconnectBackoff(5*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "firebase-rtdb://watched"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	drain(ch)

	time.Sleep(300 * time.Millisecond)
	cancel()

	if got := be.count(); got < 15 {
		t.Fatalf("only %d reconnects in 300ms with a 5ms floor after streams that delivered: the backoff is not resetting", got)
	}
}

func TestNextReconnectBackoff(t *testing.T) {
	cases := []struct {
		name     string
		d, floor time.Duration
		want     time.Duration
	}{
		{"doubles", 2 * time.Second, 2 * time.Second, 4 * time.Second},
		{"stops at the cap", 20 * time.Second, 2 * time.Second, reconnectBackoffCap},
		{"never exceeds the cap", time.Minute, 2 * time.Second, reconnectBackoffCap},
		// A floor above the cap is a deliberate configuration; capping below it
		// would retry more often than the caller asked for.
		{"honours a floor above the cap", 2 * time.Minute, 2 * time.Minute, 2 * time.Minute},
		{"cannot wrap negative", time.Duration(1) << 62, 2 * time.Second, reconnectBackoffCap},
	}
	for _, tc := range cases {
		if got := nextReconnectBackoff(tc.d, tc.floor); got != tc.want {
			t.Errorf("%s: nextReconnectBackoff(%v, %v) = %v, want %v", tc.name, tc.d, tc.floor, got, tc.want)
		}
	}
}

func TestReconnectJitterStaysInTheUpperHalf(t *testing.T) {
	// Equal jitter, not full jitter: the wait must stay close to d so that a
	// grown backoff is not undone by a draw near zero.
	for range 200 {
		got := reconnectJitter(4 * time.Second)
		if got < 2*time.Second || got > 4*time.Second {
			t.Fatalf("reconnectJitter(4s) = %v, want within [2s, 4s]", got)
		}
	}
	if got := reconnectJitter(0); got != 0 {
		t.Fatalf("reconnectJitter(0) = %v, want 0", got)
	}
}

func TestWithMaxFrameBytesReachesTheBackend(t *testing.T) {
	// The option's whole purpose is to reach the decoder, so a Provider that
	// stored the number and never passed it on would be the entire bug.
	var got httpcore.SSEConfig
	p := New(WithMaxFrameBytes(4 << 20))
	p.newBackend = func(_ context.Context, _, _ string, sse httpcore.SSEConfig) (backend, error) {
		got = sse
		return newFakeBackend(), nil
	}
	if _, err := p.Resolve(context.Background(), mustRef(t, "firebase-rtdb://k")); !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("Resolve err = %v, want ErrNotFound from the empty fake", err)
	}
	if got.MaxLine != 4<<20 || got.MaxFrame != 4<<20 {
		t.Fatalf("backend built with %+v, want both bounds at 4 MiB", got)
	}
	// Not positive is ignored, leaving httpcore's defaults selected.
	if p := New(WithMaxFrameBytes(0)); p.sse != (httpcore.SSEConfig{}) {
		t.Fatalf("WithMaxFrameBytes(0) set %+v, want the zero config", p.sse)
	}
}

func TestUnwrapJSONString(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`"hello"`, "hello"},
		{`  "spaced"  `, "spaced"},
		{`{"a":1}`, `{"a":1}`},
		{`123`, `123`},
		{`true`, `true`},
		{`not-json`, `not-json`},
		{`""`, ``},
	}
	for _, tc := range cases {
		if got := string(unwrapJSONString([]byte(tc.in))); got != tc.want {
			t.Errorf("unwrapJSONString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- the live SSE path (sdk.go) ---
//
// These exercise sdkBackend.Stream against a real HTTP server rather than the
// in-memory fakeBackend the conformance kit uses, because the fake never touches
// the SSE code at all: before this provider moved onto httpcore's bounded
// decoder, the streaming path was the one part of it with no test that could
// observe a hostile server, and it was also the part with no memory bound.

// newTestSDKBackend builds an sdkBackend pointed at url with a static token, so
// the streaming path can be driven without Application Default Credentials. The
// Admin SDK client is nil: Stream never touches it.
func newTestSDKBackend(url string) *sdkBackend {
	return newTestSDKBackendWithBounds(url, httpcore.SSEConfig{})
}

// newTestSDKBackendWithBounds is newTestSDKBackend with explicit stream bounds,
// the ones WithMaxFrameBytes sets in production.
func newTestSDKBackendWithBounds(url string, sse httpcore.SSEConfig) *sdkBackend {
	return &sdkBackend{
		dbURL:       strings.TrimRight(url, "/"),
		tokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}),
		httpClient:  &http.Client{},
		sse:         sse,
	}
}

// sseTestServer serves body as an event stream and then holds the connection
// open, the way the Realtime Database endpoint does between changes.
func sseTestServer(t *testing.T, body func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		body(w)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestSDKBackendStreamDecodesEvents(t *testing.T) {
	ts := sseTestServer(t, func(w http.ResponseWriter) {
		_, _ = fmt.Fprint(w, "event: put\ndata: {\"path\":\"/\",\"data\":{\"a\":1}}\n\n")
		_, _ = fmt.Fprint(w, ": heartbeat\n\n")
		_, _ = fmt.Fprint(w, "event: keep-alive\ndata: null\n\n")
		_, _ = fmt.Fprint(w, "event: patch\ndata: line1\ndata: line2\n\n")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := newTestSDKBackend(ts.URL).Stream(ctx, "config/service")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	want := []struct{ name, data string }{
		{"put", `{"path":"/","data":{"a":1}}`},
		{"keep-alive", "null"},
		{"patch", "line1\nline2"},
	}
	for i, w := range want {
		name, data, err := s.Recv()
		if err != nil {
			t.Fatalf("event %d: Recv err = %v", i, err)
		}
		if name != w.name || string(data) != w.data {
			t.Fatalf("event %d = (%q, %q), want (%q, %q)", i, name, data, w.name, w.data)
		}
	}
}

func TestSDKBackendStreamBoundsAnUnterminatedLine(t *testing.T) {
	// The headline of the httpcore migration. The decoder this replaced read a
	// line with bufio.Reader.ReadBytes('\n'): a server that sends bytes and no
	// newline grew that buffer for as long as it kept sending, with no ceiling
	// anywhere in the provider. Now the read stops at the ceiling and the stream
	// reports it.
	ts := sseTestServer(t, func(w http.ResponseWriter) {
		chunk := []byte(strings.Repeat("x", 64*1024))
		// Comfortably past the one-megabyte ceiling, and never a newline.
		for range 64 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := newTestSDKBackend(ts.URL).Stream(ctx, "config/service")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, _, err := s.Recv(); !errors.Is(err, httpcore.ErrSSELineTooLong) {
		t.Fatalf("Recv err = %v, want ErrSSELineTooLong", err)
	}
}

func TestSDKBackendStreamBoundsAnUndispatchedFrame(t *testing.T) {
	// The second bound, which the first does not imply: every line here is 32
	// bytes, so no line bound is ever crossed, and the blank line that would
	// dispatch the frame never comes. Only a per-frame total stops the payload
	// growing for as long as the server keeps talking.
	ts := sseTestServer(t, func(w http.ResponseWriter) {
		_, _ = fmt.Fprint(w, "event: put\n")
		line := "data: " + strings.Repeat("y", 26) + "\n"
		for range (1 << 20 / 26) + 64 {
			if _, err := fmt.Fprint(w, line); err != nil {
				return
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := newTestSDKBackend(ts.URL).Stream(ctx, "config/service")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, _, err := s.Recv(); !errors.Is(err, httpcore.ErrSSEFrameTooLong) {
		t.Fatalf("Recv err = %v, want ErrSSEFrameTooLong", err)
	}
}

func TestSDKBackendStreamRaisedBoundCarriesALargerNode(t *testing.T) {
	// What WithMaxFrameBytes is for. This frame is past BOTH default ceilings at
	// once (one data line of a megabyte and a kilobyte), which is what a node
	// larger than the default looks like on this wire, since the database opens
	// every stream with a put of the whole node. At the default it is refused;
	// with the bounds raised it must arrive intact rather than being truncated
	// or clamped back to the default.
	const size = (1 << 20) + 1024
	ts := sseTestServer(t, func(w http.ResponseWriter) {
		_, _ = fmt.Fprint(w, "event: put\ndata: "+strings.Repeat("z", size)+"\n\n")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	be := newTestSDKBackendWithBounds(ts.URL, httpcore.SSEConfig{MaxLine: 4 << 20, MaxFrame: 4 << 20})
	s, err := be.Stream(ctx, "config/service")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	name, data, err := s.Recv()
	if err != nil {
		t.Fatalf("Recv err = %v, want the frame the raised bound allows", err)
	}
	if name != "put" || len(data) != size {
		t.Fatalf("Recv = (%q, %d bytes), want (%q, %d bytes)", name, len(data), "put", size)
	}
}

// There is deliberately no test here that cancelling the context ends a live
// stream. Stream builds its request with the same context it hands the SSE
// stream, so net/http aborts the round trip and supplies context.Canceled on its
// own: such a test passes with the stream's context translation deleted
// entirely, and asserts nothing about the migrated code. httpcore's
// TestSSEStreamCancelEndsADetachedStream covers that translation at the layer
// that owns it, against a stream whose request is NOT bound to the cancelled
// context, which is the only shape that can tell the two mechanisms apart.

func TestSDKBackendStreamRejectsNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "permission denied", http.StatusForbidden)
	}))
	t.Cleanup(ts.Close)

	if _, err := newTestSDKBackend(ts.URL).Stream(context.Background(), "config/service"); err == nil {
		t.Fatal("Stream returned no error for a 403")
	}
}
