package nacos

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
	"github.com/xavidop/mamori/providertest"
)

// clearEnv removes the ambient NACOS_* configuration for one test, so a
// developer with a real Nacos exported in their shell runs the same suite CI
// does. Only NACOS_NAMESPACE and NACOS_GROUP can reach a test that injects its
// own client, but all four are cleared so adding a test that does not inject one
// cannot silently start talking to somebody's server.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"NACOS_SERVER_ADDR", "NACOS_CONTEXT_PATH", "NACOS_NAMESPACE", "NACOS_GROUP", "NACOS_USERNAME", "NACOS_PASSWORD"} {
		t.Setenv(k, "")
	}
}

// TestConformance runs the full provider conformance kit against the in-process
// fake.
//
// SkipWatch is deliberately UNSET. This is the only provider in its cohort with
// a native watch, so WatchEmitsOnMutate and WatchClosesOnCancel must actually
// execute; if they report "provider is not watchable or watch disabled", the
// type has stopped satisfying mamori.WatchableProvider and the native watch is
// gone. TestWatchConformanceCasesActuallyRun below asserts exactly that, because
// a skip is green.
func TestConformance(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	const ns = "conformance-ns"

	providertest.Run(t, providertest.Config{
		New:        func() mamori.Provider { return fake.provider(ns) },
		Ref:        func(key string) string { return "nacos://" + key },
		PointerRef: func(key, frag string) string { return "nacos://" + key + frag },
		// Nacos's listener is a comparison, not a stream: a round carries the
		// MD5 this watch believes the configuration has, and the server answers
		// immediately when that belief is already stale. The baseline read and
		// the MD5 the first round sends come from the same response (see
		// Watch), so a write landing between them is answered rather than
		// dropped, which is exactly the ordering guarantee this field asks for.
		WatchDeliversBaseline: true,
		Seed: func(_ context.Context, key, val string) error {
			fake.set(ns, defaultGroup, key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			fake.set(ns, defaultGroup, key, val)
			return nil
		},
		Fail: func(_ context.Context, _ string, err error) error {
			// httpcore.StatusForKind is the exact inverse of the table
			// ClassifyStatus applies, so the injected sentinel comes back out of
			// Resolve as itself rather than as whichever kind a hand-rolled
			// inverse happened to pick.
			fake.fail(httpcore.StatusForKind(mamori.ErrorKind(err)))
			return nil
		},
		Clear: func(_ context.Context, _ string) error {
			fake.clearFail()
			return nil
		},
		EventuallyTimeout: 10 * time.Second,
	})
}

// TestWatchConformanceCasesActuallyRun asserts that the two watch cases run
// rather than skip.
//
// providertest.RunWatch skips when the provider does not implement
// mamori.WatchableProvider, and a skipped subtest is reported green. This test
// drives the same case through a recording testing.TB, so a Skip is visible as a
// failure here: dropping Watch from *Provider (or setting SkipWatch in the
// config above) would leave TestConformance passing and this failing.
func TestWatchConformanceCasesActuallyRun(t *testing.T) {
	clearEnv(t)

	var _ mamori.WatchableProvider = (*Provider)(nil)

	fake := newFakeNacos()
	const ns = "runs-ns"
	rec := &recordingTB{TB: t}
	providertest.RunWatch(rec, providertest.Config{
		New:                   func() mamori.Provider { return fake.provider(ns) },
		Ref:                   func(key string) string { return "nacos://" + key },
		WatchDeliversBaseline: true,
		Seed: func(_ context.Context, key, val string) error {
			fake.set(ns, defaultGroup, key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			fake.set(ns, defaultGroup, key, val)
			return nil
		},
		EventuallyTimeout: 10 * time.Second,
	})

	if rec.skipped {
		t.Fatalf("WatchEmitsOnMutate SKIPPED (%q); this provider has a native watch and the case must run", rec.msg)
	}
	if rec.failed {
		t.Fatalf("WatchEmitsOnMutate failed: %s", rec.msg)
	}
	if fake.rounds() == 0 {
		t.Fatal("the watch case passed without a single listener request reaching the backend")
	}
}

// recordingTB captures Skip/Fatal instead of stopping the test, so a
// conformance case can be asserted on rather than merely run.
type recordingTB struct {
	testing.TB
	skipped bool
	failed  bool
	msg     string
}

func (r *recordingTB) Skip(args ...any) { r.skipped = true; r.msg = fmt.Sprint(args...) }
func (r *recordingTB) Skipf(format string, args ...any) {
	r.skipped = true
	r.msg = fmt.Sprintf(format, args...)
}
func (r *recordingTB) SkipNow()          { r.skipped = true }
func (r *recordingTB) Fatal(args ...any) { r.failed = true; r.msg = fmt.Sprint(args...) }
func (r *recordingTB) Fatalf(format string, args ...any) {
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
}
func (r *recordingTB) Error(args ...any) { r.failed = true; r.msg = fmt.Sprint(args...) }
func (r *recordingTB) Errorf(format string, args ...any) {
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
}
func (r *recordingTB) FailNow() { r.failed = true }
func (r *recordingTB) Helper()  {}
