package etcd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeClient is an in-memory implementation of etcdClient supporting the parts
// of the etcd v3 contract the provider relies on: a monotonically increasing
// store revision bumped on every write (surfaced as each key's ModRevision),
// and a native watch stream that pushes a PUT event to every active watcher of a
// key when that key changes. A watch channel is closed when its context is
// cancelled, matching clientv3.Watcher semantics, so no goroutine leaks.
type fakeClient struct {
	mu       sync.Mutex
	data     map[string]*mvccpb.KeyValue
	rev      int64
	watchers map[*fakeWatcher]struct{}
	fails    map[string]error
	closed   bool
}

type fakeWatcher struct {
	key   string
	inbox chan clientv3.WatchResponse
	out   chan clientv3.WatchResponse
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		data:     map[string]*mvccpb.KeyValue{},
		watchers: map[*fakeWatcher]struct{}{},
		fails:    map[string]error{},
	}
}

// fail makes the next Get for key return err, until clear(key) is called. It
// powers the providertest ErrorClassification case and
// TestResolveClassifiesNonNotFoundError. key must match the raw key form set
// uses (f.data is keyed directly by it, with no transformation), so fail and
// clear target the exact entry Seed populated.
func (f *fakeClient) fail(key string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[key] = err
}

// clear cancels a previously injected fail(key, err).
func (f *fakeClient) clear(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fails, key)
}

// failWatch pushes a canceled WatchResponse to every active watcher of key,
// with CancelReason set to reason. The real clientv3.WatchResponse.Err()
// computes its return value from CancelReason via rpctypes.Error, which
// recognizes a fixed set of well-known etcd server error texts (see
// go.etcd.io/etcd/api/v3/v3rpc/rpctypes) and, on a match, converts it into an
// rpctypes.EtcdError carrying that error's original gRPC code. This models
// exactly what a live server reports when e.g. a caller's permissions are
// revoked mid-watch, and is what lets TestWatchClassifiesNonNotFoundError
// exercise classifyEtcd's EtcdError fallback through the real Watch stream
// error path rather than a synthetic one.
func (f *fakeClient) failWatch(key, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := clientv3.WatchResponse{Canceled: true, CancelReason: reason}
	for w := range f.watchers {
		if w.key == key {
			select {
			case w.inbox <- resp:
			default:
			}
		}
	}
}

// set writes val for key, bumping the store revision (and thus the key's
// ModRevision), then pushes a PUT event to every active watcher of that key.
func (f *fakeClient) set(key, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rev++
	kv := &mvccpb.KeyValue{
		Key:            []byte(key),
		Value:          []byte(val),
		CreateRevision: f.rev,
		ModRevision:    f.rev,
		Version:        1,
	}
	if old, ok := f.data[key]; ok {
		kv.CreateRevision = old.CreateRevision
		kv.Version = old.Version + 1
	}
	f.data[key] = kv

	resp := clientv3.WatchResponse{
		Events: []*clientv3.Event{{Type: clientv3.EventTypePut, Kv: copyKV(kv)}},
	}
	for w := range f.watchers {
		if w.key == key {
			// inbox is buffered; a non-blocking send keeps set() from blocking
			// on a slow or gone consumer.
			select {
			case w.inbox <- resp:
			default:
			}
		}
	}
}

// Get implements etcdClient.
func (f *fakeClient) Get(ctx context.Context, key string, _ ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.fails[key]; ok {
		return nil, err
	}
	resp := &clientv3.GetResponse{}
	if kv := f.data[key]; kv != nil {
		resp.Kvs = []*mvccpb.KeyValue{copyKV(kv)}
		resp.Count = 1
	}
	return resp, nil
}

