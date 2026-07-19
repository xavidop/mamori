package growthbook

import (
	"context"
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

// fakeEvaluator is an in-memory evaluator that models a GrowthBook feature set.
// Un-seeded features report found=false, exactly as the SDK reports an unknown
// feature, so the provider maps them to mamori.ErrNotFound.
type fakeEvaluator struct {
	mu     sync.Mutex
	values map[string]any
}

func newFakeEvaluator() *fakeEvaluator {
	return &fakeEvaluator{values: map[string]any{}}
}

func (f *fakeEvaluator) set(key string, val any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = val
}

func (f *fakeEvaluator) evaluateFeature(ctx context.Context, key string) (any, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[key]
	return v, ok, nil
}

var _ evaluator = (*fakeEvaluator)(nil)

func parse(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

func TestConformance(t *testing.T) {
	fake := newFakeEvaluator()
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return New(withEvaluator(fake)) },
		Ref: func(key string) string { return "growthbook://" + key },
		Seed: func(_ context.Context, key, val string) error {
			// Seed a string so encodeFeatureValue returns the raw value the
			// conformance kit compares against.
			fake.set(key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			fake.set(key, val)
			return nil
		},
		// GrowthBook has an SSE endpoint, but this provider is intentionally not
		// watchable; mamori polls it. Skip the watch conformance tests.
		SkipWatch: true,
	})
}

func TestScheme(t *testing.T) {
	if s := New(withEvaluator(newFakeEvaluator())).Scheme(); s != "growthbook" {
		t.Fatalf("Scheme() = %q, want growthbook", s)
	}
}

