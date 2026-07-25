package mamori

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/mamori/secret"
	"go.uber.org/goleak"
)

func TestRedactRefLeavesPlainRefUntouched(t *testing.T) {
	ref, err := ParseRef("aws-sm://prod/db#password")
	if err != nil {
		t.Fatal(err)
	}
	got := redactRef(ref)
	if !strings.Contains(got, "prod/db") {
		t.Fatalf("redactRef dropped the path: %q", got)
	}
	if strings.Contains(got, secret.Redacted) {
		t.Fatalf("redactRef redacted a ref with no sensitive opts: %q", got)
	}
}

func TestRedactRefHidesSensitiveOpts(t *testing.T) {
	// A ref that carries an inline credential as a query option must not leak it
	// through a report that is designed to be served over HTTP.
	ref := Ref{
		Scheme: "vault",
		Path:   "kv/data/api",
		Opts:   url.Values{"token": {"s.hunter2"}, "namespace": {"team-a"}},
		Raw:    "vault://kv/data/api?token=s.hunter2&namespace=team-a",
	}
	got := redactRef(ref)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("redactRef leaked the token value: %q", got)
	}
	if !strings.Contains(got, secret.Redacted) {
		t.Fatalf("redactRef did not redact the token opt: %q", got)
	}
	if !strings.Contains(got, "team-a") {
		t.Fatalf("redactRef wrongly hid a non-sensitive opt: %q", got)
	}
	if !strings.Contains(got, "kv/data/api") {
		t.Fatalf("redactRef dropped the path: %q", got)
	}
}

func TestRedactRefDenylistIsCaseInsensitive(t *testing.T) {
	ref := Ref{
		Scheme: "x",
		Path:   "p",
		Opts:   url.Values{"APIKey": {"abc"}, "Password": {"pw"}, "SAS": {"sig"}},
		Raw:    "x://p?APIKey=abc&Password=pw&SAS=sig",
	}
	got := redactRef(ref)
	for _, leak := range []string{"abc", "pw", "sig"} {
		if strings.Contains(got, leak) {
			t.Fatalf("redactRef leaked a value under a mixed-case sensitive key: %q", got)
		}
	}
}

func TestRedactRefHidesCompoundCredentialKeys(t *testing.T) {
	// sensitiveOptKeys is exact-match, not substring: it must catch common
	// compound credential key names third-party providers may use (e.g.
	// client_secret) without over-redacting a benign key that merely contains
	// "key" as a substring, such as keyspace.
	ref := Ref{
		Scheme: "x",
		Path:   "p",
		Opts:   url.Values{"client_secret": {"s3cr3t"}, "keyspace": {"my_keyspace"}},
		Raw:    "x://p?client_secret=s3cr3t&keyspace=my_keyspace",
	}
	got := redactRef(ref)
	if strings.Contains(got, "s3cr3t") {
		t.Fatalf("redactRef leaked the client_secret value: %q", got)
	}
	if !strings.Contains(got, secret.Redacted) {
		t.Fatalf("redactRef did not redact client_secret: %q", got)
	}
	if !strings.Contains(got, "my_keyspace") {
		t.Fatalf("redactRef over-redacted the benign keyspace opt: %q", got)
	}
}

func TestRedactRefDoesNotCorruptValueContainingPlaceholder(t *testing.T) {
	// A non-sensitive opt whose value happens to contain the literal redaction
	// placeholder must round-trip intact. Round-tripping the placeholder through
	// url.Values.Encode and then blindly un-escaping "%5BREDACTED%5D" back to
	// "[REDACTED]" would also un-escape this opt's legitimately-encoded value,
	// corrupting it.
	const comment = "region=[REDACTED]-legacy"
	ref := Ref{
		Scheme: "vault",
		Path:   "kv/data/api",
		Opts:   url.Values{"token": {"s.hunter2"}, "comment": {comment}},
		Raw:    "vault://kv/data/api?token=s.hunter2&comment=" + url.QueryEscape(comment),
	}
	got := redactRef(ref)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("redactRef leaked the token value: %q", got)
	}
	if !strings.Contains(got, secret.Redacted) {
		t.Fatalf("redactRef did not redact the token opt: %q", got)
	}

	i := strings.IndexByte(got, '?')
	if i < 0 {
		t.Fatalf("redactRef dropped the query entirely: %q", got)
	}
	query, err := url.ParseQuery(got[i+1:])
	if err != nil {
		t.Fatalf("redactRef produced an unparseable query %q: %v", got[i+1:], err)
	}
	if got := query.Get("comment"); got != comment {
		t.Fatalf("redactRef corrupted the comment value: got %q, want %q", got, comment)
	}
	if got := query.Get("token"); got != secret.Redacted {
		t.Fatalf("redactRef did not redact token via decoded query: got %q", got)
	}
}

