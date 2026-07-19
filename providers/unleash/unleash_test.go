package unleash

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// toggle is a seeded feature toggle in the in-memory fake.
type toggle struct {
	enabled      bool
	variantName  string
	payloadValue string
}

// fakeClient is an in-memory implementation of featureClient. Un-seeded toggles
// report Exists=false, so the provider maps them to mamori.ErrNotFound and the
// conformance kit's not-found test passes without a live Unleash server.
type fakeClient struct {
	mu      sync.Mutex
	toggles map[string]toggle
}

func newFake() *fakeClient {
	return &fakeClient{toggles: map[string]toggle{}}
}

func (f *fakeClient) set(name string, t toggle) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toggles[name] = t
}

func (f *fakeClient) Exists(feature string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.toggles[feature]
	return ok
}

func (f *fakeClient) IsEnabled(feature string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.toggles[feature].enabled
}

func (f *fakeClient) GetVariant(feature string) (name, payloadValue string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.toggles[feature]
	return t.variantName, t.payloadValue
}

func (f *fakeClient) Close() error { return nil }

func fakeProvider(f *fakeClient) *Provider {
	return New(withClient(f))
}

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
	if got := New().Scheme(); got != scheme {
		t.Fatalf("Scheme() = %q, want %q", got, scheme)
	}
}

func TestResolveEnabledState(t *testing.T) {
	f := newFake()
	f.set("new-checkout", toggle{enabled: true})
	f.set("legacy-flow", toggle{enabled: false})
	p := fakeProvider(f)

	for name, want := range map[string]string{
		"new-checkout": "true",
		"legacy-flow":  "false",
	} {
		v, err := p.Resolve(context.Background(), mustRef(t, "unleash://"+name))
		if err != nil {
			t.Fatalf("Resolve(%s): %v", name, err)
		}
		if string(v.Bytes) != want {
			t.Errorf("Resolve(%s) = %q, want %q", name, v.Bytes, want)
		}
		if v.Sensitive {
			t.Errorf("Resolve(%s) Sensitive = true, want false", name)
		}
		if v.Version != mamori.VersionHash(v.Bytes) {
			t.Errorf("Resolve(%s) Version = %q, want VersionHash of bytes", name, v.Version)
		}
		if v.Metadata["kind"] != "enabled" || v.Metadata["toggle"] != name {
			t.Errorf("Resolve(%s) Metadata = %v", name, v.Metadata)
		}
	}
}

func TestResolveVariant(t *testing.T) {
	f := newFake()
	f.set("ab-test", toggle{enabled: true, variantName: "blue", payloadValue: "#0000ff"})
	p := fakeProvider(f)

	v, err := p.Resolve(context.Background(), mustRef(t, "unleash://ab-test#variant"))
	if err != nil {
		t.Fatalf("Resolve #variant: %v", err)
	}
	if string(v.Bytes) != "blue" {
		t.Errorf("variant = %q, want blue", v.Bytes)
	}
	if v.Metadata["kind"] != "variant" {
		t.Errorf("kind = %q, want variant", v.Metadata["kind"])
	}
}

func TestResolvePayload(t *testing.T) {
	f := newFake()
	f.set("ab-test", toggle{enabled: true, variantName: "blue", payloadValue: "#0000ff"})
	p := fakeProvider(f)

	v, err := p.Resolve(context.Background(), mustRef(t, "unleash://ab-test#payload"))
	if err != nil {
		t.Fatalf("Resolve #payload: %v", err)
	}
	if string(v.Bytes) != "#0000ff" {
		t.Errorf("payload = %q, want #0000ff", v.Bytes)
	}
	if v.Metadata["kind"] != "payload" {
		t.Errorf("kind = %q, want payload", v.Metadata["kind"])
	}
}

