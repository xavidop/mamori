package supabase

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// TestStatusToKind maps every status PostgREST is documented to return onto the
// kind this provider must report.
//
// This is the requirement CONTRIBUTING calls out separately from the
// conformance kit: the kit proves a classified error survives transit, this
// proves the classification exists at all. A provider that reformatted its
// error with %v instead of %w would pass neither, but a provider whose mapping
// was simply wrong would still pass the kit, because the kit derives the status
// it injects from the very mapping under test.
//
// The statuses are PostgREST's own documented set. 406 is the one worth
// pointing at: PostgREST returns it for a schema that is not in db-schemas
// (PGRST106), which is exactly what an operator gets for naming the vault
// schema, and it must be a terminal kind rather than a transient one.
func TestStatusToKind(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   mamori.Kind
	}{
		{"bad request", http.StatusBadRequest, mamori.KindInvalid},
		{"unauthorized", http.StatusUnauthorized, mamori.KindUnauthenticated},
		{"payment required", http.StatusPaymentRequired, mamori.KindUnavailable},
		{"forbidden", http.StatusForbidden, mamori.KindPermissionDenied},
		{"not found", http.StatusNotFound, mamori.KindNotFound},
		{"method not allowed", http.StatusMethodNotAllowed, mamori.KindUnavailable},
		{"not acceptable", http.StatusNotAcceptable, mamori.KindUnavailable},
		{"unsupported media type", http.StatusUnsupportedMediaType, mamori.KindUnavailable},
		{"range not satisfiable", http.StatusRequestedRangeNotSatisfiable, mamori.KindUnavailable},
		{"internal error", http.StatusInternalServerError, mamori.KindUnavailable},
		{"service unavailable", http.StatusServiceUnavailable, mamori.KindUnavailable},
		{"gateway timeout", http.StatusGatewayTimeout, mamori.KindUnavailable},
		// Not in PostgREST's own list, but reachable through Supabase's gateway
		// or any proxy in front of it, and both must stay transient.
		{"too many requests", http.StatusTooManyRequests, mamori.KindRateLimited},
		{"bad gateway", http.StatusBadGateway, mamori.KindUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			f := newFake()
			f.set("TOKEN", "value")
			f.fail(tt.status)

			_, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
			if err == nil {
				t.Fatalf("status %d produced no error", tt.status)
			}
			if got := mamori.ErrorKind(err); got != tt.want {
				t.Fatalf("status %d kind = %s, want %s", tt.status, got, tt.want)
			}
		})
	}
}

// TestEmptyArrayIsNotFoundAndNothingElse pins the single most consequential
// behaviour in this package, and the one that is not a status code at all.
//
// PostgREST reports "no such row" as 200 with an empty array. mamori's
// ErrNotFound is the ONE kind that changes behaviour rather than only reporting:
// it is what makes a field's default: and optional handling apply instead of
// failing the whole snapshot. Asserting merely "an error occurred" would pass if
// the empty array were classified ErrInvalid, and that difference decides
// whether a field's default is applied or a process fails to start. So every
// other kind is excluded by name.
func TestEmptyArrayIsNotFoundAndNothingElse(t *testing.T) {
	clearEnv(t)
	f := newFake()
	// A different secret exists, so the relation and the schema are both fine
	// and the ONLY reason for the empty array is that this name has no row.
	f.set("OTHER", "value")

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://ABSENT"))
	if err == nil {
		t.Fatal("an absent secret resolved successfully; PostgREST's empty array was read as a value")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound; a field's default: would not be applied", err)
	}
	for _, other := range []error{
		mamori.ErrInvalid,
		mamori.ErrUnauthenticated,
		mamori.ErrPermissionDenied,
		mamori.ErrRateLimited,
		mamori.ErrUnavailable,
	} {
		if errors.Is(err, other) {
			t.Errorf("the empty array also satisfies errors.Is(err, %v); the kind must be exactly not_found", other)
		}
	}
	if got := mamori.ErrorKind(err); got != mamori.KindNotFound {
		t.Errorf("kind = %s, want not_found", got)
	}
}

