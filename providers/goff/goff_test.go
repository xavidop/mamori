package goff

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/thomaspoignant/go-feature-flag/modules/core/ffcontext"
	"github.com/thomaspoignant/go-feature-flag/modules/core/flag"
	"github.com/thomaspoignant/go-feature-flag/modules/core/model"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeEvaluator is an in-memory evaluator standing in for a real
// go-feature-flag client. It maps a flag key to its evaluated variation value
// and returns a FLAG_NOT_FOUND result for any un-seeded flag, exactly as the
// real client does for a flag absent from the loaded configuration. This lets
// the conformance kit run with no flag-configuration file and no background
// poller (so the goroutine-leak check stays clean).
type fakeEvaluator struct {
	mu    sync.Mutex
	flags map[string]any
}

func newFake() *fakeEvaluator { return &fakeEvaluator{flags: map[string]any{}} }

// set seeds or replaces the evaluated variation for flagKey.
func (f *fakeEvaluator) set(flagKey string, val any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flags[flagKey] = val
}

func (f *fakeEvaluator) RawVariation(flagKey string, _ ffcontext.Context, _ any) (model.RawVarResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	val, ok := f.flags[flagKey]
	if !ok {
		return model.RawVarResult{
			Failed:    true,
			Reason:    flag.ReasonError,
			ErrorCode: flag.ErrorCodeFlagNotFound,
		}, errors.New("flag " + flagKey + " was not found in your configuration")
	}
	return model.RawVarResult{
		Value:         val,
		VariationType: "matched",
		Reason:        flag.ReasonTargetingMatch,
	}, nil
}

// --- Conformance ---

func TestConformance(t *testing.T) {
	fake := newFake()
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return New(withEvaluator(fake)) },
		Ref: func(key string) string { return "goff://" + key },
		Seed: func(_ context.Context, key, val string) error {
			fake.set(key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			fake.set(key, val)
			return nil
		},
		SkipWatch: true, // go-feature-flag has no native push; mamori polls.
	})
}

// --- Unit tests ---

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != scheme {
		t.Fatalf("Scheme() = %q, want %q", got, scheme)
	}
}

