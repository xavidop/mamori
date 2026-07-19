package gcs

import (
	"context"
	"errors"
	"sync"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeObject is a single stored object in the in-memory fake.
type fakeObject struct {
	data       []byte
	generation int64
	etag       string
}

// fakeGCS is an in-memory implementation of objectReader modeling GCS read
// semantics: objects are keyed by "bucket/object", each write bumps the
// generation, and an unknown object returns storage.ErrObjectNotExist (which
// the provider maps to mamori.ErrNotFound).
type fakeGCS struct {
	mu      sync.Mutex
	objects map[string]*fakeObject
	closed  bool
}

func newFakeGCS() *fakeGCS {
	return &fakeGCS{objects: map[string]*fakeObject{}}
}

// put writes val to bucket/object, creating it if needed and bumping the
// generation (mirroring a GCS overwrite).
func (f *fakeGCS) put(bucket, object, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := bucket + "/" + object
	o, ok := f.objects[key]
	if !ok {
		o = &fakeObject{}
		f.objects[key] = o
	}
	o.data = []byte(val)
	o.generation++
}

func (f *fakeGCS) read(ctx context.Context, bucket, object string) ([]byte, int64, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[bucket+"/"+object]
	if !ok {
		return nil, 0, "", storage.ErrObjectNotExist
	}
	return append([]byte(nil), o.data...), o.generation, o.etag, nil
}

func (f *fakeGCS) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// compile-time assertion that the fake satisfies the provider's client contract.
var _ objectReader = (*fakeGCS)(nil)

func parse(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

func TestConformance(t *testing.T) {
	fake := newFakeGCS()
	const bucket = "test-bucket"
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return New(WithClient(fake)) },
		Ref: func(key string) string { return "gcs://" + bucket + "/" + key },
		Seed: func(_ context.Context, key, val string) error {
			fake.put(bucket, key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			fake.put(bucket, key, val)
			return nil
		},
		// GCS has no cheap native watch; the provider is not watchable and mamori
		// polls it. Skip the watch conformance tests explicitly.
		SkipWatch: true,
	})
}