// TestEmptyArrayAppliesTheFieldDefault is the same mapping observed from where
// it actually matters: through mamori itself, on a field that declares a
// default. This is what TestEmptyArrayIsNotFoundAndNothingElse is a proxy for,
// and it would fail for a provider that returned any other kind.
func TestEmptyArrayAppliesTheFieldDefault(t *testing.T) {
	clearEnv(t)
	f := newFake()

	type config struct {
		Token string `source:"supabase://ABSENT" default:"fallback-value"`
	}
	cfg, err := mamori.Load[config](context.Background(), mamori.WithProvider(f.provider()))
	if err != nil {
		t.Fatalf("Load: %v; an absent secret must fall back to default: rather than fail the snapshot", err)
	}
	if cfg.Token != "fallback-value" {
		t.Fatalf("Token = %q, want the declared default", cfg.Token)
	}
}

// TestNotFoundIsNotReportedForAMisconfiguration is the converse guard. Anything
// that is a misconfiguration rather than an absent secret must NOT be
// not_found, because mamori would then silently apply the field's default and
// hide a broken setup behind a value that looks deliberate.
func TestNotFoundIsNotReportedForAMisconfiguration(t *testing.T) {
	clearEnv(t)

	tests := []struct {
		name  string
		build func(*fakeSupabase) *Provider
		ref   string
	}{
		{
			name:  "empty secret name",
			build: func(f *fakeSupabase) *Provider { return f.provider() },
			ref:   "supabase://",
		},
		{
			name:  "relation name with a slash",
			build: func(f *fakeSupabase) *Provider { return f.provider(WithView("public/decrypted_secrets")) },
			ref:   "supabase://TOKEN",
		},
		{
			name:  "no project URL",
			build: func(f *fakeSupabase) *Provider { return New(WithServiceKey(testServiceKey)) },
			ref:   "supabase://TOKEN",
		},
		{
			name:  "no service key",
			build: func(f *fakeSupabase) *Provider { return New(WithProjectURL(testProjectURL)) },
			ref:   "supabase://TOKEN",
		},
		{
			name:  "relation with no decrypted_secret column",
			build: func(f *fakeSupabase) *Provider { f.omitSecretColumn = true; return f.provider() },
			ref:   "supabase://TOKEN",
		},
		{
			name:  "relation with more than one row per name",
			build: func(f *fakeSupabase) *Provider { f.duplicate = true; return f.provider() },
			ref:   "supabase://TOKEN",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.set("TOKEN", "value")
			p := tt.build(f)

			ref, err := mamori.ParseRef(tt.ref)
			if err != nil {
				// An unparseable ref never reaches the provider, which is
				// itself a non-not_found rejection.
				return
			}
			_, err = p.Resolve(context.Background(), ref)
			if err == nil {
				t.Fatal("a misconfiguration resolved successfully")
			}
			if errors.Is(err, mamori.ErrNotFound) {
				t.Fatalf("err = %v is not_found; mamori would silently apply the field's default over a broken setup", err)
			}
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestErrorDetailSurfacesTheVendorMessageAndCode pins that httpcore's
// ErrorDetail hook is wired and lifts PostgREST's "message" and "code", so an
// operator sees WHY a request was refused rather than only its status. PGRST106
// is exactly what an operator who skipped the setup hits, and the status alone
// (406) says nothing useful.
func TestErrorDetailSurfacesTheVendorMessageAndCode(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("TOKEN", "value")
	f.fail(http.StatusForbidden)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "injected read failure") {
		t.Errorf("err = %q, want it to carry the vendor message", err)
	}
	if !strings.Contains(err.Error(), "PGRSTINJ") {
		t.Errorf("err = %q, want it to carry the vendor code", err)
	}
}

// TestErrorDetailShape pins the hook's field selection directly, including the
// two fields it must never read, and its fallbacks. It calls errorDetail rather
// than going through Resolve so that a field's absence is unambiguous.
func TestErrorDetailShape(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "message and code",
			body: `{"code":"PGRST106","message":"The schema must be one of the following: public","details":null,"hint":null}`,
			want: "The schema must be one of the following: public (PGRST106)",
		},
		{"message only", `{"message":"boom"}`, "boom"},
		{"code only", `{"code":"42501"}`, "42501"},
		{"neither", `{"details":"x","hint":"y"}`, ""},
		{"not json", `definitely not json`, ""},
		// The body a successful read returns is a JSON ARRAY, which unmarshals
		// into a struct with an error, so even if this hook were somehow called
		// for one it could not echo a row.
		{"a row array", `[{"decrypted_secret":"s3cr3t"}]`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorDetail(http.StatusBadRequest, []byte(tt.body)); got != tt.want {
				t.Fatalf("errorDetail = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestErrorDetailNeverReadsDetailsOrHint pins the deliberate exclusion of the
// two PostgREST fields that can carry data. "details" is documented as
// rendering the offending row, and for this provider a row IS the decrypted
// secret; "hint" is free-form text with no guarantee bounding what it quotes.
func TestErrorDetailNeverReadsDetailsOrHint(t *testing.T) {
	const leak = "s3cr3t-value-that-must-not-leak"
	body := []byte(`{"code":"42501","message":"permission denied",` +
		`"details":"Failing row contains (` + leak + `)","hint":"apikey was ` + leak + `"}`)

	got := errorDetail(http.StatusForbidden, body)
	if strings.Contains(got, leak) {
		t.Fatalf("errorDetail surfaced a details/hint field: %q", got)
	}
	if got != "permission denied (42501)" {
		t.Fatalf("errorDetail = %q, want only the message and code", got)
	}
}

// TestResolveErrorCarriesNoSecretValue pins that a failing resolve never puts
// the response body into the error, since on the success path that body
// contains the decrypted secret. The fake's error envelope deliberately echoes
// the value in "details", so this assertion is falsifiable: a provider that
// surfaced the whole body, or that read "details", would fail here.
func TestResolveErrorCarriesNoSecretValue(t *testing.T) {
	clearEnv(t)
	const secretValue = "s3cr3t-value-that-must-not-leak"
	f := newFake()
	f.set("TOKEN", secretValue)
	f.fail(http.StatusForbidden)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Fatalf("the secret value leaked into %q", err)
	}
}

// TestResolveErrorCarriesNoServiceKey pins the same rule for the credential on
// every request, and it is the higher stake of the two: a service-role key
// bypasses row-level security on the WHOLE project, so it is the key to every
// secret rather than to one.
//
// The fake echoes that key back in its "hint" field, which is what makes this
// test able to fail. A credential-hygiene test whose fake could not produce a
// leak asserts nothing at all.
func TestResolveErrorCarriesNoServiceKey(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("TOKEN", "value")

	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusInternalServerError,
	} {
		f.fail(status)
		_, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
		if err == nil {
			t.Fatalf("status %d produced no error", status)
		}
		if strings.Contains(err.Error(), testServiceKey) {
			t.Fatalf("the service-role key leaked into %q", err)
		}
	}
}

// TestResolveMalformedResponseNeverEchoesTheBody pins the deliberate absence of
// a wrapped decode cause: encoding/json quotes the offending byte in a syntax
// error, and a 200 body here contains the decrypted secret.
func TestResolveMalformedResponseNeverEchoesTheBody(t *testing.T) {
	clearEnv(t)
	const garbage = "definitely-not-json-and-possibly-the-secret"
	f := newFake()
	p := f.provider(WithHTTPClient(&http.Client{Transport: roundTripFunc(
		func(_ *http.Request) (*http.Response, error) {
			return jsonResp(http.StatusOK, garbage), nil
		})}))

	_, err := p.Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("a malformed 200 reported ErrNotFound; mamori would silently apply the field's default")
	}
	if strings.Contains(err.Error(), garbage[:10]) {
		t.Fatalf("the response body reached the error: %q", err)
	}
}

// TestResolveObjectResponseIsInvalid pins that a 200 carrying a bare JSON
// OBJECT rather than an array fails cleanly. PostgREST answers a filtered select
// with an array, so an object means something in front of it (a proxy, an error
// page, a misrouted request) replied instead, and that is not an absent secret.
func TestResolveObjectResponseIsInvalid(t *testing.T) {
	clearEnv(t)
	f := newFake()
	p := f.provider(WithHTTPClient(&http.Client{Transport: roundTripFunc(
		func(_ *http.Request) (*http.Response, error) {
			return jsonResp(http.StatusOK, `{"decrypted_secret":"s3cr3t"}`), nil
		})}))

	_, err := p.Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("an object response reported ErrNotFound")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("the response body reached the error: %q", err)
	}
}