func TestResolveString(t *testing.T) {
	fake := newFake()
	fake.set("greeting", "hola")
	p := New(withEvaluator(fake))

	v, err := p.Resolve(context.Background(), mustRef(t, "goff://greeting"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hola" {
		t.Fatalf("Bytes = %q, want hola", v.Bytes)
	}
	if v.Version == "" {
		t.Error("Version is empty")
	}
	if v.Version != mamori.VersionHash(v.Bytes) {
		t.Errorf("Version = %q, want VersionHash of bytes", v.Version)
	}
	if v.Sensitive {
		t.Error("goff values must not be marked Sensitive")
	}
}

func TestResolveTypeMapping(t *testing.T) {
	fake := newFake()
	fake.set("flag-bool-true", true)
	fake.set("flag-bool-false", false)
	fake.set("flag-int", float64(42)) // go-feature-flag parses numbers as float64
	fake.set("flag-float", 3.5)
	fake.set("flag-json", map[string]any{"theme": "dark", "max": float64(10)})
	fake.set("flag-array", []any{"a", "b"})
	p := New(withEvaluator(fake))

	cases := map[string]string{
		"flag-bool-true":  "true",
		"flag-bool-false": "false",
		"flag-int":        "42",
		"flag-float":      "3.5",
		"flag-json":       `{"max":10,"theme":"dark"}`,
		"flag-array":      `["a","b"]`,
	}
	for key, want := range cases {
		v, err := p.Resolve(context.Background(), mustRef(t, "goff://"+key))
		if err != nil {
			t.Fatalf("Resolve %s: %v", key, err)
		}
		if string(v.Bytes) != want {
			t.Errorf("%s = %q, want %q", key, v.Bytes, want)
		}
	}
}

func TestResolveJSONKey(t *testing.T) {
	fake := newFake()
	fake.set("ui-config", map[string]any{
		"theme":   "dark",
		"maxItems": float64(20),
		"beta":    true,
	})
	p := New(withEvaluator(fake))

	got := func(key string) string {
		t.Helper()
		v, err := p.Resolve(context.Background(), mustRef(t, "goff://ui-config#"+key))
		if err != nil {
			t.Fatalf("Resolve #%s: %v", key, err)
		}
		return string(v.Bytes)
	}
	if got("theme") != "dark" {
		t.Errorf("theme = %q, want dark", got("theme"))
	}
	if got("maxItems") != "20" {
		t.Errorf("maxItems = %q, want 20", got("maxItems"))
	}
	if got("beta") != "true" {
		t.Errorf("beta = %q, want true", got("beta"))
	}

	// A missing JSON key is a typed not-found.
	_, err := p.Resolve(context.Background(), mustRef(t, "goff://ui-config#absent"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing json key err = %v, want ErrNotFound", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(withEvaluator(newFake()))
	_, err := p.Resolve(context.Background(), mustRef(t, "goff://missing-flag"))
	if err == nil {
		t.Fatal("Resolve of missing flag returned nil error")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

func TestResolveContextCancelled(t *testing.T) {
	fake := newFake()
	fake.set("k", "v")
	p := New(withEvaluator(fake))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustRef(t, "goff://k")); err == nil {
		t.Fatal("Resolve with a cancelled context returned nil error")
	}
}

func TestTargetingKeyDefault(t *testing.T) {
	if New().targetingKey != defaultTargetingKey {
		t.Fatalf("default targeting key = %q, want %q", New().targetingKey, defaultTargetingKey)
	}
	if New(WithTargetingKey("")).targetingKey != defaultTargetingKey {
		t.Fatal("empty WithTargetingKey should fall back to the default")
	}
	if got := New(WithTargetingKey("service-a")).targetingKey; got != "service-a" {
		t.Fatalf("targeting key = %q, want service-a", got)
	}
}

// captureEvaluator records the evaluation context it is handed.
type captureEvaluator struct{ last ffcontext.Context }

func (c *captureEvaluator) RawVariation(_ string, evalCtx ffcontext.Context, _ any) (model.RawVarResult, error) {
	c.last = evalCtx
	return model.RawVarResult{Value: "ok", Reason: flag.ReasonStatic}, nil
}

func TestTargetingKeyUsed(t *testing.T) {
	cap := &captureEvaluator{}
	p := New(withEvaluator(cap), WithTargetingKey("tenant-7"))
	if _, err := p.Resolve(context.Background(), mustRef(t, "goff://any")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cap.last == nil {
		t.Fatal("evaluator never received an evaluation context")
	}
	if got := cap.last.GetKey(); got != "tenant-7" {
		t.Fatalf("targeting key = %q, want tenant-7", got)
	}
}

// The provider intentionally does NOT implement WatchableProvider: go-feature-flag
// has no native change push; mamori polls it and the library reloads its cache.
func TestNotWatchable(t *testing.T) {
	var p mamori.Provider = New()
	if _, ok := p.(mamori.WatchableProvider); ok {
		t.Fatal("goff provider must not implement WatchableProvider (no native watch)")
	}
}

func TestNoConfigSource(t *testing.T) {
	t.Setenv(configEnv, "")
	// No evaluator injected and no retriever configured: Resolve must fail with a
	// helpful error (not a panic and not ErrNotFound).
	p := New()
	_, err := p.Resolve(context.Background(), mustRef(t, "goff://any"))
	if err == nil {
		t.Fatal("Resolve without a config source returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("no-config error should not be ErrNotFound")
	}
}

func TestRetrieverSelection(t *testing.T) {
	if _, err := New(WithConfigFile("flags.yaml")).retriever(); err != nil {
		t.Fatalf("file retriever: %v", err)
	}
	if _, err := New(WithConfigURL("https://example.test/flags.yaml")).retriever(); err != nil {
		t.Fatalf("url retriever: %v", err)
	}
	t.Setenv(configEnv, "https://example.test/env-flags.yaml")
	if _, err := New().retriever(); err != nil {
		t.Fatalf("env url retriever: %v", err)
	}
	t.Setenv(configEnv, "/etc/flags.yaml")
	if _, err := New().retriever(); err != nil {
		t.Fatalf("env file retriever: %v", err)
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
