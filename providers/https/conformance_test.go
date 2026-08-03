package https

import (
	"context"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
	"github.com/xavidop/mamori/providertest"
)

// TestConformance runs the shared providertest kit against this provider,
// built through fakeBackend's in-process RoundTripper rather than a real
// httptest.Server: the NoGoroutineLeak case runs goleak.VerifyNone with no
// ignore options, which a live server's accept goroutine could never satisfy.
//
// SkipWatch is deliberately left unset. This provider genuinely has no Watch
// method (see the package doc on Watching), so the watch cases must skip
// because the type assertion to mamori.WatchableProvider fails, not because
// this test told them to.
//
// NoResolveErrors is likewise left unset: Resolve classifies HTTP status
// through httpcore.ClassifyStatus and Fail/Clear give the kit a real seam, so
// this provider owes the ErrorClassification case.
func TestConformance(t *testing.T) {
	f := newFake()

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return f.provider() },
		Ref: func(key string) string { return "https://test/" + key },
		Seed: func(_ context.Context, key, val string) error {
			f.set("/v1/"+key, []byte(val))
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set("/v1/"+key, []byte(val))
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
		PointerRef: func(key, frag string) string {
			return "https://test/" + key + frag
		},
	})
}
