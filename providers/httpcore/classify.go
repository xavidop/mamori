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
//	400, 422       -> mamori.ErrInvalid
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
// 422 Unprocessable Entity is named explicitly rather than left to the default,
// because the default kind is transient: mamori would back off and retry a
// request that was well formed and semantically wrong, which no amount of
// retrying can fix. Infisical is one backend in this ecosystem that answers 422.
//
// detail is an optional, caller-chosen string appended to the message. Pass a
// vendor error code or message only after deciding it cannot contain the
// resolved value; pass "" when in doubt. ClassifyStatus never reads a response
// body itself: [Config.ErrorDetail] is the hook through which one reaches it.
func ClassifyStatus(status int, detail string) error {
	err := classify(status, detail)
	if err == nil {
		return nil
	}
	// Attribute the message, as every other error this package returns is
	// attributed. A provider calling ClassifyStatus directly, which is the
	// documented pattern, would otherwise get an unsourced "http 403 ...".
	return fmt.Errorf("httpcore: %w", err)
}

// classify is ClassifyStatus without the package prefix, for Client.Do, which
// adds its own. Splitting the two keeps one copy of the table while stopping the
// prefix appearing twice in the same message.
func classify(status int, detail string) error {
	if status >= 200 && status < 300 {
		return nil
	}
	if status == http.StatusNotModified {
		return nil
	}

	var sentinel error
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
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

	// http.StatusText is "" for a vendor extension code, and the backends this
	// package targets emit them: Cloudflare's 520 through 527, for one. Naming
	// the status unconditionally would render "http 520 : mamori: unavailable",
	// with an orphaned space where the text should be.
	head := fmt.Sprintf("http %d", status)
	if text := http.StatusText(status); text != "" {
		head += " " + text
	}
	if detail == "" {
		return fmt.Errorf("%s: %w", head, sentinel)
	}
	return fmt.Errorf("%s: %s: %w", head, detail, sentinel)
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
// The inverse is one status per kind, not every status that produces it. Both
// 400 and 422 classify as KindInvalid, and 408 and 429 both as KindRateLimited;
// this returns the canonical one, which is all a Fail hook needs and all
// TestStatusForKindRoundTrips checks. Adding a status to the forward table
// therefore only forces a change here when it introduces a kind this cannot
// already produce.
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
