package vercelgc

import (
	"errors"
	"net/http"
	"testing"

	"github.com/xavidop/mamori"
)

// TestClassifyStatusUnmappedIsUnknown asserts that a status classifyStatus has
// no case for (teapot, chosen because no real Global Config response uses it)
// reports mamori.KindUnknown, matching the "anything else maps to unknown"
// contract the site page publishes, and returns statusErr unchanged rather
// than re-wrapping it.
func TestClassifyStatusUnmappedIsUnknown(t *testing.T) {
	base := errors.New(`mamori/vercel-gc: unexpected status 418 from items of store ecfg_test: teapot`)
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
// statusErr - the status code, endpoint, and store name get built into it in
// get - stays reachable through errors.Is after classification wraps it with
// a sentinel, so nothing throws away the original context.
func TestClassifyStatusPreservesChain(t *testing.T) {
	base := errors.New(`mamori/vercel-gc: unexpected status 403 from items of store ecfg_test: forbidden`)
	wrapped := classifyStatus(http.StatusForbidden, base)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	if !errors.Is(wrapped, base) {
		t.Fatalf("underlying status error no longer reachable: %v", wrapped)
	}
}

// TestClassifyStatusUnauthorizedIsNotPermissionDenied is the negative half of
// TestClassifyStatusPreservesChain's 403 case: 401 and 403 sit next to each
// other in classifyStatus's switch, and every existing test only asserts what
// each code DOES satisfy, never what it must NOT. A copy-paste swap of the
// two cases (401 mapped to ErrPermissionDenied, 403 to ErrUnauthenticated)
// would still pass every positive assertion elsewhere in this package - each
// status would just be asserted against the wrong sentinel by a test that
// only checks its own code - so this pins the negative directly.
func TestClassifyStatusUnauthorizedIsNotPermissionDenied(t *testing.T) {
	base := errors.New(`mamori/vercel-gc: unexpected status 401 from items of store ecfg_test: unauthorized`)
	got := classifyStatus(http.StatusUnauthorized, base)

	if !errors.Is(got, mamori.ErrUnauthenticated) {
		t.Fatalf("classifyStatus(401, base) = %v, want an error satisfying mamori.ErrUnauthenticated", got)
	}
	if errors.Is(got, mamori.ErrPermissionDenied) {
		t.Fatalf("classifyStatus(401, base) = %v, must not satisfy mamori.ErrPermissionDenied", got)
	}
}