func TestStatusReportsResolvedFields(t *testing.T) {
	defer goleak.VerifyNone(t)
	// Build a Watcher over a struct with two env-backed fields, then assert
	// Status returns a healthy report naming both fields at version 1.
	t.Setenv("MAMORI_STATUS_A", "alpha")
	t.Setenv("MAMORI_STATUS_B", "beta")

	type Config struct {
		A string `source:"env:MAMORI_STATUS_A"`
		B string `source:"env:MAMORI_STATUS_B"`
	}
	w, err := Watch[Config](context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	rep := w.Status()
	if rep.Snapshot != 1 {
		t.Fatalf("initial Snapshot = %d, want 1", rep.Snapshot)
	}
	if rep.Live != rep.Snapshot {
		t.Fatalf("Live %d != Snapshot %d with no pinning", rep.Live, rep.Snapshot)
	}
	if !rep.Healthy {
		t.Fatalf("fresh config reported unhealthy: %+v", rep)
	}
	if len(rep.Fields) != 2 {
		t.Fatalf("Status reported %d fields, want 2", len(rep.Fields))
	}
	for _, f := range rep.Fields {
		if f.LastKind != "" || f.LastError != "" {
			t.Errorf("field %s carries an error on a clean load: %q %q", f.Path, f.LastKind, f.LastError)
		}
		if f.LastOK.IsZero() {
			t.Errorf("field %s has zero LastOK after a successful load", f.Path)
		}
	}
}

func TestHealthNilWhenAllFresh(t *testing.T) {
	defer goleak.VerifyNone(t)
	t.Setenv("MAMORI_HEALTH_A", "x")
	type Config struct {
		A string `source:"env:MAMORI_HEALTH_A"`
	}
	w, err := Watch[Config](context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Health(); err != nil {
		t.Fatalf("Health on fresh config = %v, want nil", err)
	}
}

func TestStatusConcurrentWithReconcile(t *testing.T) {
	defer goleak.VerifyNone(t)
	// Run Status in a tight loop on one goroutine while the engine reconciles on
	// another. This must be clean under -race. It asserts nothing about values,
	// only that concurrent Status is safe.
	t.Setenv("MAMORI_RACE_A", "v0")
	type Config struct {
		A string `source:"env:MAMORI_RACE_A"`
	}
	w, err := Watch[Config](context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			_ = w.Status()
			_ = w.Health()
		}
		close(done)
	}()
	<-done
}

// statusErrConfig is a small local fixture for TestStatusReflectsErrorKindAndStaleness.
// It duplicates the shape of errAfterProvider from watch_semantics_test.go on
// purpose: a controllable provider that resolves once then always fails is a
// fixture the mamoritest package will later generalize for use outside this
// package.
type statusErrConfig struct {
	V string `source:"statuserr://x" default:"init"`
}

func TestStatusReflectsErrorKindAndStaleness(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	p := &errAfterProvider{
		scheme: "statuserr",
		val:    Value{Bytes: []byte("good"), Version: "1"},
		fail:   fmt.Errorf("%w: backend down", ErrUnavailable),
	}
	p.ok.Store(1) // the initial (fail-fast) load succeeds; every resolve after that fails

	const staleAfter = 30 * time.Second
	w, err := Watch[statusErrConfig](context.Background(),
		WithProvider(p), WithClock(clk), WithJitter(0),
		WithPollInterval(10*time.Second),
		WithStale(staleAfter),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	// The provider's next resolve (pollWatch's own baseline resolve) fails
	// almost immediately, asynchronously. Wait for it to reach the report
	// rather than sleeping a fixed amount.
	deadline := time.Now().Add(2 * time.Second)
	var rep Report
	for time.Now().Before(deadline) {
		rep = w.Status()
		if len(rep.Fields) == 1 && rep.Fields[0].LastKind != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(rep.Fields) != 1 {
		t.Fatalf("Status reported %d fields, want 1", len(rep.Fields))
	}
	f := rep.Fields[0]
	if f.LastKind != KindUnavailable {
		t.Fatalf("LastKind = %q, want %q", f.LastKind, KindUnavailable)
	}
	if f.Stale {
		t.Fatalf("field reported Stale before the WithStale threshold elapsed: %+v", f)
	}
	if !rep.Healthy {
		t.Fatalf("a transient, non-stale error must not flip Healthy: %+v", rep)
	}

	// Advance the clock past the stale threshold with no successful refresh in
	// between (the provider never succeeds again). Status recomputes Age/Stale
	// at read time from the clock, so no further reconcile activity is needed.
	clk.Advance(staleAfter + time.Second)
	rep = w.Status()
	f = rep.Fields[0]
	if !f.Stale {
		t.Fatalf("field did not report Stale after the clock advanced past WithStale: %+v", f)
	}
	if rep.Healthy {
		t.Fatalf("Healthy = true after a field went stale: %+v", rep)
	}
}

func TestStatusSnapshotVersionAdvances(t *testing.T) {
	// Pins the version sequence workstream D depends on: the initial snapshot
	// is version 1, and the first applied change advances it to 2.
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("statusver")
	wp.set("cfg/level", "info", "l1")

	type Config struct {
		Level string `source:"statusver://cfg/level" default:"info"`
	}

	w, err := Watch[Config](context.Background(), WithProvider(wp), WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Status().Snapshot; got != 1 {
		t.Fatalf("initial Snapshot = %d, want 1", got)
	}

	wp.push("cfg/level", "debug", "l2")
	waitPending(clk)
	clk.Advance(defaultDebounce)

	deadline := time.Now().Add(2 * time.Second)
	var snap uint64
	for time.Now().Before(deadline) {
		snap = w.Status().Snapshot
		if snap == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if snap != 2 {
		t.Fatalf("Snapshot after applied change = %d, want 2", snap)
	}
	if got := w.Status().Live; got != 2 {
		t.Fatalf("Live = %d, want 2", got)
	}
}

// leakConfig has a single field whose source ref carries an inline
// credential in its query string, exactly the shape sensitiveOptKeys exists
// to defend against: a real provider ref such as vault://kv/db?token=s3cr3t
// embeds a secret directly in the ref. reflk is a fake, no-op
// WatchableProvider (see watchProvider in watch_test.go), so it never sees a
// real credential; what matters here is the ref string mamori itself builds
// its own ProviderError/StaleError from.
type leakConfig struct {
	V string `source:"reflk://secret/path?token=LEAKME123"`
}

// TestReportNeverLeaksRefCredentialThroughLastError is the regression test
// for the Critical finding: FieldStatus.Ref is redacted via redactRef, but
// FieldStatus.LastError previously embedded the RAW ref (query string and
// credential intact), because ProviderError/StaleError were constructed with
// spec.Ref.Raw rather than the redacted form. A field declared with an
// inline credential in its source ref would then leak that credential in
// clear text through LastError to any caller of the (by default
// unauthenticated) admin endpoint, even though Ref itself was already safe.
//
// This is driven end-to-end: a real errored field on a running Watcher,
// through the native-watch error path into handleErr (reconciler.go), which
// is where the ProviderError this finding is about gets constructed. It
// asserts the credential appears in neither Report.Fields[].Ref,
// Report.Fields[].LastError, nor the JSON body served by GET /.
//
// Non-vacuity: with the Ref field of the ProviderError/StaleError
// constructors reverted to spec.Ref.Raw, this test fails on the LastError
// assertion (LEAKME123 appears verbatim); with redactRef applied at
// construction, it passes.
func TestReportNeverLeaksRefCredentialThroughLastError(t *testing.T) {
	defer goleak.VerifyNone(t)
	const leak = "LEAKME123"
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("reflk")
	wp.set("secret/path", "ok", "v1") // initial fail-fast Load must succeed

	w, err := Watch[leakConfig](context.Background(), WithProvider(wp), WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	// Drive the field into an errored state via the native-watch error path,
	// the same way notfound_health_test.go does; this is what lands in
	// handleErr and constructs the ProviderError under test.
	wp.pushErr("secret/path", fmt.Errorf("%w: backend exploded", ErrUnavailable))
	waitUntil(t, 2*time.Second, "field to record an error", func() bool {
		rep := w.Status()
		return len(rep.Fields) == 1 && rep.Fields[0].LastError != ""
	})

	rep := w.Status()
	if len(rep.Fields) != 1 {
		t.Fatalf("Status reported %d fields, want 1", len(rep.Fields))
	}
	f := rep.Fields[0]
	if strings.Contains(f.Ref, leak) {
		t.Fatalf("FieldStatus.Ref leaked the credential: %q", f.Ref)
	}
	if strings.Contains(f.LastError, leak) {
		t.Fatalf("FieldStatus.LastError leaked the credential: %q", f.LastError)
	}

	// Also confirm at the HTTP boundary, since that is the actual attack
	// surface the finding describes: GET / on the (by default
	// unauthenticated) admin endpoint.
	h := Handler(w)
	recRoot := httptest.NewRecorder()
	h.ServeHTTP(recRoot, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(recRoot.Body.String(), leak) {
		t.Fatalf("GET / leaked the credential: %s", recRoot.Body.String())
	}
}
