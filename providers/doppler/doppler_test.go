package doppler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

const (
	testProject = "backend"
	testConfig  = "prd"
	testToken   = "dp.st.test-token"
)

// fakeDoppler is an in-memory emulation of the Doppler REST API. The same
// http.Handler (handle) backs two transports:
//
//   - the unit tests drive it through a real httptest.Server (matching how a
//     live client would talk to https://api.doppler.com), and
//   - the conformance suite drives it through an in-process RoundTripper.
//
// The in-process transport exists because providertest's NoGoroutineLeak test
// runs goleak.VerifyNone with no ignore options, which a long-lived
// httptest.Server accept goroutine can never satisfy. Both paths exercise the
// identical handler and store, so Seed/Mutate semantics are unchanged.
type fakeDoppler struct {
	mu       sync.Mutex
	store    map[string]string // key: project/config/name -> value
	lastAuth string            // last Authorization header seen
}

func storeKey(project, config, name string) string {
	return project + "/" + config + "/" + name
}

func newFake() *fakeDoppler {
	return &fakeDoppler{store: map[string]string{}}
}

func (f *fakeDoppler) set(project, config, name, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[storeKey(project, config, name)] = val
}

func (f *fakeDoppler) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lastAuth = r.Header.Get("Authorization")
	f.mu.Unlock()

	if r.URL.Path != "/v3/configs/config/secret" {
		http.NotFound(w, r)
		return
	}

	q := r.URL.Query()
	project := q.Get("project")
	config := q.Get("config")
	name := q.Get("name")

	f.mu.Lock()
	val, ok := f.store[storeKey(project, config, name)]
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":  false,
			"messages": []string{"Could not find requested secret"},
		})
		return
	}

	resp := secretResponse{Name: name}
	resp.Value.Raw = val
	resp.Value.Computed = val
	_ = json.NewEncoder(w).Encode(resp)
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// inProcessClient serves requests through h with no sockets and no background
// goroutines, so the conformance goleak check stays clean. It honors context
// cancellation so the ContextCancel conformance test passes.
func inProcessClient(h http.Handler) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if err := req.Context().Err(); err != nil {
				return nil, err
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec.Result(), nil
		}),
	}
}

// inProcessProvider builds a provider whose HTTP client dispatches to f in
// process. Used by the conformance suite.
func (f *fakeDoppler) inProcessProvider() *Provider {
	return New(
		WithBaseURL("http://doppler.test"),
		WithToken(testToken),
		WithHTTPClient(inProcessClient(http.HandlerFunc(f.handle))),
	)
}

// serverProvider spins up a real httptest.Server for f and returns a provider
// pointed at it, plus a cleanup func. Keep-alives are disabled so no client
// connection goroutines linger. Used by unit tests.
func (f *fakeDoppler) serverProvider(t *testing.T) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)

	c := srv.Client()
	if tr, ok := c.Transport.(*http.Transport); ok {
		tr = tr.Clone()
		tr.DisableKeepAlives = true
		c.Transport = tr
	}
	return New(WithBaseURL(srv.URL), WithToken(testToken), WithHTTPClient(c))
}

// --- Unit tests (real httptest.Server) ---

func TestResolveSuccess(t *testing.T) {
	f := newFake()
	f.set(testProject, testConfig, "STRIPE_API_KEY", "sk_live_123")

	p := f.serverProvider(t)
	ref := mustRef(t, "doppler://"+testProject+"/"+testConfig+"#STRIPE_API_KEY")

	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "sk_live_123" {
		t.Fatalf("Bytes = %q, want sk_live_123", v.Bytes)
	}
	if v.Version == "" {
		t.Error("Version is empty")
	}
	if v.Version != mamori.VersionHash(v.Bytes) {
		t.Errorf("Version = %q, want VersionHash of bytes", v.Version)
	}
	if !v.Sensitive {
		t.Error("Sensitive = false, want true for a secret")
	}
	if v.Metadata["project"] != testProject || v.Metadata["config"] != testConfig || v.Metadata["name"] != "STRIPE_API_KEY" {
		t.Errorf("Metadata = %v, missing project/config/name", v.Metadata)
	}
}

