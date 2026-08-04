package hcpvaultsecrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// TestStatusToKind maps every status the HCP Vault Secrets read path can return
// onto the kind this provider must report.
//
// This is the requirement CONTRIBUTING calls out separately from the
// conformance kit: the kit proves a classified error survives transit, this
// proves the classification exists at all. A provider that reformatted its
// error with %v instead of %w would pass neither, but a provider whose mapping
// was simply wrong would still pass the kit, because the kit derives the status
// it injects from the very mapping under test.
//
// HCP's OpenAPI document enumerates only "200" and a catch-all "default"
// (googlerpcStatus), so this table is NOT a transcription of a documented list
// of failure codes: no such list is published. It is the set of statuses a
// gRPC-gateway control plane behind a load balancer actually produces, and each
// row asserts the kind mamori must act on. See the README's "Pinned API
// contract" for the gap this compensates for.
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
		// 422 must stay terminal rather than transient: mamori would otherwise
		// back off and retry a request that was well formed and semantically
		// wrong, which no amount of retrying can fix.
		{"unprocessable entity", http.StatusUnprocessableEntity, mamori.KindInvalid},
		{"too many requests", http.StatusTooManyRequests, mamori.KindRateLimited},
		{"internal error", http.StatusInternalServerError, mamori.KindUnavailable},
		// A control plane behind a load balancer produces both, and both must
		// stay transient so mamori retries rather than failing the field.
		{"bad gateway", http.StatusBadGateway, mamori.KindUnavailable},
		{"service unavailable", http.StatusServiceUnavailable, mamori.KindUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			f := newFake()
			f.set(conformanceRef("TOKEN"), "value")
			f.failRead(tt.status)

			_, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
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

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
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

// TestTokenFailureKeepsItsKind pins that a transient failure at the IDENTITY
// provider stays transient.
//
// httpcore's authError only adds ErrUnauthenticated to an UNCLASSIFIED cause,
// precisely so a 503 from the token endpoint is not reported as a terminal
// credential failure. That distinction is load bearing: mamori treats
// unauthenticated as terminal and marks the field unhealthy, while unavailable
// is a kind it expects to heal on the next poll.
func TestTokenFailureKeepsItsKind(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")
	f.failToken(http.StatusServiceUnavailable)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
	if got := mamori.ErrorKind(err); got != mamori.KindUnavailable {
		t.Fatalf("a 503 from the token endpoint reported %s, want unavailable: %v", got, err)
	}
}

// TestBadCredentialsAreUnauthenticated pins the other half of the same rule: a
// token endpoint that refuses the key pair really is a credential failure, and
// must be reported as one so `mamori doctor` names the actual problem.
func TestBadCredentialsAreUnauthenticated(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")

	p := f.provider(WithClientSecret("wrong-secret"))
	_, err := p.Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
	if got := mamori.ErrorKind(err); got != mamori.KindUnauthenticated {
		t.Fatalf("kind = %s, want unauthenticated: %v", got, err)
	}
}

// TestErrorDetailSurfacesTheVendorMessage pins that httpcore's ErrorDetail hook
// is wired and lifts HCP's "message" field, so an operator sees WHY a request
// was refused rather than only its status.
func TestErrorDetailSurfacesTheVendorMessage(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")
	f.failRead(http.StatusForbidden)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "injected read failure") {
		t.Fatalf("err = %q, want it to carry the vendor message", err)
	}
}