func TestRegistered(t *testing.T) {
	found := false
	for _, s := range mamori.RegisteredSchemes() {
		if s == "growthbook" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("growthbook not registered by init()")
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(withEvaluator(newFakeEvaluator()))
	_, err := p.Resolve(context.Background(), parse(t, "growthbook://nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveEmptyPath(t *testing.T) {
	p := New(withEvaluator(newFakeEvaluator()))
	_, err := p.Resolve(context.Background(), parse(t, "growthbook://"))
	if err == nil {
		t.Fatal("expected an error for an empty feature key")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("empty-key err should not be ErrNotFound, got %v", err)
	}
}

func TestResolveContextCancelled(t *testing.T) {
	fake := newFakeEvaluator()
	fake.set("k", "v")
	p := New(withEvaluator(fake))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, parse(t, "growthbook://k")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

// TestEncodeFeatureValue covers the value-type encoding contract directly.
func TestEncodeFeatureValue(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"bool-true", true, "true"},
		{"bool-false", false, "false"},
		{"string", "hello", "hello"},
		{"empty-string", "", ""},
		{"float-int", float64(25), "25"},
		{"float-frac", float64(3.5), "3.5"},
		{"int", 7, "7"},
		{"object", map[string]any{"a": float64(1)}, `{"a":1}`},
		{"array", []any{float64(1), "x"}, `[1,"x"]`},
		{"null", nil, "null"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := encodeFeatureValue(c.in)
			if err != nil {
				t.Fatalf("encodeFeatureValue(%v): %v", c.in, err)
			}
			if string(got) != c.want {
				t.Fatalf("encodeFeatureValue(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestResolveValueTypes exercises Resolve end-to-end through the fake for each
// value type, plus JSON #key selection and Sensitive/Version invariants.
func TestResolveValueTypes(t *testing.T) {
	fake := newFakeEvaluator()
	fake.set("flag_on", true)
	fake.set("greeting", "hi there")
	fake.set("max_items", float64(25))
	fake.set("config", map[string]any{"new_ui": true, "label": "beta"})

	ctx := context.Background()
	p := New(withEvaluator(fake))

	check := func(raw, want string) {
		t.Helper()
		v, err := p.Resolve(ctx, parse(t, raw))
		if err != nil {
			t.Fatalf("Resolve(%q): %v", raw, err)
		}
		if string(v.Bytes) != want {
			t.Fatalf("Resolve(%q) = %q, want %q", raw, v.Bytes, want)
		}
		if v.Sensitive {
			t.Errorf("Resolve(%q) Sensitive = true, want false (feature flags are not secrets)", raw)
		}
		if v.Version == "" {
			t.Errorf("Resolve(%q) Version is empty", raw)
		}
	}

	check("growthbook://flag_on", "true")
	check("growthbook://greeting", "hi there")
	check("growthbook://max_items", "25")
	check("growthbook://config", `{"label":"beta","new_ui":true}`)
	// #json-key selection over a JSON-object feature value.
	check("growthbook://config#new_ui", "true")
	check("growthbook://config#label", "beta")

	// Absent JSON key -> ErrNotFound.
	if _, err := p.Resolve(ctx, parse(t, "growthbook://config#missing")); !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing #key err = %v, want ErrNotFound", err)
	}
}

func TestResolveVersionChangesOnMutate(t *testing.T) {
	fake := newFakeEvaluator()
	fake.set("k", "one")
	p := New(withEvaluator(fake))

	v1, err := p.Resolve(context.Background(), parse(t, "growthbook://k"))
	if err != nil {
		t.Fatal(err)
	}
	fake.set("k", "two")
	v2, err := p.Resolve(context.Background(), parse(t, "growthbook://k"))
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

// TestOfflineFeatures exercises the real SDK evaluation path with an offline
// feature set supplied as JSON (WithFeatures), covering value decoding by the
// SDK (bool, string, number, object) and the unknown-feature -> ErrNotFound
// mapping - all without any network access.
func TestOfflineFeatures(t *testing.T) {
	const features = `{
		"dark_mode":    {"defaultValue": true},
		"welcome":      {"defaultValue": "Hello!"},
		"max_items":    {"defaultValue": 25},
		"feature_flags":{"defaultValue": {"new_ui": true, "label": "beta"}}
	}`
	ctx := context.Background()
	p := New(WithFeatures(features))
	t.Cleanup(func() { _ = p.Close() })

	cases := []struct{ raw, want string }{
		{"growthbook://dark_mode", "true"},
		{"growthbook://welcome", "Hello!"},
		{"growthbook://max_items", "25"},
		{"growthbook://feature_flags#new_ui", "true"},
		{"growthbook://feature_flags#label", "beta"},
	}
	for _, c := range cases {
		v, err := p.Resolve(ctx, parse(t, c.raw))
		if err != nil {
			t.Fatalf("Resolve(%q): %v", c.raw, err)
		}
		if string(v.Bytes) != c.want {
			t.Fatalf("Resolve(%q) = %q, want %q", c.raw, v.Bytes, c.want)
		}
		if v.Sensitive {
			t.Errorf("Resolve(%q) Sensitive = true, want false", c.raw)
		}
		if v.Version == "" {
			t.Errorf("Resolve(%q) Version empty", c.raw)
		}
	}

	if _, err := p.Resolve(ctx, parse(t, "growthbook://unknown_feature")); !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("unknown feature err = %v, want ErrNotFound", err)
	}
}

// TestAPIFetch exercises the real SDK API-backed path against an httptest server
// serving a GrowthBook Features API response, covering the fetch URL, JSON
// decoding, evaluation, and unknown-feature -> ErrNotFound mapping (auth aside).
func TestAPIFetch(t *testing.T) {
	const clientKey = "sdk-test-key"
	const body = `{"status":200,"features":{"welcome":{"defaultValue":"hi"},"count":{"defaultValue":42}}}`

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	ctx := context.Background()
	p := New(
		WithClientKey(clientKey),
		WithAPIHost(srv.URL),
		WithHTTPClient(srv.Client()),
	)
	t.Cleanup(func() { _ = p.Close() })

	v, err := p.Resolve(ctx, parse(t, "growthbook://welcome"))
	if err != nil {
		t.Fatalf("Resolve welcome: %v", err)
	}
	if string(v.Bytes) != "hi" {
		t.Fatalf("Bytes = %q, want hi", v.Bytes)
	}
	if want := "/api/features/" + clientKey; gotPath != want {
		t.Fatalf("fetched path = %q, want %q", gotPath, want)
	}

	v, err = p.Resolve(ctx, parse(t, "growthbook://count"))
	if err != nil {
		t.Fatalf("Resolve count: %v", err)
	}
	if string(v.Bytes) != "42" {
		t.Fatalf("Bytes = %q, want 42", v.Bytes)
	}

	if _, err := p.Resolve(ctx, parse(t, "growthbook://ghost")); !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("ghost err = %v, want ErrNotFound", err)
	}
}

// TestAPIFetchErrorStatus verifies a non-200 API response surfaces as a
// non-ErrNotFound error.
func TestAPIFetchErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `forbidden`, http.StatusForbidden)
	}))
	defer srv.Close()

	p := New(WithClientKey("k"), WithAPIHost(srv.URL), WithHTTPClient(srv.Client()))
	t.Cleanup(func() { _ = p.Close() })

	_, err := p.Resolve(context.Background(), parse(t, "growthbook://welcome"))
	if err == nil {
		t.Fatal("expected an error for a 403 response")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("403 err should not be ErrNotFound, got %v", err)
	}
}

// TestMissingClientKey verifies buildEvaluator fails cleanly when neither a
// client key nor an offline feature set is configured.
func TestMissingClientKey(t *testing.T) {
	p := New()
	_, err := p.Resolve(context.Background(), parse(t, "growthbook://welcome"))
	if err == nil {
		t.Fatal("expected an error when no client key or features are configured")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing-config err should not be ErrNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "client key") {
		t.Fatalf("err = %v, want a message mentioning the missing client key", err)
	}
}
