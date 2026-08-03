package httpcore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/xavidop/mamori"
)

// newCountingClient returns a Client whose transport records every request and
// answers 200 with body, then 304 once the caller sends a matching
// If-None-Match.
//
// The recording is mutex-guarded because TestRevalidatorConcurrentGets drives
// this transport from many goroutines at once. The callers read calls and
// conditionals only after wg.Wait(), so the lock is needed for the writes alone.
func newCountingClient(t *testing.T, etag string, body []byte) (*Client, *int, *[]string) {
	t.Helper()
	calls := 0
	var conditionals []string
	var mu sync.Mutex
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			calls++
			inm := req.Header.Get("If-None-Match")
			conditionals = append(conditionals, inm)
			mu.Unlock()
			h := http.Header{}
			h.Set("ETag", etag)
			if inm == etag {
				resp, _ := newResponse(http.StatusNotModified, nil, h)
				return resp, nil
			}
			resp, _ := newResponse(http.StatusOK, body, h)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, &calls, &conditionals
}

func TestRevalidatorSendsValidatorOnSecondGet(t *testing.T) {
	c, calls, conditionals := newCountingClient(t, `"v1"`, []byte("payload"))
	rv := NewRevalidator(c, 8)

	first, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if string(first.Body) != "payload" || first.NotModified {
		t.Fatalf("first = %+v", first)
	}

	second, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("calls = %d, want 2", *calls)
	}
	if (*conditionals)[0] != "" {
		t.Fatalf("first request sent If-None-Match %q, want none", (*conditionals)[0])
	}
	if (*conditionals)[1] != `"v1"` {
		t.Fatalf("second request sent If-None-Match %q, want \"v1\"", (*conditionals)[1])
	}
	// The caller gets the cached body back, not an empty one.
	if string(second.Body) != "payload" {
		t.Fatalf("second body = %q, want the cached payload", second.Body)
	}
	if !second.NotModified {
		t.Fatal("second NotModified = false, want true")
	}
}

