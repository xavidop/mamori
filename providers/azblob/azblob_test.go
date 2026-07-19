package azblob

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeStore is an in-memory blobDownloader. Blobs are keyed by
// "<container>/<blob>", and every put appends a new version so the ETag returned
// for a blob changes on each mutation.
type fakeStore struct {
	mu   sync.Mutex
	data map[string][]string // "container/blob" -> ordered version values
}

func newFakeStore() *fakeStore { return &fakeStore{data: map[string][]string{}} }

func (f *fakeStore) put(container, blob, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := container + "/" + blob
	f.data[k] = append(f.data[k], val)
}

func (f *fakeStore) Download(ctx context.Context, container, blob string) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	vals, ok := f.data[container+"/"+blob]
	if !ok || len(vals) == 0 {
		return nil, "", mamori.ErrNotFound
	}
	idx := len(vals) - 1 // latest
	// A real blob's ETag changes on every write; mimic that with the count.
	etag := fmt.Sprintf("\"0x%d\"", idx+1)
	return []byte(vals[idx]), etag, nil
}

func mustParse(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

func TestScheme(t *testing.T) {
	if got := New(WithClient(newFakeStore())).Scheme(); got != Scheme {
		t.Fatalf("Scheme() = %q, want %q", got, Scheme)
	}
}

func TestResolve(t *testing.T) {
	fake := newFakeStore()
	fake.put("config", "app.json", `{"level":"info"}`)
	p := New(WithClient(fake))

	v, err := p.Resolve(context.Background(), mustParse(t, "azblob://config/app.json"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != `{"level":"info"}` {
		t.Fatalf("Bytes = %q, want the JSON payload", v.Bytes)
	}
	if v.Version == "" {
		t.Error("Value.Version is empty")
	}
	if v.Sensitive {
		t.Error("Value.Sensitive = true, want false by default")
	}
	if v.Metadata["container"] != "config" || v.Metadata["blob"] != "app.json" {
		t.Errorf("Metadata = %v, want container=config blob=app.json", v.Metadata)
	}
}

func TestResolveSensitive(t *testing.T) {
	fake := newFakeStore()
	fake.put("secrets", "token", "s3cr3t")
	p := New(WithClient(fake), WithSensitive(true))

	v, err := p.Resolve(context.Background(), mustParse(t, "azblob://secrets/token"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Error("Value.Sensitive = false, want true with WithSensitive(true)")
	}
}

func TestResolveNestedBlobPath(t *testing.T) {
	fake := newFakeStore()
	fake.put("data", "envs/prod/db.txt", "postgres://prod")
	p := New(WithClient(fake))

	v, err := p.Resolve(context.Background(), mustParse(t, "azblob://data/envs/prod/db.txt"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "postgres://prod" {
		t.Fatalf("Bytes = %q, want postgres://prod", v.Bytes)
	}
	if v.Metadata["blob"] != "envs/prod/db.txt" {
		t.Errorf("Metadata[blob] = %q, want envs/prod/db.txt", v.Metadata["blob"])
	}
}

func TestResolveJSONKey(t *testing.T) {
	fake := newFakeStore()
	fake.put("config", "conn.json", `{"username":"admin","password":"hunter2"}`)
	p := New(WithClient(fake))

	v, err := p.Resolve(context.Background(), mustParse(t, "azblob://config/conn.json#password"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hunter2" {
		t.Fatalf("selected key = %q, want hunter2", v.Bytes)
	}
}

func TestResolveJSONKeyMissing(t *testing.T) {
	fake := newFakeStore()
	fake.put("config", "conn.json", `{"username":"admin"}`)
	p := New(WithClient(fake))

	_, err := p.Resolve(context.Background(), mustParse(t, "azblob://config/conn.json#password"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing key error = %v, want ErrNotFound", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(WithClient(newFakeStore()))
	_, err := p.Resolve(context.Background(), mustParse(t, "azblob://config/nope.json"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing blob error = %v, want ErrNotFound", err)
	}
}

func TestResolveBadRef(t *testing.T) {
	p := New(WithClient(newFakeStore()))
	for _, raw := range []string{
		"azblob://onlycontainer", // no blob
		"azblob:///blobonly",     // no container
	} {
		_, err := p.Resolve(context.Background(), mustParse(t, raw))
		if err == nil {
			t.Errorf("Resolve(%q) = nil error, want a malformed-ref error", raw)
		} else if errors.Is(err, mamori.ErrNotFound) {
			t.Errorf("Resolve(%q) returned ErrNotFound; a malformed ref is not not-found", raw)
		}
	}
}

func TestResolveContextCancelled(t *testing.T) {
	fake := newFakeStore()
	fake.put("config", "app.json", "x")
	p := New(WithClient(fake))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustParse(t, "azblob://config/app.json")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestVersionChangesOnMutate(t *testing.T) {
	fake := newFakeStore()
	fake.put("config", "app.json", "one")
	p := New(WithClient(fake))
	ref := mustParse(t, "azblob://config/app.json")

	v1, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	fake.put("config", "app.json", "two")
	v2, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version == v2.Version {
		t.Fatalf("Version did not change after mutate (both %q)", v1.Version)
	}
	if string(v2.Bytes) != "two" {
		t.Fatalf("post-mutate value = %q, want two", v2.Bytes)
	}
}

func TestNoAccountConfigured(t *testing.T) {
	t.Setenv(AccountEnv, "") // ensure no ambient account leaks in
	p := New()               // no fixed client, no account URL
	_, err := p.Resolve(context.Background(), mustParse(t, "azblob://config/app.json"))
	if err == nil {
		t.Fatal("Resolve with no account configured returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("missing-account error must not be ErrNotFound")
	}
}

func TestServiceURL(t *testing.T) {
	cases := map[string]string{
		"":                                     "",
		"mystorageacct":                        "https://mystorageacct.blob.core.windows.net",
		"https://acct.blob.core.windows.net":   "https://acct.blob.core.windows.net",
		"https://acct.blob.core.windows.net/":  "https://acct.blob.core.windows.net",
		"http://127.0.0.1:10000/devstoreaccount1": "http://127.0.0.1:10000/devstoreaccount1",
	}
	for in, want := range cases {
		if got := serviceURL(in); got != want {
			t.Errorf("serviceURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithAccountURLName(t *testing.T) {
	p := New(WithAccountURL("acctname"))
	if p.accountURL != "https://acctname.blob.core.windows.net" {
		t.Fatalf("accountURL = %q, want expanded URL", p.accountURL)
	}
}

func TestIsNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("boom"), false},
		{"status404", &azcore.ResponseError{StatusCode: http.StatusNotFound}, true},
		{"status403", &azcore.ResponseError{StatusCode: http.StatusForbidden}, false},
		{"blobNotFoundCode", &azcore.ResponseError{ErrorCode: "BlobNotFound"}, true},
		{"containerNotFoundCode", &azcore.ResponseError{ErrorCode: "ContainerNotFound"}, true},
	}
	for _, tc := range cases {
		if got := isNotFound(tc.err); got != tc.want {
			t.Errorf("isNotFound(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

const confContainer = "conformance"

func TestConformance(t *testing.T) {
	fake := newFakeStore()
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return New(WithClient(fake)) },
		Ref: func(key string) string { return "azblob://" + confContainer + "/" + key },
		Seed: func(_ context.Context, key, val string) error {
			fake.put(confContainer, key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			fake.put(confContainer, key, val)
			return nil
		},
		SkipWatch: true,
	})
}
