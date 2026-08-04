package heroku

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"testing"

	"github.com/xavidop/mamori"
)

// TestResolveBatchOfManyRefsCostsOneRequest is the reason this provider
// implements mamori.BatchProvider at all, asserted head-on: twelve refs against
// one app must cost exactly ONE request, not twelve, and all twelve values must
// still come back correct.
//
// Both halves are needed. A ResolveBatch that looped over Resolve would return
// every correct value and cost twelve requests; one that fetched once but
// mis-indexed its group would cost one request and lose values.
func TestResolveBatchOfManyRefsCostsOneRequest(t *testing.T) {
	clearEnv(t)
	f := newFake()
	const n = 12
	refs := make([]mamori.Ref, n)
	want := make(map[string]string, n)
	for i := range n {
		name := fmt.Sprintf("VAR_%02d", i)
		val := fmt.Sprintf("value-%02d", i)
		f.set(testApp, name, val)
		tag := "heroku://" + name
		refs[i] = mustRef(t, tag)
		want[tag] = val
	}

	got, err := f.provider().ResolveBatch(context.Background(), refs)
	if err != nil {
		t.Fatalf("ResolveBatch: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d values, want %d", len(got), n)
	}
	for tag, wantVal := range want {
		if string(got[tag].Bytes) != wantVal {
			t.Errorf("%s: got %q, want %q", tag, got[tag].Bytes, wantVal)
		}
	}

	total, byApp := f.counts()
	if total != 1 {
		t.Fatalf("got %d requests for %d refs, want 1: one GET returns every config var of an app", total, n)
	}
	if byApp[testApp] != 1 {
		t.Fatalf("got %d requests for app %q, want 1", byApp[testApp], testApp)
	}
}

// TestResolveBatchGroupsByApp pins that refs naming different apps cost one
// request per app rather than one per ref, and that each value comes back from
// the app its own ref named rather than being cross-wired with the other app's
// value for the same config var name.
func TestResolveBatchGroupsByApp(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "TARGET", "from-default-app")
	f.set("other-app", "TARGET", "from-other-app")

	got, err := f.provider().ResolveBatch(context.Background(), []mamori.Ref{
		mustRef(t, "heroku://TARGET"),
		mustRef(t, "heroku://other-app/TARGET"),
		mustRef(t, "heroku://"+testApp+"/TARGET"),
	})
	if err != nil {
		t.Fatalf("ResolveBatch: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d values, want 3", len(got))
	}
	if s := string(got["heroku://TARGET"].Bytes); s != "from-default-app" {
		t.Errorf("default-app ref: got %q, want %q", s, "from-default-app")
	}
	if s := string(got["heroku://other-app/TARGET"].Bytes); s != "from-other-app" {
		t.Errorf("other-app ref: got %q, want %q", s, "from-other-app")
	}
	// The third ref names the default app explicitly, so it must share the
	// first ref's group rather than opening a second one.
	if s := string(got["heroku://"+testApp+"/TARGET"].Bytes); s != "from-default-app" {
		t.Errorf("explicit default-app ref: got %q, want %q", s, "from-default-app")
	}

	total, byApp := f.counts()
	if total != 2 {
		t.Fatalf("got %d requests, want 2 (one per distinct app, grouped before any network call)", total)
	}
	if byApp[testApp] != 1 || byApp["other-app"] != 1 {
		t.Fatalf("requests by app = %v, want one each", byApp)
	}
}

