package gcp

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	gax "github.com/googleapis/gax-go/v2"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeSM is an in-memory implementation of smClient modeling Secret Manager's
// AccessSecretVersion semantics: secrets hold an ordered list of version
// payloads, "latest" resolves to the newest, numeric versions are 1-based, and
// unknown secrets/versions return a gRPC NotFound status.
type fakeSM struct {
	mu       sync.Mutex
	versions map[string][][]byte // "projects/P/secrets/S" -> ordered payloads
	closed   bool
}

func newFakeSM() *fakeSM {
	return &fakeSM{versions: map[string][][]byte{}}
}

func (f *fakeSM) add(project, secret, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fmt.Sprintf("projects/%s/secrets/%s", project, secret)
	f.versions[key] = append(f.versions[key], []byte(val))
}

func (f *fakeSM) AccessSecretVersion(ctx context.Context, req *secretmanagerpb.AccessSecretVersionRequest, _ ...gax.CallOption) (*secretmanagerpb.AccessSecretVersionResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := req.GetName()
	i := strings.LastIndex(name, "/versions/")
	if i < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "malformed name %q", name)
	}
	secretPath := name[:i]
	version := name[i+len("/versions/"):]

	f.mu.Lock()
	defer f.mu.Unlock()
	vers, ok := f.versions[secretPath]
	if !ok || len(vers) == 0 {
		return nil, status.Errorf(codes.NotFound, "secret %q not found", secretPath)
	}
	var idx int
	if version == "latest" {
		idx = len(vers) - 1
	} else {
		n, err := strconv.Atoi(version)
		if err != nil || n < 1 || n > len(vers) {
			return nil, status.Errorf(codes.NotFound, "version %q not found for %q", version, secretPath)
		}
		idx = n - 1
	}
	return &secretmanagerpb.AccessSecretVersionResponse{
		Name:    fmt.Sprintf("%s/versions/%d", secretPath, idx+1),
		Payload: &secretmanagerpb.SecretPayload{Data: vers[idx]},
	}, nil
}

func (f *fakeSM) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// compile-time assertion that the fake satisfies the provider's client contract.
var _ smClient = (*fakeSM)(nil)

func parse(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

func TestConformance(t *testing.T) {
	fake := newFakeSM()
	const project = "test-project"
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return New(WithClient(fake)) },
		Ref: func(key string) string { return "gcp-sm://" + project + "/" + key },
		Seed: func(_ context.Context, key, val string) error {
			fake.add(project, key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			fake.add(project, key, val)
			return nil
		},
	})
}

func TestResolveLatest(t *testing.T) {
	fake := newFakeSM()
	fake.add("proj", "db", "s3cr3t")
	p := New(WithClient(fake))

	v, err := p.Resolve(context.Background(), parse(t, "gcp-sm://proj/db"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "s3cr3t" {
		t.Fatalf("Bytes = %q, want s3cr3t", v.Bytes)
	}
	if !v.Sensitive {
		t.Error("Sensitive = false, want true")
	}
	if v.Version != "projects/proj/secrets/db/versions/1" {
		t.Fatalf("Version = %q, want the resolved resource name", v.Version)
	}
}

func TestResolveJSONKey(t *testing.T) {
	fake := newFakeSM()
	fake.add("proj", "config", `{"api_key":"abc123","port":8080}`)
	p := New(WithClient(fake))

	// string field is returned unquoted
	v, err := p.Resolve(context.Background(), parse(t, "gcp-sm://proj/config#api_key"))
	if err != nil {
		t.Fatalf("Resolve #api_key: %v", err)
	}
	if string(v.Bytes) != "abc123" {
		t.Fatalf("Bytes = %q, want abc123", v.Bytes)
	}
	if !v.Sensitive {
		t.Error("Sensitive = false, want true")
	}

	// numeric field is returned as its JSON encoding
	v, err = p.Resolve(context.Background(), parse(t, "gcp-sm://proj/config#port"))
	if err != nil {
		t.Fatalf("Resolve #port: %v", err)
	}
	if string(v.Bytes) != "8080" {
		t.Fatalf("Bytes = %q, want 8080", v.Bytes)
	}

	// absent key maps to ErrNotFound
	_, err = p.Resolve(context.Background(), parse(t, "gcp-sm://proj/config#missing"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing key err = %v, want ErrNotFound", err)
	}
}

func TestResolveVersionOption(t *testing.T) {
	fake := newFakeSM()
	fake.add("proj", "rot", "v1")
	fake.add("proj", "rot", "v2")
	fake.add("proj", "rot", "v3")
	p := New(WithClient(fake))

	// explicit version pins the payload
	v, err := p.Resolve(context.Background(), parse(t, "gcp-sm://proj/rot?version=1"))
	if err != nil {
		t.Fatalf("Resolve version=1: %v", err)
	}
	if string(v.Bytes) != "v1" {
		t.Fatalf("version=1 Bytes = %q, want v1", v.Bytes)
	}
	if v.Version != "projects/proj/secrets/rot/versions/1" {
		t.Fatalf("Version = %q", v.Version)
	}

	// latest (default) returns the newest
	v, err = p.Resolve(context.Background(), parse(t, "gcp-sm://proj/rot"))
	if err != nil {
		t.Fatalf("Resolve latest: %v", err)
	}
	if string(v.Bytes) != "v3" {
		t.Fatalf("latest Bytes = %q, want v3", v.Bytes)
	}

	// unknown version -> ErrNotFound
	_, err = p.Resolve(context.Background(), parse(t, "gcp-sm://proj/rot?version=99"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("bad version err = %v, want ErrNotFound", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(WithClient(newFakeSM()))
	_, err := p.Resolve(context.Background(), parse(t, "gcp-sm://proj/nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveBadPath(t *testing.T) {
	p := New(WithClient(newFakeSM()))
	// no "/" separating project and secret
	_, err := p.Resolve(context.Background(), parse(t, "gcp-sm://onlyproject"))
	if err == nil {
		t.Fatal("expected an error for a path without <project>/<secret>")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("bad-path err should not be ErrNotFound, got %v", err)
	}
}

func TestResolveContextCancelled(t *testing.T) {
	fake := newFakeSM()
	fake.add("proj", "db", "x")
	p := New(WithClient(fake))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, parse(t, "gcp-sm://proj/db")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestScheme(t *testing.T) {
	if s := New(WithClient(newFakeSM())).Scheme(); s != "gcp-sm" {
		t.Fatalf("Scheme() = %q, want gcp-sm", s)
	}
}

func TestRegistered(t *testing.T) {
	found := false
	for _, s := range mamori.RegisteredSchemes() {
		if s == "gcp-sm" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("gcp-sm not registered by init()")
	}
}

func TestLazyClientFactory(t *testing.T) {
	fake := newFakeSM()
	fake.add("proj", "db", "lazy")
	calls := 0
	p := New(WithClientFactory(func(context.Context) (smClient, error) {
		calls++
		return fake, nil
	}))

	if calls != 0 {
		t.Fatalf("factory called %d times before first Resolve, want 0", calls)
	}
	if _, err := p.Resolve(context.Background(), parse(t, "gcp-sm://proj/db")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := p.Resolve(context.Background(), parse(t, "gcp-sm://proj/db")); err != nil {
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
