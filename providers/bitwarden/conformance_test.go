package bitwarden

import (
	"context"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
	"github.com/xavidop/mamori/providertest"
)

// TestConformance runs the shared providertest kit against this provider,
// built through fakeBackend's in-process RoundTripper rather than a real
// httptest.Server: the NoGoroutineLeak case snapshots goroutines before any
// subtest runs, and a live server's accept goroutine could never satisfy it.
//
// Config.Key maps each logical name onto a stable UUID. This provider
// addresses a secret by its id and refuses anything that is not one (see
// secretID), while the kit names keys arbitrarily; Key is the seam the kit
// provides for exactly that translation.
//
// SkipWatch is deliberately left unset. Secrets Manager exposes no push
// channel, so this provider genuinely has no Watch method and the watch cases
// must skip because the type assertion to mamori.WatchableProvider fails, not
// because this test told them to.
//
// NoResolveErrors is likewise left unset: Resolve classifies HTTP status
// through httpcore, and Fail/Clear give the kit a real seam, so this provider
// owes the ErrorClassification case.
func TestConformance(t *testing.T) {
	f := newFake(t)

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return f.provider() },
		Ref: func(key string) string { return "bitwarden-sm://" + key },
		Key: secretUUID,
		Seed: func(_ context.Context, key, val string) error {
			f.set(key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set(key, val)
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
		// A fragment on this provider is a mamori.SelectKey selector applied
		// to the decrypted value, so it is a JSON selector and the kit's
		// pointer cases apply.
		PointerRef: func(key, frag string) string {
			return "bitwarden-sm://" + key + frag
		},
	})
}
