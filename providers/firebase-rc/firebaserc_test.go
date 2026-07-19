package firebaserc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeBackend is an in-memory templateFetcher that models a server Remote Config
// template. Each set bumps the template version, mirroring the way publishing a
// new template increments its versionNumber.
type fakeBackend struct {
	mu      sync.Mutex
	params  map[string]string
	version int
	fetches int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{params: map[string]string{}}
}

func (f *fakeBackend) set(key, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.params[key] = val
	f.version++
}

func (f *fakeBackend) fetchTemplate(ctx context.Context) (*template, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches++
	params := make(map[string]parameter, len(f.params))
	for k, v := range f.params {
		params[k] = parameter{value: v, hasValue: true}
	}
	return &template{
		version:    strconv.Itoa(f.version),
		parameters: params,
	}, nil
}

// compile-time assertion that the fake satisfies the fetcher contract.
var _ templateFetcher = (*fakeBackend)(nil)

func parse(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

func TestConformance(t *testing.T) {
	fake := newFakeBackend()
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return New(WithFetcher(fake)) },
		Ref: func(key string) string { return "firebase-rc://" + key },
		Seed: func(_ context.Context, key, val string) error {
			fake.set(key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			fake.set(key, val)
			return nil
		},
		// The server Remote Config template has no cheap native push, so the
		// provider is deliberately not watchable; mamori polls it.
		SkipWatch: true,
	})
}

