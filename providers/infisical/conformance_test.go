package infisical

import (
	"context"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
	"github.com/xavidop/mamori/providertest"
)

// TestConformance runs the shared providertest kit against this provider, built
// through fakeInfisical's in-process RoundTripper rather than a real
// httptest.Server: the NoGoroutineLeak case runs goleak.VerifyNone with no
// ignore options, which a live server's accept goroutine could never satisfy.
//
// SkipWatch is deliberately left unset. This provider genuinely has no Watch
// method (see the package doc on Watching), so the watch cases must skip
// because the type assertion to mamori.WatchableProvider fails, not because
// this test told them to.
//
// NoResolveErrors is likewise left unset: Resolve classifies HTTP status
// through httpcore's ClassifyStatus and Fail/Clear give the kit a real seam, so
// this provider owes the ErrorClassification case.
func TestConformance(t *testing.T) {
	f := newFake()

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return f.provider() },
		Ref: func(key string) string { return "infisical://" + key },
		Seed: func(_ context.Context, key, val string) error {
			f.set(conformanceRef(key), val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set(conformanceRef(key), val)
			return nil
		},
		// The backend surfaces a failure as a status code, not as a mamori
		// error, so the injected sentinel is turned back into the status that
		// produces it with httpcore.StatusForKind, the exported inverse of
		// ClassifyStatus. Injecting one fixed status instead would exercise a
		// single classification case five times rather than five cases once,
		// and would still report green.
		//
		// Only the read path is failed. Infisical answers a failure on the
		// whole request rather than per secret, and failing the login leg too
		// would route the error through httpcore's authError, which adds
		// ErrUnauthenticated to an unclassified cause - the kit would then see
		// one kind for every case. A token that is valid but refused one
		// particular secret is also the real shape of a per-secret failure.
		Fail: func(_ context.Context, _ string, err error) error {
			f.failRead(httpcore.StatusForKind(mamori.ErrorKind(err)))
			return nil
		},
		Clear: func(_ context.Context, _ string) error {
			f.clearRead()
			return nil
		},
		// A secret's value may itself be JSON, and #key selects into it through
		// mamori.SelectKey exactly as in every other provider, so this case
		// applies and must run.
		PointerRef: func(key, frag string) string {
			return "infisical://" + key + frag
		},
	})
}
