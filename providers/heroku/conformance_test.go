package heroku

import (
	"context"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
	"github.com/xavidop/mamori/providertest"
)

// TestConformance runs the shared providertest kit against this provider, built
// through fakeHeroku's in-process RoundTripper rather than a real
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
	clearEnv(t)
	f := newFake()

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return f.provider() },
		// One path segment, so the app comes from the provider option. The kit
		// generates keys containing hyphens ("classify-notfound-3"), which
		// Heroku's own schema pattern ^\w+$ would reject - and which this
		// provider deliberately does not enforce, for the reason the package
		// doc gives. The kit exercising exactly that shape is a useful accident.
		Ref: func(key string) string { return "heroku://" + key },
		Seed: func(_ context.Context, key, val string) error {
			f.set(testApp, key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set(testApp, key, val)
			return nil
		},
		// The backend surfaces a failure as a status code, not as a mamori
		// error, so the injected sentinel is turned back into the status that
		// produces it with httpcore.StatusForKind, the exported inverse of
		// ClassifyStatus. Injecting one fixed status instead would exercise a
		// single classification case five times rather than five cases once,
		// and would still report green.
		//
		// The failure is applied to the whole backend rather than to one key,
		// because that is what this endpoint can express: it answers per
		// request, not per config var, and one request carries every var of the
		// app.
		Fail: func(_ context.Context, _ string, err error) error {
			f.fail(httpcore.StatusForKind(mamori.ErrorKind(err)))
			return nil
		},
		Clear: func(_ context.Context, _ string) error {
			f.clearFail()
			return nil
		},
		// A config var's value may itself be JSON - a service account blob, a
		// bundle of settings - and #key selects into it through
		// mamori.SelectKey exactly as in every other provider, so this case
		// applies and must run. The fragment slot here is a JSON selector, not
		// a backend-native key: the config var NAME lives in the path.
		PointerRef: func(key, frag string) string {
			return "heroku://" + key + frag
		},
	})
}

// TestProviderImplementsBatchProvider pins the interface this provider exists
// to satisfy. Losing it would not break a single test above: mamori falls back
// to Resolve per ref, every value still comes back correct, and the only
// symptom is twelve requests where there was one.
func TestProviderImplementsBatchProvider(t *testing.T) {
	var _ mamori.BatchProvider = (*Provider)(nil)
}

// TestProviderIsNotWatchable pins the package doc's decision: the Platform API
// exposes no streaming or blocking read of config vars, so New() must not
// satisfy mamori.WatchableProvider and mamori wraps it in the polling adapter
// instead. Without this, the conformance watch cases would skip silently
// whether that was intended or an accident.
func TestProviderIsNotWatchable(t *testing.T) {
	if _, ok := any(New()).(mamori.WatchableProvider); ok {
		t.Fatal("heroku must not implement WatchableProvider")
	}
}