// TestErrorMessageIgnoresANonStringMessage pins the hook's safe fallback. A
// shape it cannot decode must suppress the detail rather than be guessed at,
// and must certainly not fall back to embedding the whole body.
func TestErrorMessageIgnoresANonStringMessage(t *testing.T) {
	body := []byte(`{"code":3,"message":["name is required","name must be uppercase"]}`)
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
//
// The fake's error envelope deliberately echoes the value in a sibling field
// beside "message", which is what makes this assertion able to fail at all.
func TestResolveErrorCarriesNoSecretValue(t *testing.T) {
	clearEnv(t)
	const secretValue = "s3cr3t-value-that-must-not-leak"
	f := newFake()
	f.set(conformanceRef("TOKEN"), secretValue)
	f.failRead(http.StatusForbidden)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
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

// TestResolveErrorCarriesNoCredential pins the same rule for the two
// credentials in play: the access token on every read request, and the client
// secret the token was bought with.
//
// httpcore redacts a URL's query and userinfo, but the bearer token travels in
// a header, so what this really guards is that no code path here copies the
// request or its headers into a message. The fake echoes both, so a provider
// that surfaced the whole error body would fail this.
func TestResolveErrorCarriesNoCredential(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")
	f.failRead(http.StatusInternalServerError)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
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

// TestTokenExchangeErrorCarriesNoClientSecret pins the same rule on the leg
// that actually CARRIES the client secret. The fake's token failure envelope
// echoes it back, exactly as a real identity provider might when reporting
// which credential it rejected.
func TestTokenExchangeErrorCarriesNoClientSecret(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")
	f.failToken(http.StatusForbidden)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testClientSecret) {
		t.Fatalf("the client secret leaked into %q", err)
	}
}

// TestProviderNeverPrintsACredential pins the trap that motivates every closure
// in this package.
//
// fmt's %v, %+v and %#v walk unexported struct fields by REFLECTION, and
// reflection cannot call a String or GoString method on a value it reaches that
// way. A redaction method on a wrapper type would therefore not save a plain
// string field: fmt falls back to printing the raw contents, so a debug dump,
// a panic trace, or a %+v in someone else's log line would print the credential
// in cleartext.
//
// The resolve happens FIRST so the access token is cached by the time the
// Provider is printed: a token held in a struct field would be reachable only
// after it had been bought, which is exactly when a naive test would miss it.
func TestProviderNeverPrintsACredential(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "the-resolved-value")

	p := f.provider()
	if _, err := p.Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	for _, format := range []string{"%v", "%+v", "%#v"} {
		dump := fmt.Sprintf(format, p)
		if strings.Contains(dump, testClientSecret) {
			t.Errorf("fmt.Sprintf(%q, provider) printed the client secret", format)
		}
		if strings.Contains(dump, testAccessToken) {
			t.Errorf("fmt.Sprintf(%q, provider) printed the access token", format)
		}
		if strings.Contains(dump, "the-resolved-value") {
			t.Errorf("fmt.Sprintf(%q, provider) printed a resolved secret value", format)
		}
	}
}

// TestResolveMalformedResponseNeverEchoesTheBody pins the deliberate absence of
// a wrapped decode cause: a 200 body is the secret, and encoding/json quotes
// the offending byte in a syntax error.
func TestResolveMalformedResponseNeverEchoesTheBody(t *testing.T) {
	clearEnv(t)
	const garbage = "definitely-not-json-and-possibly-the-secret"
	f := newFake()
	p := f.provider(WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == tokenPath {
			return jsonResp(http.StatusOK, `{"access_token":"`+testAccessToken+`","expires_in":3600}`), nil
		}
		return jsonResp(http.StatusOK, garbage), nil
	})}))

	_, err := p.Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if strings.Contains(err.Error(), garbage[:10]) {
		t.Fatalf("the response body reached the error: %q", err)
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
		if req.URL.Path == tokenPath {
			return jsonResp(http.StatusOK, `{"access_token":"`+testAccessToken+`","expires_in":3600}`), nil
		}
		return jsonResp(http.StatusOK, `{"secrets":[]}`), nil
	})}))

	_, err := p.Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("a malformed 200 reported ErrNotFound; mamori would silently apply the field's default")
	}
}

// TestNonStaticSecretIsRejected pins the deliberate limit of this provider.
//
// A rotating or dynamic secret answers with no static_version, and its value
// lives in a MAP under a different key whose shape this package has not pinned
// against a live backend. Failing loudly is the honest answer: returning an
// empty value would look like a successful read of an empty secret, and
// ErrNotFound would make mamori apply the field's default while the secret
// plainly exists.
func TestNonStaticSecretIsRejected(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.setNonStatic(conformanceRef("ROTATING"), "rotating")

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://ROTATING"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("a rotating secret reported ErrNotFound; mamori would silently apply the field's default")
	}
	if !strings.Contains(err.Error(), "static") {
		t.Errorf("err = %q, want it to name the static-secret limitation", err)
	}
}
