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
