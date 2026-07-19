package firebasertdb

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
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
}

type fakeEntry struct {
	val  []byte
	etag string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		data:    map[string]fakeEntry{},
		streams: map[*fakeStream]struct{}{},
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
		Ref:  func(key string) string { return "firebase-rtdb://" + key },
		Seed: func(_ context.Context, key, val string) error { fake.set(key, val); return nil },
		Mutate: func(_ context.Context, key, val string) error {
			fake.set(key, val)
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
	p.newBackend = func(context.Context, string, string) (backend, error) {
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

func TestSSEDecoder(t *testing.T) {
	// A representative Realtime Database SSE stream: an initial put, a heartbeat
	// comment, a keep-alive event, a patch, a multi-line data event, then EOF.
	payload := "" +
		"event: put\n" +
		"data: {\"path\":\"/\",\"data\":{\"a\":1}}\n" +
		"\n" +
		": heartbeat\n" +
		"event: keep-alive\n" +
		"data: null\n" +
		"\n" +
		"event: patch\n" +
		"data: {\"path\":\"/\",\"data\":{\"b\":2}}\n" +
		"\n" +
		"event: put\n" +
		"data: line1\n" +
		"data: line2\n" +
		"\n"

	dec := newSSEDecoder(strings.NewReader(payload))

	type ev struct {
		name string
		data string
	}
	want := []ev{
		{"put", `{"path":"/","data":{"a":1}}`},
		{"keep-alive", "null"},
		{"patch", `{"path":"/","data":{"b":2}}`},
		{"put", "line1\nline2"},
	}
	for i, w := range want {
		name, data, err := dec.next()
		if err != nil {
			t.Fatalf("event %d: unexpected err %v", i, err)
		}
		if name != w.name || string(data) != w.data {
			t.Fatalf("event %d = (%q, %q), want (%q, %q)", i, name, data, w.name, w.data)
		}
	}
	if _, _, err := dec.next(); err != io.EOF {
		t.Fatalf("final next() err = %v, want io.EOF", err)
	}
}
