package vercelgc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/xavidop/mamori"
)

func TestSchemeIsRegistered(t *testing.T) {
	found := false
	for _, s := range mamori.RegisteredSchemes() {
		if s == scheme {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("scheme %q is not registered; got %v", scheme, mamori.RegisteredSchemes())
	}
}

func ref(t *testing.T, tag string) mamori.Ref {
	t.Helper()
	r, err := mamori.ParseRef(tag)
	if err != nil {
		t.Fatalf("parsing ref %q: %v", tag, err)
	}
	return r
}

func TestResolveReturnsValue(t *testing.T) {
	f := newFake()
	f.set(testStore, "log-level", `"debug"`)
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "vercel-gc://log-level"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("got %q, want %q", v.Bytes, "debug")
	}
}

func TestResolveSendsBearerToken(t *testing.T) {
	f := newFake()
	f.set(testStore, "k", `"v"`)
	p := f.provider()

	if _, err := p.Resolve(context.Background(), ref(t, "vercel-gc://k")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.mu.Lock()
	auth := f.lastAuth
	f.mu.Unlock()
	if auth != "Bearer "+testToken {
		t.Fatalf("got Authorization %q, want %q", auth, "Bearer "+testToken)
	}
}

// The whole point of the digest: an unchanged store must never refetch bodies.
func TestResolveDigestGatesItemFetches(t *testing.T) {
	f := newFake()
	f.set(testStore, "a", `"1"`)
	f.set(testStore, "b", `"2"`)
	p := f.provider()
	ctx := context.Background()

	for range 5 {
		if _, err := p.Resolve(ctx, ref(t, "vercel-gc://a")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := p.Resolve(ctx, ref(t, "vercel-gc://b")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	digests, items := f.counts()
	if digests != 10 {
		t.Errorf("got %d digest requests, want 10 (one per Resolve)", digests)
	}
	if items != 1 {
		t.Errorf("got %d items requests, want 1 (the store never changed)", items)
	}

	f.set(testStore, "a", `"changed"`)
	v, err := p.Resolve(ctx, ref(t, "vercel-gc://a"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "changed" {
		t.Fatalf("got %q, want %q", v.Bytes, "changed")
	}
	if _, items = f.counts(); items != 2 {
		t.Errorf("got %d items requests after one edit, want 2", items)
	}

	// The fresh snapshot fetched above must actually be installed, not just
	// fetched and handed to this one caller: an implementation that refetched
	// on a digest change but never wrote the result back into p.snapshots would
	// pass every assertion so far, then silently refetch /items on every
	// following Resolve forever. Two more resolves of the same unchanged key
	// must therefore hold at 2 items requests, not climb to 3 or 4.
	for range 2 {
		v, err = p.Resolve(ctx, ref(t, "vercel-gc://a"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(v.Bytes) != "changed" {
			t.Fatalf("got %q, want %q", v.Bytes, "changed")
		}
	}
	if _, items = f.counts(); items != 2 {
		t.Errorf("got %d items requests after two further unchanged resolves, want still 2 (the fresh snapshot was not installed)", items)
	}
}

func TestResolveNotFound(t *testing.T) {
	f := newFake()
	f.set(testStore, "present", `"yes"`)
	p := f.provider()

	_, err := p.Resolve(context.Background(), ref(t, "vercel-gc://absent"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrNotFound", err)
	}
}

func TestResolveNullIsAValueNotNotFound(t *testing.T) {
	f := newFake()
	f.set(testStore, "maybe", `null`)
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "vercel-gc://maybe"))
	if err != nil {
		t.Fatalf("a key stored as null exists; got error %v", err)
	}
	if string(v.Bytes) != "null" {
		t.Fatalf("got %q, want %q", v.Bytes, "null")
	}
}

func TestResolveExplicitStore(t *testing.T) {
	f := newFake()
	f.set(testStore, "k", `"default-store"`)
	f.set("ecfg_other", "k", `"other-store"`)
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "vercel-gc://ecfg_other/k"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "other-store" {
		t.Fatalf("got %q, want %q", v.Bytes, "other-store")
	}
}

// Two stores must hold independent snapshots: editing one may not invalidate
// or shadow the other.
func TestResolveTwoStoresAreIndependent(t *testing.T) {
	f := newFake()
	f.set(testStore, "k", `"a1"`)
	f.set("ecfg_other", "k", `"b1"`)
	p := f.provider()
	ctx := context.Background()

	if _, err := p.Resolve(ctx, ref(t, "vercel-gc://k")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := p.Resolve(ctx, ref(t, "vercel-gc://ecfg_other/k")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, itemsBefore := f.counts()

	f.set("ecfg_other", "k", `"b2"`)

	v, err := p.Resolve(ctx, ref(t, "vercel-gc://k"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "a1" {
		t.Fatalf("default store changed under an edit to another store: got %q", v.Bytes)
	}
	if _, items := f.counts(); items != itemsBefore {
		t.Errorf("editing another store refetched the default store: %d then %d", itemsBefore, items)
	}

	v, err = p.Resolve(ctx, ref(t, "vercel-gc://ecfg_other/k"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "b2" {
		t.Fatalf("got %q, want %q", v.Bytes, "b2")
	}
}

// Concurrent resolves across two stores exercise the snapshot map and the
// benign duplicate-fetch path under -race. Values must stay coherent: a
// resolve never returns another store's body, and never a partially installed
// snapshot.
func TestResolveConcurrentAcrossStores(t *testing.T) {
	f := newFake()
	f.set(testStore, "k", `"a"`)
	f.set("ecfg_other", "k", `"b"`)
	p := f.provider()
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tag, want := "vercel-gc://k", "a"
			if i%2 == 1 {
				tag, want = "vercel-gc://ecfg_other/k", "b"
			}
			r, err := mamori.ParseRef(tag)
			if err != nil {
				errs <- err
				return
			}
			v, err := p.Resolve(ctx, r)
			if err != nil {
				errs <- err
				return
			}
			if string(v.Bytes) != want {
				errs <- fmt.Errorf("%s: got %q, want %q", tag, v.Bytes, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// The digest endpoint returns JSON whose exact shape Vercel does not pin, so
// both a bare string and an object with a digest field must parse.
func TestDigestShapes(t *testing.T) {
	for _, body := range []string{`"abc123"`, `{"digest":"abc123"}`} {
		t.Run(body, func(t *testing.T) {
			got, err := parseDigest([]byte(body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != "abc123" {
				t.Fatalf("got %q, want %q", got, "abc123")
			}
		})
	}
}

// An empty digest must never parse successfully: parseDigest("") installs a
// snapshot tagged with digest "", and because every later call finds
// held.digest == "" too, /items is never refetched again for the process
// lifetime - Refresh and Doctor would both report success while silently
// serving arbitrarily stale config, forever, with no error and no log. The
// object branch already rejects an empty "digest" field; this covers the
// bare-string branch, which used to accept both "" and null.
func TestDigestShapesRejected(t *testing.T) {
	for _, body := range []string{`""`, `null`, `{"digest":""}`, `[1,2,3]`} {
		t.Run(body, func(t *testing.T) {
			_, err := parseDigest([]byte(body))
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("parseDigest(%s): got %v, want an error satisfying mamori.ErrInvalid", body, err)
			}
		})
	}
}

func TestResolveErrorClassification(t *testing.T) {
	tests := []struct {
		code int
		want error
	}{
		{http.StatusUnauthorized, mamori.ErrUnauthenticated},
		{http.StatusForbidden, mamori.ErrPermissionDenied},
		{http.StatusTooManyRequests, mamori.ErrRateLimited},
		{http.StatusBadRequest, mamori.ErrInvalid},
		{http.StatusInternalServerError, mamori.ErrUnavailable},
		{http.StatusBadGateway, mamori.ErrUnavailable},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			f := newFake()
			f.set(testStore, "k", `"v"`)
			p := f.provider()
			f.failStatus(testStore, tc.code)

			_, err := p.Resolve(context.Background(), ref(t, "vercel-gc://k"))
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: got %v, want an error satisfying %v", tc.code, err, tc.want)
			}
		})
	}
}

// A failed items fetch must not poison the held snapshot: once the backend
// recovers, the next Resolve must return the fresh value rather than getting
// stuck on whatever the failed attempt half-observed.
//
// The digest is left succeeding throughout (via failEndpoint, not
// failStatus): failing the whole store would fail the digest fetch that
// precedes snapshotFor, so snapshotFor's fetch-and-install path - the only
// code that could actually install a bad snapshot - would never run. Bumping
// the store between priming and the injected failure is what forces
// snapshotFor to see a moved digest and attempt the doomed fetch, rather than
// short-circuiting on a still-matching one.
func TestResolveRecoversAfterFailureCleared(t *testing.T) {
	f := newFake()
	f.set(testStore, "k", `"v"`)
	p := f.provider()
	ctx := context.Background()

	if _, err := p.Resolve(ctx, ref(t, "vercel-gc://k")); err != nil {
		t.Fatalf("priming resolve failed: %v", err)
	}

	f.set(testStore, "k", `"changed"`)
	f.failEndpoint(testStore, "items", http.StatusInternalServerError)
	if _, err := p.Resolve(ctx, ref(t, "vercel-gc://k")); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrUnavailable", err)
	}

	f.clearFail(testStore)
	v, err := p.Resolve(ctx, ref(t, "vercel-gc://k"))
	if err != nil {
		t.Fatalf("resolve after clearing the failure: %v", err)
	}
	if string(v.Bytes) != "changed" {
		t.Fatalf("got %q, want %q", v.Bytes, "changed")
	}
}

// TestResolveErrorClassification (above) fails the whole store, so every case
// there is actually observed through the digest fetch: fetchDigest runs first,
// and a whole-store failure lands on it before an items fetch ever happens.
// This exercises the other half of classifyStatus's callers - a failure on the
// items fetch itself - which otherwise has zero coverage. As above, the store
// is bumped after priming so the digest fetch succeeds and snapshotFor
// actually attempts (and fails) fetchItems.
func TestResolveItemsFetchErrorIsClassified(t *testing.T) {
	f := newFake()
	f.set(testStore, "k", `"v"`)
	p := f.provider()
	ctx := context.Background()

	if _, err := p.Resolve(ctx, ref(t, "vercel-gc://k")); err != nil {
		t.Fatalf("priming resolve failed: %v", err)
	}

	f.set(testStore, "k", `"changed"`)
	f.failEndpoint(testStore, "items", http.StatusForbidden)

	_, err := p.Resolve(ctx, ref(t, "vercel-gc://k"))
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrPermissionDenied", err)
	}
}

func TestResolveUnknownStoreIsNotFound(t *testing.T) {
	f := newFake()
	p := f.provider()

	_, err := p.Resolve(context.Background(), ref(t, "vercel-gc://ecfg_missing/k"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrNotFound", err)
	}
}

func TestResolveHonorsContextCancellation(t *testing.T) {
	f := newFake()
	f.set(testStore, "k", `"v"`)
	p := f.provider()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.Resolve(ctx, ref(t, "vercel-gc://k")); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
