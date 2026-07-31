package cloudflarekv

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// statusForFailure maps the mamori sentinel providertest.RunErrorClassification
// injects to the HTTP status that would produce it through classifyStatus.
// This provider's client surfaces a failure as a status code on the request,
// not as a mamori error, so the conformance Fail hook has to invert that
// mapping to actually exercise every classification case; injecting one fixed
// status regardless of err would only ever exercise the case that status
// happens to map to (see classifyStatus's doc comment for the mapping this
// mirrors).
func statusForFailure(err error) int {
	switch {
	case errors.Is(err, mamori.ErrPermissionDenied):
		return http.StatusForbidden
	case errors.Is(err, mamori.ErrUnauthenticated):
		return http.StatusUnauthorized
	case errors.Is(err, mamori.ErrRateLimited):
		return http.StatusTooManyRequests
	case errors.Is(err, mamori.ErrInvalid):
		return http.StatusBadRequest
	default: // mamori.ErrUnavailable and anything unrecognized
		return http.StatusServiceUnavailable
	}
}

// TestConformance runs the shared providertest kit against this provider,
// built through fakeKV's in-process RoundTripper (fake_test.go) rather than a
// real httptest.Server: the NoGoroutineLeak case runs goleak.VerifyNone with
// no ignore options, which a live server's accept goroutine could never
// satisfy.
//
// This provider genuinely has no Watch method (see cloudflarekv.go's doc
// comment on Watching), so SkipWatch is deliberately left unset: the watch
// cases must skip on their own because the type assertion to
// mamori.WatchableProvider fails, not because this test told them to.
// NoResolveErrors is likewise left unset: Resolve classifies HTTP status
// through classifyStatus and Fail/Clear give the kit a real seam to inject
// through, so this provider owes the ErrorClassification case.
func TestConformance(t *testing.T) {
	f := newFake()

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider {
			return f.provider()
		},
		Ref: func(key string) string { return "cloudflare-kv://" + key },
		Seed: func(_ context.Context, key, val string) error {
			f.set(testNamespace, key, []byte(val))
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set(testNamespace, key, []byte(val))
			return nil
		},
		Fail: func(_ context.Context, _ string, err error) error {
			// Workers KV's REST API surfaces a failure as a status code on the
			// whole request, not per key, so the whole namespace is failed and
			// cleared rather than one key within it. statusForFailure maps the
			// injected sentinel to the status that produces it through
			// classifyStatus, so each of the five ErrorClassification cases
			// actually round-trips through the real status-to-error mapping
			// instead of only ever exercising one of them.
			f.failStatus(testNamespace, statusForFailure(err))
			return nil
		},
		Clear: func(_ context.Context, _ string) error {
			f.clearFail(testNamespace)
			return nil
		},
		PointerRef: func(key, frag string) string {
			return "cloudflare-kv://" + key + frag
		},
	})
}
