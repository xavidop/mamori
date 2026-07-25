package doppler

import (
	"errors"
	"net/http"
	"testing"

	"github.com/xavidop/mamori"
)

func TestClassifyDopplerStatus(t *testing.T) {
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
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			base := errors.New("boom")
			got := mamori.ErrorKind(classifyDopplerStatus(tc.status, base))
			if got != tc.want {
				t.Fatalf("status %d: ErrorKind = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestClassifyDopplerStatusPreservesStatusError(t *testing.T) {
	base := errors.New(`mamori/doppler: unexpected status 403 fetching secret "K": forbidden`)
	wrapped := classifyDopplerStatus(http.StatusForbidden, base)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	if !errors.Is(wrapped, base) {
		t.Fatalf("underlying status error no longer reachable: %v", wrapped)
	}
}

func TestClassifyDopplerStatusNilIsNil(t *testing.T) {
	if err := classifyDopplerStatus(http.StatusForbidden, nil); err != nil {
		t.Fatalf("classifyDopplerStatus(403, nil) = %v, want nil", err)
	}
}

// TestClassifyDopplerStatusUnknownReturnsStatusErrorVerbatim asserts that an
// unmapped status returns the original statusErr unchanged (not re-wrapped),
// so its diagnostic text and type are untouched for a code mamori has no
// classification for.
func TestClassifyDopplerStatusUnknownReturnsStatusErrorVerbatim(t *testing.T) {
	base := errors.New(`mamori/doppler: unexpected status 409 fetching secret "K": conflict`)
	got := classifyDopplerStatus(http.StatusConflict, base)
	if got != base {
		t.Fatalf("classifyDopplerStatus(409, base) = %v, want base returned unchanged", got)
	}
}
