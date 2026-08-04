package heroku

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// TestStatusToKind maps every status the Platform API reference documents onto
// the kind this provider reports for it.
//
// This is the requirement CONTRIBUTING calls out separately from the
// conformance kit: the kit proves a classified error survives transit, this
// proves the classification exists at all. A provider whose mapping was simply
// wrong would still pass the kit, because the kit derives the status it injects
// from the very mapping under test (httpcore.StatusForKind).
//
// Three rows are honest rather than ideal, and are marked so below: 402, 406
// and 410 are terminal in practice but land in httpcore's transient default, so
// mamori will back off and retry them. That is accepted rather than overridden.
// httpcore exists so one table classifies every HTTP provider, and a
// provider-local override is precisely the drift its README warns about - it
// would also desynchronise this package from httpcore.StatusForKind, the
// inverse the conformance kit's Fail hook depends on. See the module README's
// "Error classification" section.
func TestStatusToKind(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   mamori.Kind
	}{
		{"400 bad_request", http.StatusBadRequest, mamori.KindInvalid},
		{"401 unauthorized", http.StatusUnauthorized, mamori.KindUnauthenticated},
		// 402 delinquent: the account owes money. Terminal until someone pays,
		// but it does heal without a code change, so "unavailable" is at least
		// not a lie.
		{"402 delinquent", http.StatusPaymentRequired, mamori.KindUnavailable},
		{"403 forbidden or suspended", http.StatusForbidden, mamori.KindPermissionDenied},
		// 404 is the load-bearing row: it is the only kind that changes what
		// mamori DOES rather than what it reports. See the next test.
		{"404 not_found", http.StatusNotFound, mamori.KindNotFound},
		// 406 not_acceptable: the version header is missing or names a version
		// the API no longer serves. This provider always sends version=3, so
		// reaching this means Heroku retired it and no retry will help.
		{"406 not_acceptable", http.StatusNotAcceptable, mamori.KindUnavailable},
		{"409 conflict", http.StatusConflict, mamori.KindUnavailable},
		{"410 gone", http.StatusGone, mamori.KindUnavailable},
		{"416 requested_range_not_satisfiable", http.StatusRequestedRangeNotSatisfiable, mamori.KindUnavailable},
		// 422 invalid_params / verification_needed. httpcore names 422
		// explicitly rather than leaving it to the transient default, because a
		// well-formed but semantically wrong request can never be fixed by
		// retrying.
		{"422 invalid_params", http.StatusUnprocessableEntity, mamori.KindInvalid},
		{"429 rate_limit", http.StatusTooManyRequests, mamori.KindRateLimited},
		{"500 internal", http.StatusInternalServerError, mamori.KindUnavailable},
		{"503 unavailable", http.StatusServiceUnavailable, mamori.KindUnavailable},
		// Not in the vendor's list, but reachable through any proxy or load
		// balancer in front of api.heroku.com, and it must stay transient.
		{"502 from a proxy", http.StatusBadGateway, mamori.KindUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			f := newFake()
			f.set(testApp, "TOKEN", "value")
			f.fail(tc.status)

			_, err := f.provider().Resolve(context.Background(), mustRef(t, "heroku://TOKEN"))
			if err == nil {
				t.Fatalf("status %d produced no error", tc.status)
			}
			if got := mamori.ErrorKind(err); got != tc.want {
				t.Fatalf("status %d kind = %s, want %s", tc.status, got, tc.want)
			}
		})
	}
}

