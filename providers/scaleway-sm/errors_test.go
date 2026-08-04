package scalewaysm

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// TestResolveClassifiesStatus pins the status-to-kind mapping this provider
// inherits from httpcore.ClassifyStatus, exercised end to end through Resolve
// against a real response. There is no longer a hand-rolled classifyStatus to
// table-test directly: the mapping lives in one place for every httpcore-backed
// provider.
//
// Three rows differ from the mapping this provider carried before it moved onto
// httpcore, and each is deliberate:
//
//   - 422 was previously unclassified (KindUnknown), so mamori treated it as an
//     unknown failure and kept retrying a request that was well formed and
//     semantically wrong. It is now KindInvalid.
//   - 408 was previously unclassified. It is now KindRateLimited, alongside 429.
//   - 418, and every other status neither this provider nor httpcore names, was
//     previously KindUnknown and is now KindUnavailable: httpcore has one
//     default for an unrecognized status rather than per-provider silence.
//
// 404 is absent on purpose: access takes it back for a message of its own,
// which TestResolveUnknownSecretIsNotFound covers.
func TestResolveClassifiesStatus(t *testing.T) {
	cases := []struct {
		code int
		want mamori.Kind
	}{
		{http.StatusUnauthorized, mamori.KindUnauthenticated},
		{http.StatusForbidden, mamori.KindPermissionDenied},
		{http.StatusTooManyRequests, mamori.KindRateLimited},
		{http.StatusRequestTimeout, mamori.KindRateLimited},
		{http.StatusBadRequest, mamori.KindInvalid},
		{http.StatusUnprocessableEntity, mamori.KindInvalid},
		{http.StatusInternalServerError, mamori.KindUnavailable},
		{http.StatusBadGateway, mamori.KindUnavailable},
		{http.StatusTeapot, mamori.KindUnavailable},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			f := newFakeSM()
			f.set("/", "k", []byte("v"))
			f.failStatus("/", "k", tc.code)
			p := f.provider()

			_, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://k"))
			if err == nil {
				t.Fatalf("status %d: Resolve returned a nil error", tc.code)
			}
			if got := mamori.ErrorKind(err); got != tc.want {
				t.Fatalf("status %d: ErrorKind = %q, want %q (err: %v)", tc.code, got, tc.want, err)
			}
		})
	}
}

// TestResolveErrorMessagePreservesContext guards what the migration onto
// httpcore had to keep: the secret name, the HTTP status, and the verbatim
// (bounded) upstream body all still reach the message, and the classification
// is still reachable with errors.Is rather than flattened into text.
func TestResolveErrorMessagePreservesContext(t *testing.T) {
	f := newFakeSM()
	f.set("/", "db-password", []byte("v"))
	f.failStatus("/", "db-password", http.StatusForbidden)
	p := f.provider()

	_, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://db-password"))
	if err == nil {
		t.Fatal("Resolve returned a nil error for a 403 response")
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", err)
	}
	for _, want := range []string{"db-password", "403"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error message lost %q: %v", want, err)
		}
	}
}
