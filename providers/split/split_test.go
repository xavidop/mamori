package split

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeSplit is an in-memory Split client. Treatments are stored per
// (traffic key, feature flag) pair; an un-seeded flag evaluates to "control",
// exactly as the real Split client does for a missing/archived flag. It
// implements treatmentClient, so it drops straight into the provider via
// withClient and the conformance kit runs with no live Split backend.
type fakeSplit struct {
	mu    sync.Mutex
	store map[string]string // key: trafficKey \x00 feature -> treatment
}

func newFake() *fakeSplit {
	return &fakeSplit{store: map[string]string{}}
}

func storeKey(trafficKey, feature string) string {
	return trafficKey + "\x00" + feature
}

func (f *fakeSplit) set(trafficKey, feature, treatment string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[storeKey(trafficKey, feature)] = treatment
}

func (f *fakeSplit) Treatment(key, feature string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.store[storeKey(key, feature)]; ok {
		return t
	}
	return controlTreatment
}

func (f *fakeSplit) Destroy() {}

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

func TestResolveDefaultKey(t *testing.T) {
	f := newFake()
	f.set(defaultKey, "new-checkout", "on")

	p := New(withClient(f))
	v, err := p.Resolve(context.Background(), mustRef(t, "split://new-checkout"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "on" {
		t.Fatalf("Bytes = %q, want on", v.Bytes)
	}
	if v.Version == "" {
		t.Error("Version is empty")
	}
	if v.Version != mamori.VersionHash(v.Bytes) {
		t.Errorf("Version = %q, want VersionHash of bytes", v.Version)
	}
	if v.Sensitive {
		t.Error("Sensitive = true, want false for a feature flag")
	}
	if v.Metadata["flag"] != "new-checkout" || v.Metadata["key"] != defaultKey {
		t.Errorf("Metadata = %v, want flag=new-checkout key=%s", v.Metadata, defaultKey)
	}
}

func TestResolveTrafficKeyFromRef(t *testing.T) {
	f := newFake()
	// Same flag, different treatments per traffic key.
	f.set("user-42", "homepage-layout", "variant-b")
	f.set(defaultKey, "homepage-layout", "control-group")

	p := New(withClient(f))

	v, err := p.Resolve(context.Background(), mustRef(t, "split://homepage-layout?key=user-42"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "variant-b" {
		t.Fatalf("Bytes = %q, want variant-b (ref ?key must win)", v.Bytes)
	}
	if v.Metadata["key"] != "user-42" {
		t.Errorf("Metadata key = %q, want user-42", v.Metadata["key"])
	}
}

func TestResolveDefaultKeyOption(t *testing.T) {
	f := newFake()
	f.set("tenant-7", "beta-feature", "off")

	// WithKey sets the default traffic key; ref carries no ?key.
	p := New(WithKey("tenant-7"), withClient(f))
	v, err := p.Resolve(context.Background(), mustRef(t, "split://beta-feature"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "off" {
		t.Fatalf("Bytes = %q, want off", v.Bytes)
	}
	if v.Metadata["key"] != "tenant-7" {
		t.Errorf("Metadata key = %q, want tenant-7", v.Metadata["key"])
	}
}

func TestResolveControlIsNotFound(t *testing.T) {
	f := newFake() // nothing seeded -> control
	p := New(withClient(f))

	_, err := p.Resolve(context.Background(), mustRef(t, "split://unknown-flag"))
	if err == nil {
		t.Fatal("Resolve of control treatment returned nil error")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

func TestResolveRequiresFlagName(t *testing.T) {
	f := newFake()
	p := New(withClient(f))
	_, err := p.Resolve(context.Background(), mustRef(t, "split://"))
	if err == nil {
		t.Fatal("Resolve with an empty flag name returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("empty-flag error should not be ErrNotFound; it is a malformed ref")
	}
}

func TestResolveContextCancelled(t *testing.T) {
	f := newFake()
	f.set(defaultKey, "flag", "on")
	p := New(withClient(f))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Resolve(ctx, mustRef(t, "split://flag"))
	if err == nil {
		t.Fatal("Resolve with a cancelled context returned nil error")
	}
}

func TestResolveMissingAPIKey(t *testing.T) {
	// No injected client and no SPLIT_API_KEY: the lazy build path must fail with
	// a real error, never a spurious ErrNotFound, and without touching the network.
	t.Setenv("SPLIT_API_KEY", "")
	p := New()
	_, err := p.Resolve(context.Background(), mustRef(t, "split://flag"))
	if err == nil {
		t.Fatal("Resolve with no SDK key returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("no-key error should not be ErrNotFound")
	}
}

func TestClose(t *testing.T) {
	f := newFake()
	p := New(withClient(f))
	// The injected client is set, so Close destroys it.
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close again with no client is fine.
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// The Split provider intentionally does NOT implement WatchableProvider (no
// native per-flag change notification); mamori polls it instead.
func TestNotWatchable(t *testing.T) {
	var p mamori.Provider = New()
	if _, ok := p.(mamori.WatchableProvider); ok {
		t.Fatal("split provider must not implement WatchableProvider (no native watch)")
	}
}

// --- Conformance ---

func TestConformance(t *testing.T) {
	f := newFake()

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return New(withClient(f)) },
		Ref: func(key string) string { return "split://" + key },
		Seed: func(_ context.Context, key, val string) error {
			f.set(defaultKey, key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set(defaultKey, key, val)
			return nil
		},
		SkipWatch: true, // Split has no native per-flag change notification.
	})
}
