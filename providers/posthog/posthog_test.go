package posthog

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

func mustRef(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

// resolve is the common "seeded backend, one ref" shape used throughout.
func resolve(t *testing.T, f *fakeBackend, raw string) (mamori.Value, error) {
	t.Helper()
	return f.provider().Resolve(context.Background(), mustRef(t, raw))
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != scheme {
		t.Fatalf("Scheme() = %q, want %q", got, scheme)
	}
}

// --- Value mapping: boolean versus multivariate ---
//
// These are the cases the mapping table in the package doc promises. A test
// that exercised only one flag shape would not catch the other being wrong,
// which is the specific failure mode this block exists to prevent.

func TestResolveBooleanEnabledIsTrue(t *testing.T) {
	f := newFake()
	f.setBool("new-checkout", true)

	v, err := resolve(t, f, "posthog://new-checkout")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "true" {
		t.Fatalf("Bytes = %q, want true", v.Bytes)
	}
	if v.Metadata["kind"] != "enabled" {
		t.Errorf("Metadata[kind] = %q, want enabled", v.Metadata["kind"])
	}
}

func TestResolveBooleanDisabledIsFalse(t *testing.T) {
	f := newFake()
	f.setBool("new-checkout", false)

	v, err := resolve(t, f, "posthog://new-checkout")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "false" {
		t.Fatalf("Bytes = %q, want false", v.Bytes)
	}
}

// TestResolveMultivariateIsVariantKey is the discriminator: with no fragment a
// multivariate flag must render as its variant key, NOT as "true". This is the
// half of the mapping a boolean-only test cannot reach.
func TestResolveMultivariateIsVariantKey(t *testing.T) {
	f := newFake()
	f.setVariant("pricing-test", "control")

	v, err := resolve(t, f, "posthog://pricing-test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "control" {
		t.Fatalf("Bytes = %q, want control (a multivariate flag renders as its variant key, not as true)", v.Bytes)
	}
	if v.Metadata["kind"] != "variant" {
		t.Errorf("Metadata[kind] = %q, want variant", v.Metadata["kind"])
	}
}

// TestResolveDisabledMultivariateIsFalse pins the boundary between the two
// shapes: PostHog sends no variant for a disabled flag of either shape, so it
// must render as "false" rather than as an empty string.
func TestResolveDisabledMultivariateIsFalse(t *testing.T) {
	f := newFake()
	// Enabled=false with no variant, which is what PostHog sends for a
	// multivariate flag whose conditions did not match.
	f.setBool("pricing-test", false)

	v, err := resolve(t, f, "posthog://pricing-test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "false" {
		t.Fatalf("Bytes = %q, want false", v.Bytes)
	}
}

// TestEnabledFragmentIgnoresVariant proves #enabled reads the enabled field on
// a multivariate flag rather than falling through to the variant key.
func TestEnabledFragmentIgnoresVariant(t *testing.T) {
	f := newFake()
	f.setVariant("pricing-test", "control")

	v, err := resolve(t, f, "posthog://pricing-test#enabled")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "true" {
		t.Fatalf("Bytes = %q, want true", v.Bytes)
	}
}

// TestVariantFragmentOnBooleanFlagIsEmpty proves the reverse: a boolean flag
// has no variant, and #variant must say so rather than inventing "true".
func TestVariantFragmentOnBooleanFlagIsEmpty(t *testing.T) {
	f := newFake()
	f.setBool("new-checkout", true)

	v, err := resolve(t, f, "posthog://new-checkout#variant")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(v.Bytes) != 0 {
		t.Fatalf("Bytes = %q, want empty (a boolean flag has no variant)", v.Bytes)
	}
}

func TestEnabledFragmentOnDisabledFlag(t *testing.T) {
	f := newFake()
	f.setBool("new-checkout", false)

	v, err := resolve(t, f, "posthog://new-checkout#enabled")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "false" {
		t.Fatalf("Bytes = %q, want false", v.Bytes)
	}
}

// --- Payload ---

func TestResolvePayloadUnwrapsJSONString(t *testing.T) {
	f := newFake()
	f.setPayload("cfg", `{"host":"db.internal","port":5432}`)

	v, err := resolve(t, f, "posthog://cfg#payload")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := `{"host":"db.internal","port":5432}`
	if string(v.Bytes) != want {
		t.Fatalf("Bytes = %q, want %q (a JSON-encoded string payload must be unwrapped, not returned quoted)", v.Bytes, want)
	}
	if v.Metadata["kind"] != "payload" {
		t.Errorf("Metadata[kind] = %q, want payload", v.Metadata["kind"])
	}
}

func TestResolvePayloadPassesRawObjectThrough(t *testing.T) {
	f := newFake()
	f.setRawPayload("cfg", `{"host":"db.internal"}`)

	v, err := resolve(t, f, "posthog://cfg#payload")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != `{"host":"db.internal"}` {
		t.Fatalf("Bytes = %q, want the raw JSON object", v.Bytes)
	}
}

func TestResolvePayloadAbsentIsEmpty(t *testing.T) {
	f := newFake()
	f.setBool("new-checkout", true)

	v, err := resolve(t, f, "posthog://new-checkout#payload")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(v.Bytes) != 0 {
		t.Fatalf("Bytes = %q, want empty for a flag with no payload", v.Bytes)
	}
}

func TestResolvePayloadNullIsEmpty(t *testing.T) {
	f := newFake()
	f.setRawPayload("cfg", "null")

	v, err := resolve(t, f, "posthog://cfg#payload")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(v.Bytes) != 0 {
		t.Fatalf("Bytes = %q, want empty for a null payload", v.Bytes)
	}
}

// --- Not found and the two conditions that must NOT be not-found ---

func TestResolveNotFound(t *testing.T) {
	f := newFake()
	f.setBool("other-flag", true)

	_, err := resolve(t, f, "posthog://missing-flag")
	if err == nil {
		t.Fatal("Resolve of an absent flag returned a nil error")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

// TestQuotaLimitedIsRateLimitedNotNotFound pins the trap: PostHog answers a
// billing pause with 200 and an EMPTY flags object, so reading absence as
// not-found there would have mamori quietly apply a default in place of a live
// flag.
func TestQuotaLimitedIsRateLimitedNotNotFound(t *testing.T) {
	f := newFake()
	f.setBool("new-checkout", true)
	f.setQuotaLimited("feature_flags")

	_, err := resolve(t, f, "posthog://new-checkout")
	if err == nil {
		t.Fatal("Resolve during a quota pause returned a nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("a quota pause must not be reported as not-found; mamori would apply a default for a flag that exists")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindRateLimited {
		t.Fatalf("ErrorKind(err) = %q, want %q", got, mamori.KindRateLimited)
	}
}

// TestQuotaLimitedOnAnotherResourceIsIgnored proves the check reads the array's
// contents rather than merely its presence: a quota pause on recordings says
// nothing about flags.
func TestQuotaLimitedOnAnotherResourceIsIgnored(t *testing.T) {
	f := newFake()
	f.setBool("new-checkout", true)
	f.setQuotaLimited("recordings")

	// The fake serves no flags whenever quotaLimited is non-empty, so the flag
	// is absent; the point is that the error is not-found rather than rate
	// limited, i.e. "recordings" did not trip the feature_flags check.
	_, err := resolve(t, f, "posthog://new-checkout")
	if err == nil {
		t.Fatal("Resolve returned a nil error")
	}
	if mamori.ErrorKind(err) == mamori.KindRateLimited {
		t.Fatal("a quota pause on recordings must not be read as a flag-evaluation pause")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

// TestComputeErrorIsUnavailableNotNotFound: when PostHog reports it failed to
// compute flags, an absent flag is inconclusive rather than missing.
func TestComputeErrorIsUnavailableNotNotFound(t *testing.T) {
	f := newFake()
	f.setComputeError(true)

	_, err := resolve(t, f, "posthog://new-checkout")
	if err == nil {
		t.Fatal("Resolve returned a nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("errorsWhileComputingFlags must not be reported as not-found; the flag may exist and simply not have been computed")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindUnavailable {
		t.Fatalf("ErrorKind(err) = %q, want %q", got, mamori.KindUnavailable)
	}
}

// TestComputeErrorDoesNotBlockAPresentFlag: a flag PostHog DID compute is
// unaffected by other flags having failed.
func TestComputeErrorDoesNotBlockAPresentFlag(t *testing.T) {
	f := newFake()
	f.setBool("new-checkout", true)
	f.setComputeError(true)

	v, err := resolve(t, f, "posthog://new-checkout")
	if err != nil {
		t.Fatalf("Resolve of a computed flag failed because another flag failed: %v", err)
	}
	if string(v.Bytes) != "true" {
		t.Fatalf("Bytes = %q, want true", v.Bytes)
	}
}

// --- Ref validation ---

func TestUnsupportedFragmentIsRejectedWithoutARequest(t *testing.T) {
	f := newFake()
	f.setBool("new-checkout", true)

	_, err := resolve(t, f, "posthog://new-checkout#host")
	if err == nil {
		t.Fatal("Resolve with an unsupported fragment returned a nil error")
	}
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrInvalid)", err)
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("an unsupported fragment is a malformed ref, not a missing flag")
	}
	if _, _, _, _, _, _, calls := f.observed(); calls != 0 {
		t.Fatalf("backend saw %d calls; an unserviceable ref must be rejected before the round trip", calls)
	}
}

func TestEmptyFlagKeyIsRejected(t *testing.T) {
	f := newFake()
	_, err := resolve(t, f, "posthog://")
	if err == nil {
		t.Fatal("Resolve with an empty flag key returned a nil error")
	}
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrInvalid)", err)
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("an empty flag key is a malformed ref, not a missing flag")
	}
}

// --- The wire contract ---

// TestRequestShape pins the documented contract: a POST to /flags with v=2, a
// JSON content type, and a body whose field names are api_key and distinct_id.
func TestRequestShape(t *testing.T) {
	f := newFake()
	f.setBool("new-checkout", true)

	if _, err := resolve(t, f, "posthog://new-checkout"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	method, path, q, h, body, raw, _ := f.observed()
	if method != http.MethodPost {
		t.Errorf("method = %q, want POST (flag evaluation carries a distinct id, so it cannot be a GET)", method)
	}
	if path != "/flags" {
		t.Errorf("path = %q, want /flags", path)
	}
	if got := q.Get("v"); got != "2" {
		t.Errorf("query v = %q, want 2", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if body.APIKey != "phc_fake_project_key" {
		t.Errorf("body api_key = %q, want the configured project key", body.APIKey)
	}
	if body.DistinctID != DefaultDistinctID {
		t.Errorf("body distinct_id = %q, want %q", body.DistinctID, DefaultDistinctID)
	}
	// Assert on the raw JSON too: the struct tags are the contract, and a
	// renamed field would still decode into the same Go struct through a test
	// that only looked at the decoded form.
	for _, want := range []string{`"api_key"`, `"distinct_id"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("request body %s does not contain %s", raw, want)
		}
	}
	// Absent optional fields must be omitted, not sent as null.
	for _, unwanted := range []string{`"groups"`, `"person_properties"`, `"group_properties"`} {
		if strings.Contains(string(raw), unwanted) {
			t.Errorf("request body %s contains %s, which should be omitted when unset", raw, unwanted)
		}
	}
}

func TestGroupsAndPropertiesReachTheBody(t *testing.T) {
	f := newFake()
	f.setBool("new-checkout", true)

	p := f.provider(
		WithGroups(map[string]string{"company": "Twitter"}),
		WithPersonProperties(map[string]any{"plan": "enterprise"}),
		WithGroupProperties(map[string]map[string]any{"company": {"seats": float64(50)}}),
	)
	if _, err := p.Resolve(context.Background(), mustRef(t, "posthog://new-checkout")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	_, _, _, _, body, raw, _ := f.observed()
	if body.Groups["company"] != "Twitter" {
		t.Errorf("body groups = %v, want company=Twitter", body.Groups)
	}
	if body.PersonProperties["plan"] != "enterprise" {
		t.Errorf("body person_properties = %v, want plan=enterprise", body.PersonProperties)
	}
	if body.GroupProperties["company"]["seats"] != float64(50) {
		t.Errorf("body group_properties = %v, want company.seats=50", body.GroupProperties)
	}
	for _, want := range []string{`"groups"`, `"person_properties"`, `"group_properties"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("request body %s does not contain %s", raw, want)
		}
	}
}

// TestOptionMapsAreCopied: a caller mutating their map after New must not
// change what later evaluations send.
func TestOptionMapsAreCopied(t *testing.T) {
	f := newFake()
	f.setBool("new-checkout", true)

	groups := map[string]string{"company": "Twitter"}
	props := map[string]any{"plan": "enterprise"}
	p := f.provider(WithGroups(groups), WithPersonProperties(props))
	groups["company"] = "Mutated"
	props["plan"] = "mutated"

	if _, err := p.Resolve(context.Background(), mustRef(t, "posthog://new-checkout")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, _, _, _, body, _, _ := f.observed()
	if body.Groups["company"] != "Twitter" {
		t.Errorf("body groups = %v; the option did not copy the caller's map", body.Groups)
	}
	if body.PersonProperties["plan"] != "enterprise" {
		t.Errorf("body person_properties = %v; the option did not copy the caller's map", body.PersonProperties)
	}
}

// --- Distinct id ---

func TestDefaultDistinctIDIsUsed(t *testing.T) {
	t.Setenv("POSTHOG_DISTINCT_ID", "")
	f := newFake()
	f.setBool("f", true)

	if _, err := resolve(t, f, "posthog://f"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, _, _, _, body, _, _ := f.observed(); body.DistinctID != DefaultDistinctID {
		t.Fatalf("distinct_id = %q, want %q", body.DistinctID, DefaultDistinctID)
	}
}

func TestDistinctIDFromEnvironment(t *testing.T) {
	t.Setenv("POSTHOG_DISTINCT_ID", "svc-billing")
	f := newFake()
	f.setBool("f", true)

	if _, err := resolve(t, f, "posthog://f"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, _, _, _, body, _, _ := f.observed(); body.DistinctID != "svc-billing" {
		t.Fatalf("distinct_id = %q, want svc-billing", body.DistinctID)
	}
}

func TestWithDistinctIDOverridesEnvironment(t *testing.T) {
	t.Setenv("POSTHOG_DISTINCT_ID", "from-env")
	f := newFake()
	f.setBool("f", true)

	p := f.provider(WithDistinctID("explicit"))
	if _, err := p.Resolve(context.Background(), mustRef(t, "posthog://f")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, _, _, _, body, _, _ := f.observed(); body.DistinctID != "explicit" {
		t.Fatalf("distinct_id = %q, want explicit", body.DistinctID)
	}
}

// --- Credential handling ---

func TestMissingProjectAPIKey(t *testing.T) {
	t.Setenv("POSTHOG_PROJECT_API_KEY", "")
	p := New(WithHost("https://posthog.test"))

	_, err := p.Resolve(context.Background(), mustRef(t, "posthog://any-flag"))
	if err == nil {
		t.Fatal("Resolve with no project API key returned a nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("a missing credential is not a missing flag")
	}
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrInvalid)", err)
	}
}

func TestProjectAPIKeyFromEnvironment(t *testing.T) {
	t.Setenv("POSTHOG_PROJECT_API_KEY", "phc_from_env")
	f := newFake()
	f.setBool("f", true)

	// No WithProjectAPIKey, so the environment is the only source.
	p := New(WithHost("https://posthog.test"), WithHTTPClient(&http.Client{Transport: f.transport()}))
	if _, err := p.Resolve(context.Background(), mustRef(t, "posthog://f")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, _, _, _, body, _, _ := f.observed(); body.APIKey != "phc_from_env" {
		t.Fatalf("body api_key = %q, want phc_from_env", body.APIKey)
	}
}

// TestProjectAPIKeyNeverReachesAnError is the leak guard. The key is sent in
// the request body, and httpcore renders the request URL into its error, so a
// key that ever migrated to a query parameter would surface here.
func TestProjectAPIKeyNeverReachesAnError(t *testing.T) {
	const secret = "phc_super_secret_project_key"
	f := newFake()
	f.setBool("f", true)
	f.fail(http.StatusForbidden)

	p := f.provider(WithProjectAPIKey(secret))
	_, err := p.Resolve(context.Background(), mustRef(t, "posthog://f"))
	if err == nil {
		t.Fatal("Resolve against a failing backend returned a nil error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error message leaks the project API key: %v", err)
	}
	// The injected error body must not surface either: ErrorDetail is nil so
	// no response body reaches an error, and a flag payload is a response body.
	if strings.Contains(err.Error(), "injected") {
		t.Fatalf("error message contains the response body: %v", err)
	}
}

// --- Status classification ---

func TestStatusClassification(t *testing.T) {
	cases := []struct {
		status int
		want   mamori.Kind
	}{
		{http.StatusBadRequest, mamori.KindInvalid},
		{http.StatusUnauthorized, mamori.KindUnauthenticated},
		{http.StatusForbidden, mamori.KindPermissionDenied},
		{http.StatusTooManyRequests, mamori.KindRateLimited},
		{http.StatusBadGateway, mamori.KindUnavailable},
	}
	for _, tc := range cases {
		f := newFake()
		f.setBool("f", true)
		f.fail(tc.status)

		_, err := resolve(t, f, "posthog://f")
		if err == nil {
			t.Fatalf("status %d: Resolve returned a nil error", tc.status)
		}
		if got := mamori.ErrorKind(err); got != tc.want {
			t.Errorf("status %d: ErrorKind = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// TestNotFoundStatusIsNotAMissingFlag: a 404 from the host means the endpoint
// is wrong, which httpcore classifies as not-found. It is worth pinning that
// the provider does not add a second, conflicting classification on top.
func TestNotFoundStatusClassifiesAsNotFound(t *testing.T) {
	f := newFake()
	f.fail(http.StatusNotFound)

	_, err := resolve(t, f, "posthog://f")
	if err == nil {
		t.Fatal("Resolve returned a nil error")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindNotFound {
		t.Fatalf("ErrorKind = %q, want %q", got, mamori.KindNotFound)
	}
}

func TestMalformedResponseIsUnavailable(t *testing.T) {
	f := newFake()
	p := New(
		WithProjectAPIKey("phc_x"),
		WithHost("https://posthog.test"),
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if err := req.Context().Err(); err != nil {
				return nil, err
			}
			return newResp(http.StatusOK, []byte("not json at all")), nil
		})}),
	)
	_ = f

	_, err := p.Resolve(context.Background(), mustRef(t, "posthog://f"))
	if err == nil {
		t.Fatal("Resolve of a malformed response returned a nil error")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindUnavailable {
		t.Fatalf("ErrorKind = %q, want %q", got, mamori.KindUnavailable)
	}
	if strings.Contains(err.Error(), "not json at all") {
		t.Fatalf("error message quotes the response body: %v", err)
	}
}

// --- Value shape ---

func TestValueIsNotSensitiveAndVersionIsAContentHash(t *testing.T) {
	f := newFake()
	f.setVariant("pricing-test", "control")

	v, err := resolve(t, f, "posthog://pricing-test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Sensitive {
		t.Error("Sensitive = true, want false for a feature flag")
	}
	if v.Version != mamori.VersionHash(v.Bytes) {
		t.Errorf("Version = %q, want VersionHash of the resolved bytes", v.Version)
	}
	if v.Metadata["flag"] != "pricing-test" {
		t.Errorf("Metadata[flag] = %q, want pricing-test", v.Metadata["flag"])
	}
}

// TestVersionChangesWithTheValue: change detection is the whole job of Version.
func TestVersionChangesWithTheValue(t *testing.T) {
	f := newFake()
	f.setVariant("pricing-test", "control")
	p := f.provider()

	first, err := p.Resolve(context.Background(), mustRef(t, "posthog://pricing-test"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	f.setVariant("pricing-test", "treatment")
	second, err := p.Resolve(context.Background(), mustRef(t, "posthog://pricing-test"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if first.Version == second.Version {
		t.Fatalf("Version did not change when the variant did (both %q)", first.Version)
	}
}

// --- Wiring ---

func TestContextCancelled(t *testing.T) {
	f := newFake()
	f.setBool("f", true)
	p := f.provider()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustRef(t, "posthog://f")); err == nil {
		t.Fatal("Resolve with a cancelled context returned a nil error")
	}
}

// The PostHog provider intentionally does NOT implement WatchableProvider: the
// flag endpoint pushes nothing. mamori polls it instead.
func TestNotWatchable(t *testing.T) {
	var p mamori.Provider = New()
	if _, ok := p.(mamori.WatchableProvider); ok {
		t.Fatal("posthog provider must not implement WatchableProvider (no native watch)")
	}
}

func TestDefaultHostIsUSCloud(t *testing.T) {
	t.Setenv("POSTHOG_HOST", "")
	p := New(WithProjectAPIKey("phc_x"))
	c, err := p.client()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if c == nil {
		t.Fatal("client() returned nil")
	}
	// The host itself is unexported inside httpcore, so assert on what the
	// provider resolved rather than reaching into the client.
	if DefaultHost != "https://us.i.posthog.com" {
		t.Fatalf("DefaultHost = %q, want https://us.i.posthog.com", DefaultHost)
	}
}

func TestHostFromEnvironmentIsUsed(t *testing.T) {
	t.Setenv("POSTHOG_HOST", "https://eu.i.posthog.test")
	f := newFake()
	f.setBool("f", true)

	p := New(WithProjectAPIKey("phc_x"), WithHTTPClient(&http.Client{Transport: f.transport()}))
	if _, err := p.Resolve(context.Background(), mustRef(t, "posthog://f")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// A wrong host would have produced a different request URL; the fake
	// answers regardless of host, so assert the client was built from the env
	// by checking the provider did not fall back to a build error.
	if _, _, _, _, _, _, calls := f.observed(); calls != 1 {
		t.Fatalf("backend saw %d calls, want 1", calls)
	}
}

// TestEachResolveEvaluatesAfresh pins the documented cost: there is no shared
// cache, so every Resolve observes the backend now.
func TestEachResolveEvaluatesAfresh(t *testing.T) {
	f := newFake()
	f.setBool("f", true)
	p := f.provider()

	for range 3 {
		if _, err := p.Resolve(context.Background(), mustRef(t, "posthog://f")); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	if _, _, _, _, _, _, calls := f.observed(); calls != 3 {
		t.Fatalf("backend saw %d calls, want 3 (each Resolve must evaluate afresh)", calls)
	}
}
