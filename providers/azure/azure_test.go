package azure

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeVault is an in-memory kvClient. Each set appends a new version, so the
// version string returned for the latest secret changes on every mutation.
type fakeVault struct {
	mu    sync.Mutex
	data  map[string][]string // secret name -> ordered version values (1-based)
	fails map[string]error    // secret name -> injected error
}

func newFakeVault() *fakeVault {
	return &fakeVault{data: map[string][]string{}, fails: map[string]error{}}
}

func (f *fakeVault) set(name, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[name] = append(f.data[name], val)
}

// fail makes the next GetSecret for name return err, until clear(name) is
// called. It powers the providertest ErrorClassification case. name must use
// the same key form as set, so the conformance case's Seed and Fail hit the
// same entry.
func (f *fakeVault) fail(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[name] = err
}

// clear cancels a previously injected fail(name, err).
func (f *fakeVault) clear(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fails, name)
}

func (f *fakeVault) GetSecret(ctx context.Context, name, version string, _ *azsecrets.GetSecretOptions) (azsecrets.GetSecretResponse, error) {
	if err := ctx.Err(); err != nil {
		return azsecrets.GetSecretResponse{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.fails[name]; ok {
		return azsecrets.GetSecretResponse{}, err
	}

	vals, ok := f.data[name]
	if !ok || len(vals) == 0 {
		return azsecrets.GetSecretResponse{}, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: "SecretNotFound"}
	}

	idx := len(vals) - 1 // latest
	if version != "" {
		n, err := strconv.Atoi(version)
		if err != nil || n < 1 || n > len(vals) {
			return azsecrets.GetSecretResponse{}, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: "SecretVersionNotFound"}
		}
		idx = n - 1
	}

	verStr := strconv.Itoa(idx + 1)
	id := azsecrets.ID("https://fakevault.vault.azure.net/secrets/" + name + "/" + verStr)
	val := vals[idx]
	return azsecrets.GetSecretResponse{
		Secret: azsecrets.Secret{ID: &id, Value: &val},
	}, nil
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
	if got := New(WithClient(newFakeVault())).Scheme(); got != Scheme {
		t.Fatalf("Scheme() = %q, want %q", got, Scheme)
	}
}

func TestRegisteredSchemes(t *testing.T) {
	got := map[string]bool{}
	for _, s := range mamori.RegisteredSchemes() {
		got[s] = true
	}
	for _, want := range []string{Scheme, SchemeAppConfig} {
		if !got[want] {
			t.Errorf("scheme %q was not registered by init()", want)
		}
	}
}

func TestConstructorSchemes(t *testing.T) {
	if s := New().Scheme(); s != Scheme {
		t.Errorf("Provider.Scheme() = %q, want %q", s, Scheme)
	}
	if s := NewAppConfig().Scheme(); s != SchemeAppConfig {
		t.Errorf("AppConfigProvider.Scheme() = %q, want %q", s, SchemeAppConfig)
	}
}

