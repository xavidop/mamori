package configcat

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeClient is an in-memory implementation of settingClient. Un-seeded keys are
// simply absent from keys(), which is exactly how the provider decides
// not-found; the fake never fabricates a default value for a missing key.
type fakeClient struct {
	mu     sync.RWMutex
	values map[string]any
}

func newFake() *fakeClient { return &fakeClient{values: map[string]any{}} }

func (f *fakeClient) set(key string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = v
}

func (f *fakeClient) keys() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]string, 0, len(f.values))
	for k := range f.values {
		out = append(out, k)
	}
	return out
}

func (f *fakeClient) value(key string) any {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.values[key]
}

func (f *fakeClient) close() {}

func mustRef(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

// --- Unit tests (in-memory fake) ---

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != Scheme {
		t.Fatalf("Scheme() = %q, want %q", got, Scheme)
	}
}

func TestResolveBool(t *testing.T) {
	f := newFake()
	f.set("isAwesomeFeatureEnabled", true)
	p := newWithClient(f)

	v, err := p.Resolve(context.Background(), mustRef(t, "configcat://isAwesomeFeatureEnabled"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "true" {
		t.Fatalf("Bytes = %q, want true", v.Bytes)
	}
	if v.Version != mamori.VersionHash(v.Bytes) {
		t.Errorf("Version = %q, want VersionHash of bytes", v.Version)
	}
	if v.Sensitive {
		t.Error("Sensitive = true, want false for a feature flag")
	}
	if v.Metadata["key"] != "isAwesomeFeatureEnabled" {
		t.Errorf("Metadata[key] = %q, want the setting key", v.Metadata["key"])
	}
}

func TestResolveFalse(t *testing.T) {
	f := newFake()
	f.set("flag", false)
	p := newWithClient(f)

	v, err := p.Resolve(context.Background(), mustRef(t, "configcat://flag"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "false" {
		t.Fatalf("Bytes = %q, want false", v.Bytes)
	}
}

func TestResolveString(t *testing.T) {
	f := newFake()
	f.set("greeting", "hello world")
	p := newWithClient(f)

	v, err := p.Resolve(context.Background(), mustRef(t, "configcat://greeting"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hello world" {
		t.Fatalf("Bytes = %q, want 'hello world'", v.Bytes)
	}
}

func TestResolveNumbers(t *testing.T) {
	f := newFake()
	f.set("intFlag", 42)
	f.set("floatFlag", 3.14)
	f.set("wholeFloat", 5.0)
	p := newWithClient(f)

	cases := map[string]string{
		"intFlag":    "42",
		"floatFlag":  "3.14",
		"wholeFloat": "5",
	}
	for key, want := range cases {
		v, err := p.Resolve(context.Background(), mustRef(t, "configcat://"+key))
		if err != nil {
			t.Fatalf("Resolve(%q): %v", key, err)
		}
		if string(v.Bytes) != want {
			t.Errorf("Resolve(%q) = %q, want %q", key, v.Bytes, want)
		}
	}
}

func TestResolveNotFound(t *testing.T) {
	f := newFake()
	f.set("present", true)
	p := newWithClient(f)

	_, err := p.Resolve(context.Background(), mustRef(t, "configcat://missing"))
	if err == nil {
		t.Fatal("Resolve of missing setting returned nil error")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

func TestResolveEmptyKey(t *testing.T) {
	f := newFake()
	p := newWithClient(f)

	_, err := p.Resolve(context.Background(), mustRef(t, "configcat://"))
	if err == nil {
		t.Fatal("Resolve with an empty setting key returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("empty-key error should not be ErrNotFound; it is a malformed ref")
	}
}

func TestResolveContextCancelled(t *testing.T) {
	f := newFake()
	f.set("flag", true)
	p := newWithClient(f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Resolve(ctx, mustRef(t, "configcat://flag"))
	if err == nil {
		t.Fatal("Resolve with a cancelled context returned nil error")
	}
}

func TestResolveNoSDKKey(t *testing.T) {
	// A real provider (no injected client) with no key configured must fail
	// cleanly rather than reaching the network.
	t.Setenv(envSDKKey, "")
	p := New()
	_, err := p.Resolve(context.Background(), mustRef(t, "configcat://flag"))
	if err == nil {
		t.Fatal("Resolve without an SDK key returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("no-key error should not be ErrNotFound")
	}
}

func TestSelectKeyOnJSONValue(t *testing.T) {
	f := newFake()
	f.set("payload", `{"level":"debug","port":8080}`)
	p := newWithClient(f)

	v, err := p.Resolve(context.Background(), mustRef(t, "configcat://payload#level"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("Bytes = %q, want debug", v.Bytes)
	}
}

// The ConfigCat provider intentionally does NOT implement WatchableProvider (the
// SDK offers no push notification); mamori polls it instead.
func TestNotWatchable(t *testing.T) {
	var p mamori.Provider = New()
	if _, ok := p.(mamori.WatchableProvider); ok {
		t.Fatal("configcat provider must not implement WatchableProvider (no native push)")
	}
}

// --- Conformance ---

func TestConformance(t *testing.T) {
	f := newFake()

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return newWithClient(f) },
		Ref: func(key string) string { return "configcat://" + key },
		Seed: func(_ context.Context, key, val string) error {
			f.set(key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set(key, val)
			return nil
		},
		SkipWatch: true, // ConfigCat has no native change notification; mamori polls.
	})
}
