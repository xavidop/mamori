package cloudflarekv

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// TestResolveBatchChunksAt250 pins the chunking requirement head-on: 250 refs
// against one namespace must produce exactly 3 bulk requests (100 + 100 +
// 50), and every one of the 250 values must still come back correct. A
// silent truncation at the 100-key ceiling would drop the last 150 fields
// with no error at all; asserting both the request count, the exact size of
// each request, and every value is what catches that.
func TestResolveBatchChunksAt250(t *testing.T) {
	f := newFake()
	const n = 250
	refs := make([]mamori.Ref, n)
	want := make(map[string]string, n)
	for i := range n {
		key := fmt.Sprintf("k%03d", i)
		val := fmt.Sprintf("v%03d", i)
		f.set(testNamespace, key, []byte(val))
		tag := "cloudflare-kv://" + key
		refs[i] = ref(t, tag)
		want[tag] = val
	}
	p := f.provider()

	got, err := p.ResolveBatch(context.Background(), refs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d values, want %d", len(got), n)
	}
	for tag, wantVal := range want {
		if string(got[tag].Bytes) != wantVal {
			t.Errorf("%s: got %q, want %q", tag, got[tag].Bytes, wantVal)
		}
	}

	_, bulk := f.counts()
	if bulk != 3 {
		t.Fatalf("got %d bulk requests for 250 keys, want 3 (100+100+50): a silent truncation at 100 would drop the rest with no error at all", bulk)
	}

	f.mu.Lock()
	sizes := append([]int(nil), f.bulkSizes...)
	f.mu.Unlock()
	if len(sizes) != 3 || sizes[0] != 100 || sizes[1] != 100 || sizes[2] != 50 {
		t.Fatalf("got bulk request sizes %v, want [100 100 50]", sizes)
	}
}

// TestResolveBatchChunkBoundary is the likeliest place for an off-by-one:
// exactly 100 keys must still fit in a single request, and one more key past
// that must spill into a second request.
func TestResolveBatchChunkBoundary(t *testing.T) {
	tests := []struct {
		name      string
		n         int
		wantReqs  int
		wantSizes []int
	}{
		{name: "exactly 100 keys is one request", n: 100, wantReqs: 1, wantSizes: []int{100}},
		{name: "101 keys is two requests", n: 101, wantReqs: 2, wantSizes: []int{100, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			refs := make([]mamori.Ref, tc.n)
			for i := range tc.n {
				key := fmt.Sprintf("k%03d", i)
				f.set(testNamespace, key, []byte("v"))
				refs[i] = ref(t, "cloudflare-kv://"+key)
			}
			p := f.provider()

			got, err := p.ResolveBatch(context.Background(), refs)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tc.n {
				t.Fatalf("got %d values, want %d", len(got), tc.n)
			}
			_, bulk := f.counts()
			if bulk != tc.wantReqs {
				t.Fatalf("got %d bulk requests for %d keys, want %d", bulk, tc.n, tc.wantReqs)
			}

			// Assert the exact size of every chunk, not just the request
			// count: a chunking bug that recomputes its end index wrong but
			// still advances start by bulkMaxKeys each iteration can produce
			// the "right" number of requests by accident (e.g. by
			// re-fetching one key across two overlapping chunks), and a
			// count-only assertion would not catch that.
			f.mu.Lock()
			sizes := append([]int(nil), f.bulkSizes...)
			f.mu.Unlock()
			if len(sizes) != len(tc.wantSizes) {
				t.Fatalf("got bulk request sizes %v, want %v", sizes, tc.wantSizes)
			}
			for i, want := range tc.wantSizes {
				if sizes[i] != want {
					t.Fatalf("got bulk request sizes %v, want %v", sizes, tc.wantSizes)
				}
			}
		})
	}
}

// TestResolveBatchGroupsByNamespace pins that refs pointing at different
// namespaces (via ?namespace=) are grouped into one bulk request per
// namespace, not one request per ref, and that each value comes back from
// the namespace its own ref named rather than being cross-wired with the
// other namespace's value for the same key name.
func TestResolveBatchGroupsByNamespace(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "a", []byte("from-default"))
	f.set("other-namespace", "a", []byte("from-other"))
	p := f.provider()

	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{
		ref(t, "cloudflare-kv://a"),
		ref(t, "cloudflare-kv://a?namespace=other-namespace"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d values, want 2", len(got))
	}
	if string(got["cloudflare-kv://a"].Bytes) != "from-default" {
		t.Errorf("default-namespace ref: got %q, want %q", got["cloudflare-kv://a"].Bytes, "from-default")
	}
	if string(got["cloudflare-kv://a?namespace=other-namespace"].Bytes) != "from-other" {
		t.Errorf("other-namespace ref: got %q, want %q", got["cloudflare-kv://a?namespace=other-namespace"].Bytes, "from-other")
	}

	_, bulk := f.counts()
	if bulk != 2 {
		t.Fatalf("got %d bulk requests, want 2 (one per namespace, grouped before any network call)", bulk)
	}
}

