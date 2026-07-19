package mongodb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeBackend is an in-memory implementation of backend. It stores documents per
// collection keyed by _id and reproduces the parts of MongoDB the provider
// relies on: point lookups by _id or an arbitrary field, and a change-stream-like
// signal (WatchDoc) that fires whenever the target document is written. Change
// streams themselves need a live replica set, so the fake models their observable
// behaviour rather than the wire protocol; the real *mongo.ChangeStream path is
// covered by the //go:build integration test.
type fakeBackend struct {
	mu       sync.Mutex
	colls    map[string]map[string]bson.M
	watchers map[*fakeWatch]struct{}
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		colls:    map[string]map[string]bson.M{},
		watchers: map[*fakeWatch]struct{}{},
	}
}

// put writes doc (keyed by id) into collection and wakes every watcher whose
// selector matches it.
func (f *fakeBackend) put(collection, id string, doc bson.M) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.colls[collection]
	if m == nil {
		m = map[string]bson.M{}
		f.colls[collection] = m
	}
	m[id] = cloneDoc(doc)
	for w := range f.watchers {
		if w.collection == collection && matchesDoc(w.keyField, w.keyValue, doc) {
			select {
			case w.events <- struct{}{}:
			default: // coalesce: Next re-reads current state anyway
			}
		}
	}
}

// remove deletes a document and wakes matching watchers (models a delete event).
func (f *fakeBackend) remove(collection, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.colls[collection]
	if m == nil {
		return
	}
	doc := m[id]
	delete(m, id)
	for w := range f.watchers {
		if w.collection == collection && (doc == nil || matchesDoc(w.keyField, w.keyValue, doc)) {
			select {
			case w.events <- struct{}{}:
			default:
			}
		}
	}
}

func (f *fakeBackend) FindDoc(ctx context.Context, collection, keyField, keyValue string) (bson.M, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.colls[collection]
	if m == nil {
		return nil, notFound(collection, keyValue)
	}
	if keyField == "" {
		doc, ok := m[keyValue]
		if !ok {
			return nil, notFound(collection, keyValue)
		}
		return cloneDoc(doc), nil
	}
	for _, doc := range m {
		if fmt.Sprint(doc[keyField]) == keyValue {
			return cloneDoc(doc), nil
		}
	}
	return nil, notFound(collection, keyValue)
}

func (f *fakeBackend) WatchDoc(_ context.Context, collection, keyField, keyValue string) (changeCursor, error) {
	w := &fakeWatch{
		be:         f,
		collection: collection,
		keyField:   keyField,
		keyValue:   keyValue,
		events:     make(chan struct{}, 8),
	}
	f.mu.Lock()
	f.watchers[w] = struct{}{}
	f.mu.Unlock()
	return w, nil
}

// fakeWatch is the fake's changeCursor: a signal channel woken by put/remove.
type fakeWatch struct {
	be         *fakeBackend
	collection string
	keyField   string
	keyValue   string
	events     chan struct{}
}

func (w *fakeWatch) Next(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case _, ok := <-w.events:
		return ok
	}
}

func (w *fakeWatch) Err() error { return nil }

func (w *fakeWatch) Close(context.Context) error {
	w.be.mu.Lock()
	defer w.be.mu.Unlock()
	if _, ok := w.be.watchers[w]; ok {
		delete(w.be.watchers, w)
		close(w.events)
	}
	return nil
}

func matchesDoc(keyField, keyValue string, doc bson.M) bool {
	if keyField == "" {
		return fmt.Sprint(doc["_id"]) == keyValue
	}
	return fmt.Sprint(doc[keyField]) == keyValue
}

func cloneDoc(doc bson.M) bson.M {
	if doc == nil {
		return nil
	}
	cp := make(bson.M, len(doc))
	for k, v := range doc {
		cp[k] = v
	}
	return cp
}

