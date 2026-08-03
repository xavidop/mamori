package httpcore

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

func TestClassifyStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   mamori.Kind
	}{
		{"ok", http.StatusOK, ""},
		{"created", http.StatusCreated, ""},
		{"not modified", http.StatusNotModified, ""},
		{"bad request", http.StatusBadRequest, mamori.KindInvalid},
		{"unauthorized", http.StatusUnauthorized, mamori.KindUnauthenticated},
		{"forbidden", http.StatusForbidden, mamori.KindPermissionDenied},
		{"not found", http.StatusNotFound, mamori.KindNotFound},
		{"request timeout", http.StatusRequestTimeout, mamori.KindRateLimited},
		{"too many requests", http.StatusTooManyRequests, mamori.KindRateLimited},
		{"internal error", http.StatusInternalServerError, mamori.KindUnavailable},
		{"bad gateway", http.StatusBadGateway, mamori.KindUnavailable},
		{"teapot", http.StatusTeapot, mamori.KindUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ClassifyStatus(tt.status, "")
			if tt.want == "" {
				if err != nil {
					t.Fatalf("ClassifyStatus(%d) = %v, want nil", tt.status, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ClassifyStatus(%d) = nil, want %s", tt.status, tt.want)
			}
			if got := mamori.ErrorKind(err); got != tt.want {
				t.Fatalf("ClassifyStatus(%d) kind = %s, want %s", tt.status, got, tt.want)
			}
		})
	}
}

// TestClassifyStatusIncludesDetail proves the caller-supplied detail reaches the
// message, since that is the only channel a provider has for a vendor's error
// text.
func TestClassifyStatusIncludesDetail(t *testing.T) {
	err := ClassifyStatus(http.StatusForbidden, "token lacks secrets:read")
	if err == nil {
		t.Fatal("ClassifyStatus returned nil")
	}
	if !strings.Contains(err.Error(), "token lacks secrets:read") {
		t.Fatalf("detail missing from %q", err.Error())
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("errors.Is(ErrPermissionDenied) = false for %v", err)
	}
}

// TestStatusForKindRoundTrips is the reason StatusForKind is exported rather
// than hand-rolled per provider: it pins the two tables as exact inverses, so
// neither can drift without this failing. A drifted inverse makes a conformance
// Fail hook inject a status that maps to a different kind, which silently
// exercises one classification case five times instead of five cases once.
func TestStatusForKindRoundTrips(t *testing.T) {
	kinds := []mamori.Kind{
		mamori.KindInvalid,
		mamori.KindUnauthenticated,
		mamori.KindPermissionDenied,
		mamori.KindNotFound,
		mamori.KindRateLimited,
		mamori.KindUnavailable,
	}
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			status := StatusForKind(k)
			err := ClassifyStatus(status, "")
			if err == nil {
				t.Fatalf("StatusForKind(%s) = %d, which ClassifyStatus treats as success", k, status)
			}
			if got := mamori.ErrorKind(err); got != k {
				t.Fatalf("round trip %s -> %d -> %s, want %s", k, status, got, k)
			}
		})
	}
}

// TestStatusForKindUnknown pins the fallback. ClassifyStatus never produces
// KindUnknown, so the inverse has no exact answer and must pick a status that
// is at least an honest failure.
func TestStatusForKindUnknown(t *testing.T) {
	if got := StatusForKind(mamori.KindUnknown); got < 500 {
		t.Fatalf("StatusForKind(unknown) = %d, want a 5xx", got)
	}
}