// TestResolveBatchDedupedKeyFansOutToAllRefs pins the dedup path
// ResolveBatch's grouping introduces: two refs selecting different #fields
// of the same key must share one bulk-request slot (the key travels on the
// wire exactly once) but each must still resolve to its own distinct
// selected value, keyed by its own Ref.Raw.
//
// No other test in this file puts two refs against the same key in one
// ResolveBatch call, so this is the only test exercising the fan-out side of
// byKey's dedup. A regression that keeps only the last ref sharing a key
// (e.g. byKey[key] = []mamori.Ref{r} instead of appending) would still
// dedupe the request correctly - bulk request count and key-on-the-wire
// count would both still read as 1 - while silently dropping every sibling
// selector but the last; only the third assertion below, that both refs
// come back with their own distinct values, catches that.
func TestResolveBatchDedupedKeyFansOutToAllRefs(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "api-config", []byte(`{"timeout":"5s","retries":"3"}`))
	p := f.provider()

	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{
		ref(t, "cloudflare-kv://api-config#timeout"),
		ref(t, "cloudflare-kv://api-config#retries"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, bulk := f.counts()
	if bulk != 1 {
		t.Fatalf("got %d bulk requests, want 1 (both refs share one key)", bulk)
	}

	f.mu.Lock()
	sizes := append([]int(nil), f.bulkSizes...)
	f.mu.Unlock()
	if len(sizes) != 1 || sizes[0] != 1 {
		t.Fatalf("got bulk request sizes %v, want [1]: the shared key must appear once on the wire, not twice", sizes)
	}

	if len(got) != 2 {
		t.Fatalf("got %d values, want 2 (both refs sharing the deduped key must resolve, not just the last one)", len(got))
	}
	if string(got["cloudflare-kv://api-config#timeout"].Bytes) != "5s" {
		t.Errorf("timeout selector: got %q, want %q", got["cloudflare-kv://api-config#timeout"].Bytes, "5s")
	}
	if string(got["cloudflare-kv://api-config#retries"].Bytes) != "3" {
		t.Errorf("retries selector: got %q, want %q", got["cloudflare-kv://api-config#retries"].Bytes, "3")
	}
}

// TestResolveBatchOmitsAbsentKeySiblingsSurvive is the BatchProvider contract
// at its most basic: an absent key must be omitted from the result map, not
// fail the whole call, and a sibling ref in the same batch must still
// resolve.
//
// It also carries the spec's "Resolve and ResolveBatch agree" testing-table
// promise for the full mamori.Value, not just Bytes: every other assertion in
// this file compares .Bytes only, and the sole full-observable comparison
// otherwise lives in the integration test, which skips without live
// credentials. Version, Sensitive, and Metadata are asserted equal between
// the two paths for the same ref here so that parity is pinned offline too.
func TestResolveBatchOmitsAbsentKeySiblingsSurvive(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "present", []byte("yes"))
	p := f.provider()

	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{
		ref(t, "cloudflare-kv://present"),
		ref(t, "cloudflare-kv://absent"),
	})
	if err != nil {
		t.Fatalf("an absent key must not fail the batch, got %v", err)
	}
	if _, ok := got["cloudflare-kv://absent"]; ok {
		t.Error("absent key must be omitted from the result map")
	}
	if len(got) != 1 {
		t.Fatalf("got %d values, want 1 (the sibling ref must still resolve)", len(got))
	}
	if string(got["cloudflare-kv://present"].Bytes) != "yes" {
		t.Errorf("sibling: got %q, want %q", got["cloudflare-kv://present"].Bytes, "yes")
	}

	single, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://present"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	batched := got["cloudflare-kv://present"]
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

