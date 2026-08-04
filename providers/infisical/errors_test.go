package infisical

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// TestStatusToKind maps every status Infisical's read path is documented to
// return onto the kind this provider must report.
//
// This is the requirement CONTRIBUTING calls out separately from the
// conformance kit: the kit proves a classified error survives transit, this
// proves the classification exists at all. A provider that reformatted its
// error with %v instead of %w would pass neither, but a provider whose mapping
// was simply wrong would still pass the kit, because the kit derives the status
// it injects from the very mapping under test.
func TestStatusToKind(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   mamori.Kind
	}{
		{"bad request", http.StatusBadRequest, mamori.KindInvalid},
		{"unauthorized", http.StatusUnauthorized, mamori.KindUnauthenticated},
		{"forbidden", http.StatusForbidden, mamori.KindPermissionDenied},
		{"not found", http.StatusNotFound, mamori.KindNotFound},
		// 422 is the reason httpcore names it explicitly rather than leaving it
		// to the default: the default kind is transient, so mamori would back
		// off and retry a request that was well formed and semantically wrong.
		// Infisical is the backend in this ecosystem that answers with it.
		{"unprocessable entity", http.StatusUnprocessableEntity, mamori.KindInvalid},
		{"internal error", http.StatusInternalServerError, mamori.KindUnavailable},
		// Not documented by the vendor, but reachable through any proxy or CDN
		// in front of a self-hosted install, and both must stay transient.
		{"bad gateway", http.StatusBadGateway, mamori.KindUnavailable},
		{"too many requests", http.StatusTooManyRequests, mamori.KindRateLimited},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			f := newFake()
			f.set(conformanceRef("TOKEN"), "value")
			f.failRead(tt.status)

			_, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
			if err == nil {
				t.Fatalf("status %d produced no error", tt.status)
			}
			if got := mamori.ErrorKind(err); got != tt.want {
				t.Fatalf("status %d kind = %s, want %s", tt.status, got, tt.want)
			}
		})
	}
}

// TestNotFoundIsNotFoundAndNothingElse pins the single most consequential row
// of the table above on its own.
//
// ErrNotFound is the ONE kind that changes mamori's behaviour rather than only
// its reporting: it is what makes a field's default: and optional handling
// apply instead of failing the whole snapshot. A 404 misclassified as anything
// else turns an absent optional secret into a hard startup failure; anything
// else misclassified as 404 turns a real failure into a silently defaulted
// value.
func TestNotFoundIsNotFoundAndNothingElse(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")
	f.failRead(http.StatusNotFound)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
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

// TestErrorDetailSurfacesTheVendorMessage pins that httpcore's ErrorDetail hook
// is wired and lifts Infisical's "message" field, so an operator sees WHY a
// request was refused rather than only its status.
func TestErrorDetailSurfacesTheVendorMessage(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")
	f.failRead(http.StatusForbidden)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "injected read failure") {
		t.Fatalf("err = %q, want it to carry the vendor message", err)
	}
}

// TestErrorMessageIgnoresANonStringMessage pins the hook's safe fallback.
// Infisical answers some validation failures with an ARRAY of messages; that
// shape must suppress the detail rather than be guessed at, and must certainly
// not fall back to embedding the whole body.
func TestErrorMessageIgnoresANonStringMessage(t *testing.T) {
	body := []byte(`{"statusCode":422,"message":["name must be uppercase","name is required"],"error":"Unprocessable"}`)
	if got := errorMessage(http.StatusUnprocessableEntity, body); got != "" {
		t.Fatalf("errorMessage = %q, want \"\" for a non-string message", got)
	}
	if got := errorMessage(http.StatusForbidden, []byte("not json at all")); got != "" {
		t.Fatalf("errorMessage = %q, want \"\" for a body that is not JSON", got)
	}
}

// TestResolveErrorCarriesNoSecretValue pins that a failing resolve never puts
// the response body into the error, since on the success path that body IS the
// secret. Only the field the ErrorDetail hook selects may reach a message.
func TestResolveErrorCarriesNoSecretValue(t *testing.T) {
	clearEnv(t)
	const secretValue = "s3cr3t-value-that-must-not-leak"
	f := newFake()
	f.set(conformanceRef("TOKEN"), secretValue)
	f.failRead(http.StatusForbidden)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
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

// TestResolveErrorCarriesNoAccessToken pins the same rule for the credential
// that is on every read request. httpcore redacts a URL's query and userinfo,
// but the bearer token travels in a header, so what this really guards is that
// no code path here copies the request or its headers into a message.
func TestResolveErrorCarriesNoAccessToken(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")
	f.failRead(http.StatusInternalServerError)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testAccessToken) {
		t.Fatalf("the access token leaked into %q", err)
	}
	if strings.Contains(err.Error(), testClientSecret) {
		t.Fatalf("the client secret leaked into %q", err)
	}
}

// TestResolveMalformedResponseNeverEchoesTheBody pins the deliberate absence of
// a wrapped decode cause on the read path, for the same reason as on the login
// path: a 200 body is the secret, and encoding/json quotes the offending byte.
func TestResolveMalformedResponseNeverEchoesTheBody(t *testing.T) {
	clearEnv(t)
	const garbage = "definitely-not-json-and-possibly-the-secret"
	f := newFake()
	p := f.provider(WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == loginPath {
			return jsonResp(http.StatusOK, `{"accessToken":"`+testAccessToken+`","expiresIn":3600}`), nil
		}
		return jsonResp(http.StatusOK, garbage), nil
	})}))

	_, err := p.Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if strings.Contains(err.Error(), garbage[:10]) {
		t.Fatalf("the response body reached the error: %q", err)
	}
	// The substring check above cannot fail on its own. encoding/json quotes a
	// SINGLE byte in a syntax error, so wrapping the cause yields "invalid
	// character 'o' in literal null", which contains no ten-character run of
	// the body. What distinguishes a dropped cause from a wrapped one is
	// whether any encoding/json text reaches the message at all.
	if strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("the json decode cause was wrapped, so a quoted byte of the body can reach the error: %q", err)
	}
}

// TestResolveResponseWithoutSecretObject pins that a 200 carrying no "secret"
// object fails with ErrInvalid rather than resolving to an empty value.
//
// It must not be ErrNotFound: a 404 is how the backend reports an absent
// secret, and reporting a protocol violation the same way would make mamori
// apply the field's default while the backend was in fact misbehaving.
func TestResolveResponseWithoutSecretObject(t *testing.T) {
	clearEnv(t)
	f := newFake()
	p := f.provider(WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == loginPath {
			return jsonResp(http.StatusOK, `{"accessToken":"`+testAccessToken+`","expiresIn":3600}`), nil
		}
		return jsonResp(http.StatusOK, `{"secrets":[]}`), nil
	})}))

	_, err := p.Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("a malformed 200 reported ErrNotFound; mamori would silently apply the field's default")
	}
}

// TestResolveEmptySecretValueIsNotNotFound pins the distinction the pointer in
// secretEnvelope exists for: a secret whose value is legitimately the empty
// string resolves, rather than being mistaken for an absent one.
func TestResolveEmptySecretValueIsNotNotFound(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("EMPTY"), "")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://EMPTY"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(v.Bytes) != 0 {
		t.Fatalf("Bytes = %q, want empty", v.Bytes)
	}
	if v.Version == "" {
		t.Error("Version is empty; an empty value still has a revision")
	}
}