func TestResolve(t *testing.T) {
	fake := newFakeVault()
	fake.set("db-password", "s3cr3t")
	p := New(WithClient(fake))

	v, err := p.Resolve(context.Background(), mustParse(t, "azure-kv://myvault/db-password"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "s3cr3t" {
		t.Fatalf("Bytes = %q, want s3cr3t", v.Bytes)
	}
	if !v.Sensitive {
		t.Error("Value.Sensitive = false, want true for a Key Vault secret")
	}
	if v.Version == "" {
		t.Error("Value.Version is empty")
	}
	if v.Metadata["vault"] != "myvault" {
		t.Errorf("Metadata[vault] = %q, want myvault", v.Metadata["vault"])
	}
}

func TestResolveJSONKey(t *testing.T) {
	fake := newFakeVault()
	fake.set("conn", `{"username":"admin","password":"hunter2"}`)
	p := New(WithClient(fake))

	v, err := p.Resolve(context.Background(), mustParse(t, "azure-kv://myvault/conn#password"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hunter2" {
		t.Fatalf("selected key = %q, want hunter2", v.Bytes)
	}
}

func TestResolveJSONKeyMissing(t *testing.T) {
	fake := newFakeVault()
	fake.set("conn", `{"username":"admin"}`)
	p := New(WithClient(fake))

	_, err := p.Resolve(context.Background(), mustParse(t, "azure-kv://myvault/conn#password"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing key error = %v, want ErrNotFound", err)
	}
}

func TestResolveVersionPin(t *testing.T) {
	fake := newFakeVault()
	fake.set("api-key", "v1-value")
	fake.set("api-key", "v2-value")
	p := New(WithClient(fake))

	// Latest.
	latest, err := p.Resolve(context.Background(), mustParse(t, "azure-kv://myvault/api-key"))
	if err != nil {
		t.Fatalf("Resolve latest: %v", err)
	}
	if string(latest.Bytes) != "v2-value" {
		t.Fatalf("latest = %q, want v2-value", latest.Bytes)
	}

	// Pinned to version 1.
	pinned, err := p.Resolve(context.Background(), mustParse(t, "azure-kv://myvault/api-key?version=1"))
	if err != nil {
		t.Fatalf("Resolve pinned: %v", err)
	}
	if string(pinned.Bytes) != "v1-value" {
		t.Fatalf("pinned = %q, want v1-value", pinned.Bytes)
	}
	if latest.Version == pinned.Version {
		t.Errorf("latest and pinned share version %q; want distinct", latest.Version)
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(WithClient(newFakeVault()))
	_, err := p.Resolve(context.Background(), mustParse(t, "azure-kv://myvault/nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing secret error = %v, want ErrNotFound", err)
	}
}

func TestResolveBadRef(t *testing.T) {
	p := New(WithClient(newFakeVault()))
	for _, raw := range []string{
		"azure-kv://onlyvault",   // no secret
		"azure-kv:///secretonly", // no vault
	} {
		if _, err := p.Resolve(context.Background(), mustParse(t, raw)); err == nil {
			t.Errorf("Resolve(%q) = nil error, want a malformed-ref error", raw)
		} else if errors.Is(err, mamori.ErrNotFound) {
			t.Errorf("Resolve(%q) returned ErrNotFound; a malformed ref is not not-found", raw)
		}
	}
}

func TestResolveContextCancelled(t *testing.T) {
	fake := newFakeVault()
	fake.set("k", "v")
	p := New(WithClient(fake))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustParse(t, "azure-kv://myvault/k")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestConformance(t *testing.T) {
	fake := newFakeVault()
	providertest.Run(t, providertest.Config{
		New:        func() mamori.Provider { return New(WithClient(fake)) },
		Ref:        func(key string) string { return "azure-kv://testvault/" + key },
		PointerRef: func(key, frag string) string { return "azure-kv://testvault/" + key + frag },
		Seed: func(_ context.Context, key, val string) error {
			fake.set(key, val)
			return nil
		},
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
	})
}

// TestResolveClassifiesNonNotFoundError exercises classifyAzure through
// Resolve itself, not just as a direct function call. The conformance
// ErrorClassification case cannot catch a regression here: it injects a
// mamori sentinel directly (not an *azcore.ResponseError), so it passes even
// if the classifyAzure call were deleted from Resolve's fallback branch. This
// test injects a real *azcore.ResponseError through fakeVault, the same shape
// a live Key Vault call would return, so it fails if the wiring is removed.
func TestResolveClassifiesNonNotFoundError(t *testing.T) {
	fake := newFakeVault()
	fake.set("db", "s3cr3t")
	fake.fail("db", &azcore.ResponseError{StatusCode: http.StatusForbidden, ErrorCode: "Forbidden"})
	p := New(WithClient(fake))

	_, err := p.Resolve(context.Background(), mustParse(t, "azure-kv://myvault/db"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyAzure may not be wired into Resolve", got, mamori.KindPermissionDenied)
	}
}
