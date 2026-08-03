package httpcore

import (
	"fmt"
	"net/http"

	"github.com/xavidop/mamori"
)

// ClassifyStatus maps an HTTP status code onto a wrapped mamori error sentinel.
// It returns nil for any 2xx and for 304 Not Modified, which is a successful
// conditional GET rather than a failure.
//
// The mapping is:
//
//	400            -> mamori.ErrInvalid
//	401            -> mamori.ErrUnauthenticated
//	403            -> mamori.ErrPermissionDenied
//	404            -> mamori.ErrNotFound
//	408, 429       -> mamori.ErrRateLimited
//	anything else  -> mamori.ErrUnavailable
//
// 404 maps to mamori.ErrNotFound because that is the only kind that drives
// mamori's behavior: it is what makes a field's default: or optional handling
// apply instead of failing the whole snapshot.
//
// detail is an optional, caller-chosen string appended to the message. Pass a
// vendor error code or message only after deciding it cannot contain the
// resolved value; pass "" when in doubt. ClassifyStatus never reads a response
// body itself.
func ClassifyStatus(status int, detail string) error {
	if status >= 200 && status < 300 {
		return nil
	}
	if status == http.StatusNotModified {
		return nil
	}

	var sentinel error
	switch status {
	case http.StatusBadRequest:
		sentinel = mamori.ErrInvalid
	case http.StatusUnauthorized:
		sentinel = mamori.ErrUnauthenticated
	case http.StatusForbidden:
		sentinel = mamori.ErrPermissionDenied
	case http.StatusNotFound:
		sentinel = mamori.ErrNotFound
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		sentinel = mamori.ErrRateLimited
	default:
		sentinel = mamori.ErrUnavailable
	}

	if detail == "" {
		return fmt.Errorf("http %d %s: %w", status, http.StatusText(status), sentinel)
	}
	return fmt.Errorf("http %d %s: %s: %w", status, http.StatusText(status), detail, sentinel)
}

// StatusForKind is the inverse of ClassifyStatus: it returns an HTTP status that
// ClassifyStatus maps back to k.
//
// It exists for conformance tests. providertest's ErrorClassification case
// injects a mamori sentinel, but an HTTP backend's fake can only fail a request
// with a status code, so the test has to turn the sentinel back into the status
// that produces it. Injecting one fixed status instead would exercise a single
// classification case five times rather than five cases once, and the test would
// still pass.
//
// Exporting it keeps the table and its inverse in one file, where a change to
// one that is not mirrored in the other fails TestStatusForKindRoundTrips
// immediately, rather than silently weakening the conformance test of every
// provider that copied an older version of the mapping.
//
// mamori.KindUnknown has no exact inverse, because ClassifyStatus never produces
// it. It maps to 500, which is at least an honest failure.
func StatusForKind(k mamori.Kind) int {
	switch k {
	case mamori.KindInvalid:
		return http.StatusBadRequest
	case mamori.KindUnauthenticated:
		return http.StatusUnauthorized
	case mamori.KindPermissionDenied:
		return http.StatusForbidden
	case mamori.KindNotFound:
		return http.StatusNotFound
	case mamori.KindRateLimited:
		return http.StatusTooManyRequests
	case mamori.KindUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
