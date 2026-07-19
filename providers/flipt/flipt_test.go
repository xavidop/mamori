package flipt

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"

	"go.flipt.io/flipt/rpc/flipt/evaluation"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testNamespace = "default"

// variantEntry is a seeded variant flag in the fake.
type variantEntry struct {
	key        string
	attachment string
}

// fakeEval is an in-memory implementation of the evaluator interface. Un-seeded
// flags evaluate to a gRPC NotFound status error, exactly as a real Flipt server
// reports a missing flag through the SDK transports. Evaluating a flag with the
// wrong kind (a boolean call on a variant flag or vice versa) returns an
// InvalidArgument status error, matching Flipt's type-mismatch behavior.
type fakeEval struct {
	mu       sync.Mutex
	booleans map[string]bool
	variants map[string]variantEntry
	lastReq  *evaluation.EvaluationRequest
}

func newFake() *fakeEval {
	return &fakeEval{
		booleans: map[string]bool{},
		variants: map[string]variantEntry{},
	}
}

func storeKey(namespace, flag string) string { return namespace + "/" + flag }

func (f *fakeEval) setVariant(namespace, flag, key, attachment string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.variants[storeKey(namespace, flag)] = variantEntry{key: key, attachment: attachment}
}

func (f *fakeEval) setBoolean(namespace, flag string, enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.booleans[storeKey(namespace, flag)] = enabled
}

func (f *fakeEval) record(v *evaluation.EvaluationRequest) {
	f.mu.Lock()
	f.lastReq = v
	f.mu.Unlock()
}

func (f *fakeEval) Boolean(ctx context.Context, v *evaluation.EvaluationRequest) (*evaluation.BooleanEvaluationResponse, error) {
	f.record(v)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := storeKey(v.GetNamespaceKey(), v.GetFlagKey())
	f.mu.Lock()
	enabled, isBool := f.booleans[key]
	_, isVariant := f.variants[key]
	f.mu.Unlock()
	switch {
	case isBool:
		return &evaluation.BooleanEvaluationResponse{Enabled: enabled, FlagKey: v.GetFlagKey()}, nil
	case isVariant:
		return nil, status.Error(codes.InvalidArgument, "flag type VARIANT_FLAG_TYPE invalid for boolean evaluation")
	default:
		return nil, status.Error(codes.NotFound, "flag not found")
	}
}

func (f *fakeEval) Variant(ctx context.Context, v *evaluation.EvaluationRequest) (*evaluation.VariantEvaluationResponse, error) {
	f.record(v)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := storeKey(v.GetNamespaceKey(), v.GetFlagKey())
	f.mu.Lock()
	entry, isVariant := f.variants[key]
	_, isBool := f.booleans[key]
	f.mu.Unlock()
	switch {
	case isVariant:
		return &evaluation.VariantEvaluationResponse{
			Match:             true,
			VariantKey:        entry.key,
			VariantAttachment: entry.attachment,
			FlagKey:           v.GetFlagKey(),
		}, nil
	case isBool:
		return nil, status.Error(codes.InvalidArgument, "flag type BOOLEAN_FLAG_TYPE invalid for variant evaluation")
	default:
		return nil, status.Error(codes.NotFound, "flag not found")
	}
}

func (f *fakeEval) entityID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastReq == nil {
		return ""
	}
	return f.lastReq.GetEntityId()
}

// compile-time assertion that the fake satisfies the same interface as the real
// SDK evaluation client.
var _ evaluator = (*fakeEval)(nil)

// --- Unit tests ---

