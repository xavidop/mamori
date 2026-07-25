package firebaserc

import (
	"errors"
	"net/http"
	"testing"

	"github.com/xavidop/mamori"
)

func TestClassifyFirebaseRC(t *testing.T) {
	cases := []struct {
		status int
		want   mamori.Kind
	}{
		{http.StatusForbidden, mamori.KindPermissionDenied},
		{http.StatusUnauthorized, mamori.KindUnauthenticated},
		{http.StatusTooManyRequests, mamori.KindRateLimited},
		{http.StatusInternalServerError, mamori.KindUnavailable},
		{http.StatusBadGateway, mamori.KindUnavailable},
		{http.StatusServiceUnavailable, mamori.KindUnavailable},
		{http.StatusBadRequest, mamori.KindInvalid},
		{http.StatusConflict, mamori.KindUnknown},
		{http.StatusNotFound, mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			err := errors.New("Remote Config API returned " + http.StatusText(tc.status))
			if got := mamori.ErrorKind(classifyFirebaseRC(tc.status, err)); got != tc.want {
				t.Fatalf("status %d: ErrorKind = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

// TestClassifyFirebaseRCPreservesUnderlyingError asserts classification
// double-wraps: the returned error must still satisfy errors.Is against the
// original API error, not just the sentinel, so diagnostics are not lost.
func TestClassifyFirebaseRCPreservesUnderlyingError(t *testing.T) {
	orig := errors.New("Remote Config API returned 403 Forbidden")
	wrapped := classifyFirebaseRC(http.StatusForbidden, orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	if !errors.Is(wrapped, orig) {
		t.Fatalf("underlying error lost: %v", wrapped)
	}
}

func TestClassifyFirebaseRCNilIsNil(t *testing.T) {
	if err := classifyFirebaseRC(http.StatusForbidden, nil); err != nil {
		t.Fatalf("classifyFirebaseRC(status, nil) = %v, want nil", err)
	}
}

// TestClassifyFirebaseRCUnmappedStatusIsUnknown asserts a status with no
// mapping (e.g. 409 Conflict) reports KindUnknown rather than being guessed
// at, and that the original error passes through unwrapped in that case.
func TestClassifyFirebaseRCUnmappedStatusIsUnknown(t *testing.T) {
	orig := errors.New("Remote Config API returned 409 Conflict")
	got := classifyFirebaseRC(http.StatusConflict, orig)
	if got != orig {
		t.Fatalf("unmapped status: got %v, want the original error unchanged", got)
	}
	if kind := mamori.ErrorKind(got); kind != mamori.KindUnknown {
		t.Fatalf("unmapped status kind = %q, want unknown", kind)
	}
}