func TestRevalidatorKeysSeparately(t *testing.T) {
	c, calls, conditionals := newCountingClient(t, `"v1"`, []byte("payload"))
	rv := NewRevalidator(c, 8)

	if _, err := rv.Get(context.Background(), "a", Request{Path: "cfg"}); err != nil {
		t.Fatalf("Get a: %v", err)
	}
	if _, err := rv.Get(context.Background(), "b", Request{Path: "cfg"}); err != nil {
		t.Fatalf("Get b: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("calls = %d, want 2", *calls)
	}
	for i, inm := range *conditionals {
		if inm != "" {
			t.Fatalf("request %d sent If-None-Match %q, want none for a distinct key", i, inm)
		}
	}
}

func TestRevalidatorEvictsBeyondMaxEntries(t *testing.T) {
	c, _, _ := newCountingClient(t, `"v1"`, []byte("payload"))
	rv := NewRevalidator(c, 2)

	for _, k := range []string{"a", "b", "c"} {
		if _, err := rv.Get(context.Background(), k, Request{Path: "cfg"}); err != nil {
			t.Fatalf("Get %s: %v", k, err)
		}
	}
	if got := rv.len(); got != 2 {
		t.Fatalf("entries = %d, want 2", got)
	}
}

func TestRevalidatorDropsEntryOnError(t *testing.T) {
	var fail bool
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			if fail {
				resp, _ := newResponse(http.StatusInternalServerError, nil, nil)
				return resp, nil
			}
			h := http.Header{}
			h.Set("ETag", `"v1"`)
			resp, _ := newResponse(http.StatusOK, []byte("payload"), h)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rv := NewRevalidator(c, 8)

	if _, err := rv.Get(context.Background(), "k", Request{Path: "cfg"}); err != nil {
		t.Fatalf("seed Get: %v", err)
	}
	fail = true
	if _, err := rv.Get(context.Background(), "k", Request{Path: "cfg"}); err == nil {
		t.Fatal("failing Get returned nil error")
	}
	// A failed revalidation must not leave a stale validator behind, or the next
	// success would be answered from a cache entry the backend never confirmed.
	if got := rv.len(); got != 0 {
		t.Fatalf("entries = %d after failure, want 0", got)
	}
}

// TestRevalidatorKeepsCachedValidatorsOn304 pins that a 304 hit reports the
// validators the cache holds, not whatever the 304 response happened to carry.
//
// RFC 7232 says a backend should repeat ETag on a 304, but real backends, CDNs
// and proxies especially, sometimes omit it. Copying the 304's own empty ETag
// makes Version fall back to a body hash, so a genuinely unmodified poll reports
// a changed Version and mamori runs a spurious update.
//
// newCountingClient cannot express this, because it sets ETag on every response
// and so cannot distinguish "validator from the cache" from "validator from the
// response". That is exactly why this test builds its own backend.
func TestRevalidatorKeepsCachedValidatorsOn304(t *testing.T) {
	calls := 0
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Header.Get("If-None-Match") == `"v1"` {
				// Deliberately omit the validators, as a non-compliant backend does.
				resp, _ := newResponse(http.StatusNotModified, nil, nil)
				return resp, nil
			}
			h := http.Header{}
			h.Set("ETag", `"v1"`)
			h.Set("Last-Modified", "Wed, 21 Oct 2026 07:28:00 GMT")
			resp, _ := newResponse(http.StatusOK, []byte("payload"), h)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rv := NewRevalidator(c, 8)

	first, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	second, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if second.ETag != first.ETag {
		t.Fatalf("ETag on the 304 = %q, want the cached %q", second.ETag, first.ETag)
	}
	if second.LastModified != first.LastModified {
		t.Fatalf("LastModified on the 304 = %q, want the cached %q", second.LastModified, first.LastModified)
	}
	if got, want := Version(second, second.Body), Version(first, first.Body); got != want {
		t.Fatalf("Version changed across an unmodified poll: %q then %q", want, got)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

// TestRevalidatorDoesNotAliasCachedBody pins that a caller cannot corrupt the
// cache by writing into a body it was handed. Without a copy on both sides, the
// Revalidator and every caller share one backing array, so one caller decoding
// in place silently changes what the next poll returns.
func TestRevalidatorDoesNotAliasCachedBody(t *testing.T) {
	c, _, _ := newCountingClient(t, `"v1"`, []byte("payload"))
	rv := NewRevalidator(c, 8)

	first, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	first.Body[0] = 'X'

	second, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if string(second.Body) != "payload" {
		t.Fatalf("cached body = %q, want payload; a caller's write reached the cache", second.Body)
	}
}

// TestRevalidatorRetriesWhenEntryEvictedDuringRequest covers the gap between
// reading the validators and writing the response back. Get releases the lock
// for the network call, so another caller can evict the entry in between. The
// backend then answers 304 for a validator this Revalidator no longer holds,
// and there is no cached body to return, so the fallback must retry
// unconditionally rather than hand back an empty one.
//
// The eviction is forced from inside the RoundTripper, which IS the window
// under test: it runs after validators() and before store(). Get holds no lock
// there, so calling drop from the transport cannot deadlock.
func TestRevalidatorRetriesWhenEntryEvictedDuringRequest(t *testing.T) {
	var rv *Revalidator
	evicted := false
	calls := 0

	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			calls++
			h := http.Header{}
			h.Set("ETag", `"v1"`)
			if req.Header.Get("If-None-Match") == `"v1"` {
				if !evicted {
					evicted = true
					rv.drop("k")
				}
				resp, _ := newResponse(http.StatusNotModified, nil, h)
				return resp, nil
			}
			resp, _ := newResponse(http.StatusOK, []byte("payload"), h)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rv = NewRevalidator(c, 8)

	if _, err := rv.Get(context.Background(), "k", Request{Path: "cfg"}); err != nil {
		t.Fatalf("seed Get: %v", err)
	}
	got, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if string(got.Body) != "payload" {
		t.Fatalf("Body = %q, want the payload recovered by the unconditional retry", got.Body)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3: the seed, the 304 whose entry vanished, and the retry", calls)
	}
}

// TestRevalidatorFailsOn304WithoutValidators pins that a 304 answering a request
// that carried no validators fails loudly instead of returning an empty body.
//
// This is not only the eviction race. It is EVERY FIRST POLL of a key: with no
// cache entry, Get sends an unconditional request, and a backend that answers
// 304 anyway used to fall straight through to a Response with a nil Body and a
// nil error. mamori applies that as an empty string, which is the same silently
// wrong value the cached-validator rule exists to prevent, on the one path that
// rule cannot cover.
//
// The kind is ErrUnavailable rather than ErrInvalid: a backend violating RFC
// 7232 is a backend fault that may clear, so mamori should back off and retry
// rather than treat the ref as permanently broken.
func TestRevalidatorFailsOn304WithoutValidators(t *testing.T) {
	calls := 0
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			calls++
			resp, _ := newResponse(http.StatusNotModified, nil, nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rv := NewRevalidator(c, 8)

	resp, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err == nil {
		t.Fatalf("first Get returned %+v with a nil error; an empty body would be applied as an empty value", resp)
	}
	if !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if resp != nil {
		t.Fatalf("Get returned a non-nil Response %+v alongside its error", resp)
	}
	// Two calls: the unconditional first poll, and the unconditional retry the
	// no-cached-body branch makes before giving up.
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if got := rv.len(); got != 0 {
		t.Fatalf("entries = %d after the failure, want 0", got)
	}
}

func TestRevalidatorConcurrentGets(t *testing.T) {
	c, _, _ := newCountingClient(t, `"v1"`, []byte("payload"))
	rv := NewRevalidator(c, 64)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i%4)
			if _, err := rv.Get(context.Background(), key, Request{Path: "cfg"}); err != nil {
				t.Errorf("Get: %v", err)
			}
		}(i)
	}
	wg.Wait()
}