// TestResolveAndResolveBatchAgreeOnAbsentField is the regression pin the
// brief calls out by name: providers/vercel-gc shipped a Critical defect
// here. Its ResolveBatch returned any valueFor error verbatim, and
// mamori.SelectKey wraps mamori.ErrNotFound when a selected #field is absent,
// so deleting one field from a JSON value failed the whole batch and took
// every sibling ref down with it. This asserts both halves: Resolve reports
// the absent field as an error satisfying mamori.ErrNotFound (as it always
// has), and ResolveBatch omits only that one ref from the result map while
// its sibling still resolves - losing the sibling is the actual damage, so
// it is asserted explicitly rather than only that the call itself succeeds.
func TestResolveAndResolveBatchAgreeOnAbsentField(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "api-config", []byte(`{"timeout":"5s"}`))
	f.set(testNamespace, "sibling", []byte("present"))
	p := f.provider()

	_, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://api-config#retries"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("Resolve: got %v, want an error satisfying mamori.ErrNotFound", err)
	}

	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{
		ref(t, "cloudflare-kv://api-config#retries"),
		ref(t, "cloudflare-kv://sibling"),
	})
	if err != nil {
		t.Fatalf("a field-level not-found must not fail the batch, got %v", err)
	}
	if _, ok := got["cloudflare-kv://api-config#retries"]; ok {
		t.Error("ref with an absent selected field must be omitted from the result map")
	}
	if len(got) != 1 {
		t.Fatalf("got %d values, want 1 (the sibling ref must survive, not be lost with the batch)", len(got))
	}
	if string(got["cloudflare-kv://sibling"].Bytes) != "present" {
		t.Errorf("sibling: got %q, want %q", got["cloudflare-kv://sibling"].Bytes, "present")
	}
}

// TestResolveBatchFailsOnInvalidSelection: selecting a #field of a
// non-object value is a malformed request against that payload, not an
// absence (see TestResolveSelectingFieldOfNonObjectIsInvalid in
// resolve_test.go), so it must still fail the whole batch rather than being
// swallowed alongside a genuine not-found. This is the deliberate divergence
// the brief calls out: only mamori.ErrNotFound is swallowed.
func TestResolveBatchFailsOnInvalidSelection(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "log-level", []byte("info"))
	p := f.provider()

	_, err := p.ResolveBatch(context.Background(), []mamori.Ref{
		ref(t, "cloudflare-kv://log-level#timeout"),
	})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrInvalid", err)
	}
}

// TestResolveBatchSuccessFalseIsInvalid pins the JSON-envelope failure mode
// that has no equivalent on the single-key GET path: a 200 response whose
// body carries "success":false is a Cloudflare-reported failure, not an
// absence, and must classify as mamori.ErrInvalid.
func TestResolveBatchSuccessFalseIsInvalid(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "k", []byte("v"))
	f.failStatus(testNamespace, http.StatusOK) // 200 status, success:false body
	p := f.provider()

	_, err := p.ResolveBatch(context.Background(), []mamori.Ref{ref(t, "cloudflare-kv://k")})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrInvalid", err)
	}
}

// TestResolveBatchClassifiesFailureStatus pins that an HTTP-level failure
// status on the bulk endpoint runs through the same classifyStatus mapping
// as the single-key GET path (see TestResolveClassifiesFailureStatus in
// resolve_test.go), rather than the bulk path having quietly grown its own,
// unclassified error handling.
//
// 404 is deliberately not in this table: unlike every status here, it never
// reaches classifyStatus at all (bulkGet has its own branch for it, mirroring
// get's), and it does not fail the batch the way these do - see
// TestResolveBatchNotFoundNamespaceOmitsKeys and
// TestResolveBatchSurvivesSiblingNamespaceNotFound below for its actual,
// divergent behavior.
func TestResolveBatchClassifiesFailureStatus(t *testing.T) {
	tests := []struct {
		code int
		want error
	}{
		{http.StatusUnauthorized, mamori.ErrUnauthenticated},
		{http.StatusForbidden, mamori.ErrPermissionDenied},
		{http.StatusTooManyRequests, mamori.ErrRateLimited},
		{http.StatusBadRequest, mamori.ErrInvalid},
		{http.StatusInternalServerError, mamori.ErrUnavailable},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			f := newFake()
			f.set(testNamespace, "k", []byte("v"))
			f.failStatus(testNamespace, tc.code)
			p := f.provider()

			_, err := p.ResolveBatch(context.Background(), []mamori.Ref{ref(t, "cloudflare-kv://k")})
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: got %v, want an error satisfying %v", tc.code, err, tc.want)
			}
		})
	}
}

// TestResolveBatchNotFoundNamespaceOmitsKeys pins the one status that
// diverges from TestResolveBatchClassifiesFailureStatus's table: a 404 on the
// bulk endpoint must not fail the batch at all. It means the namespace itself
// does not exist (see bulkGet's 404 branch), so ResolveBatch treats every key
// requested against it exactly like an absent key - omitted from the result
// map so mamori applies each ref's default - rather than returning an
// unclassified error and failing every sibling ref in the batch, which is
// what the shipped code did (a plain 404 fell into classifyStatus's default
// case).
func TestResolveBatchNotFoundNamespaceOmitsKeys(t *testing.T) {
	f := newFake()
	f.failStatus(testNamespace, http.StatusNotFound)
	p := f.provider()

	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{ref(t, "cloudflare-kv://k")})
	if err != nil {
		t.Fatalf("a 404 namespace must not fail the batch, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d values, want 0 (the 404 namespace's keys must be omitted)", len(got))
	}
}