func TestResolve(t *testing.T) {
	fake := newFakeBackend()
	fake.set("welcome_message", "Hello!")
	p := New(WithFetcher(fake))

	v, err := p.Resolve(context.Background(), parse(t, "firebase-rc://welcome_message"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "Hello!" {
		t.Fatalf("Bytes = %q, want Hello!", v.Bytes)
	}
	if v.Sensitive {
		t.Error("Sensitive = true, want false (Remote Config parameters are not secrets)")
	}
	if v.Version == "" {
		t.Error("Version is empty; want the template version number")
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(WithFetcher(newFakeBackend()))
	_, err := p.Resolve(context.Background(), parse(t, "firebase-rc://nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveJSONKey(t *testing.T) {
	fake := newFakeBackend()
	fake.set("feature_flags", `{"new_ui":true,"max_items":25,"label":"beta"}`)
	p := New(WithFetcher(fake))

	// string field is returned unquoted
	v, err := p.Resolve(context.Background(), parse(t, "firebase-rc://feature_flags#label"))
	if err != nil {
		t.Fatalf("Resolve #label: %v", err)
	}
	if string(v.Bytes) != "beta" {
		t.Fatalf("Bytes = %q, want beta", v.Bytes)
	}

	// non-string field is returned as its JSON encoding
	v, err = p.Resolve(context.Background(), parse(t, "firebase-rc://feature_flags#max_items"))
	if err != nil {
		t.Fatalf("Resolve #max_items: %v", err)
	}
	if string(v.Bytes) != "25" {
		t.Fatalf("Bytes = %q, want 25", v.Bytes)
	}

	// absent key maps to ErrNotFound
	_, err = p.Resolve(context.Background(), parse(t, "firebase-rc://feature_flags#missing"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing key err = %v, want ErrNotFound", err)
	}
}

func TestResolveVersionChangesOnMutate(t *testing.T) {
	fake := newFakeBackend()
	fake.set("k", "one")
	p := New(WithFetcher(fake))

	v1, err := p.Resolve(context.Background(), parse(t, "firebase-rc://k"))
	if err != nil {
		t.Fatal(err)
	}
	fake.set("k", "two")
	v2, err := p.Resolve(context.Background(), parse(t, "firebase-rc://k"))
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version == v2.Version {
		t.Fatalf("Version did not change after mutate (both %q)", v1.Version)
	}
	if string(v2.Bytes) != "two" {
		t.Fatalf("post-mutate Bytes = %q, want two", v2.Bytes)
	}
}

func TestResolveContextCancelled(t *testing.T) {
	fake := newFakeBackend()
	fake.set("k", "v")
	p := New(WithFetcher(fake))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, parse(t, "firebase-rc://k")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestScheme(t *testing.T) {
	if s := New(WithFetcher(newFakeBackend())).Scheme(); s != "firebase-rc" {
		t.Fatalf("Scheme() = %q, want firebase-rc", s)
	}
}

func TestRegistered(t *testing.T) {
	found := false
	for _, s := range mamori.RegisteredSchemes() {
		if s == "firebase-rc" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("firebase-rc not registered by init()")
	}
}

// TestDecodeTemplate exercises the REST response decoding directly, including
// the in-app-default (no server value) case.
func TestDecodeTemplate(t *testing.T) {
	body := []byte(`{
		"parameters": {
			"welcome":   {"defaultValue": {"value": "hi"}},
			"empty":     {"defaultValue": {"value": ""}},
			"in_app":    {"defaultValue": {"useInAppDefault": true}},
			"no_default": {"description": "conditional-only"}
		},
		"version": {"versionNumber": "42"}
	}`)
	tmpl, err := decodeTemplate(body)
	if err != nil {
		t.Fatalf("decodeTemplate: %v", err)
	}
	if tmpl.version != "42" {
		t.Fatalf("version = %q, want 42", tmpl.version)
	}
	if p := tmpl.parameters["welcome"]; !p.hasValue || p.value != "hi" {
		t.Fatalf("welcome = %+v, want {hi true}", p)
	}
	if p := tmpl.parameters["empty"]; !p.hasValue || p.value != "" {
		t.Fatalf("empty = %+v, want a present empty value", p)
	}
	if p := tmpl.parameters["in_app"]; p.hasValue {
		t.Fatalf("in_app hasValue = true, want false (in-app default)")
	}
	if p := tmpl.parameters["no_default"]; p.hasValue {
		t.Fatalf("no_default hasValue = true, want false (no server value)")
	}
}

// TestHTTPFetcher exercises the real REST fetch path against an httptest server,
// covering the endpoint URL, JSON decoding, version reporting, and the
// in-app-default -> ErrNotFound mapping (auth aside, via an injected client).
func TestHTTPFetcher(t *testing.T) {
	const body = `{"parameters":{"welcome":{"defaultValue":{"value":"hi"}},"legacy":{"defaultValue":{"useInAppDefault":true}}},"version":{"versionNumber":"7"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/demo/remoteConfig" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	p := New(
		WithProjectID("demo"),
		WithHTTPClient(srv.Client()),
		WithBaseURL(srv.URL),
	)
	t.Cleanup(func() { _ = p.Close() })

	v, err := p.Resolve(context.Background(), parse(t, "firebase-rc://welcome"))
	if err != nil {
		t.Fatalf("Resolve welcome: %v", err)
	}
	if string(v.Bytes) != "hi" {
		t.Fatalf("Bytes = %q, want hi", v.Bytes)
	}
	if v.Version != "7" {
		t.Fatalf("Version = %q, want 7", v.Version)
	}
	if v.Sensitive {
		t.Error("Sensitive = true, want false")
	}

	// in-app-default parameter -> ErrNotFound
	if _, err := p.Resolve(context.Background(), parse(t, "firebase-rc://legacy")); !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("legacy err = %v, want ErrNotFound", err)
	}

	// unknown parameter -> ErrNotFound
	if _, err := p.Resolve(context.Background(), parse(t, "firebase-rc://ghost")); !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("ghost err = %v, want ErrNotFound", err)
	}
}

// TestHTTPFetcherErrorStatus verifies a non-200 API response surfaces as a
// non-ErrNotFound error.
func TestHTTPFetcherErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"status":"PERMISSION_DENIED"}}`, http.StatusForbidden)
	}))
	defer srv.Close()

	p := New(WithProjectID("demo"), WithHTTPClient(srv.Client()), WithBaseURL(srv.URL))
	_, err := p.Resolve(context.Background(), parse(t, "firebase-rc://welcome"))
	if err == nil {
		t.Fatal("expected an error for a 403 response")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("403 err should not be ErrNotFound, got %v", err)
	}
}

// TestMissingProjectID verifies buildFetcher fails cleanly when no project ID is
// available (and an HTTP client is injected so ADC is not consulted).
func TestMissingProjectID(t *testing.T) {
	p := New(WithHTTPClient(&http.Client{}))
	_, err := p.Resolve(context.Background(), parse(t, "firebase-rc://welcome"))
	if err == nil {
		t.Fatal("expected an error when no project ID is configured")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing-project-ID err should not be ErrNotFound, got %v", err)
	}
}

// TestLazyFetcherCached verifies the fetcher is built once and cached across
// resolves.
func TestLazyFetcherCached(t *testing.T) {
	fake := newFakeBackend()
	fake.set("k", "v")
	p := New(WithFetcher(fake))

	for i := 0; i < 3; i++ {
		if _, err := p.Resolve(context.Background(), parse(t, "firebase-rc://k")); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	if fake.fetches != 3 {
		t.Fatalf("fetches = %d, want 3 (one per resolve)", fake.fetches)
	}
}
