package firestore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeStore is an in-memory implementation of the backend interface. It models
// the parts of the Firestore contract the provider relies on: documents keyed by
// "<collection>/<doc>", a monotonically increasing UpdateTime bumped on every
// write, and snapshot-listener semantics (a stream emits the current state as a
// baseline, then a fresh snapshot each time the global version advances, until
// its context is cancelled). It lets providertest.Run - including the native
// watch tests - run without a live Firestore.
type fakeStore struct {
	mu       sync.Mutex
	docs     map[string]fakeDoc
	version  uint64
	baseTime time.Time
	waiters  chan struct{} // closed (and replaced) on every write to wake blocked streams
}

type fakeDoc struct {
	data       map[string]interface{}
	updateTime time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		docs:     map[string]fakeDoc{},
		baseTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		waiters:  make(chan struct{}),
	}
}

func docKey(collection, doc string) string { return collection + "/" + doc }

// set writes data for collection/doc, bumping the global version and the
// document's UpdateTime, then wakes any blocked snapshot streams.
func (f *fakeStore) set(collection, doc string, data map[string]interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version++
	f.docs[docKey(collection, doc)] = fakeDoc{
		data:       data,
		updateTime: f.baseTime.Add(time.Duration(f.version) * time.Millisecond),
	}
	close(f.waiters)
	f.waiters = make(chan struct{})
}

// Get implements backend.
func (f *fakeStore) Get(ctx context.Context, collection, doc string) (snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.docs[docKey(collection, doc)]
	if !ok {
		return fakeSnapshot{exists: false}, nil
	}
	return fakeSnapshot{exists: true, data: d.data, updateTime: d.updateTime}, nil
}

// Snapshots implements backend.
func (f *fakeStore) Snapshots(ctx context.Context, collection, doc string) (snapshotStream, error) {
	return &fakeStream{store: f, ctx: ctx, collection: collection, doc: doc}, nil
}

// Close implements backend.
func (f *fakeStore) Close() error { return nil }

// fakeSnapshot implements the snapshot interface.
type fakeSnapshot struct {
	exists     bool
	data       map[string]interface{}
	updateTime time.Time
}

func (s fakeSnapshot) Exists() bool                 { return s.exists }
func (s fakeSnapshot) Data() map[string]interface{} { return s.data }
func (s fakeSnapshot) UpdateTime() time.Time        { return s.updateTime }

// fakeStream implements snapshotStream with baseline + change-notification
// semantics tied to the store's global version.
type fakeStream struct {
	store           *fakeStore
	ctx             context.Context
	collection, doc string
	started         bool
	lastVersion     uint64
}

func (s *fakeStream) Next() (snapshot, error) {
	for {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		s.store.mu.Lock()
		cur := s.store.version
		d, ok := s.store.docs[docKey(s.collection, s.doc)]
		wake := s.store.waiters
		s.store.mu.Unlock()

		// Emit the baseline on the first call, then whenever the global version
		// has advanced since we last emitted.
		if !s.started || cur != s.lastVersion {
			s.started = true
			s.lastVersion = cur
			if !ok {
				return fakeSnapshot{exists: false}, nil
			}
			return fakeSnapshot{exists: true, data: d.data, updateTime: d.updateTime}, nil
		}

		select {
		case <-s.ctx.Done():
			return nil, s.ctx.Err()
		case <-wake:
			// A write happened; loop and re-read the (now-changed) state.
		}
	}
}

func (s *fakeStream) Stop() {}

// compile-time check that the fake satisfies the backend interface.
var _ backend = (*fakeStore)(nil)

func TestConformance(t *testing.T) {
	store := newFakeStore()
	const collection = "conform"
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return New(withBackend(store)) },
		// The document stores the value under a "value" field so a scalar can be
		// round-tripped through a document-shaped backend.
		Ref: func(key string) string { return "firestore://" + collection + "/" + key + "#value" },
		Seed: func(_ context.Context, key, val string) error {
			store.set(collection, key, map[string]interface{}{"value": val})
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			store.set(collection, key, map[string]interface{}{"value": val})
			return nil
		},
		EventuallyTimeout: 3 * time.Second,
	})
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != "firestore" {
		t.Fatalf("Scheme() = %q, want firestore", got)
	}
}

