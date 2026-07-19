package flagsmith

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeSource is an in-memory implementation of flagSource. Un-seeded features
// resolve to ErrNotFound, mirroring the real SDK (which returns an error from
// GetFlag when a feature is absent and no default handler is set).
type fakeSource struct {
	mu    sync.Mutex
	flags map[string]featureState
}

func newFake() *fakeSource {
	return &fakeSource{flags: map[string]featureState{}}
}

// set seeds a feature's value with enabled=true (the shape Seed/Mutate need).
func (f *fakeSource) set(name string, value any) {
	f.setFlag(name, value, true)
}

// setFlag seeds a feature's full state.
func (f *fakeSource) setFlag(name string, value any, enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flags[name] = featureState{value: value, enabled: enabled}
}

func (f *fakeSource) getFeature(ctx context.Context, name string) (featureState, error) {
	if err := ctx.Err(); err != nil {
		return featureState{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.flags[name]
	if !ok {
		return featureState{}, fmt.Errorf("mamori/flagsmith: feature %q not found: %w", name, mamori.ErrNotFound)
	}
	return st, nil
}

// --- Unit tests ---

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != scheme {
		t.Fatalf("Scheme() = %q, want %q", got, scheme)
	}
}

func TestResolveValue(t *testing.T) {
	f := newFake()
	f.setFlag("homepage_banner", "Welcome!", true)

	p := newWithSource(f)
	v, err := p.Resolve(context.Background(), mustRef(t, "flagsmith://homepage_banner"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "Welcome!" {
		t.Fatalf("Bytes = %q, want Welcome!", v.Bytes)
	}
	if v.Version != mamori.VersionHash(v.Bytes) {
		t.Errorf("Version = %q, want VersionHash of bytes", v.Version)
	}
	if v.Sensitive {
		t.Error("Sensitive = true, want false for a feature flag")
	}
	if v.Metadata["feature"] != "homepage_banner" {
		t.Errorf("Metadata[feature] = %q, want homepage_banner", v.Metadata["feature"])
	}
}

func TestResolveEnabledTrue(t *testing.T) {
	f := newFake()
	f.setFlag("new_flow", "irrelevant", true)

	p := newWithSource(f)
	v, err := p.Resolve(context.Background(), mustRef(t, "flagsmith://new_flow#enabled"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "true" {
		t.Fatalf("Bytes = %q, want true", v.Bytes)
	}
}

func TestResolveEnabledFalse(t *testing.T) {
	f := newFake()
	f.setFlag("new_flow", "irrelevant", false)

	p := newWithSource(f)
	v, err := p.Resolve(context.Background(), mustRef(t, "flagsmith://new_flow#enabled"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "false" {
		t.Fatalf("Bytes = %q, want false", v.Bytes)
	}
}

func TestResolveNonStringValues(t *testing.T) {
	f := newFake()
	f.setFlag("max_items", float64(42), true)
	f.setFlag("is_beta", true, true)
	f.setFlag("empty", nil, true)

	p := newWithSource(f)

	cases := map[string]string{
		"max_items": "42",
		"is_beta":   "true",
		"empty":     "",
	}
	for feature, want := range cases {
		v, err := p.Resolve(context.Background(), mustRef(t, "flagsmith://"+feature))
		if err != nil {
			t.Fatalf("Resolve(%s): %v", feature, err)
		}
		if string(v.Bytes) != want {
			t.Errorf("Resolve(%s) = %q, want %q", feature, v.Bytes, want)
		}
	}
}

func TestResolveJSONSubkey(t *testing.T) {
	f := newFake()
	f.setFlag("config", `{"host":"db.internal","port":5432}`, true)

	p := newWithSource(f)
	v, err := p.Resolve(context.Background(), mustRef(t, "flagsmith://config#host"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "db.internal" {
		t.Fatalf("Bytes = %q, want db.internal", v.Bytes)
	}
}

func TestResolveNotFound(t *testing.T) {
	f := newFake()
	p := newWithSource(f)
	_, err := p.Resolve(context.Background(), mustRef(t, "flagsmith://missing_feature"))
	if err == nil {
		t.Fatal("Resolve of missing feature returned nil error")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

func TestResolveRequiresFeatureName(t *testing.T) {
	f := newFake()
	p := newWithSource(f)
	_, err := p.Resolve(context.Background(), mustRef(t, "flagsmith://"))
	if err == nil {
		t.Fatal("Resolve with an empty feature name returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("empty-feature-name error should not be ErrNotFound; it is a malformed ref")
	}
}

func TestResolveMissingEnvironmentKey(t *testing.T) {
	t.Setenv("FLAGSMITH_ENVIRONMENT_KEY", "")
	// No WithEnvironmentKey and no env var: the real source cannot be built.
	p := New()
	_, err := p.Resolve(context.Background(), mustRef(t, "flagsmith://any_feature"))
	if err == nil {
		t.Fatal("Resolve with no environment key returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("no-key error should not be ErrNotFound")
	}
}

func TestContextCancelled(t *testing.T) {
	f := newFake()
	f.set("feat", "v")
	p := newWithSource(f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustRef(t, "flagsmith://feat")); err == nil {
		t.Fatal("Resolve with a cancelled context returned nil error")
	}
}

// The Flagsmith provider intentionally does NOT implement WatchableProvider (no
// native change notification exposed here); mamori polls it instead.
func TestNotWatchable(t *testing.T) {
	var p mamori.Provider = New()
	if _, ok := p.(mamori.WatchableProvider); ok {
		t.Fatal("flagsmith provider must not implement WatchableProvider (no native watch)")
	}
}

// --- Conformance ---

func TestConformance(t *testing.T) {
	f := newFake()

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return newWithSource(f) },
		Ref: func(key string) string { return "flagsmith://" + key },
		Seed: func(_ context.Context, key, val string) error {
			f.set(key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set(key, val)
			return nil
		},
		SkipWatch: true, // Flagsmith has no native change notification here.
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
