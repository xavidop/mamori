package firebaserc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// statusProvider returns a provider whose Remote Config endpoint answers every
// request with status and body.
func statusProvider(t *testing.T, status int, body string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p := New(WithProjectID("demo"), WithHTTPClient(srv.Client()), WithBaseURL(srv.URL))
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestFetchTemplateClassifiesStatus pins the status-to-kind mapping this
// provider inherits from httpcore.ClassifyStatus, exercised end to end through
// Resolve against real responses. There is no longer a hand-rolled
// classifyFirebaseRC to table-test directly: the mapping lives in one place for
// every httpcore-backed provider.
//
// Three rows differ from the mapping this provider carried before it moved onto
// httpcore:
//
//   - 422 was previously unclassified (KindUnknown). It is now KindInvalid, so
//     mamori stops retrying a request that can never succeed.
//   - 408 was previously unclassified. It is now KindRateLimited, alongside 429.
//   - 409, and every other status neither this provider nor httpcore names, was
//     previously KindUnknown and is now KindUnavailable.
//
// 404 is in the table with its ORIGINAL kind, KindUnknown, and that is not an
// oversight: see notFoundIsNotAMissingParameter for why a 404 on the template
// endpoint must not reach mamori as ErrNotFound.
func TestFetchTemplateClassifiesStatus(t *testing.T) {
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
		{http.StatusNotFound, mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			p := statusProvider(t, tc.status, `{"error":{"status":"NOPE"}}`)
			_, err := p.Resolve(context.Background(), parse(t, "firebase-rc://welcome"))
			if err == nil {
				t.Fatalf("status %d: Resolve returned a nil error", tc.status)
			}
			if got := mamori.ErrorKind(err); got != tc.want {
				t.Fatalf("status %d: ErrorKind = %q, want %q (err: %v)", tc.status, got, tc.want, err)
			}
		})
	}
}

// TestTemplate404IsNotNotFound is the guard for the one status this provider
// takes back from httpcore's table. A 404 on the template endpoint means the
// project does not exist or the Remote Config API is not enabled on it, which
// affects every ref at once; mamori.ErrNotFound is the one kind that makes a
// field apply its default instead of failing, so a 404 leaking through as
// not-found would turn a wrong project id into silence.
func TestTemplate404IsNotNotFound(t *testing.T) {
	p := statusProvider(t, http.StatusNotFound, `{"error":{"status":"NOT_FOUND","message":"Requested entity was not found."}}`)
	_, err := p.Resolve(context.Background(), parse(t, "firebase-rc://welcome"))
	if err == nil {
		t.Fatal("Resolve returned a nil error for a 404 template response")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("a 404 on the template endpoint must not reach mamori as ErrNotFound: %v", err)
	}
	// The diagnostic must survive the sentinel being dropped.
	for _, want := range []string{"404", "Requested entity was not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("404 message lost %q: %v", want, err)
		}
	}
}

// TestFetchTemplateErrorQuotesAPIDiagnostic guards what the migration onto
// httpcore had to keep: the API's own error body still reaches the message, so
// an operator sees why a request was refused rather than only its status.
func TestFetchTemplateErrorQuotesAPIDiagnostic(t *testing.T) {
	p := statusProvider(t, http.StatusForbidden, `{"error":{"status":"PERMISSION_DENIED"}}`)
	_, err := p.Resolve(context.Background(), parse(t, "firebase-rc://welcome"))
	if err == nil {
		t.Fatal("Resolve returned a nil error for a 403 response")
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", err)
	}
	for _, want := range []string{"403", "PERMISSION_DENIED"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error message lost %q: %v", want, err)
		}
	}
}

// TestErrorDetailIsBounded pins snippet's bound: a hostile or broken upstream
// must not be able to put an unbounded response body into an error string.
func TestErrorDetailIsBounded(t *testing.T) {
	const marker = "TAIL_MARKER_MUST_NOT_SURVIVE"
	p := statusProvider(t, http.StatusInternalServerError, strings.Repeat("A", 20000)+marker)

	_, err := p.Resolve(context.Background(), parse(t, "firebase-rc://welcome"))
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