// TestNotFoundIsNotFoundAndNothingElse pins the single most consequential row
// of the table above on its own.
//
// ErrNotFound is the ONE kind that changes mamori's behaviour rather than only
// its reporting: it is what makes a field's `default:` and optional handling
// apply instead of failing the whole snapshot. A 404 misclassified as anything
// else turns an absent optional app into a hard startup failure; anything else
// misclassified as 404 turns a real failure into a silently defaulted value.
func TestNotFoundIsNotFoundAndNothingElse(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "TOKEN", "value")
	f.fail(http.StatusNotFound)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "heroku://TOKEN"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	for _, other := range []error{
		mamori.ErrInvalid,
		mamori.ErrUnauthenticated,
		mamori.ErrPermissionDenied,
		mamori.ErrRateLimited,
		mamori.ErrUnavailable,
	} {
		if errors.Is(err, other) {
			t.Errorf("a 404 also satisfies errors.Is(err, %v); the kind must be exactly not_found", other)
		}
	}
}

// TestErrorDetailSurfacesTheVendorErrorID pins that httpcore's ErrorDetail hook
// is wired and lifts Heroku's "id", so an operator sees WHY a request was
// refused rather than only its status.
func TestErrorDetailSurfacesTheVendorErrorID(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "TOKEN", "value")
	f.fail(http.StatusForbidden)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "heroku://TOKEN"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "injected_failure") {
		t.Fatalf("err = %q, want it to carry the vendor error id", err)
	}
}

// TestResolveErrorCarriesNoTokenAndNoConfigVarValue is the credential-hygiene
// test, and it is falsifiable by construction: fake_test.go's failure envelope
// deliberately ECHOES both the Authorization header the request carried and
// every config var value the app holds, in a sibling field beside the "id" this
// provider is allowed to surface.
//
// Without that echo the assertion would be vacuous - it would pass against a
// provider that pasted the entire response body into its error message, because
// there would be nothing secret in the body to find. With it, exactly one
// implementation passes: one whose ErrorDetail hook selects a single field from
// a closed vocabulary.
func TestResolveErrorCarriesNoTokenAndNoConfigVarValue(t *testing.T) {
	clearEnv(t)
	const secretValue = "postgres://u:s3cr3t-must-not-leak@db.example.com/app"
	f := newFake()
	f.set(testApp, "DATABASE_URL", secretValue)
	f.fail(http.StatusForbidden)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "heroku://DATABASE_URL"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Fatalf("the config var value leaked into %q", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("the API token leaked into %q", err)
	}
	// The detail the hook IS allowed to surface must still be there, so this
	// test cannot be satisfied by suppressing every detail.
	if !strings.Contains(err.Error(), "injected_failure") {
		t.Fatalf("err = %q, want the vendor error id to survive", err)
	}
}

