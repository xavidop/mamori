package doppler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	fails    map[string]error  // key: project/config/name -> injected transport error
	lastAuth string            // last Authorization header seen
}

func storeKey(project, config, name string) string {
	return project + "/" + config + "/" + name
}

func newFake() *fakeDoppler {
	return &fakeDoppler{store: map[string]string{}, fails: map[string]error{}}
}

func (f *fakeDoppler) set(project, config, name, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[storeKey(project, config, name)] = val
}

// fail makes the next request for project/config/name return err as a raw
// transport failure (the RoundTripper returns it before the handler ever
// runs), until clear(project, config, name) is called. It powers the
// providertest ErrorClassification case.
func (f *fakeDoppler) fail(project, config, name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[storeKey(project, config, name)] = err
}

// clear cancels a previously injected fail(project, config, name, err).
func (f *fakeDoppler) clear(project, config, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fails, storeKey(project, config, name))
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
		WithHTTPClient(f.failClient()),
	)
}

// failClient behaves like inProcessClient (in-process, no sockets, honors
// context cancellation), but first checks fails for an injected transport
// error keyed by the requested project/config/name and, when present,
// returns it directly instead of invoking the handler.
//
// This is the seam the providertest ErrorClassification case and
// TestResolveInjectedSentinelSurvives exercise: http.Client.Do wraps a
// RoundTripper error in *url.Error, which is Unwrap-compatible, and
// doppler.go's Resolve returns that error verbatim on a transport failure
// (it never touches an HTTP response at all), so an injected mamori sentinel
// survives errors.Is unchanged. This is a separate path from
// classifyDopplerStatus, which only classifies a real HTTP status on a real
// response; both must work independently.
func (f *fakeDoppler) failClient() *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if err := req.Context().Err(); err != nil {
				return nil, err
			}
			q := req.URL.Query()
			key := storeKey(q.Get("project"), q.Get("config"), q.Get("name"))
			f.mu.Lock()
			err, failing := f.fails[key]
			f.mu.Unlock()
			if failing {
				return nil, err
			}
			rec := httptest.NewRecorder()
			f.handle(rec, req)
			return rec.Result(), nil
		}),
	}
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
		// No PointerRef: #<name> is the secret's name, a required, backend-native
		// identifier (see Resolve), not a path into a JSON document.
		Seed: func(_ context.Context, key, val string) error {
			f.set(testProject, testConfig, key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set(testProject, testConfig, key, val)
			return nil
		},
		Fail: func(_ context.Context, key string, err error) error {
			f.fail(testProject, testConfig, key, err)
			return nil
		},
		Clear: func(_ context.Context, key string) error {
			f.clear(testProject, testConfig, key)
			return nil
		},
		SkipWatch: true, // Doppler has no native change notification.
	})
}

// TestResolveInjectedSentinelSurvives exercises the RoundTripper injection
// seam directly (not just through providertest's ErrorClassification case,
// which would still pass even if Resolve's transport-error branch were
// broken, as long as it returned some non-nil error unmodified). It injects
// mamori.ErrPermissionDenied as a raw transport failure - the RoundTripper
// returns it before fakeDoppler.handle ever runs, so there is no HTTP
// response to classify - and asserts it comes back out of Resolve unchanged
// despite http.Client.Do wrapping it in *url.Error along the way.
func TestResolveInjectedSentinelSurvives(t *testing.T) {
	f := newFake()
	f.set(testProject, testConfig, "K", "v")
	f.fail(testProject, testConfig, "K", mamori.ErrPermissionDenied)

	p := New(WithBaseURL("http://doppler.test"), WithToken(testToken), WithHTTPClient(f.failClient()))
	_, err := p.Resolve(context.Background(), mustRef(t, "doppler://"+testProject+"/"+testConfig+"#K"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the transport was failing")
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrPermissionDenied)", err)
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q", got, mamori.KindPermissionDenied)
	}
}

// TestResolveClassifiesRealStatus exercises classifyDopplerStatus through
// Resolve itself, using a real HTTP 403 response from a handler (not an
// injected transport error). Neither the conformance ErrorClassification
// case nor TestResolveInjectedSentinelSurvives can catch a regression in
// Resolve's default-branch wiring: both inject a mamori sentinel directly at
// the RoundTripper, bypassing the HTTP-status path entirely, so they would
// keep passing even if the classifyDopplerStatus call were deleted from
// Resolve. This test fails if that wiring is removed.
func TestResolveClassifiesRealStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"messages":["insufficient permissions"]}`))
	}))
	defer srv.Close()

	p := New(WithBaseURL(srv.URL), WithToken(testToken), WithHTTPClient(srv.Client()))
	_, err := p.Resolve(context.Background(), mustRef(t, "doppler://"+testProject+"/"+testConfig+"#K"))
	if err == nil {
		t.Fatal("Resolve returned a nil error for a 403 response")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyDopplerStatus may not be wired into Resolve", got, mamori.KindPermissionDenied)
	}
}

// TestResolveErrorBodyIsBounded pins errBodyLimit: the diagnostic read in
// Resolve's default status branch must never let a hostile or broken
// upstream put an unbounded response into an error string. Every other test
// in this file uses response bodies far smaller than the bound, so none of
// them would notice if io.LimitReader(resp.Body, errBodyLimit) were replaced
// with resp.Body directly; this one sends a body far larger than the bound
// with a distinctive trailing marker, so an unbounded read would carry the
// marker straight into the returned error.
func TestResolveErrorBodyIsBounded(t *testing.T) {
	const marker = "TAIL_MARKER_MUST_NOT_SURVIVE"
	oversized := strings.Repeat("A", 20000) + marker

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, oversized)
	}))
	defer srv.Close()

	p := New(WithBaseURL(srv.URL), WithToken(testToken), WithHTTPClient(srv.Client()))
	_, err := p.Resolve(context.Background(), mustRef(t, "doppler://"+testProject+"/"+testConfig+"#K"))
	if err == nil {
		t.Fatal("want an error for a 500 response")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error body was not bounded: the trailing marker reached the error text: %v", err)
	}
	if len(err.Error()) > 10000 {
		t.Fatalf("error message is %d bytes long; the diagnostic read must be bounded well below the %d-byte oversized body", len(err.Error()), len(oversized))
	}
}

func mustRef(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}
