package cosmos

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

// fakeStore is an in-memory itemReader. Items are keyed by
// "<database>/<container>/<id>", and every put bumps a revision counter so the
// ETag returned for an item changes on each mutation - mimicking Cosmos, whose
// ETag advances on every write.
type fakeStore struct {
	mu    sync.Mutex
	items map[string]*fakeItem

	// noETag makes ReadItem return an empty ETag, exercising the "_etag" /
	// content-hash version fallbacks.
	noETag bool

	// lastPK records the partition key value of the most recent ReadItem, so
	// tests can assert the ?pk option (and the id default) are wired through.
	lastPK string
}

type fakeItem struct {
	body []byte
	n    int
}

func newFakeStore() *fakeStore { return &fakeStore{items: map[string]*fakeItem{}} }

func key(database, container, id string) string {
	return database + "/" + container + "/" + id
}

func (f *fakeStore) put(database, container, id, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(database, container, id)
	it := f.items[k]
	if it == nil {
		it = &fakeItem{}
		f.items[k] = it
	}
	it.body = []byte(val)
	it.n++
}

func (f *fakeStore) ReadItem(ctx context.Context, database, container, id, partitionKey string) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPK = partitionKey
	it, ok := f.items[key(database, container, id)]
	if !ok {
		return nil, "", mamori.ErrNotFound
	}
	etag := ""
	if !f.noETag {
		etag = fmt.Sprintf("\"%d\"", it.n)
	}
	return append([]byte(nil), it.body...), etag, nil
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

