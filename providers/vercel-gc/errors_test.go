package vercelgc

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
// 401 and 403 are both present, which is the point of including them: they sit
// next to each other in any status mapping, and a copy-paste swap of the two
// would still satisfy a test that only ever asserted one of them.
// TestResolveUnauthorizedIsNotPermissionDenied pins the negative directly.
//
// 404 is absent on purpose: get takes it back for a message of its own, which
// TestResolveUnknownStoreIsNotFound covers.
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
			f := newFake()
			f.set(testStore, "k", `"v"`)
			f.failStatus(testStore, tc.code)
			p := f.provider()

			_, err := p.Resolve(context.Background(), ref(t, "vercel-gc://k"))
			if err == nil {
				t.Fatalf("status %d: Resolve returned a nil error", tc.code)
			}
			if got := mamori.ErrorKind(err); got != tc.want {
				t.Fatalf("status %d: ErrorKind = %q, want %q (err: %v)", tc.code, got, tc.want, err)
			}
		})
	}
}

// TestResolveUnauthorizedIsNotPermissionDenied is the negative half of the 401
// row above: every other assertion in this package checks only what a status
// DOES satisfy, never what it must NOT, so a swap of the two adjacent cases
// would pass everywhere else.
func TestResolveUnauthorizedIsNotPermissionDenied(t *testing.T) {
	f := newFake()
	f.set(testStore, "k", `"v"`)
	f.failStatus(testStore, http.StatusUnauthorized)
	p := f.provider()

	_, err := p.Resolve(context.Background(), ref(t, "vercel-gc://k"))
	if !errors.Is(err, mamori.ErrUnauthenticated) {
		t.Fatalf("401: got %v, want an error satisfying mamori.ErrUnauthenticated", err)
	}
	if errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("401: got %v, must not satisfy mamori.ErrPermissionDenied", err)
	}
}

// TestResolveErrorMessagePreservesContext guards what the migration onto
// httpcore had to keep: the endpoint, the store, the HTTP status, and the
// verbatim (bounded) upstream body all still reach the message, and the
// classification is still reachable with errors.Is rather than flattened into
// text.
func TestResolveErrorMessagePreservesContext(t *testing.T) {
	f := newFake()
	f.set(testStore, "k", `"v"`)
	f.failStatus(testStore, http.StatusForbidden)
	p := f.provider()

	_, err := p.Resolve(context.Background(), ref(t, "vercel-gc://k"))
	if err == nil {
		t.Fatal("Resolve returned a nil error for a 403 response")
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", err)
	}
	for _, want := range []string{"digest", testStore, "403"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error message lost %q: %v", want, err)
		}
	}
}
