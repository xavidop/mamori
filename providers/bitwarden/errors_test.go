package bitwarden

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// TestSecretStatusClassification is the status-to-Kind table, kept separate
// from the conformance kit on purpose.
//
// The kit's ErrorClassification case walks a handful of mamori sentinels and
// asks the provider to produce each; it proves the wiring is connected but not
// which HTTP status maps where. This table asserts the mapping itself, so a
// change to httpcore's ClassifyStatus that altered, say, 422, would surface
// here as a named failure rather than as a subtly different retry policy in
// production.
//
// 422 and 429 are the two rows worth stating explicitly. 422 must be invalid
// and NOT the transient default, because mamori retries transient failures and
// no number of retries fixes a semantically wrong request. 429 must be
// rate_limited so the backoff that exists for it is the one that runs.
func TestSecretStatusClassification(t *testing.T) {
	cases := []struct {
		status int
		want   mamori.Kind
	}{
		{http.StatusBadRequest, mamori.KindInvalid},
		{http.StatusUnauthorized, mamori.KindUnauthenticated},
		{http.StatusForbidden, mamori.KindPermissionDenied},
		{http.StatusNotFound, mamori.KindNotFound},
		{http.StatusRequestTimeout, mamori.KindRateLimited},
		{http.StatusUnprocessableEntity, mamori.KindInvalid},
		{http.StatusTooManyRequests, mamori.KindRateLimited},
		{http.StatusInternalServerError, mamori.KindUnavailable},
		{http.StatusBadGateway, mamori.KindUnavailable},
		{http.StatusServiceUnavailable, mamori.KindUnavailable},
		// A status http.StatusText does not know must still classify rather
		// than fall through untyped; Cloudflare's 520 is the real-world case.
		{520, mamori.KindUnavailable},
	}

	id := secretUUID("status")
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			f := newFake(t)
			f.set(id, "value")
			f.fail(tc.status)

			_, err := f.provider().Resolve(context.Background(), mustRef(t, "bitwarden-sm://"+id))
			if err == nil {
				t.Fatalf("status %d resolved successfully", tc.status)
			}
			if got := mamori.ErrorKind(err); got != tc.want {
				t.Errorf("status %d classified as %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// TestIdentityFailureClassification asserts that a rejected token exchange is
// classified from the identity endpoint's status, and that its RFC 6749 error
// code reaches the message.
//
// The code is the one part of a rejected exchange that is safe to surface: it
// is a fixed token such as "invalid_client", never free text and never derived
// from the client secret. Without it a misconfigured machine account is an
// opaque 400.
func TestIdentityFailureClassification(t *testing.T) {
	cases := []struct {
		status int
		want   mamori.Kind
	}{
		{http.StatusBadRequest, mamori.KindInvalid},
		{http.StatusUnauthorized, mamori.KindUnauthenticated},
		{http.StatusServiceUnavailable, mamori.KindUnavailable},
	}

	id := secretUUID("identity-failure")
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			f := newFake(t)
			f.set(id, "value")
			f.identityStatus = tc.status

			_, err := f.provider().Resolve(context.Background(), mustRef(t, "bitwarden-sm://"+id))
			if err == nil {
				t.Fatal("a rejected token exchange resolved successfully")
			}
			if got := mamori.ErrorKind(err); got != tc.want {
				t.Errorf("identity status %d classified as %v, want %v", tc.status, got, tc.want)
			}
			if !strings.Contains(err.Error(), "invalid_client") {
				t.Errorf("error %q does not carry the OAuth2 error code", err)
			}
			// The machine account id is named on purpose, so a deployment with
			// several can tell which one failed.
			if !strings.Contains(err.Error(), fakeClientID) {
				t.Errorf("error %q does not name the machine account", err)
			}
		})
	}
}

// TestRefValidation asserts that a ref which does not name a UUID is rejected
// as ErrInvalid and never as ErrNotFound.
//
// The distinction is load-bearing rather than cosmetic: ErrNotFound is the one
// kind that makes mamori apply a field's DEFAULT, so a malformed ref
// classified that way would silently become a default value in production
// instead of an error. Interpolation makes that reachable, because a ref path
// arrives with ${VAR} already substituted and an unset variable yields an
// empty or partial path.
func TestRefValidation(t *testing.T) {
	cases := []struct{ name, ref string }{
		{"empty path", "bitwarden-sm://"},
		{"a name rather than an id", "bitwarden-sm://STRIPE_API_KEY"},
		{"two segments", "bitwarden-sm://project/secret"},
		{"truncated uuid", "bitwarden-sm://ec2c1d46-6a4b-4751-a310"},
		{"uuid with a non-hex character", "bitwarden-sm://zc2c1d46-6a4b-4751-a310-af9601317f2d"},
	}

	f := newFake(t)
	p := f.provider()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := mamori.ParseRef(tc.ref)
			if err != nil {
				// An unparseable ref fails even earlier, which is also fine.
				return
			}
			_, err = p.Resolve(context.Background(), ref)
			if err == nil {
				t.Fatal("a malformed ref resolved successfully")
			}
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
			if errors.Is(err, mamori.ErrNotFound) {
				t.Error("a malformed ref classified as ErrNotFound, which would make mamori apply the field default")
			}
		})
	}
}

// TestRefValidationHappensBeforeAnyRequest asserts the UUID check short
// circuits the network, so a bad ref cannot spend a token exchange.
func TestRefValidationHappensBeforeAnyRequest(t *testing.T) {
	f := newFake(t)
	p := f.provider()

	if _, err := p.Resolve(context.Background(), mustRef(t, "bitwarden-sm://not-a-uuid")); err == nil {
		t.Fatal("a malformed ref resolved successfully")
	}
	if n := f.exchangeCount(); n != 0 {
		t.Errorf("a malformed ref caused %d token exchanges, want 0", n)
	}
}

// TestConstructionErrorsSurfaceAtResolve asserts that a bad URL, which New
// cannot return because its signature must let init register a provider, still
// reaches the caller as a classified error from the first Resolve. That is
// where `mamori doctor` looks.
func TestConstructionErrorsSurfaceAtResolve(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
	}{
		{"http identity URL without opt-in", []Option{WithIdentityURL("http://identity.test")}},
		{"http API URL without opt-in", []Option{WithAPIURL("http://api.test")}},
		{"non-http scheme", []Option{WithAPIURL("ftp://api.test")}},
		{"unparsable URL", []Option{WithAPIURL("://nope")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]Option{WithAccessToken(fakeAccessToken)}, tc.opts...)
			_, err := New(opts...).Resolve(context.Background(), mustRef(t, "bitwarden-sm://"+secretUUID("x")))
			if err == nil {
				t.Fatal("a misconfigured provider resolved successfully")
			}
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestAllowInsecurePermitsHTTPAndNothingElse asserts the opt-in is narrow: it
// admits cleartext http and is not a way to skip the scheme check itself.
func TestAllowInsecurePermitsHTTPAndNothingElse(t *testing.T) {
	if p := New(WithAPIURL("http://api.test"), WithAllowInsecure(true)); p.err != nil {
		t.Errorf("AllowInsecure did not permit http://: %v", p.err)
	}
	if p := New(WithAPIURL("ftp://api.test"), WithAllowInsecure(true)); p.err == nil {
		t.Error("AllowInsecure permitted a non-http scheme")
	}
}
