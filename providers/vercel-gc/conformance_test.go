package vercelgc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
	"github.com/xavidop/mamori/providertest"
)

// statusForFailure maps the mamori sentinel providertest.RunErrorClassification
// injects to the HTTP status that produces it. The read API surfaces failures
// as a status code, not as a mamori error, so the conformance Fail hook has to
// invert the classification to actually exercise every case; injecting one
// fixed status regardless of err would only ever exercise the case that status
// happens to map to.
//
// The inverse is httpcore.StatusForKind rather than a switch copied into this
// file: a hand-rolled inverse that drifts from the forward table does not fail
// loudly, it just makes this case exercise one classification five times while
// still reporting green.
func statusForFailure(err error) int {
	return httpcore.StatusForKind(mamori.ErrorKind(err))
}

func TestConformance(t *testing.T) {
	f := newFake()
	// Seed the store so it exists before the first resolve; an unknown store
	// is a 404, which is a different case from an absent key.
	f.set(testStore, "__conformance_bootstrap", `"ok"`)

	// quote renders a plain string as the JSON the API would store.
	quote := func(val string) string {
		b, err := json.Marshal(val)
		if err != nil {
			t.Fatalf("marshaling %q: %v", val, err)
		}
		return string(b)
	}

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider {
			return f.provider()
		},
		Ref: func(key string) string { return "vercel-gc://" + key },
		Seed: func(_ context.Context, key, val string) error {
			f.set(testStore, key, quote(val))
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set(testStore, key, quote(val))
			return nil
		},
		Fail: func(_ context.Context, _ string, err error) error {
			// The read API surfaces failures per request, not per key, so the
			// whole store is failed and cleared. statusForFailure maps the
			// injected sentinel back to the status that produces it, so each of
			// the five ErrorClassification cases actually round-trips through
			// the real status-to-error mapping instead of only ever exercising
			// Unavailable.
			f.failStatus(testStore, statusForFailure(err))
			return nil
		},
		Clear: func(_ context.Context, _ string) error {
			f.clearFail(testStore)
			return nil
		},
		PointerRef: func(key, frag string) string {
			return "vercel-gc://" + key + frag
		},
	})
}