// TestResolveBatchOmitsAbsentVarSiblingsSurvive is the BatchProvider contract
// at its most basic: an absent config var must be OMITTED from the result map,
// not fail the whole call, and a sibling ref must still resolve.
//
// It also pins that Resolve and ResolveBatch agree on the whole mamori.Value
// and not only on Bytes. Every other assertion in this file compares Bytes,
// and Version, Sensitive and Metadata are exactly the fields a second code path
// can silently drop.
func TestResolveBatchOmitsAbsentVarSiblingsSurvive(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "PRESENT", "yes")
	p := f.provider()

	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{
		mustRef(t, "heroku://PRESENT"),
		mustRef(t, "heroku://ABSENT"),
	})
	if err != nil {
		t.Fatalf("an absent config var must not fail the batch, got %v", err)
	}
	if _, ok := got["heroku://ABSENT"]; ok {
		t.Error("absent config var must be omitted from the result map")
	}
	if len(got) != 1 {
		t.Fatalf("got %d values, want 1 (the sibling ref must still resolve)", len(got))
	}
	if s := string(got["heroku://PRESENT"].Bytes); s != "yes" {
		t.Errorf("sibling: got %q, want %q", s, "yes")
	}

	single, err := p.Resolve(context.Background(), mustRef(t, "heroku://PRESENT"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	batched := got["heroku://PRESENT"]
	if single.Version != batched.Version {
		t.Errorf("Version mismatch: Resolve=%q ResolveBatch=%q", single.Version, batched.Version)
	}
	if single.Sensitive != batched.Sensitive {
		t.Errorf("Sensitive mismatch: Resolve=%v ResolveBatch=%v", single.Sensitive, batched.Sensitive)
	}
	if !maps.Equal(single.Metadata, batched.Metadata) {
		t.Errorf("Metadata mismatch: Resolve=%v ResolveBatch=%v", single.Metadata, batched.Metadata)
	}
}

// TestResolveBatchOmitsNullVar pins that the vendor schema's null value is
// treated as an absence on the batch path too, exactly as Resolve treats it.
// The two paths reach valueFor by different routes, and only this asserts the
// batch one swallows a null rather than resolving it or failing on it.
func TestResolveBatchOmitsNullVar(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.setNull(testApp, "REMOVED")
	f.set(testApp, "PRESENT", "yes")

	got, err := f.provider().ResolveBatch(context.Background(), []mamori.Ref{
		mustRef(t, "heroku://REMOVED"),
		mustRef(t, "heroku://PRESENT"),
	})
	if err != nil {
		t.Fatalf("a null config var must not fail the batch, got %v", err)
	}
	if _, ok := got["heroku://REMOVED"]; ok {
		t.Error("a null config var must be omitted from the result map")
	}
	if len(got) != 1 {
		t.Fatalf("got %d values, want 1", len(got))
	}
}

// TestResolveAndResolveBatchAgreeOnAbsentField is the regression pin the house
// convention names by provider: providers/vercel-gc shipped a Critical defect
// here, returning any selection error verbatim from ResolveBatch. mamori.SelectKey
// wraps mamori.ErrNotFound when a selected #field is absent, so deleting one
// field from a JSON config var failed the whole batch and took every sibling
// ref down with it. Losing the sibling is the actual damage, so it is asserted
// explicitly rather than only that the call itself succeeds.
func TestResolveAndResolveBatchAgreeOnAbsentField(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "CREDS", `{"user":"app"}`)
	f.set(testApp, "SIBLING", "present")
	p := f.provider()

	_, err := p.Resolve(context.Background(), mustRef(t, "heroku://CREDS#password"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("Resolve: got %v, want an error satisfying mamori.ErrNotFound", err)
	}

	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{
		mustRef(t, "heroku://CREDS#password"),
		mustRef(t, "heroku://SIBLING"),
	})
	if err != nil {
		t.Fatalf("a field-level not-found must not fail the batch, got %v", err)
	}
	if _, ok := got["heroku://CREDS#password"]; ok {
		t.Error("ref with an absent selected field must be omitted from the result map")
	}
	if len(got) != 1 {
		t.Fatalf("got %d values, want 1 (the sibling ref must survive, not be lost with the batch)", len(got))
	}
	if s := string(got["heroku://SIBLING"].Bytes); s != "present" {
		t.Errorf("sibling: got %q, want %q", s, "present")
	}
}

// TestResolveBatchFailsOnInvalidSelection pins the deliberate divergence: only
// mamori.ErrNotFound is swallowed. Selecting a #field of a value that is not a
// JSON object is a malformed request against that payload, not an absence, so
// it must still fail the whole batch rather than being quietly rendered as
// "this field took its default".
func TestResolveBatchFailsOnInvalidSelection(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "LOG_LEVEL", "info")

	_, err := f.provider().ResolveBatch(context.Background(), []mamori.Ref{
		mustRef(t, "heroku://LOG_LEVEL#timeout"),
	})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrInvalid", err)
	}
}