func TestResolveObject(t *testing.T) {
	fake := newFakeGCS()
	fake.put("bkt", "app/config.json", "hello-world")
	p := New(WithClient(fake))

	v, err := p.Resolve(context.Background(), parse(t, "gcs://bkt/app/config.json"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hello-world" {
		t.Fatalf("Bytes = %q, want hello-world", v.Bytes)
	}
	// generation 1 (first put) reported as the version string
	if v.Version != "1" {
		t.Fatalf("Version = %q, want 1", v.Version)
	}
	// sensitive is off by default
	if v.Sensitive {
		t.Error("Sensitive = true, want false by default")
	}
}

func TestObjectNameWithSlashes(t *testing.T) {
	fake := newFakeGCS()
	// Object name has multiple slashes; only the first "/" splits bucket/object.
	fake.put("my-bucket", "env/prod/nested/settings.yaml", "deep")
	p := New(WithClient(fake))

	v, err := p.Resolve(context.Background(), parse(t, "gcs://my-bucket/env/prod/nested/settings.yaml"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "deep" {
		t.Fatalf("Bytes = %q, want deep", v.Bytes)
	}
}

func TestResolveJSONKey(t *testing.T) {
	fake := newFakeGCS()
	fake.put("bkt", "config.json", `{"api_key":"abc123","port":8080}`)
	p := New(WithClient(fake))

	// string field is returned unquoted
	v, err := p.Resolve(context.Background(), parse(t, "gcs://bkt/config.json#api_key"))
	if err != nil {
		t.Fatalf("Resolve #api_key: %v", err)
	}
	if string(v.Bytes) != "abc123" {
		t.Fatalf("Bytes = %q, want abc123", v.Bytes)
	}

	// numeric field is returned as its JSON encoding
	v, err = p.Resolve(context.Background(), parse(t, "gcs://bkt/config.json#port"))
	if err != nil {
		t.Fatalf("Resolve #port: %v", err)
	}
	if string(v.Bytes) != "8080" {
		t.Fatalf("Bytes = %q, want 8080", v.Bytes)
	}

	// absent key maps to ErrNotFound
	_, err = p.Resolve(context.Background(), parse(t, "gcs://bkt/config.json#missing"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing key err = %v, want ErrNotFound", err)
	}
}

func TestVersionFallbacks(t *testing.T) {
	fake := newFakeGCS()
	// Craft objects directly so we can exercise each branch of the version chain.
	fake.objects["b/withgen"] = &fakeObject{data: []byte("y"), generation: 42, etag: "ignored"}
	fake.objects["b/gen0-etag"] = &fakeObject{data: []byte("x"), generation: 0, etag: "etag-abc"}
	fake.objects["b/gen0-noetag"] = &fakeObject{data: []byte("hashme"), generation: 0, etag: ""}
	p := New(WithClient(fake))

	// generation wins when present
	v, err := p.Resolve(context.Background(), parse(t, "gcs://b/withgen"))
	if err != nil {
		t.Fatalf("Resolve withgen: %v", err)
	}
	if v.Version != "42" {
		t.Fatalf("Version = %q, want 42 (generation)", v.Version)
	}

	// etag is used when there is no generation
	v, err = p.Resolve(context.Background(), parse(t, "gcs://b/gen0-etag"))
	if err != nil {
		t.Fatalf("Resolve gen0-etag: %v", err)
	}
	if v.Version != "etag-abc" {
		t.Fatalf("Version = %q, want etag-abc", v.Version)
	}

	// content hash is the last resort
	v, err = p.Resolve(context.Background(), parse(t, "gcs://b/gen0-noetag"))
	if err != nil {
		t.Fatalf("Resolve gen0-noetag: %v", err)
	}
	if want := mamori.VersionHash([]byte("hashme")); v.Version != want {
		t.Fatalf("Version = %q, want %q (content hash)", v.Version, want)
	}
}

func TestSensitiveOption(t *testing.T) {
	fake := newFakeGCS()
	fake.put("bkt", "secret.txt", "s3cr3t")
	p := New(WithClient(fake), WithSensitive())

	v, err := p.Resolve(context.Background(), parse(t, "gcs://bkt/secret.txt"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Error("Sensitive = false, want true with WithSensitive()")
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(WithClient(newFakeGCS()))
	_, err := p.Resolve(context.Background(), parse(t, "gcs://bkt/nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveBadPath(t *testing.T) {
	p := New(WithClient(newFakeGCS()))
	cases := []string{
		"gcs://onlybucket", // no "/" separating bucket and object
		"gcs://bkt/",       // empty object
	}
	for _, raw := range cases {
		_, err := p.Resolve(context.Background(), parse(t, raw))
		if err == nil {
			t.Fatalf("%s: expected an error for a malformed ref", raw)
		}
		if errors.Is(err, mamori.ErrNotFound) {
			t.Fatalf("%s: bad-path err should not be ErrNotFound, got %v", raw, err)
		}
	}
}

func TestResolveContextCancelled(t *testing.T) {
	fake := newFakeGCS()
	fake.put("bkt", "obj", "x")
	p := New(WithClient(fake))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, parse(t, "gcs://bkt/obj")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestScheme(t *testing.T) {
	if s := New(WithClient(newFakeGCS())).Scheme(); s != "gcs" {
		t.Fatalf("Scheme() = %q, want gcs", s)
	}
}

func TestRegistered(t *testing.T) {
	found := false
	for _, s := range mamori.RegisteredSchemes() {
		if s == "gcs" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("gcs not registered by init()")
	}
}

func TestNotWatchable(t *testing.T) {
	// The provider must NOT implement WatchableProvider: GCS has no cheap native
	// watch, so mamori polls it.
	var p mamori.Provider = New(WithClient(newFakeGCS()))
	if _, ok := p.(mamori.WatchableProvider); ok {
		t.Fatal("gcs provider implements WatchableProvider; it must rely on mamori polling")
	}
}

func TestLazyClientFactory(t *testing.T) {
	fake := newFakeGCS()
	fake.put("bkt", "obj", "lazy")
	calls := 0
	p := New(WithClientFactory(func(context.Context) (objectReader, error) {
		calls++
		return fake, nil
	}))

	if calls != 0 {
		t.Fatalf("factory called %d times before first Resolve, want 0", calls)
	}
	if _, err := p.Resolve(context.Background(), parse(t, "gcs://bkt/obj")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := p.Resolve(context.Background(), parse(t, "gcs://bkt/obj")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if calls != 1 {
		t.Fatalf("factory called %d times, want 1 (lazy, cached)", calls)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fake.closed {
		t.Error("Close did not close the underlying client")
	}
}