func TestResolveWholeDocument(t *testing.T) {
	store := newFakeStore()
	store.set("config", "app", map[string]interface{}{"host": "db.internal", "port": 5432})
	p := New(withBackend(store))

	v, err := p.Resolve(context.Background(), mustRef(t, "firestore://config/app"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// json.Marshal of a map sorts keys, so the encoding is deterministic.
	if got, want := string(v.Bytes), `{"host":"db.internal","port":5432}`; got != want {
		t.Fatalf("Bytes = %q, want %q", got, want)
	}
	if v.Version == "" {
		t.Fatal("Version is empty")
	}
	if v.Sensitive {
		t.Fatal("Firestore values must not be marked Sensitive")
	}
}

func TestResolveField(t *testing.T) {
	store := newFakeStore()
	store.set("config", "app", map[string]interface{}{
		"host":     "db.internal",
		"port":     5432,
		"password": "s3cr3t",
	})
	p := New(withBackend(store))

	got := func(field string) string {
		t.Helper()
		v, err := p.Resolve(context.Background(), mustRef(t, "firestore://config/app#"+field))
		if err != nil {
			t.Fatalf("Resolve #%s: %v", field, err)
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

	// A missing field is a typed not-found.
	_, err := p.Resolve(context.Background(), mustRef(t, "firestore://config/app#nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing field err = %v, want ErrNotFound", err)
	}
}

func TestResolveVersionFromUpdateTime(t *testing.T) {
	store := newFakeStore()
	store.set("config", "app", map[string]interface{}{"value": "one"})
	p := New(withBackend(store))

	ref := mustRef(t, "firestore://config/app")
	v1, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Version is the RFC3339Nano UpdateTime.
	if _, err := time.Parse(time.RFC3339Nano, v1.Version); err != nil {
		t.Fatalf("Version %q is not RFC3339Nano: %v", v1.Version, err)
	}

	store.set("config", "app", map[string]interface{}{"value": "two"})
	v2, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve after mutate: %v", err)
	}
	if v1.Version == v2.Version {
		t.Fatalf("Version did not change after write (both %q)", v1.Version)
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(withBackend(newFakeStore()))
	_, err := p.Resolve(context.Background(), mustRef(t, "firestore://config/absent"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveBadPath(t *testing.T) {
	p := New(withBackend(newFakeStore()))
	// Missing the document segment.
	_, err := p.Resolve(context.Background(), mustRef(t, "firestore://onlycollection"))
	if err == nil {
		t.Fatal("Resolve of a ref without a document returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("a malformed ref must not be reported as not-found")
	}
}

func TestResolveContextCancelled(t *testing.T) {
	store := newFakeStore()
	store.set("config", "app", map[string]interface{}{"value": "v"})
	p := New(withBackend(store))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustRef(t, "firestore://config/app")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestWatchEmitsBaselineAndChange(t *testing.T) {
	store := newFakeStore()
	store.set("config", "app", map[string]interface{}{"value": "v1"})
	p := New(withBackend(store))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "firestore://config/app#value"))
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

	// A change to the document must produce an Update via the snapshot listener.
	store.set("config", "app", map[string]interface{}{"value": "v2"})
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("change err: %v", u.Err)
		}
		if string(u.Value.Bytes) != "v2" {
			t.Fatalf("change = %q, want v2", u.Value.Bytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("change not delivered")
	}
}

func TestWatchClosesOnCancel(t *testing.T) {
	store := newFakeStore()
	store.set("config", "app", map[string]interface{}{"value": "v"})
	p := New(withBackend(store))

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Watch(ctx, mustRef(t, "firestore://config/app"))
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
