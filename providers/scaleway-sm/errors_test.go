package scalewaysm

import (
	"errors"
	"net/http"
	"testing"

	"github.com/xavidop/mamori"
)

// TestClassifyStatusUnmappedIsUnknown asserts that a status classifyStatus has
// no case for (teapot, chosen because no real Secret Manager response uses
// it) reports mamori.KindUnknown, and returns statusErr unchanged rather than
// re-wrapping it.
func TestClassifyStatusUnmappedIsUnknown(t *testing.T) {
	base := errors.New(`mamori/scaleway-sm: unexpected status 418 accessing secret "k": teapot`)
	got := classifyStatus(http.StatusTeapot, base)

	if got != base {
		t.Fatalf("classifyStatus(418, base) = %v, want base returned unchanged", got)
	}
	if kind := mamori.ErrorKind(got); kind != mamori.KindUnknown {
		t.Fatalf("ErrorKind = %q, want %q", kind, mamori.KindUnknown)
	}
}

func TestClassifyStatusNilIsNil(t *testing.T) {
	if err := classifyStatus(http.StatusForbidden, nil); err != nil {
		t.Fatalf("classifyStatus(403, nil) = %v, want nil", err)
	}
}

// TestClassifyStatusPreservesChain asserts that the diagnostic text in
// statusErr stays reachable through errors.Is after classification wraps it
// with a sentinel, so nothing throws away the original context.
func TestClassifyStatusPreservesChain(t *testing.T) {
	base := errors.New(`mamori/scaleway-sm: unexpected status 403 accessing secret "k": forbidden`)
	wrapped := classifyStatus(http.StatusForbidden, base)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	if !errors.Is(wrapped, base) {
		t.Fatalf("underlying status error no longer reachable: %v", wrapped)
	}
}

// TestClassifyStatusMapsOrdinaryHTTPSemantics pins classifyStatus's mapping
// for every status this provider is documented to recognize.
func TestClassifyStatusMapsOrdinaryHTTPSemantics(t *testing.T) {
	tests := []struct {
		code int
		want error
	}{
		{http.StatusUnauthorized, mamori.ErrUnauthenticated},
		{http.StatusForbidden, mamori.ErrPermissionDenied},
		{http.StatusTooManyRequests, mamori.ErrRateLimited},
		{http.StatusBadRequest, mamori.ErrInvalid},
		{http.StatusInternalServerError, mamori.ErrUnavailable},
		{http.StatusBadGateway, mamori.ErrUnavailable},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			base := errors.New("injected")
			got := classifyStatus(tc.code, base)
			if !errors.Is(got, tc.want) {
				t.Fatalf("classifyStatus(%d, base) = %v, want an error satisfying %v", tc.code, got, tc.want)
			}
		})
	}
}