// TestResolveBatchErrorCarriesNoTokenAndNoConfigVarValue pins the same rule on
// the batch path. It reaches fetch by a different route, and a failure there
// carries EVERY config var of the app rather than one, so it is the larger leak
// of the two.
func TestResolveBatchErrorCarriesNoTokenAndNoConfigVarValue(t *testing.T) {
	clearEnv(t)
	const secretValue = "s3cr3t-batch-value-must-not-leak"
	f := newFake()
	f.set(testApp, "DATABASE_URL", secretValue)
	f.set(testApp, "OTHER", "plain")
	f.fail(http.StatusInternalServerError)

	_, err := f.provider().ResolveBatch(context.Background(), []mamori.Ref{
		mustRef(t, "heroku://DATABASE_URL"),
		mustRef(t, "heroku://OTHER"),
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Fatalf("a config var value leaked into %q", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("the API token leaked into %q", err)
	}
}

// TestErrorIDIgnoresAnUnusableBody pins the hook's safe fallback. A body that
// is not JSON (a proxy's HTML error page) or whose "id" is not a string must
// suppress the detail rather than be guessed at, and must certainly not fall
// back to embedding the whole body.
func TestErrorIDIgnoresAnUnusableBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not JSON at all", "<html><body>502 Bad Gateway</body></html>"},
		{"id is not a string", `{"id":["rate_limit","forbidden"],"message":"nope"}`},
		{"no id field", `{"message":"nope"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorID(http.StatusForbidden, []byte(tc.body)); got != "" {
				t.Fatalf("errorID = %q, want \"\"", got)
			}
		})
	}
}

// TestErrorIDNeverSurfacesTheMessage pins the deliberate choice of "id" over
// "message". "message" is free prose the vendor may reword, and this endpoint's
// success body is the app's entire config var document; "id" comes from a
// documented closed vocabulary and cannot carry a value.
func TestErrorIDNeverSurfacesTheMessage(t *testing.T) {
	body := []byte(`{"id":"forbidden","message":"the app's DATABASE_URL is postgres://u:s3cr3t@h/db"}`)
	got := errorID(http.StatusForbidden, body)
	if got != "forbidden" {
		t.Fatalf("errorID = %q, want %q", got, "forbidden")
	}
	if strings.Contains(got, "s3cr3t") {
		t.Fatalf("the vendor message reached the detail: %q", got)
	}
}

// TestMalformedResponseNeverEchoesTheBody pins the deliberate absence of a
// wrapped decode cause: a 200 body is the app's whole config var document, and
// encoding/json quotes the offending byte in a syntax error.
//
// The second assertion is the load-bearing one, and it exists because the first
// one alone CANNOT FAIL. A *json.SyntaxError renders as "invalid character 's'
// looking for beginning of value": it quotes exactly one byte of the body, so a
// provider that wrapped the cause with a second %w would leak a single
// character and no substring check long enough to be meaningful would ever
// match it. Asserting that the phrase encoding/json uses is absent is what
// makes the wrapping itself observable, at any leak size down to one byte.
func TestMalformedResponseNeverEchoesTheBody(t *testing.T) {
	clearEnv(t)
	const garbage = "s3cr3t-not-json-and-possibly-every-secret-this-app-has"
	f := newFake()
	p := f.provider(WithHTTPClient(&http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, garbage), nil
	})}))

	_, err := p.Resolve(context.Background(), mustRef(t, "heroku://TOKEN"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if strings.Contains(err.Error(), garbage[:12]) {
		t.Fatalf("the response body reached the error: %q", err)
	}
	if strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("the encoding/json cause reached the error, quoting a byte of the body: %q", err)
	}
}

// TestNullDocumentIsInvalidNotAnEmptyApp pins the branch a nil map would
// otherwise swallow. A 200 whose body is literal "null" unmarshals into a nil
// map with NO error, and a nil map answers every lookup with "absent" - so
// without this branch a misbehaving backend would report every ref of the app
// as not-found and mamori would silently apply every default.
//
// It must not be ErrNotFound for the same reason: that is the kind that makes
// mamori default the field.
func TestNullDocumentIsInvalidNotAnEmptyApp(t *testing.T) {
	clearEnv(t)
	f := newFake()
	p := f.provider(WithHTTPClient(&http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusOK, "null"), nil
	})}))

	_, err := p.Resolve(context.Background(), mustRef(t, "heroku://TOKEN"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("a null document reported ErrNotFound; mamori would silently apply every field's default")
	}
}

// TestEmptyDocumentIsAnEmptyAppNotAFailure pins the other side: a 200 carrying
// "{}" is an app with no config vars, which is a legitimate state, so each ref
// must report not-found individually rather than the whole read failing.
func TestEmptyDocumentIsAnEmptyAppNotAFailure(t *testing.T) {
	clearEnv(t)
	f := newFake() // testApp exists with no vars

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "heroku://TOKEN"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestTransportFailureIsUnavailableAndRedacted pins that a network-level
// failure classifies as transient and that nothing from the request leaks into
// its message. net/http wraps a transport failure in a *url.Error that renders
// the request URL; httpcore rebuilds that with a redacted URL, and this asserts
// the provider does not undo it by adding its own copy.
func TestTransportFailureIsUnavailableAndRedacted(t *testing.T) {
	clearEnv(t)
	f := newFake()
	p := f.provider(WithHTTPClient(&http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	})}))

	_, err := p.Resolve(context.Background(), mustRef(t, "heroku://TOKEN"))
	if !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("the API token leaked into %q", err)
	}
}
