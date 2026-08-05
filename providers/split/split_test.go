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
	mu        sync.Mutex
	store     map[string]string // key: trafficKey \x00 feature -> treatment
	destroyed bool              // set by Destroy, asserted by the ownership test
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

// Destroy records the call rather than tearing anything down. It deliberately
// does not affect Treatment: the conformance kit builds two providers over one
// shared fake, and a fake that started returning "control" after the first
// Destroy would report the resulting failure as "Resolve before Close",
// pointing at the wrong thing.
func (f *fakeSplit) Destroy() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyed = true
}

// isDestroyed reports whether Destroy has been called, under the same lock
// every other field of the fake is read through.
func (f *fakeSplit) isDestroyed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.destroyed
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
	// The client is injected, so Close leaves it alone and only marks the
	// provider closed; TestCloseLeavesInjectedClientOpen asserts that split.
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close again is fine.
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
		// No PointerRef: split refs carry no fragment at all - the traffic key
		// comes from ?key=, and Resolve never reads ref.Key (see Resolve).
		Seed: func(_ context.Context, key, val string) error {
			f.set(defaultKey, key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set(defaultKey, key, val)
			return nil
		},
		SkipWatch: true, // Split has no native per-flag change notification.
		// treatmentClient.Treatment returns a bare string; not-found is the
		// "control" sentinel, not an error, so ErrorClassification is exempt.
		NoResolveErrors: true,
	})
}

// TestResolveAfterCloseIsUnavailable pins the terminal half of the Close
// contract. The factory records every call, so the assertion is not that the
// post-close Resolve was merely slow or merely failed, but that no client was
// built at all: without the closed guard, Close's nil client reads as "not yet
// created" and the next Resolve stands up a fresh Split factory.
func TestResolveAfterCloseIsUnavailable(t *testing.T) {
	f := newFake()
	f.set(defaultKey, "new-checkout", "on")
	p := New(withClient(f))
	var calls int
	p.newClient = func(context.Context) (treatmentClient, error) {
		calls++
		return f, nil
	}

	if _, err := p.Resolve(context.Background(), mustRef(t, "split://new-checkout")); err != nil {
		t.Fatalf("Resolve before Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if _, err := p.Resolve(context.Background(), mustRef(t, "split://new-checkout")); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("Resolve after Close = %v; want ErrUnavailable", err)
	}
	if calls != 0 {
		t.Fatalf("factory called %d times, want 0: a closed provider rebuilt its client", calls)
	}
}

// TestCloseWithoutResolveDoesNotBuildAClient covers the other lifecycle: a
// configured-but-unused provider is closed without ever standing up a factory.
func TestCloseWithoutResolveDoesNotBuildAClient(t *testing.T) {
	p := New(WithAPIKey("sdk-key"))
	var calls int
	p.newClient = func(context.Context) (treatmentClient, error) {
		calls++
		return newFake(), nil
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if calls != 0 {
		t.Fatalf("factory called %d times, want 0", calls)
	}
}

// TestCloseLeavesInjectedClientOpen is the ownership half of the Close
// contract: a client handed over by the caller belongs to the caller, so Close
// must stop using it without destroying it. withClient stands in for the public
// WithClient here, which wraps a caller-built *splitclient.SplitClient in an
// sdkClient whose Destroy would tear down that very client. The second
// assertion is what keeps this from being half a fix: the provider must still
// go terminal, so declining to destroy the caller's client cannot be
// implemented by declining to close at all.
func TestCloseLeavesInjectedClientOpen(t *testing.T) {
	f := newFake()
	f.set(defaultKey, "new-checkout", "on")
	p := New(withClient(f))

	if _, err := p.Resolve(context.Background(), mustRef(t, "split://new-checkout")); err != nil {
		t.Fatalf("Resolve before Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if f.isDestroyed() {
		t.Error("Close destroyed an injected client; it belongs to the caller")
	}
	if _, err := p.Resolve(context.Background(), mustRef(t, "split://new-checkout")); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("Resolve after Close = %v; want ErrUnavailable", err)
	}
}

// TestCloseDestroysSelfBuiltClient is the other side of the same rule: a client
// the provider constructed for itself is the provider's to destroy.
func TestCloseDestroysSelfBuiltClient(t *testing.T) {
	f := newFake()
	f.set(defaultKey, "new-checkout", "on")
	p := New(WithAPIKey("sdk-key"))
	p.newClient = func(context.Context) (treatmentClient, error) { return f, nil }

	if _, err := p.Resolve(context.Background(), mustRef(t, "split://new-checkout")); err != nil {
		t.Fatalf("Resolve before Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !f.isDestroyed() {
		t.Error("Close did not destroy a client the provider built itself")
	}
}