func TestResolveVariant(t *testing.T) {
	f := newFake()
	f.setVariant(testNamespace, "plan-tier", "enterprise", "")
	p := newWithClient(f)

	v, err := p.Resolve(context.Background(), mustRef(t, "flipt://"+testNamespace+"/plan-tier"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "enterprise" {
		t.Fatalf("Bytes = %q, want enterprise", v.Bytes)
	}
	if v.Version == "" || v.Version != mamori.VersionHash(v.Bytes) {
		t.Errorf("Version = %q, want VersionHash of bytes", v.Version)
	}
	if v.Sensitive {
		t.Error("Sensitive = true, want false for a feature flag")
	}
	if v.Metadata["namespace"] != testNamespace || v.Metadata["flag"] != "plan-tier" || v.Metadata["type"] != "variant" {
		t.Errorf("Metadata = %v, missing/incorrect namespace/flag/type", v.Metadata)
	}
}

func TestResolveVariantAttachment(t *testing.T) {
	f := newFake()
	attachment := `{"limit":100,"tier":"gold"}`
	f.setVariant(testNamespace, "plan-tier", "gold", attachment)
	p := newWithClient(f)

	v, err := p.Resolve(context.Background(), mustRef(t, "flipt://"+testNamespace+"/plan-tier#attachment"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != attachment {
		t.Fatalf("Bytes = %q, want the variant attachment %q", v.Bytes, attachment)
	}
	if v.Metadata["type"] != "variant-attachment" {
		t.Errorf("Metadata type = %q, want variant-attachment", v.Metadata["type"])
	}
}

func TestResolveBooleanTrue(t *testing.T) {
	f := newFake()
	f.setBoolean(testNamespace, "new-checkout", true)
	p := newWithClient(f)

	v, err := p.Resolve(context.Background(), mustRef(t, "flipt://"+testNamespace+"/new-checkout"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "true" {
		t.Fatalf("Bytes = %q, want true", v.Bytes)
	}
	if v.Metadata["type"] != "boolean" {
		t.Errorf("Metadata type = %q, want boolean", v.Metadata["type"])
	}
}

func TestResolveBooleanFalse(t *testing.T) {
	f := newFake()
	f.setBoolean(testNamespace, "new-checkout", false)
	p := newWithClient(f)

	v, err := p.Resolve(context.Background(), mustRef(t, "flipt://"+testNamespace+"/new-checkout"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "false" {
		t.Fatalf("Bytes = %q, want false", v.Bytes)
	}
}

func TestResolveNotFound(t *testing.T) {
	f := newFake()
	p := newWithClient(f)

	_, err := p.Resolve(context.Background(), mustRef(t, "flipt://"+testNamespace+"/missing"))
	if err == nil {
		t.Fatal("Resolve of missing flag returned nil error")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

func TestResolveEntityDefault(t *testing.T) {
	f := newFake()
	f.setVariant(testNamespace, "flag", "on", "")
	p := newWithClient(f)

	if _, err := p.Resolve(context.Background(), mustRef(t, "flipt://"+testNamespace+"/flag")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := f.entityID(); got != defaultEntity {
		t.Fatalf("entity id = %q, want default %q", got, defaultEntity)
	}
}

func TestResolveEntityOverride(t *testing.T) {
	f := newFake()
	f.setVariant(testNamespace, "flag", "on", "")
	p := newWithClient(f)

	if _, err := p.Resolve(context.Background(), mustRef(t, "flipt://"+testNamespace+"/flag?entity=user-42")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := f.entityID(); got != "user-42" {
		t.Fatalf("entity id = %q, want user-42", got)
	}
}

func TestResolveBadPath(t *testing.T) {
	f := newFake()
	p := newWithClient(f)
	// Only a namespace, no flag key.
	_, err := p.Resolve(context.Background(), mustRef(t, "flipt://only-namespace"))
	if err == nil {
		t.Fatal("Resolve with a single path segment returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("malformed-path error should not be ErrNotFound")
	}
}

func TestResolveUnsupportedFragment(t *testing.T) {
	f := newFake()
	f.setVariant(testNamespace, "flag", "on", "")
	p := newWithClient(f)
	_, err := p.Resolve(context.Background(), mustRef(t, "flipt://"+testNamespace+"/flag#bogus"))
	if err == nil {
		t.Fatal("Resolve with an unsupported fragment returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("unsupported-fragment error should not be ErrNotFound")
	}
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != "flipt" {
		t.Fatalf("Scheme() = %q, want flipt", got)
	}
}

// The Flipt provider intentionally does NOT implement WatchableProvider (no
// native change notification for evaluation); mamori polls it instead.
func TestNotWatchable(t *testing.T) {
	var p mamori.Provider = New()
	if _, ok := p.(mamori.WatchableProvider); ok {
		t.Fatal("flipt provider must not implement WatchableProvider (no native watch)")
	}
}

// TestLazyClientBuild exercises the real SDK construction path (no network is
// performed until an evaluation is issued).
func TestLazyClientBuild(t *testing.T) {
	p := New(WithURL("http://localhost:8080/"), WithToken("test-token"))
	if ev := p.evaluatorFor(); ev == nil {
		t.Fatal("evaluatorFor returned nil client")
	}
	// Cached on subsequent calls.
	if p.evaluatorFor() == nil {
		t.Fatal("evaluatorFor returned nil on second call")
	}
}

func TestLazyClientFromEnv(t *testing.T) {
	t.Setenv("FLIPT_URL", "http://flipt.internal:8080")
	t.Setenv("FLIPT_TOKEN", "env-token")
	p := New()
	if ev := p.evaluatorFor(); ev == nil {
		t.Fatal("evaluatorFor returned nil client built from environment")
	}
}

// --- Conformance ---

func TestConformance(t *testing.T) {
	f := newFake()

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return newWithClient(f) },
		Ref: func(key string) string {
			return "flipt://" + testNamespace + "/" + key
		},
		Seed: func(_ context.Context, key, val string) error {
			// Model every seeded flag as a variant flag whose matched variant key
			// is the seeded value, so Resolve returns the value verbatim.
			f.setVariant(testNamespace, key, val, "")
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.setVariant(testNamespace, key, val, "")
			return nil
		},
		SkipWatch: true, // Flipt evaluation has no native change notification.
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
