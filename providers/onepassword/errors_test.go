package onepassword

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// statusProvider returns a provider pointed at a server that answers every
// request with status and body.
func statusProvider(t *testing.T, status int, body string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(WithHost(srv.URL), WithToken(testToken), WithHTTPClient(srv.Client()))
}

// TestResolveClassifiesStatus pins the status-to-kind mapping this provider
// inherits from httpcore.ClassifyStatus, exercised end to end through Resolve
// against real responses. There is no longer a hand-rolled classifyOnePassword
// to table-test directly: the mapping lives in one place for every
// httpcore-backed provider.
//
// Three rows differ from the mapping this provider carried before it moved onto
// httpcore, and each is deliberate:
//
//   - 422 was previously unclassified (KindUnknown). It is now KindInvalid, so
//     mamori stops retrying a request that was well formed and semantically
//     wrong and can never succeed.
//   - 408 was previously unclassified. It is now KindRateLimited, alongside
//     429.
//   - 409, and every other status neither this provider nor httpcore names,
//     was previously KindUnknown and is now KindUnavailable: httpcore has one
//     default for an unrecognized status rather than per-provider silence.
//
// 404 is absent from the table on purpose: resolveVaultID and resolveItem each
// translate it into their own not-found message, which TestNotFound covers.
func TestResolveClassifiesStatus(t *testing.T) {
	cases := []struct {
		status int
		want   mamori.Kind
	}{
		{http.StatusForbidden, mamori.KindPermissionDenied},
		{http.StatusUnauthorized, mamori.KindUnauthenticated},
		{http.StatusTooManyRequests, mamori.KindRateLimited},
		{http.StatusRequestTimeout, mamori.KindRateLimited},
		{http.StatusInternalServerError, mamori.KindUnavailable},
		{http.StatusBadGateway, mamori.KindUnavailable},
		{http.StatusServiceUnavailable, mamori.KindUnavailable},
		{http.StatusBadRequest, mamori.KindInvalid},
		{http.StatusUnprocessableEntity, mamori.KindInvalid},
		{http.StatusConflict, mamori.KindUnavailable},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			p := statusProvider(t, tc.status, `{"message":"nope"}`)
			ref, err := mamori.ParseRef("op://" + confVaultName + "/" + confItemTitle + "/password")
			if err != nil {
				t.Fatalf("ParseRef: %v", err)
			}
			_, err = p.Resolve(context.Background(), ref)
			if err == nil {
				t.Fatalf("status %d: Resolve returned a nil error", tc.status)
			}
			if got := mamori.ErrorKind(err); got != tc.want {
				t.Fatalf("status %d: ErrorKind = %q, want %q (err: %v)", tc.status, got, tc.want, err)
			}
		})
	}
}

// TestResolveErrorMessagePreservesContext guards what the migration onto
// httpcore had to keep: the operation name, the HTTP status, and the verbatim
// (bounded) Connect diagnostic body all still reach the message, and the
// classification is still reachable with errors.Is rather than flattened into
// text.
func TestResolveErrorMessagePreservesContext(t *testing.T) {
	p := statusProvider(t, http.StatusForbidden, `{"message":"access denied"}`)
	ref, err := mamori.ParseRef("op://" + confVaultName + "/" + confItemTitle + "/password")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	_, err = p.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("Resolve returned a nil error for a 403 response")
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"vault lookup", "403", "access denied"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message lost %q: %v", want, err)
		}
	}
}

// TestResolveErrorBodyIsBounded pins errDetailLimit: the diagnostic body quoted
// into an error must never let a hostile or broken Connect server put an
// unbounded response into an error string.
func TestResolveErrorBodyIsBounded(t *testing.T) {
	const marker = "TAIL_MARKER_MUST_NOT_SURVIVE"
	p := statusProvider(t, http.StatusInternalServerError, strings.Repeat("A", 20000)+marker)

	ref, err := mamori.ParseRef("op://" + confVaultName + "/" + confItemTitle + "/password")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	_, err = p.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("want an error for a 500 response")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error body was not bounded: the trailing marker reached the error text: %v", err)
	}
	if len(err.Error()) > 1000 {
		t.Fatalf("error message is %d bytes long; the quoted body must be bounded well below the oversized response", len(err.Error()))
	}
}
