package posthog

import (
	"context"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
	"github.com/xavidop/mamori/providertest"
)

// TestConformance runs the shared providertest kit against this provider,
// driven through fakeBackend's in-process RoundTripper rather than a real
// httptest.Server: the NoGoroutineLeak case runs goleak.VerifyNone with no
// ignore options, which a live server's accept goroutine could never satisfy.
//
// Ref carries #payload because the kit's ResolveSeeded and Version cases
// require the exact seeded string to flow back out of Resolve unchanged, and
// the payload facet is the one that does so verbatim. The other two facets are
// derived ("true"/"false", or a variant key), so neither could round-trip an
// arbitrary conformance value.
//
// SkipWatch is deliberately left unset. This provider genuinely has no Watch
// method (PostHog's flag endpoint pushes nothing), so the watch cases must skip
// because the type assertion to mamori.WatchableProvider fails, not because
// this test told them to.
//
// NoResolveErrors is likewise left unset: this backend has a real per-resolve
// error surface. Resolve classifies HTTP status through httpcore.ClassifyStatus,
// so the provider owes the ErrorClassification case rather than being exempt
// from it the way a bool-returning SDK client (providers/unleash,
// providers/split, providers/configcat) legitimately is.
func TestConformance(t *testing.T) {
	f := newFake()

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return f.provider() },
		Ref: func(key string) string { return "posthog://" + key + "#payload" },
		// No PointerRef: #enabled/#variant/#payload each select a facet of the
		// evaluated flag result, not a path into a JSON document, so nothing
		// here routes through mamori.SelectKey and there is no pointer support
		// to test. providers/unleash, providers/flipt and providers/split leave
		// it nil for the same reason.
		Seed: func(_ context.Context, key, val string) error {
			f.setPayload(key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.setPayload(key, val)
			return nil
		},
		// The backend surfaces a failure as a status code, not as a mamori
		// error, so the injected sentinel is turned back into the status that
		// produces it via httpcore.StatusForKind, the exported inverse of
		// ClassifyStatus. Injecting one fixed status instead would exercise a
		// single classification case five times rather than five cases once.
		Fail: func(_ context.Context, _ string, err error) error {
			f.fail(httpcore.StatusForKind(mamori.ErrorKind(err)))
			return nil
		},
		Clear: func(_ context.Context, _ string) error {
			f.clearFail()
			return nil
		},
	})
}
