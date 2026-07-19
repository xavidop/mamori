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
	resp := &clientv3.GetResponse{}
	if kv := f.data[key]; kv != nil {
		resp.Kvs = []*mvccpb.KeyValue{copyKV(kv)}
		resp.Count = 1
	}
	return resp, nil
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
		Ref:  func(key string) string { return "etcd://" + key },
		Seed: func(_ context.Context, key, val string) error { fake.set(key, val); return nil },
		Mutate: func(_ context.Context, key, val string) error {
			fake.set(key, val)
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

func mustRef(t *testing.T, s string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return ref
}