func TestResolveWholeDocument(t *testing.T) {
	fake := newFakeStore()
	fake.put("appdb", "settings", "app", `{"level":"info","_etag":"\"abc\""}`)
	p := New(WithClient(fake))

	v, err := p.Resolve(context.Background(), mustParse(t, "cosmos://appdb/settings/app"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != `{"level":"info","_etag":"\"abc\""}` {
		t.Fatalf("Bytes = %q, want the document JSON", v.Bytes)
	}
	if v.Version == "" {
		t.Error("Value.Version is empty")
	}
	if v.Sensitive {
		t.Error("Value.Sensitive = true, want false by default")
	}
	if v.Metadata["database"] != "appdb" || v.Metadata["container"] != "settings" || v.Metadata["id"] != "app" {
		t.Errorf("Metadata = %v, want database=appdb container=settings id=app", v.Metadata)
	}
}

func TestResolveJSONKey(t *testing.T) {
	fake := newFakeStore()
	fake.put("appdb", "conn", "prod", `{"username":"admin","password":"hunter2"}`)
	p := New(WithClient(fake))

	v, err := p.Resolve(context.Background(), mustParse(t, "cosmos://appdb/conn/prod#password"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hunter2" {
		t.Fatalf("selected key = %q, want hunter2", v.Bytes)
	}
}

func TestResolveJSONKeyMissing(t *testing.T) {
	fake := newFakeStore()
	fake.put("appdb", "conn", "prod", `{"username":"admin"}`)
	p := New(WithClient(fake))

	_, err := p.Resolve(context.Background(), mustParse(t, "cosmos://appdb/conn/prod#password"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing key error = %v, want ErrNotFound", err)
	}
}

func TestResolveSensitive(t *testing.T) {
	fake := newFakeStore()
	fake.put("appdb", "secrets", "token", "s3cr3t")
	p := New(WithClient(fake), WithSensitive(true))

	v, err := p.Resolve(context.Background(), mustParse(t, "cosmos://appdb/secrets/token"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Error("Value.Sensitive = false, want true with WithSensitive(true)")
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(WithClient(newFakeStore()))
	_, err := p.Resolve(context.Background(), mustParse(t, "cosmos://appdb/settings/nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing item error = %v, want ErrNotFound", err)
	}
}

func TestResolveBadRef(t *testing.T) {
	p := New(WithClient(newFakeStore()))
	for _, raw := range []string{
		"cosmos://onlydb",        // no container/id
		"cosmos://db/container",  // no id
		"cosmos:///container/id", // no database
	} {
		_, err := p.Resolve(context.Background(), mustParse(t, raw))
		if err == nil {
			t.Errorf("Resolve(%q) = nil error, want a malformed-ref error", raw)
		} else if errors.Is(err, mamori.ErrNotFound) {
			t.Errorf("Resolve(%q) returned ErrNotFound; a malformed ref is not not-found", raw)
		}
	}
}

func TestPartitionKeyDefaultsToID(t *testing.T) {
	fake := newFakeStore()
	fake.put("appdb", "settings", "app", "x")
	p := New(WithClient(fake))

	if _, err := p.Resolve(context.Background(), mustParse(t, "cosmos://appdb/settings/app")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if fake.lastPK != "app" {
		t.Fatalf("partition key = %q, want the id %q by default", fake.lastPK, "app")
	}
}

func TestPartitionKeyOption(t *testing.T) {
	fake := newFakeStore()
	fake.put("appdb", "settings", "app", "x")
	p := New(WithClient(fake))

	if _, err := p.Resolve(context.Background(), mustParse(t, "cosmos://appdb/settings/app?pk=tenant-7")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if fake.lastPK != "tenant-7" {
		t.Fatalf("partition key = %q, want ?pk value %q", fake.lastPK, "tenant-7")
	}
}

func TestVersionFromResponseETag(t *testing.T) {
	fake := newFakeStore()
	fake.put("appdb", "settings", "app", "one")
	p := New(WithClient(fake))
	ref := mustParse(t, "cosmos://appdb/settings/app")

	v1, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	fake.put("appdb", "settings", "app", "two")
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

func TestVersionFromDocETag(t *testing.T) {
	fake := newFakeStore()
	fake.noETag = true // force the response ETag empty -> use the document _etag
	fake.put("appdb", "settings", "app", `{"level":"info","_etag":"\"docrev1\""}`)
	p := New(WithClient(fake))

	v, err := p.Resolve(context.Background(), mustParse(t, "cosmos://appdb/settings/app"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Version != `"docrev1"` {
		t.Fatalf("Version = %q, want the document _etag %q", v.Version, `"docrev1"`)
	}
}

func TestVersionFallsBackToHash(t *testing.T) {
	fake := newFakeStore()
	fake.noETag = true // no response ETag and no _etag in the body -> content hash
	fake.put("appdb", "settings", "app", "plain-value")
	p := New(WithClient(fake))

	v, err := p.Resolve(context.Background(), mustParse(t, "cosmos://appdb/settings/app"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Version != mamori.VersionHash([]byte("plain-value")) {
		t.Fatalf("Version = %q, want the content hash", v.Version)
	}
}

func TestResolveContextCancelled(t *testing.T) {
	fake := newFakeStore()
	fake.put("appdb", "settings", "app", "x")
	p := New(WithClient(fake))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustParse(t, "cosmos://appdb/settings/app")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestNoAccountConfigured(t *testing.T) {
	t.Setenv(EndpointEnv, "")         // ensure no ambient config leaks in
	t.Setenv(ConnectionStringEnv, "") //
	p := New()                        // no fixed client, no endpoint, no connection string
	_, err := p.Resolve(context.Background(), mustParse(t, "cosmos://appdb/settings/app"))
	if err == nil {
		t.Fatal("Resolve with no account configured returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("missing-account error must not be ErrNotFound")
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
		{"wrapped404", fmt.Errorf("read: %w", &azcore.ResponseError{StatusCode: http.StatusNotFound}), true},
	}
	for _, tc := range cases {
		if got := isNotFound(tc.err); got != tc.want {
			t.Errorf("isNotFound(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

const (
	confDatabase  = "conformance"
	confContainer = "items"
)

func TestConformance(t *testing.T) {
	fake := newFakeStore()
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return New(WithClient(fake)) },
		Ref: func(k string) string {
			return "cosmos://" + confDatabase + "/" + confContainer + "/" + k
		},
		Seed: func(_ context.Context, k, val string) error {
			fake.put(confDatabase, confContainer, k, val)
			return nil
		},
		Mutate: func(_ context.Context, k, val string) error {
			fake.put(confDatabase, confContainer, k, val)
			return nil
		},
		SkipWatch: true,
	})
}