// TestResolveBatchSurvivesSiblingNamespaceNotFound is Finding 1's headline
// regression: a batch spanning a good namespace and a namespace that 404s
// must still return the good namespace's values, matching the invariant
// doctor.go's probeField doc comment states for BatchProvider in general -
// ResolveBatch exists to cut round trips, not to change what a single ref
// resolves to. Before this fix, the 404 fell through classifyStatus's default
// case as an unclassified error and failed the whole call, losing the good
// namespace's ref along with the bad one's.
func TestResolveBatchSurvivesSiblingNamespaceNotFound(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "good", []byte("still-here"))
	f.failStatus("missing-namespace", http.StatusNotFound)
	p := f.provider()

	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{
		ref(t, "cloudflare-kv://good"),
		ref(t, "cloudflare-kv://k?namespace=missing-namespace"),
	})
	if err != nil {
		t.Fatalf("a sibling namespace's 404 must not fail the batch, got %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d values, want 1 (the good namespace's ref must survive)", len(got))
	}
	if string(got["cloudflare-kv://good"].Bytes) != "still-here" {
		t.Errorf("good namespace: got %q, want %q", got["cloudflare-kv://good"].Bytes, "still-here")
	}
	if _, ok := got["cloudflare-kv://k?namespace=missing-namespace"]; ok {
		t.Error("the 404 namespace's ref must be omitted, not resolved")
	}
}

// TestResolveBatchNamespaceWithSlashIsEscaped is
// TestResolveNamespaceWithSlashIsEscaped's counterpart for bulkGet
// (resolve_test.go's covers get): url.PathEscape(s.namespace) is a separate
// call in bulkGet, and a batch's namespace is exactly as ref-controlled
// (via ?namespace=) as a single Resolve's is.
func TestResolveBatchNamespaceWithSlashIsEscaped(t *testing.T) {
	f := newFake()
	const evilNamespace = "abc/def"
	f.set(evilNamespace, "k", []byte("v"))
	p := f.provider()

	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{ref(t, "cloudflare-kv://k?namespace="+evilNamespace)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got["cloudflare-kv://k?namespace="+evilNamespace].Bytes) != "v" {
		t.Fatalf("got %q, want %q", got["cloudflare-kv://k?namespace="+evilNamespace].Bytes, "v")
	}

	f.mu.Lock()
	path := f.lastPath
	f.mu.Unlock()
	wantSubstr := "/storage/kv/namespaces/abc%2Fdef/bulk/get"
	if !strings.Contains(path, wantSubstr) {
		t.Fatalf("request path %q does not contain the escaped namespace %q; the namespace must be url.PathEscape'd before it reaches the request URL", path, wantSubstr)
	}
}

func TestResolveBatchEmpty(t *testing.T) {
	f := newFake()
	p := f.provider()

	got, err := p.ResolveBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d values, want 0", len(got))
	}
	if _, bulk := f.counts(); bulk != 0 {
		t.Fatalf("an empty batch must make no requests, got %d bulk requests", bulk)
	}
}

// TestResolveBatchSendsTypeText pins a requirement the value-matching
// assertions above cannot catch on their own: without "type":"text" in the
// request body, Cloudflare may JSON-parse a value and return an object
// rather than the opaque string bytes this provider treats every value as.
// fake_test.go's handleBulk does not model that divergence - it always
// returns strings regardless of what "type" was sent - so this reads the
// request body directly via lastBulkType instead of relying on the fake to
// simulate the failure mode that omitting "type" would trigger for real.
func TestResolveBatchSendsTypeText(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "k", []byte("v"))
	p := f.provider()

	if _, err := p.ResolveBatch(context.Background(), []mamori.Ref{ref(t, "cloudflare-kv://k")}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	f.mu.Lock()
	got := f.lastBulkType
	f.mu.Unlock()
	if got != "text" {
		t.Fatalf(`got bulk request "type" %q, want "text"`, got)
	}
}

func TestProviderImplementsBatchProvider(t *testing.T) {
	var _ mamori.BatchProvider = (*Provider)(nil)
}

// TestProviderIsNotWatchable pins cloudflarekv.go's documented decision: the
// Workers KV REST API exposes no streaming or blocking read, so New() must
// not satisfy mamori.WatchableProvider, and mamori wraps it in the polling
// adapter instead.
func TestProviderIsNotWatchable(t *testing.T) {
	if _, ok := any(New()).(mamori.WatchableProvider); ok {
		t.Fatal("cloudflare-kv must not implement WatchableProvider")
	}
}