func notFound(collection, keyValue string) error {
	return fmt.Errorf("mongodb: document %q not found in collection %q: %w", keyValue, collection, mamori.ErrNotFound)
}

var _ backend = (*fakeBackend)(nil)

// --- conformance ---------------------------------------------------------------

func TestConformance(t *testing.T) {
	fake := newFakeBackend()
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return New(withBackend(fake)) },
		// The conformance kit stores each value as a document's "value" field and
		// selects it with #value, so the resolved bytes equal the seeded string.
		Ref: func(key string) string { return "mongodb://conformance/" + key + "#value" },
		Seed: func(_ context.Context, key, val string) error {
			fake.put("conformance", key, bson.M{"_id": key, "value": val})
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			fake.put("conformance", key, bson.M{"_id": key, "value": val})
			return nil
		},
		EventuallyTimeout: 3 * time.Second,
	})
}

// --- unit tests ----------------------------------------------------------------

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != "mongodb" {
		t.Fatalf("Scheme() = %q, want mongodb", got)
	}
}

func TestResolveDocumentAsJSON(t *testing.T) {
	fake := newFakeBackend()
	fake.put("users", "u1", bson.M{"_id": "u1", "name": "Ada", "age": 42})
	p := New(withBackend(fake))

	v, err := p.Resolve(context.Background(), mustRef(t, "mongodb://users/u1"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Deterministic (sorted-key) JSON of the whole document.
	if got := string(v.Bytes); got != `{"_id":"u1","age":42,"name":"Ada"}` {
		t.Fatalf("Bytes = %q", got)
	}
	if v.Version == "" {
		t.Fatal("Version must not be empty")
	}
	if v.Sensitive {
		t.Fatal("MongoDB values must not be marked Sensitive by default")
	}

	// The version must be stable across repeated resolves of an unchanged doc.
	v2, err := p.Resolve(context.Background(), mustRef(t, "mongodb://users/u1"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v2.Version != v.Version {
		t.Fatalf("Version not stable: %q vs %q", v.Version, v2.Version)
	}
}

func TestResolveField(t *testing.T) {
	fake := newFakeBackend()
	fake.put("secrets", "app-db", bson.M{
		"_id":      "app-db",
		"password": "s3cr3t",
		"port":     5432,
		"meta":     bson.M{"region": "eu"},
	})
	p := New(withBackend(fake))

	get := func(field string) string {
		t.Helper()
		v, err := p.Resolve(context.Background(), mustRef(t, "mongodb://secrets/app-db#"+field))
		if err != nil {
			t.Fatalf("Resolve #%s: %v", field, err)
		}
		return string(v.Bytes)
	}
	if got := get("password"); got != "s3cr3t" {
		t.Fatalf("password = %q", got)
	}
	if got := get("port"); got != "5432" {
		t.Fatalf("port = %q, want 5432", got)
	}
	// An object field is returned as its JSON encoding.
	if got := get("meta"); got != `{"region":"eu"}` {
		t.Fatalf("meta = %q", got)
	}

	// A missing field is a typed not-found.
	_, err := p.Resolve(context.Background(), mustRef(t, "mongodb://secrets/app-db#nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing field err = %v, want ErrNotFound", err)
	}
}

func TestResolveByKeyOption(t *testing.T) {
	fake := newFakeBackend()
	fake.put("users", "internal-id-1", bson.M{
		"_id":    "internal-id-1",
		"email":  "ada@example.com",
		"apiKey": "abc123",
	})
	p := New(withBackend(fake))

	// Select the document by its email field, then pick apiKey. Per the mamori
	// grammar the #field fragment precedes the ?key option.
	v, err := p.Resolve(context.Background(), mustRef(t, "mongodb://users/ada@example.com#apiKey?key=email"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "abc123" {
		t.Fatalf("apiKey = %q, want abc123", v.Bytes)
	}

	// A value that matches no document's email is not found.
	_, err = p.Resolve(context.Background(), mustRef(t, "mongodb://users/none@example.com#apiKey?key=email"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(withBackend(newFakeBackend()))
	_, err := p.Resolve(context.Background(), mustRef(t, "mongodb://users/missing"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestVersionFromField(t *testing.T) {
	fake := newFakeBackend()
	fake.put("cfg", "a", bson.M{"_id": "a", "value": "x", "version": "rev-7"})
	fake.put("cfg", "b", bson.M{"_id": "b", "value": "x", "version": 9})
	p := New(withBackend(fake))

	va, err := p.Resolve(context.Background(), mustRef(t, "mongodb://cfg/a#value"))
	if err != nil {
		t.Fatalf("Resolve a: %v", err)
	}
	if va.Version != "rev-7" {
		t.Fatalf("string version = %q, want rev-7", va.Version)
	}

	vb, err := p.Resolve(context.Background(), mustRef(t, "mongodb://cfg/b#value"))
	if err != nil {
		t.Fatalf("Resolve b: %v", err)
	}
	if vb.Version != "9" {
		t.Fatalf("numeric version = %q, want 9", vb.Version)
	}
}

func TestVersionChangesOnMutate(t *testing.T) {
	fake := newFakeBackend()
	fake.put("cfg", "a", bson.M{"_id": "a", "value": "one"})
	p := New(withBackend(fake))
	ref := mustRef(t, "mongodb://cfg/a#value")

	v1, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	fake.put("cfg", "a", bson.M{"_id": "a", "value": "two"})
	v2, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v1.Version == v2.Version {
		t.Fatalf("Version did not change after mutate (both %q)", v1.Version)
	}
	if string(v2.Bytes) != "two" {
		t.Fatalf("Bytes = %q, want two", v2.Bytes)
	}
}

func TestResolveContextCancelled(t *testing.T) {
	fake := newFakeBackend()
	fake.put("cfg", "a", bson.M{"_id": "a", "value": "x"})
	p := New(withBackend(fake))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustRef(t, "mongodb://cfg/a#value")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestResolveBadPath(t *testing.T) {
	p := New(withBackend(newFakeBackend()))
	// Missing the /<docid> segment.
	_, err := p.Resolve(context.Background(), mustRef(t, "mongodb://onlycollection"))
	if err == nil {
		t.Fatal("Resolve of a ref without <docid> returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("a malformed path should be a usage error, not ErrNotFound")
	}
}

func TestWatchBaselineAndChange(t *testing.T) {
	fake := newFakeBackend()
	fake.put("cfg", "a", bson.M{"_id": "a", "value": "v1"})
	p := New(withBackend(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "mongodb://cfg/a#value"))
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

	// A write to the watched document must produce an Update.
	fake.put("cfg", "a", bson.M{"_id": "a", "value": "v2"})
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

	// A write to an unrelated document must NOT wake this watch.
	fake.put("cfg", "other", bson.M{"_id": "other", "value": "z"})
	select {
	case u := <-ch:
		t.Fatalf("unrelated write produced an update: %+v", u)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestWatchDeleteEmitsNotFound(t *testing.T) {
	fake := newFakeBackend()
	fake.put("cfg", "a", bson.M{"_id": "a", "value": "v1"})
	p := New(withBackend(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "mongodb://cfg/a#value"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	<-ch // baseline

	fake.remove("cfg", "a")
	select {
	case u := <-ch:
		if !errors.Is(u.Err, mamori.ErrNotFound) {
			t.Fatalf("delete update err = %v, want ErrNotFound", u.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delete not delivered")
	}
}

func TestWatchClosesOnCancel(t *testing.T) {
	fake := newFakeBackend()
	fake.put("cfg", "a", bson.M{"_id": "a", "value": "v1"})
	p := New(withBackend(fake))

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Watch(ctx, mustRef(t, "mongodb://cfg/a#value"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return // closed as required
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