func TestResolveComputedPreferred(t *testing.T) {
	// When computed differs from raw (references resolved), computed wins.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var resp secretResponse
		resp.Name = "DB_URL"
		resp.Value.Raw = "${BASE}/db"
		resp.Value.Computed = "postgres://host/db"
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := New(WithBaseURL(srv.URL), WithToken(testToken), WithHTTPClient(srv.Client()))
	v, err := p.Resolve(context.Background(), mustRef(t, "doppler://p/c#DB_URL"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "postgres://host/db" {
		t.Fatalf("Bytes = %q, want computed value", v.Bytes)
	}
}

func TestResolveNotFound(t *testing.T) {
	f := newFake()
	p := f.serverProvider(t)
	ref := mustRef(t, "doppler://"+testProject+"/"+testConfig+"#MISSING")

	_, err := p.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("Resolve of missing secret returned nil error")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

func TestResolveAuthHeader(t *testing.T) {
	f := newFake()
	f.set(testProject, testConfig, "TOKEN", "value")

	p := f.serverProvider(t)
	_, err := p.Resolve(context.Background(), mustRef(t, "doppler://"+testProject+"/"+testConfig+"#TOKEN"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	f.mu.Lock()
	got := f.lastAuth
	f.mu.Unlock()
	if got != "Bearer "+testToken {
		t.Fatalf("Authorization header = %q, want %q", got, "Bearer "+testToken)
	}
}

func TestResolveRequiresKey(t *testing.T) {
	f := newFake()
	p := f.serverProvider(t)
	_, err := p.Resolve(context.Background(), mustRef(t, "doppler://"+testProject+"/"+testConfig))
	if err == nil {
		t.Fatal("Resolve without #SECRET_NAME returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("missing-key error should not be ErrNotFound; it is a malformed ref")
	}
}

func TestResolveBadPath(t *testing.T) {
	f := newFake()
	p := f.serverProvider(t)
	// Only one path segment (no config).
	_, err := p.Resolve(context.Background(), mustRef(t, "doppler://onlyproject#KEY"))
	if err == nil {
		t.Fatal("Resolve with a single path segment returned nil error")
	}
}

func TestResolveMissingToken(t *testing.T) {
	f := newFake()
	f.set(testProject, testConfig, "K", "v")
	t.Setenv("DOPPLER_TOKEN", "")
	// No WithToken and DOPPLER_TOKEN empty.
	p := New(WithBaseURL("http://doppler.test"), WithHTTPClient(inProcessClient(http.HandlerFunc(f.handle))))
	_, err := p.Resolve(context.Background(), mustRef(t, "doppler://"+testProject+"/"+testConfig+"#K"))
	if err == nil {
		t.Fatal("Resolve with no token returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("no-token error should not be ErrNotFound")
	}
}

func TestTokenFromEnv(t *testing.T) {
	f := newFake()
	f.set(testProject, testConfig, "K", "v")
	t.Setenv("DOPPLER_TOKEN", testToken)
	// No WithToken: token must come from the environment lazily at resolve time.
	p := New(WithBaseURL("http://doppler.test"), WithHTTPClient(inProcessClient(http.HandlerFunc(f.handle))))
	v, err := p.Resolve(context.Background(), mustRef(t, "doppler://"+testProject+"/"+testConfig+"#K"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "v" {
		t.Fatalf("Bytes = %q, want v", v.Bytes)
	}
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != "doppler" {
		t.Fatalf("Scheme() = %q, want doppler", got)
	}
}

// The Doppler provider intentionally does NOT implement WatchableProvider (no
// native change notification); mamori polls it instead.
func TestNotWatchable(t *testing.T) {
	var p mamori.Provider = New()
	if _, ok := p.(mamori.WatchableProvider); ok {
		t.Fatal("doppler provider must not implement WatchableProvider (no native watch)")
	}
}

// --- Conformance ---

func TestConformance(t *testing.T) {
	f := newFake()

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return f.inProcessProvider() },
		Ref: func(key string) string {
			return "doppler://" + testProject + "/" + testConfig + "#" + key
		},
		Seed: func(_ context.Context, key, val string) error {
			f.set(testProject, testConfig, key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set(testProject, testConfig, key, val)
			return nil
		},
		SkipWatch: true, // Doppler has no native change notification.
	})
}

func mustRef(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}
