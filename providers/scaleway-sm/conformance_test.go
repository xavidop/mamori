package scalewaysm

import (
	"context"
	"errors"
	"net/http"
	"sync"
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
// built through fakeSM's in-process RoundTripper (fake_test.go) rather than a
// real httptest.Server: the NoGoroutineLeak case runs goleak.VerifyNone with
// no ignore options, which a live server's accept goroutine could never
// satisfy.
//
// This provider genuinely has no Watch method (see scalewaysm.go's doc
// comment on Watching), so SkipWatch is deliberately left unset: the watch
// cases must skip on their own because the type assertion to
// mamori.WatchableProvider fails, not because this test told them to.
// NoResolveErrors is likewise left unset: Resolve classifies HTTP status
// through classifyStatus and Fail/Clear give the kit a real seam to inject
// through, so this provider owes the ErrorClassification case.
func TestConformance(t *testing.T) {
	f := newFakeSM()

	// revision tracks, per conformance key, the highest revision number
	// written so far. Seed always starts a key at revision 1; Mutate bumps
	// it. Unlike every sibling provider in this stack, Version here is a
	// real backend revision, not mamori.VersionHash(bytes) (see valueFor's
	// doc comment) - a Mutate that simply overwrote revision 1 in place
	// would still change the stored bytes, but VersionMonotonic would then
	// pass only because the fake happens to also stamp a hash, which this
	// provider does not do. Bumping the revision is what makes that case
	// test the thing it claims to test.
	var mu sync.Mutex
	revision := map[string]uint32{}

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider {
			return f.provider()
		},
		Ref: func(key string) string { return "scaleway-sm://" + key },
		Seed: func(_ context.Context, key, val string) error {
			mu.Lock()
			revision[key] = 1
			mu.Unlock()
			f.setRevision("/", key, 1, []byte(val), nil, true)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			mu.Lock()
			revision[key]++
			rev := revision[key]
			mu.Unlock()
			// A new, higher-numbered enabled revision: resolveRevisionSelector's
			// "latest_enabled" path (the ref default) picks it up automatically,
			// exactly as a real operator writing a new secret version would.
			f.setRevision("/", key, rev, []byte(val), nil, true)
			return nil
		},
		Fail: func(_ context.Context, key string, err error) error {
			// Map the injected sentinel back to the HTTP status that produces it
			// through the real classifyStatus, so all five ErrorClassification
			// cases round-trip through the actual status-to-error mapping instead
			// of only ever exercising whichever one a single hard-coded status
			// happens to hit.
			f.failStatus("/", key, statusForFailure(err))
			return nil
		},
		Clear: func(_ context.Context, key string) error {
			f.clearFail("/", key)
			return nil
		},
		PointerRef: func(key, frag string) string {
			return "scaleway-sm://" + key + frag
		},
	})
}

// TestProviderIsNotBatchable pins the module's deliberate absence of
// mamori.BatchProvider: Secret Manager's access-secret-version endpoint
// returns one revision of one secret, and there is no bulk endpoint that
// returns many secrets' payloads in a single call (see scalewaysm.go's doc
// comment on Batching). A ResolveBatch that looped over refs internally
// would claim a round-trip saving that does not exist, so this provider must
// never implement the interface, and this test fails loudly if a future
// change adds one.
func TestProviderIsNotBatchable(t *testing.T) {
	if _, ok := any(New()).(mamori.BatchProvider); ok {
		t.Fatal("scaleway-sm must not implement BatchProvider: the API has no bulk access endpoint, so a ResolveBatch would be a loop claiming a round trip saving that does not exist")
	}
}

// TestProviderIsNotWatchable pins the module's deliberate absence of
// mamori.WatchableProvider: the Secret Manager REST API exposes no streaming
// or blocking read, so mamori wraps this provider in its polling adapter
// instead (see scalewaysm.go's doc comment on Watching). The conformance
// suite's own watch cases already skip because this type assertion fails;
// this test pins that absence directly so a future change cannot add a fake
// Watch backed by an internal ticker without being caught here too.
func TestProviderIsNotWatchable(t *testing.T) {
	if _, ok := any(New()).(mamori.WatchableProvider); ok {
		t.Fatal("scaleway-sm must not implement WatchableProvider: the Secret Manager REST API exposes no streaming or blocking read, and mamori's polling adapter covers this instead")
	}
}