// Close implements etcdClient. It only records that Close was called, so
// tests can assert whether a caller-injected fake was (or was not) released.
func (f *fakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// Watch implements etcdClient. It registers a watcher for key and returns a
// channel that receives a PUT WatchResponse on every subsequent change, and is
// closed when ctx is cancelled.
func (f *fakeClient) Watch(ctx context.Context, key string, _ ...clientv3.OpOption) clientv3.WatchChan {
	w := &fakeWatcher{
		key:   key,
		inbox: make(chan clientv3.WatchResponse, 16),
		out:   make(chan clientv3.WatchResponse),
	}
	f.mu.Lock()
	f.watchers[w] = struct{}{}
	f.mu.Unlock()

	go func() {
		defer close(w.out)
		defer func() {
			f.mu.Lock()
			delete(f.watchers, w)
			f.mu.Unlock()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case resp := <-w.inbox:
				select {
				case w.out <- resp:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return w.out
}

// copyKV returns a deep copy of kv. It constructs a fresh KeyValue from the
// exported fields rather than dereferencing kv, because the protobuf-generated
// KeyValue embeds internal state that must not be copied by value.
func copyKV(kv *mvccpb.KeyValue) *mvccpb.KeyValue {
	if kv == nil {
		return nil
	}
	cp := &mvccpb.KeyValue{
		CreateRevision: kv.CreateRevision,
		ModRevision:    kv.ModRevision,
		Version:        kv.Version,
		Lease:          kv.Lease,
	}
	if kv.Key != nil {
		cp.Key = append([]byte(nil), kv.Key...)
	}
	if kv.Value != nil {
		cp.Value = append([]byte(nil), kv.Value...)
	}
	return cp
}

func TestConformance(t *testing.T) {
	fake := newFakeClient()
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider {
			return New(withClient(fake))
		},
		Ref:        func(key string) string { return "etcd://" + key },
		PointerRef: func(key, frag string) string { return "etcd://" + key + frag },
		Seed:       func(_ context.Context, key, val string) error { fake.set(key, val); return nil },
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
	if got := New().Scheme(); got != "etcd" {
		t.Fatalf("Scheme() = %q, want etcd", got)
	}
}

func TestResolveValueAndVersion(t *testing.T) {
	fake := newFakeClient()
	fake.set("config/app", "hello")
	p := New(withClient(fake))

	ref := mustRef(t, "etcd://config/app")
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hello" {
		t.Fatalf("Bytes = %q, want hello", v.Bytes)
	}
	if v.Version != "1" {
		t.Fatalf("Version = %q, want 1 (ModRevision)", v.Version)
	}
	if v.Sensitive {
		t.Fatal("etcd values must not be marked Sensitive")
	}

	// A write bumps ModRevision, so the version must change.
	fake.set("config/app", "world")
	v2, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve after mutate: %v", err)
	}
	if v2.Version == v.Version {
		t.Fatalf("Version did not change after write (both %q)", v.Version)
	}
	if string(v2.Bytes) != "world" {
		t.Fatalf("Bytes = %q, want world", v2.Bytes)
	}
}

func TestResolveJSONKey(t *testing.T) {
	fake := newFakeClient()
	fake.set("config/db", `{"host":"db.internal","port":5432,"password":"s3cr3t"}`)
	p := New(withClient(fake))

	got := func(key string) string {
		t.Helper()
		v, err := p.Resolve(context.Background(), mustRef(t, "etcd://config/db#"+key))
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
	_, err := p.Resolve(context.Background(), mustRef(t, "etcd://config/db#nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing json key err = %v, want ErrNotFound", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(withClient(newFakeClient()))
	_, err := p.Resolve(context.Background(), mustRef(t, "etcd://absent"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestResolveClassifiesNonNotFoundError exercises classifyEtcd through
// Resolve itself, not just as a direct function call. The conformance
// ErrorClassification case cannot catch a regression here: it injects a
// mamori sentinel directly (not a gRPC status), so it passes even if the
// classifyEtcd call were deleted from Resolve's fallback branch. This test
// injects a real gRPC status through fakeClient.fail, the same shape a live
// etcd server would return, so it fails if the wiring is removed.
func TestResolveClassifiesNonNotFoundError(t *testing.T) {
	fake := newFakeClient()
	fake.set("config/app", "hello")
	fake.fail("config/app", status.Error(codes.PermissionDenied, "etcdserver: permission denied"))
	p := New(withClient(fake))

	_, err := p.Resolve(context.Background(), mustRef(t, "etcd://config/app"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyEtcd may not be wired into Resolve", got, mamori.KindPermissionDenied)
	}
}

func TestResolveContextCancelled(t *testing.T) {
	fake := newFakeClient()
	fake.set("k", "v")
	p := New(withClient(fake))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustRef(t, "etcd://k")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestConnNoEndpoints(t *testing.T) {
	t.Setenv("ETCD_ENDPOINTS", "")
	p := New()
	_, err := p.Resolve(context.Background(), mustRef(t, "etcd://k"))
	if err == nil {
		t.Fatal("Resolve without endpoints returned nil error")
	}
}

func TestEndpointsFromEnv(t *testing.T) {
	t.Setenv("ETCD_ENDPOINTS", " a:2379 , b:2379 ,, c:2379 ")
	got := endpointsFromEnv()
	want := []string{"a:2379", "b:2379", "c:2379"}
	if len(got) != len(want) {
		t.Fatalf("endpoints = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("endpoints = %v, want %v", got, want)
		}
	}
}

func TestWatchEmitsChange(t *testing.T) {
	fake := newFakeClient()
	fake.set("watched", "v1")
	p := New(withClient(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "etcd://watched"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// etcd's native watch delivers events from the current revision onward, so
	// a mutation after Watch produces a PUT Update carrying the new value.
	fake.set("watched", "v2")
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("change err: %v", u.Err)
		}
		if string(u.Value.Bytes) != "v2" {
			t.Fatalf("change = %q, want v2", u.Value.Bytes)
		}
		if u.Value.Version != "2" {
			t.Fatalf("change version = %q, want 2", u.Value.Version)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mutation not delivered")
	}
}

func TestWatchJSONKey(t *testing.T) {
	fake := newFakeClient()
	fake.set("cfg", `{"level":"info"}`)
	p := New(withClient(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "etcd://cfg#level"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	fake.set("cfg", `{"level":"debug"}`)
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("change err: %v", u.Err)
		}
		if string(u.Value.Bytes) != "debug" {
			t.Fatalf("change = %q, want debug", u.Value.Bytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mutation not delivered")
	}
}

// TestWatchClassifiesNonNotFoundError proves classifyEtcd is wired into
// Watch's own stream-error branch (etcd.go's `if err := resp.Err(); err !=
// nil` case), not just Resolve. fakeClient.failWatch pushes a canceled
// WatchResponse whose CancelReason is one of the exact server error texts
// etcd's real WatchResponse.Err() recognizes, so this goes through the
// genuine rpctypes conversion classifyEtcd's fallback exists to handle,
// rather than a synthetic status error. Without this test, deleting the
// classifyEtcd call from Watch's error branch would go unnoticed by every
// other test in this package.
func TestWatchClassifiesNonNotFoundError(t *testing.T) {
	fake := newFakeClient()
	fake.set("watched", "v1")
	p := New(withClient(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "etcd://watched"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	fake.failWatch("watched", "etcdserver: permission denied")
	select {
	case u := <-ch:
		if u.Err == nil {
			t.Fatal("Watch emitted a nil error while the stream was failing")
		}
		if got := mamori.ErrorKind(u.Err); got != mamori.KindPermissionDenied {
			t.Fatalf("ErrorKind(u.Err) = %q, want %q; classifyEtcd may not be wired into Watch's stream error path", got, mamori.KindPermissionDenied)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no Update emitted")
	}
}

func TestWatchClosesOnCancel(t *testing.T) {
	fake := newFakeClient()
	fake.set("k", "v")
	p := New(withClient(fake))

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Watch(ctx, mustRef(t, "etcd://k"))
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

// --- Close ---

// TestCloseWithoutUseIsSafe pins the "safe with no prior use" half of the
// Close contract: Close on a provider that never resolved must not dial and
// must not panic, and a second Close must stay clean.
func TestCloseWithoutUseIsSafe(t *testing.T) {
	p := New()
	if err := p.Close(); err != nil {
		t.Fatalf("Close on an unused provider: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if p.ownClient || p.cli != nil {
		t.Error("Close built a client on a provider that never resolved")
	}
}

// TestResolveAfterCloseIsUnavailable pins the terminal half of the contract:
// once Close has run, Resolve must refuse locally (via the p.closed check in
// conn) rather than reaching into the fake client it was told to stop using.
func TestResolveAfterCloseIsUnavailable(t *testing.T) {
	fake := newFakeClient()
	fake.set("k", "v")
	p := New(withClient(fake))

	if _, err := p.Resolve(context.Background(), mustRef(t, "etcd://k")); err != nil {
		t.Fatalf("Resolve before Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := p.Resolve(context.Background(), mustRef(t, "etcd://k")); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("Resolve after Close = %v; want ErrUnavailable", err)
	}
}

// TestCloseDoesNotCloseInjectedClient is the ownership half of rule 5: a
// *clientv3.Client handed in with WithClient belongs to the caller, so Close
// must stop using it without closing it. clientv3.Client exposes no "is
// closed" getter, so this is verified by calling the real Close ourselves
// afterward: a *clientv3.Client that has already been closed returns a
// non-nil error ("context canceled", from the client's internal context
// being cancelled by the first Close) on a second Close, so a nil result here
// proves this is the first real close - i.e. Provider.Close left it alone.
//
// It also proves the other half of rule 5's ordering requirement - that Close
// still marks the provider closed on the not-owned path - since a Close that
// returned early here (skipping p.closed = true to avoid touching the
// injected client) would pass the "still usable" assertion below while
// silently breaking Resolve's terminality for every WithClient caller.
func TestCloseDoesNotCloseInjectedClient(t *testing.T) {
	// clientv3.New never dials synchronously, so this construction is instant
	// and needs no reachable server.
	real, err := clientv3.New(clientv3.Config{Endpoints: []string{"203.0.113.1:2379"}})
	if err != nil {
		t.Fatalf("clientv3.New: %v", err)
	}
	p := New(WithClient(real))

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := real.Close(); err != nil {
		t.Fatalf("injected client Close after provider Close: %v; want nil "+
			"(a non-nil error here means Provider.Close already closed it)", err)
	}

	if _, err := p.Resolve(context.Background(), mustRef(t, "etcd://k")); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("Resolve after Close = %v; want ErrUnavailable (closed flag not set on the injected path)", err)
	}
}

// TestCloseReleasesSelfBuiltClient is the positive half of rule 5's ownership
// tracking, the mirror image of TestCloseDoesNotCloseInjectedClient above: a
// client this provider built itself must actually be released, not merely
// left alone. Without this, deleting "p.ownClient = true" from conn's build
// branch (etcd.go) leaves every other test in this file green, since none of
// them ever force that branch and then check the client came back closed.
func TestCloseReleasesSelfBuiltClient(t *testing.T) {
	p := New(WithEndpoints("203.0.113.1:2379"))

	// conn's build branch calls clientv3.New, which never dials synchronously,
	// so this reaches the p.ownClient = true build site without a reachable
	// server.
	cli, err := p.conn()
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if !p.ownClient {
		t.Fatal("conn did not claim ownership of the client it built")
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The provider-built client must actually have been closed: a
	// *clientv3.Client that has already been closed returns a non-nil error
	// ("context canceled", from the client's internal context being
	// cancelled by the first Close) on a second Close, so calling it
	// ourselves now must return a non-nil error, not nil.
	if err := cli.Close(); err == nil {
		t.Fatal("self-built client Close after provider Close returned nil; " +
			"want an already-closed error (Provider.Close did not actually close it)")
	}
}

func mustRef(t *testing.T, s string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return ref
}
