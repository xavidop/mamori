package openfeature

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	of "github.com/open-feature/go-sdk/openfeature"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

func TestParseFlagType(t *testing.T) {
	tests := []struct {
		raw     string
		want    flagType
		wantErr bool
	}{
		{raw: "openfeature://f", want: typeAuto},
		{raw: "openfeature://f?type=bool", want: typeBool},
		{raw: "openfeature://f?type=string", want: typeString},
		{raw: "openfeature://f?type=int", want: typeInt},
		{raw: "openfeature://f?type=float", want: typeFloat},
		{raw: "openfeature://f?type=object", want: typeObject},
		{raw: "openfeature://f?type=bogus", wantErr: true},
		{raw: "openfeature://f?type=", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			ref, err := mamori.ParseRef(tt.raw)
			if err != nil {
				t.Fatalf("ParseRef(%q): %v", tt.raw, err)
			}
			got, err := parseFlagType(ref)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseFlagType(%q) = %v, want error", tt.raw, got)
				}
				if !errors.Is(err, mamori.ErrInvalid) {
					t.Errorf("error %v does not satisfy errors.Is(ErrInvalid)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlagType(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("parseFlagType(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// In-memory fake for openfeature.IClient.
//
// of.IClient is embedded rather than fully implemented: this provider calls
// only the five *ValueDetails methods, and a nil embedded interface turns any
// other call into an immediate panic, which is what should happen if the
// provider starts using an API this fake does not model.
// ---------------------------------------------------------------------------

type fakeFlag struct {
	value   any
	variant string
	// kind constrains which evaluation succeeds, so the fake can model a
	// strictly-typed OpenFeature provider that reports TYPE_MISMATCH for the
	// wrong evaluation method. An empty kind accepts every method.
	kind string
}

type fakeClient struct {
	of.IClient // never populated; see the comment above

	mu    sync.Mutex
	flags map[string]fakeFlag
	fails map[string]of.ErrorCode
	// failErrs stores an arbitrary error to return from the next evaluation of
	// a flag, keyed separately from fails (which stores an OpenFeature error
	// code). This is what the providertest conformance kit's Fail/Clear seam
	// uses: providertest.Config.Fail is handed an already-classified mamori
	// error and requires the provider's Resolve to hand it back with its
	// errors.Is chain intact, which is a different thing from this provider's
	// own classifyOF mapping (exercised directly by TestErrorClassification
	// below). Keeping the two separate lets both be tested without either
	// one masking the other.
	failErrs map[string]error
	// calls records how many times each evaluation method ran, keyed by type
	// name, so the fallback-chain tests can assert both order and stopping.
	calls map[string]int
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		flags:    map[string]fakeFlag{},
		fails:    map[string]of.ErrorCode{},
		failErrs: map[string]error{},
		calls:    map[string]int{},
	}
}

func (f *fakeClient) set(flag string, value any, variant, kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flags[flag] = fakeFlag{value: value, variant: variant, kind: kind}
}

// fail makes the next evaluation of flag return code, until clear is called.
func (f *fakeClient) fail(flag string, code of.ErrorCode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[flag] = code
}

// failErr makes the next evaluation of flag return err verbatim (reported
// under the generic GENERAL code, which this provider deliberately leaves
// unclassified - see classifyOF), until clear is called. It is how
// providertest's Fail seam is wired: providertest hands this an
// already-classified mamori sentinel and checks that Resolve's wrapping
// preserves its errors.Is chain rather than flattening it, exactly as every
// other mamori provider's fake does for that seam. Real OpenFeature evaluation
// failures never carry a mamori sentinel; that is what fail (above) models.
func (f *fakeClient) failErr(flag string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failErrs[flag] = err
}

func (f *fakeClient) clear(flag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fails, flag)
	delete(f.failErrs, flag)
}

func (f *fakeClient) callCount(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[kind]
}

// lookup is the shared body of all five evaluation methods. It returns the
// resolution detail the SDK would produce, plus the matching error.
func (f *fakeClient) lookup(kind, flag string) (any, of.ResolutionDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[kind]++

	if err, ok := f.failErrs[flag]; ok {
		return nil, of.ResolutionDetail{
			Reason:    of.ErrorReason,
			ErrorCode: of.GeneralCode,
		}, err
	}

	if code, ok := f.fails[flag]; ok {
		return nil, of.ResolutionDetail{
			Reason:       of.ErrorReason,
			ErrorCode:    code,
			ErrorMessage: "injected " + string(code),
		}, fmt.Errorf("injected %s", code)
	}

	fl, ok := f.flags[flag]
	if !ok {
		return nil, of.ResolutionDetail{
			Reason:    of.ErrorReason,
			ErrorCode: of.FlagNotFoundCode,
		}, fmt.Errorf("flag %q not found", flag)
	}
	if fl.kind != "" && fl.kind != kind {
		return nil, of.ResolutionDetail{
			Reason:    of.ErrorReason,
			ErrorCode: of.TypeMismatchCode,
		}, fmt.Errorf("flag %q is not a %s", flag, kind)
	}
	return fl.value, of.ResolutionDetail{
		Variant: fl.variant,
		Reason:  of.StaticReason,
	}, nil
}

func (f *fakeClient) BooleanValueDetails(_ context.Context, flag string, def bool, _ of.EvaluationContext, _ ...of.Option) (of.BooleanEvaluationDetails, error) {
	v, rd, err := f.lookup("bool", flag)
	out := of.BooleanEvaluationDetails{Value: def}
	out.FlagKey, out.ResolutionDetail = flag, rd
	if err != nil {
		return out, err
	}
	b, ok := v.(bool)
	if !ok {
		out.ResolutionDetail = of.ResolutionDetail{Reason: of.ErrorReason, ErrorCode: of.TypeMismatchCode}
		return out, fmt.Errorf("flag %q is not a bool", flag)
	}
	out.Value = b
	return out, nil
}

func (f *fakeClient) StringValueDetails(_ context.Context, flag string, def string, _ of.EvaluationContext, _ ...of.Option) (of.StringEvaluationDetails, error) {
	v, rd, err := f.lookup("string", flag)
	out := of.StringEvaluationDetails{Value: def}
	out.FlagKey, out.ResolutionDetail = flag, rd
	if err != nil {
		return out, err
	}
	s, ok := v.(string)
	if !ok {
		out.ResolutionDetail = of.ResolutionDetail{Reason: of.ErrorReason, ErrorCode: of.TypeMismatchCode}
		return out, fmt.Errorf("flag %q is not a string", flag)
	}
	out.Value = s
	return out, nil
}

func (f *fakeClient) IntValueDetails(_ context.Context, flag string, def int64, _ of.EvaluationContext, _ ...of.Option) (of.IntEvaluationDetails, error) {
	v, rd, err := f.lookup("int", flag)
	out := of.IntEvaluationDetails{Value: def}
	out.FlagKey, out.ResolutionDetail = flag, rd
	if err != nil {
		return out, err
	}
	n, ok := v.(int64)
	if !ok {
		out.ResolutionDetail = of.ResolutionDetail{Reason: of.ErrorReason, ErrorCode: of.TypeMismatchCode}
		return out, fmt.Errorf("flag %q is not an int", flag)
	}
	out.Value = n
	return out, nil
}

func (f *fakeClient) FloatValueDetails(_ context.Context, flag string, def float64, _ of.EvaluationContext, _ ...of.Option) (of.FloatEvaluationDetails, error) {
	v, rd, err := f.lookup("float", flag)
	out := of.FloatEvaluationDetails{Value: def}
	out.FlagKey, out.ResolutionDetail = flag, rd
	if err != nil {
		return out, err
	}
	x, ok := v.(float64)
	if !ok {
		out.ResolutionDetail = of.ResolutionDetail{Reason: of.ErrorReason, ErrorCode: of.TypeMismatchCode}
		return out, fmt.Errorf("flag %q is not a float", flag)
	}
	out.Value = x
	return out, nil
}

func (f *fakeClient) ObjectValueDetails(_ context.Context, flag string, def any, _ of.EvaluationContext, _ ...of.Option) (of.InterfaceEvaluationDetails, error) {
	v, rd, err := f.lookup("object", flag)
	out := of.InterfaceEvaluationDetails{Value: def}
	out.FlagKey, out.ResolutionDetail = flag, rd
	if err != nil {
		return out, err
	}
	out.Value = v
	return out, nil
}

func newTestProvider(c of.IClient) *Provider { return New(WithClient(c)) }

func resolve(t *testing.T, p *Provider, raw string) (mamori.Value, error) {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return p.Resolve(context.Background(), ref)
}

func TestResolveRendersEachType(t *testing.T) {
	tests := []struct {
		name  string
		ref   string
		value any
		kind  string
		want  string
	}{
		{name: "bool", ref: "openfeature://f?type=bool", value: true, kind: "bool", want: "true"},
		{name: "string", ref: "openfeature://f?type=string", value: "hello", kind: "string", want: "hello"},
		{name: "int", ref: "openfeature://f?type=int", value: int64(42), kind: "int", want: "42"},
		{name: "float", ref: "openfeature://f?type=float", value: 1.5, kind: "float", want: "1.5"},
		{name: "object", ref: "openfeature://f?type=object", value: map[string]any{"a": float64(1)}, kind: "object", want: `{"a":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeClient()
			fake.set("f", tt.value, "", tt.kind)
			v, err := resolve(t, newTestProvider(fake), tt.ref)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got := string(v.Bytes); got != tt.want {
				t.Errorf("Bytes = %q, want %q", got, tt.want)
			}
			if v.Sensitive {
				t.Error("Sensitive = true, want false: a feature flag is configuration, not a secret")
			}
		})
	}
}

// TestResolveAutoFallbackOrderAndStop asserts the untyped chain tries object,
// then bool, then string, AND stops at the first success. The stopping half
// matters as much as the order: a chain that kept going would turn one
// resolve into three evaluations against a vendor that may rate-limit or bill
// per call.
func TestResolveAutoFallbackOrderAndStop(t *testing.T) {
	fake := newFakeClient()
	fake.set("f", true, "", "bool") // only a bool evaluation succeeds
	v, err := resolve(t, newTestProvider(fake), "openfeature://f")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := string(v.Bytes); got != "true" {
		t.Errorf("Bytes = %q, want %q", got, "true")
	}
	if n := fake.callCount("object"); n != 1 {
		t.Errorf("object evaluations = %d, want 1: object must be tried first", n)
	}
	if n := fake.callCount("bool"); n != 1 {
		t.Errorf("bool evaluations = %d, want 1", n)
	}
	if n := fake.callCount("string"); n != 0 {
		t.Errorf("string evaluations = %d, want 0: the chain must stop at the first success", n)
	}
}

// TestResolveTypedPinSkipsFallback is the property the docs tell operators to
// rely on when they pin ?type=.
func TestResolveTypedPinSkipsFallback(t *testing.T) {
	fake := newFakeClient()
	fake.set("f", true, "", "bool")
	if _, err := resolve(t, newTestProvider(fake), "openfeature://f?type=bool"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if n := fake.callCount("object"); n != 0 {
		t.Errorf("object evaluations = %d, want 0: ?type= must pin the evaluation", n)
	}
	if n := fake.callCount("bool"); n != 1 {
		t.Errorf("bool evaluations = %d, want 1", n)
	}
}

// TestResolveAutoExhaustedReportsInvalid covers a flag no evaluation in the
// chain can read. It must not report not-found, which would make mamori apply
// a default for a flag that exists.
func TestResolveAutoExhaustedReportsInvalid(t *testing.T) {
	fake := newFakeClient()
	fake.set("f", 1.5, "", "float") // float is not in the auto chain
	_, err := resolve(t, newTestProvider(fake), "openfeature://f")
	if err == nil {
		t.Fatal("Resolve returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("exhausted fallback reported ErrNotFound; the flag exists, so a default must not be applied")
	}
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Errorf("error %v does not satisfy errors.Is(ErrInvalid)", err)
	}
	if !strings.Contains(err.Error(), "type=") {
		t.Errorf("error %q does not tell the user to pin ?type=", err)
	}
}

func TestResolveVersionPrefersVariant(t *testing.T) {
	fake := newFakeClient()
	fake.set("f", "on", "variant-a", "string")
	v, err := resolve(t, newTestProvider(fake), "openfeature://f?type=string")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Version != "variant-a" {
		t.Errorf("Version = %q, want the evaluation's variant %q", v.Version, "variant-a")
	}
}

func TestResolveVersionFallsBackToHash(t *testing.T) {
	fake := newFakeClient()
	fake.set("f", "on", "", "string") // no variant
	v, err := resolve(t, newTestProvider(fake), "openfeature://f?type=string")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := mamori.VersionHash(v.Bytes); v.Version != want {
		t.Errorf("Version = %q, want the VersionHash fallback %q", v.Version, want)
	}
}

func TestResolveJSONPointerSelection(t *testing.T) {
	fake := newFakeClient()
	fake.set("f", map[string]any{"db": map[string]any{"port": float64(5432)}}, "", "object")
	v, err := resolve(t, newTestProvider(fake), "openfeature://f#/db/port?type=object")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := string(v.Bytes); got != "5432" {
		t.Errorf("Bytes = %q, want %q", got, "5432")
	}
}

func TestResolveNotFound(t *testing.T) {
	fake := newFakeClient()
	_, err := resolve(t, newTestProvider(fake), "openfeature://missing?type=string")
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("Resolve error = %v, want errors.Is(ErrNotFound)", err)
	}
}

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		code of.ErrorCode
		want mamori.Kind
	}{
		{of.FlagNotFoundCode, mamori.KindNotFound},
		{of.TypeMismatchCode, mamori.KindInvalid},
		{of.ParseErrorCode, mamori.KindInvalid},
		{of.InvalidContextCode, mamori.KindInvalid},
		{of.TargetingKeyMissingCode, mamori.KindInvalid},
		{of.ProviderNotReadyCode, mamori.KindUnavailable},
		{of.ProviderFatalCode, mamori.KindUnavailable},
		{of.GeneralCode, mamori.KindUnknown},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			fake := newFakeClient()
			fake.set("f", "v", "", "string")
			fake.fail("f", tt.code)
			_, err := resolve(t, newTestProvider(fake), "openfeature://f?type=string")
			if err == nil {
				t.Fatal("Resolve returned nil error")
			}
			if got := mamori.ErrorKind(err); got != tt.want {
				t.Errorf("ErrorKind = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTargetingKeyAndAttributes(t *testing.T) {
	fake := newFakeClient()
	fake.set("f", "v", "", "string")
	p := New(WithClient(fake), WithTargetingKey("svc-a"), WithAttributes(map[string]any{"region": "eu"}))
	if p.evalCtx.TargetingKey() != "svc-a" {
		t.Errorf("TargetingKey = %q, want %q", p.evalCtx.TargetingKey(), "svc-a")
	}
	if got := p.evalCtx.Attributes()["region"]; got != "eu" {
		t.Errorf("attribute region = %v, want %q", got, "eu")
	}
}

func TestDefaultTargetingKey(t *testing.T) {
	p := New(WithClient(newFakeClient()))
	if p.evalCtx.TargetingKey() != defaultTargetingKey {
		t.Errorf("TargetingKey = %q, want %q", p.evalCtx.TargetingKey(), defaultTargetingKey)
	}
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != Scheme {
		t.Errorf("Scheme() = %q, want %q", got, Scheme)
	}
}

func TestRegisteredScheme(t *testing.T) {
	for _, s := range mamori.RegisteredSchemes() {
		if s == Scheme {
			return
		}
	}
	t.Errorf("scheme %q was not registered by init()", Scheme)
}

// seedConformanceValue stores val for key in fake, for use by the
// providertest conformance kit's Seed/Mutate hooks.
//
// The kit seeds and mutates through a single func(ctx, key, val string)
// signature, so it gives no way to say up front which evaluation method the
// case's ref will pin. Every case except JSONPointerSelection seeds a plain
// string and pins ?type=string, so the common path stores kind "string"
// unchanged. JSONPointerSelection seeds a JSON object payload and pins its
// PointerRef at ?type=object: storing that payload verbatim as a Go string
// under kind "string" would make the object evaluation this test needs
// report TYPE_MISMATCH, and even if it were accepted, re-marshaling a raw
// Go string produces a quoted JSON string, not the object mamori.SelectKey
// needs to navigate. So a value that parses as JSON is stored decoded (a
// map/slice/etc, matching what a real OpenFeature object evaluation returns)
// under an empty kind, which fakeFlag treats as "accept any evaluation
// method" - exactly what a value with no single fixed kind needs.
func seedConformanceValue(fake *fakeClient, key, val string) {
	var decoded any
	if json.Valid([]byte(val)) && json.Unmarshal([]byte(val), &decoded) == nil {
		if _, isObj := decoded.(map[string]any); isObj {
			fake.set(key, decoded, "", "")
			return
		}
	}
	fake.set(key, val, "", "string")
}

func TestConformance(t *testing.T) {
	fake := newFakeClient()
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return New(WithClient(fake)) },
		// Ref deliberately does NOT pin ?type=string here, even though every
		// seeded value in this suite is a string: providertest's DecodeOption
		// case builds its ref as ParseRef(c.Ref(key) + "?decode=base64"),
		// which assumes c.Ref returns no query string of its own. A pinned
		// "...?type=string?decode=base64" would parse as a single opaque
		// query value (ParseRef only recognizes the first '?'), so the
		// resolved ?type would be the literal string "string?decode=base64"
		// rather than "string". Leaving Ref unpinned costs the auto chain's
		// extra object/bool evaluations on every conformance case, which is
		// fine for an in-memory fake; TestResolveTypedPinSkipsFallback is
		// what actually proves the pin saves real evaluations in production.
		Ref:        func(key string) string { return Scheme + "://" + key },
		PointerRef: func(key, frag string) string { return Scheme + "://" + key + frag + "?type=object" },
		Seed:       func(_ context.Context, key, val string) error { seedConformanceValue(fake, key, val); return nil },
		Mutate:     func(_ context.Context, key, val string) error { seedConformanceValue(fake, key, val); return nil },
		// Fail hands the fake an already-classified mamori sentinel and checks
		// that Resolve's wrapping preserves its errors.Is chain, the same
		// property every other mamori provider's Fail seam verifies. This is
		// distinct from classifyOF, which classifies real OpenFeature error
		// codes and is exercised directly by TestErrorClassification above.
		Fail:  func(_ context.Context, key string, err error) error { fake.failErr(key, err); return nil },
		Clear: func(_ context.Context, key string) error { fake.clear(key); return nil },
		// No goroutine-leak exemption needed: the OpenFeature go-sdk's own
		// package init() unconditionally starts a permanent background
		// goroutine (eventExecutor.startEventListener in event_executor.go)
		// the moment the openfeature package is imported, before this test,
		// this provider, or even New, ever runs - but providertest's
		// NoGoroutineLeak case snapshots the goroutines already running
		// (goleak.IgnoreCurrent) before constructing the provider under test,
		// so that pre-existing goroutine is absorbed rather than mistaken for
		// a leak. This provider starts no goroutine of its own anywhere in
		// Resolve/evaluate (grep openfeature.go for "go func" and there is
		// none), so the case still runs, unmodified, and still catches a real
		// leak in this package's own code.
	})
}