// TestResolveBatchSurvivesSiblingAppNotFound pins the app-level counterpart of
// the absent-var rule, and the invariant doctor.go states for BatchProvider in
// general: ResolveBatch exists to cut round trips, not to change what a single
// ref resolves to.
//
// On this endpoint a 404 can only mean the APP is absent or invisible to the
// token, never that one var is missing, so a single mistyped app name among
// many refs must not take the whole batch down with it. Resolve of the same ref
// reports ErrNotFound, so omitting it here is the agreeing behaviour.
func TestResolveBatchSurvivesSiblingAppNotFound(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "GOOD", "still-here")
	p := f.provider()

	if _, err := p.Resolve(context.Background(), mustRef(t, "heroku://missing-app/ANY")); !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("Resolve of a missing app: got %v, want ErrNotFound", err)
	}

	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{
		mustRef(t, "heroku://GOOD"),
		mustRef(t, "heroku://missing-app/ANY"),
	})
	if err != nil {
		t.Fatalf("a sibling app's 404 must not fail the batch, got %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d values, want 1 (the good app's ref must survive)", len(got))
	}
	if s := string(got["heroku://GOOD"].Bytes); s != "still-here" {
		t.Errorf("good app: got %q, want %q", s, "still-here")
	}
	if _, ok := got["heroku://missing-app/ANY"]; ok {
		t.Error("the missing app's ref must be omitted, not resolved")
	}
}

// TestResolveBatchFailsOnRealBackendFailure pins the other side of that split:
// a 401, 403, 429 or 5xx is a real failure and must fail the batch. Swallowing
// it the way a 404 is swallowed would render an expired token as "every field
// took its default", which is the quietest possible way to deploy a broken
// configuration.
func TestResolveBatchFailsOnRealBackendFailure(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			clearEnv(t)
			f := newFake()
			f.set(testApp, "GOOD", "value")
			f.fail(status)

			if _, err := f.provider().ResolveBatch(context.Background(), []mamori.Ref{
				mustRef(t, "heroku://GOOD"),
			}); err == nil {
				t.Fatalf("status %d produced no error; the batch must fail rather than silently default every field", status)
			}
		})
	}
}

// TestResolveBatchEmpty pins that an empty batch makes no request at all.
func TestResolveBatchEmpty(t *testing.T) {
	clearEnv(t)
	f := newFake()

	got, err := f.provider().ResolveBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("ResolveBatch: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d values, want 0", len(got))
	}
	if total, _ := f.counts(); total != 0 {
		t.Fatalf("an empty batch must make no requests, got %d", total)
	}
}

// TestResolveBatchFailsOnAMalformedRef pins that a ref this grammar cannot
// parse fails the batch rather than being omitted. A malformed ref is a
// configuration error, and omitting it would make mamori apply the field's
// default - turning a typo into a silently wrong value, which is exactly what
// targetFor's ErrInvalid classification exists to prevent.
func TestResolveBatchFailsOnAMalformedRef(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "GOOD", "value")

	_, err := f.provider().ResolveBatch(context.Background(), []mamori.Ref{
		mustRef(t, "heroku://GOOD"),
		mustRef(t, "heroku://app/extra/TOO_MANY"),
	})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid", err)
	}
}

// TestResolveBatchDedupedVarFansOutToAllRefs pins that two refs selecting
// different #fields of the same config var each resolve to their own value.
// Nothing else in this file puts two refs on one var, and a grouping bug that
// kept only the last ref for a name would still read one request and still
// return "a" value.
func TestResolveBatchDedupedVarFansOutToAllRefs(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "CREDS", `{"user":"app","password":"p1"}`)

	got, err := f.provider().ResolveBatch(context.Background(), []mamori.Ref{
		mustRef(t, "heroku://CREDS#user"),
		mustRef(t, "heroku://CREDS#password"),
	})
	if err != nil {
		t.Fatalf("ResolveBatch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d values, want 2 (both refs on one var must resolve, not just the last)", len(got))
	}
	if s := string(got["heroku://CREDS#user"].Bytes); s != "app" {
		t.Errorf("user selector: got %q, want %q", s, "app")
	}
	if s := string(got["heroku://CREDS#password"].Bytes); s != "p1" {
		t.Errorf("password selector: got %q, want %q", s, "p1")
	}
	if total, _ := f.counts(); total != 1 {
		t.Fatalf("got %d requests, want 1", total)
	}
}
