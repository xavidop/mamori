package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// ---------------------------------------------------------------------------
// In-memory fake for S3.
// ---------------------------------------------------------------------------

// missingBucket is a bucket name the fake treats as non-existent, so tests can
// exercise the NoSuchBucket -> ErrNotFound mapping.
const missingBucket = "no-such-bucket"

type fakeObject struct {
	body      []byte
	etag      string // stored quoted, exactly as S3 returns it
	versionID string
}

type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]fakeObject // keyed by "<bucket>/<key>"
	counter int
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string]fakeObject{}}
}

// put stores/overwrites an object, assigning a fresh (globally unique) quoted
// ETag so that every write changes the version, exactly like real S3.
func (f *fakeS3) put(bucket, key, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counter++
	f.objects[bucket+"/"+key] = fakeObject{
		body: []byte(val),
		etag: fmt.Sprintf("%q", fmt.Sprintf("etag-%d", f.counter)),
	}
}

// putVersioned stores an object with an explicit VersionId and no ETag, to
// exercise the VersionId fallback in value().
func (f *fakeS3) putVersioned(bucket, key, val, versionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[bucket+"/"+key] = fakeObject{body: []byte(val), versionID: versionID}
}

func (f *fakeS3) GetObject(ctx context.Context, in *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket := awssdk.ToString(in.Bucket)
	key := awssdk.ToString(in.Key)
	if bucket == missingBucket {
		return nil, &s3types.NoSuchBucket{Message: awssdk.String("The specified bucket does not exist")}
	}
	obj, ok := f.objects[bucket+"/"+key]
	if !ok {
		return nil, &s3types.NoSuchKey{Message: awssdk.String("The specified key does not exist.")}
	}
	out := &awss3.GetObjectOutput{
		Body: io.NopCloser(bytes.NewReader(obj.body)),
	}
	if obj.etag != "" {
		out.ETag = awssdk.String(obj.etag)
	}
	if obj.versionID != "" {
		out.VersionId = awssdk.String(obj.versionID)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

func mustParse(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

// ---------------------------------------------------------------------------
// Conformance kit against the in-memory fake. SkipWatch: S3 is polled, never
// watched, so the provider does not implement WatchableProvider.
// ---------------------------------------------------------------------------

const testBucket = "mamori-test-bucket"

func TestConformance(t *testing.T) {
	fake := newFakeS3()
	providertest.Run(t, providertest.Config{
		New:       func() mamori.Provider { return newWithClient(fake) },
		Ref:       func(key string) string { return scheme + "://" + testBucket + "/" + key },
		Seed:      func(_ context.Context, key, val string) error { fake.put(testBucket, key, val); return nil },
		Mutate:    func(_ context.Context, key, val string) error { fake.put(testBucket, key, val); return nil },
		SkipWatch: true,
	})
}

// ---------------------------------------------------------------------------
// Registration.
// ---------------------------------------------------------------------------

func TestRegisteredScheme(t *testing.T) {
	for _, s := range mamori.RegisteredSchemes() {
		if s == scheme {
			return
		}
	}
	t.Errorf("scheme %q was not registered by init()", scheme)
}

func TestConstructorScheme(t *testing.T) {
	if s := New(WithRegion("eu-west-1")).Scheme(); s != scheme {
		t.Errorf("Provider.Scheme() = %q, want %q", s, scheme)
	}
}

// ---------------------------------------------------------------------------
// Unit tests.
// ---------------------------------------------------------------------------

func TestResolveObject(t *testing.T) {
	fake := newFakeS3()
	fake.put(testBucket, "config/app.json", `{"log":"debug"}`)
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "s3://"+testBucket+"/config/app.json"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != `{"log":"debug"}` {
		t.Errorf("Bytes = %q, want the JSON object", v.Bytes)
	}
	if v.Version != "etag-1" {
		t.Errorf("Version = %q, want etag-1 (ETag with quotes stripped)", v.Version)
	}
	if v.Sensitive {
		t.Error("value must not be Sensitive by default")
	}
}

func TestResolveJSONKey(t *testing.T) {
	fake := newFakeS3()
	fake.put(testBucket, "prod/db.json", `{"username":"neo","password":"trinity"}`)
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "s3://"+testBucket+"/prod/db.json#password"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "trinity" {
		t.Errorf("Bytes = %q, want trinity", v.Bytes)
	}
	// The version tracks the whole object, not the selected field.
	if v.Version != "etag-1" {
		t.Errorf("Version = %q, want the whole-object ETag etag-1", v.Version)
	}
}

func TestResolveNestedKeyWithSlashes(t *testing.T) {
	fake := newFakeS3()
	fake.put(testBucket, "secrets/tls/chain.pem", "-----BEGIN CERTIFICATE-----")
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "s3://"+testBucket+"/secrets/tls/chain.pem"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "-----BEGIN CERTIFICATE-----" {
		t.Errorf("Bytes = %q, want the PEM header", v.Bytes)
	}
}

func TestResolveSensitiveOption(t *testing.T) {
	fake := newFakeS3()
	fake.put(testBucket, "secret.env", "TOKEN=abc")
	p := newWithClient(fake, WithSensitive(true))

	v, err := p.Resolve(context.Background(), mustParse(t, "s3://"+testBucket+"/secret.env"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Error("WithSensitive(true) must mark the value Sensitive")
	}
}

func TestResolveVersionIdFallback(t *testing.T) {
	fake := newFakeS3()
	fake.putVersioned(testBucket, "obj", "payload", "ver-xyz")
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "s3://"+testBucket+"/obj"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Version != "ver-xyz" {
		t.Errorf("Version = %q, want the VersionId ver-xyz (ETag absent)", v.Version)
	}
}

func TestResolveVersionHashFallback(t *testing.T) {
	fake := newFakeS3()
	fake.putVersioned(testBucket, "obj", "payload", "") // no ETag, no VersionId
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "s3://"+testBucket+"/obj"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Version != mamori.VersionHash([]byte("payload")) {
		t.Errorf("Version = %q, want VersionHash fallback", v.Version)
	}
}

func TestResolveNoSuchKey(t *testing.T) {
	p := newWithClient(newFakeS3())
	_, err := p.Resolve(context.Background(), mustParse(t, "s3://"+testBucket+"/missing"))
	if err == nil {
		t.Fatal("expected error for missing object")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

func TestResolveNoSuchBucket(t *testing.T) {
	p := newWithClient(newFakeS3())
	_, err := p.Resolve(context.Background(), mustParse(t, "s3://"+missingBucket+"/whatever"))
	if err == nil {
		t.Fatal("expected error for missing bucket")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

func TestResolveBadRefMissingKey(t *testing.T) {
	p := newWithClient(newFakeS3())
	// A ref with a bucket but no object key is a configuration error, NOT a
	// not-found: it must surface loudly rather than be silently defaulted.
	_, err := p.Resolve(context.Background(), mustParse(t, "s3://bucket-only"))
	if err == nil {
		t.Fatal("expected error for a ref without an object key")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("a malformed ref must not map to ErrNotFound, got %v", err)
	}
}

func TestNotWatchable(t *testing.T) {
	// The provider must NOT implement WatchableProvider: mamori polls S3 using
	// the ETag Version instead.
	if _, ok := any(New()).(mamori.WatchableProvider); ok {
		t.Error("S3 provider must not implement WatchableProvider (it is polled)")
	}
}
