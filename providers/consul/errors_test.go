package consul

import (
	"errors"
	"net/http"
	"testing"

	"github.com/hashicorp/consul/api"
	"github.com/xavidop/mamori"
)

func TestClassifyConsul(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"Forbidden", api.StatusError{Code: http.StatusForbidden, Body: "ACL support disabled"}, mamori.KindPermissionDenied},
		{"Unauthorized", api.StatusError{Code: http.StatusUnauthorized}, mamori.KindUnauthenticated},
		{"TooManyRequests", api.StatusError{Code: http.StatusTooManyRequests}, mamori.KindRateLimited},
		{"InternalServerError", api.StatusError{Code: http.StatusInternalServerError}, mamori.KindUnavailable},
		{"ServiceUnavailable", api.StatusError{Code: http.StatusServiceUnavailable}, mamori.KindUnavailable},
		{"BadRequest", api.StatusError{Code: http.StatusBadRequest}, mamori.KindInvalid},
		// 404 is deliberately unmapped here: KV.Get already turns a real 404 into
		// a nil pair with no error (see valueFor), so classifyConsul never sees
		// one in practice. This case locks in that if a StatusError{404} ever did
		// reach classifyConsul it would not be silently reinterpreted as
		// ErrNotFound.
		{"NotFoundUnmapped", api.StatusError{Code: http.StatusNotFound}, mamori.KindUnknown},
		{"UnmappedStatus", api.StatusError{Code: http.StatusTeapot}, mamori.KindUnknown},
		{"PlainError", errors.New("connection refused"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifyConsul(tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyConsul(%v)) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyConsulNilIsNil(t *testing.T) {
	if err := classifyConsul(nil); err != nil {
		t.Fatalf("classifyConsul(nil) = %v, want nil", err)
	}
}

// TestClassifyConsulPreservesStatusError guards that classification does not
// discard the original SDK error: callers who already reach it with
// errors.As (to read se.Code, se.Body) must keep working.
func TestClassifyConsulPreservesStatusError(t *testing.T) {
	orig := api.StatusError{Code: http.StatusForbidden, Body: "Permission denied: token does not have permission 'key:read' on \"config/app\""}
	wrapped := classifyConsul(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var se api.StatusError
	if !errors.As(wrapped, &se) {
		t.Fatalf("errors.As can no longer reach api.StatusError: %v", wrapped)
	}
	if se.Code != http.StatusForbidden {
		t.Fatalf("recovered Code = %d, want %d", se.Code, http.StatusForbidden)
	}
}
