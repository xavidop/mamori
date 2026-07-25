package onepassword

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

func TestClassifyOnePassword(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   mamori.Kind
	}{
		{"Forbidden", http.StatusForbidden, mamori.KindPermissionDenied},
		{"Unauthorized", http.StatusUnauthorized, mamori.KindUnauthenticated},
		{"TooManyRequests", http.StatusTooManyRequests, mamori.KindRateLimited},
		{"InternalServerError", http.StatusInternalServerError, mamori.KindUnavailable},
		{"BadGateway", http.StatusBadGateway, mamori.KindUnavailable},
		{"ServiceUnavailable", http.StatusServiceUnavailable, mamori.KindUnavailable},
		{"BadRequest", http.StatusBadRequest, mamori.KindInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifyOnePassword(tc.status))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyOnePassword(%d)) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

// TestClassifyOnePasswordUnmappedIsNil checks classifyOnePassword's own
// return value for statuses it does not recognize, including 404 (handled by
// callers directly, not by this function) and a couple of arbitrary
// unrecognized codes. It intentionally does not go through mamori.ErrorKind:
// ErrorKind(nil) is the empty Kind, not KindUnknown, so asserting KindUnknown
// here would be asserting the wrong thing. The real "an unmapped status
// stays unknown" guarantee is end-to-end, through statusError, which falls
// back to the plain (non-nil) message error when classifyOnePassword returns
// nil; see TestStatusErrorUnmappedStatusIsUnclassified.
func TestClassifyOnePasswordUnmappedIsNil(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusTeapot, http.StatusConflict} {
		if err := classifyOnePassword(status); err != nil {
			t.Fatalf("classifyOnePassword(%d) = %v, want nil", status, err)
		}
	}
}

// TestStatusErrorPreservesMessage guards the %w: %w wrapping in statusError:
// the classification sentinel must be reachable with errors.Is, and the
// original op/status/body message must still be reachable in Error(), not
// discarded in favor of the sentinel alone.
func TestStatusErrorPreservesMessage(t *testing.T) {
	err := statusError("vault lookup", http.StatusForbidden, []byte(`{"message":"access denied"}`))
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", err)
	}
	if !strings.Contains(err.Error(), "vault lookup") {
		t.Fatalf("op name lost from message: %v", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("status code lost from message: %v", err)
	}
}

func TestStatusErrorUnmappedStatusIsUnclassified(t *testing.T) {
	err := statusError("item lookup", http.StatusConflict, nil)
	if got := mamori.ErrorKind(err); got != mamori.KindUnknown {
		t.Fatalf("ErrorKind(statusError(409)) = %q, want %q", got, mamori.KindUnknown)
	}
}