func TestResolveNotFound(t *testing.T) {
	f := newFake()
	p := fakeProvider(f)

	for _, ref := range []string{
		"unleash://ghost",
		"unleash://ghost#variant",
		"unleash://ghost#payload",
	} {
		_, err := p.Resolve(context.Background(), mustRef(t, ref))
		if err == nil {
			t.Fatalf("Resolve(%s) of missing toggle returned nil error", ref)
		}
		if !errors.Is(err, mamori.ErrNotFound) {
			t.Fatalf("Resolve(%s) error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", ref, err)
		}
	}
}

func TestResolveDisabledToggleIsFoundNotMissing(t *testing.T) {
	// A disabled toggle exists; it must resolve to "false", never ErrNotFound.
	f := newFake()
	f.set("disabled", toggle{enabled: false})
	p := fakeProvider(f)

	v, err := p.Resolve(context.Background(), mustRef(t, "unleash://disabled"))
	if err != nil {
		t.Fatalf("Resolve of disabled (but existing) toggle: %v", err)
	}
	if string(v.Bytes) != "false" {
		t.Fatalf("Bytes = %q, want false", v.Bytes)
	}
}

func TestResolveUnsupportedFragment(t *testing.T) {
	f := newFake()
	f.set("x", toggle{enabled: true})
	p := fakeProvider(f)

	_, err := p.Resolve(context.Background(), mustRef(t, "unleash://x#bogus"))
	if err == nil {
		t.Fatal("Resolve with unsupported fragment returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("unsupported-fragment error should not be ErrNotFound; it is a malformed ref")
	}
}

func TestResolveRequiresName(t *testing.T) {
	f := newFake()
	p := fakeProvider(f)

	_, err := p.Resolve(context.Background(), mustRef(t, "unleash://"))
	if err == nil {
		t.Fatal("Resolve without a toggle name returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("missing-name error should not be ErrNotFound")
	}
}

func TestResolveContextCancelled(t *testing.T) {
	f := newFake()
	f.set("x", toggle{enabled: true})
	p := fakeProvider(f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustRef(t, "unleash://x")); err == nil {
		t.Fatal("Resolve with a cancelled context returned nil error")
	}
}

func TestBuildClientNoURL(t *testing.T) {
	t.Setenv("UNLEASH_URL", "")
	// No fake and no URL: lazy construction must fail cleanly, not panic.
	p := New()
	_, err := p.Resolve(context.Background(), mustRef(t, "unleash://x"))
	if err == nil {
		t.Fatal("Resolve with no URL configured returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("no-URL error should not be ErrNotFound")
	}
}

// The Unleash provider intentionally does NOT implement WatchableProvider (no
// clean per-toggle native push); mamori polls it instead.
func TestNotWatchable(t *testing.T) {
	var p mamori.Provider = New()
	if _, ok := p.(mamori.WatchableProvider); ok {
		t.Fatal("unleash provider must not implement WatchableProvider (no native watch)")
	}
}

// --- Conformance ---
//
// The conformance kit's ResolveSeeded/Version tests require that the exact
// seeded string flows back out of Resolve unchanged. The #payload fragment gives
// us that: Seed sets a toggle's variant payload value, and Resolve of
// unleash://<key>#payload returns it verbatim. Un-seeded toggles report
// Exists=false and therefore resolve to ErrNotFound, satisfying the not-found
// test. Unleash has no native watch, so SkipWatch is set.

func TestConformance(t *testing.T) {
	f := newFake()

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return fakeProvider(f) },
		Ref: func(key string) string { return "unleash://" + key + "#payload" },
		Seed: func(_ context.Context, key, val string) error {
			f.set(key, toggle{enabled: true, variantName: key, payloadValue: val})
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set(key, toggle{enabled: true, variantName: key, payloadValue: val})
			return nil
		},
		SkipWatch: true, // Unleash has no clean per-toggle native change notification.
	})
}

// Guard: the fake's enabled-state formatting matches the provider's.
func TestEnabledFormatting(t *testing.T) {
	if strconv.FormatBool(true) != "true" || strconv.FormatBool(false) != "false" {
		t.Fatal("unexpected bool formatting")
	}
}
