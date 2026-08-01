package vercelgc

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

const (
	testStore = "ecfg_test"
	testToken = "tok_test"
)

// fakeGC is an in-memory emulation of the Global Config read API. It serves
// GET /<store>/digest and GET /<store>/items, and counts both so tests can
// assert exactly how many item bodies were fetched.
type fakeGC struct {
	mu     sync.Mutex
	stores map[string]map[string]jsonRaw // store id -> key -> raw JSON
	rev    map[string]int                // store id -> bumped on every edit, rendered as the digest
	// failCode is store id -> endpoint -> status to return until cleared. The
	// empty endpoint key ("") means every endpoint, which is what failStatus
	// uses; failEndpoint sets one endpoint only, letting a test fail /items
	// while /digest keeps succeeding (or vice versa).
	failCode  map[string]map[string]int
	digestReq int
	itemsReq  int
	lastAuth  string

	// malformedItems is store id -> whether its /items endpoint should serve
	// a body that fails to json.Unmarshal into map[string]jsonRaw, until the
	// test replaces the fake. It powers TestResolveItemsDecodeFailureIsInvalid.
	malformedItems map[string]bool

	// callOrder records every request's endpoint ("digest" or "items"), in
	// the order the fake's handler observed them, regardless of outcome
	// (success, injected failure, or unknown store). It powers
	// TestResolveFetchesDigestBeforeItems.
	callOrder []string
}

func newFake() *fakeGC {
	return &fakeGC{
		stores:         map[string]map[string]jsonRaw{},
		rev:            map[string]int{},
		failCode:       map[string]map[string]int{},
		malformedItems: map[string]bool{},
	}
}

// setMalformedItems makes store's /items endpoint serve a body that fails to
// json.Unmarshal into map[string]jsonRaw, until the test replaces the fake.
func (f *fakeGC) setMalformedItems(store string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.malformedItems[store] = true
}

// order returns every request's endpoint observed so far, in arrival order.
func (f *fakeGC) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.callOrder...)
}

// set writes a raw JSON value and bumps the store digest, exactly as a real
// Global Config edit does.
func (f *fakeGC) set(store, key, rawJSON string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stores[store] == nil {
		f.stores[store] = map[string]jsonRaw{}
	}
	f.stores[store][key] = jsonRaw(rawJSON)
	f.rev[store]++
}

// failStatus makes every request for store return code until clearFail.
func (f *fakeGC) failStatus(store string, code int) {
	f.setFail(store, "", code)
}

// failEndpoint makes only requests to store's endpoint ("digest" or "items")
// return code until clearFail, leaving the other endpoint to succeed.
//
// This is what lets a test put the injected failure strictly after the digest
// fetch: failStatus fails the digest fetch too, so Resolve never reaches
// snapshotFor's fetch-and-install path at all. failEndpoint(store, "items",
// ...) instead lets the digest succeed, so snapshotFor is entered, sees a
// moved digest, and its call to fetchItems is what fails - the only path that
// can genuinely test that a failed fetch does not install a snapshot, and the
// only path that exercises classifyStatus from the items fetch rather than the
// digest fetch.
func (f *fakeGC) failEndpoint(store, endpoint string, code int) {
	f.setFail(store, endpoint, code)
}

func (f *fakeGC) setFail(store, endpoint string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCode[store] == nil {
		f.failCode[store] = map[string]int{}
	}
	f.failCode[store][endpoint] = code
}

// clearFail removes every injected failure for store, whole-store and
// per-endpoint alike.
func (f *fakeGC) clearFail(store string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failCode, store)
}

// failCodeFor returns the injected status for (store, endpoint), preferring an
// endpoint-specific failure over a whole-store one. Callers must hold f.mu.
func (f *fakeGC) failCodeFor(store, endpoint string) (code int, fail bool) {
	byEndpoint, ok := f.failCode[store]
	if !ok {
		return 0, false
	}
	if code, ok := byEndpoint[endpoint]; ok {
		return code, true
	}
	if code, ok := byEndpoint[""]; ok {
		return code, true
	}
	return 0, false
}

// counts returns how many digest and items requests have been served.
func (f *fakeGC) counts() (digest, items int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.digestReq, f.itemsReq
}

func (f *fakeGC) handle(w http.ResponseWriter, r *http.Request) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) != 2 {
		http.NotFound(w, r)
		return
	}
	store, endpoint := segs[0], segs[1]

	f.mu.Lock()
	f.lastAuth = r.Header.Get("Authorization")
	f.callOrder = append(f.callOrder, endpoint)
	if code, ok := f.failCodeFor(store, endpoint); ok {
		f.mu.Unlock()
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"error":{"code":"injected","message":"injected failure"}}`)
		return
	}
	items, known := f.stores[store]
	rev := f.rev[store]
	switch endpoint {
	case "digest":
		f.digestReq++
	case "items":
		f.itemsReq++
	}
	f.mu.Unlock()

	if !known {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"edge_config_not_found"}}`)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	switch endpoint {
	case "digest":
		// The read API documents a digest endpoint returning JSON but does not
		// pin the shape, so the fake serves the bare-string form and
		// TestDigestShapes covers the object form as well.
		_, _ = io.WriteString(w, strconv.Quote("rev"+strconv.Itoa(rev)))
	case "items":
		f.mu.Lock()
		malformed := f.malformedItems[store]
		f.mu.Unlock()
		if malformed {
			// Deliberately invalid JSON: fetchItems's json.Unmarshal into
			// map[string]jsonRaw must fail on this, exercising the decode
			// failure branch TestResolveItemsDecodeFailureIsInvalid pins.
			_, _ = io.WriteString(w, `{not valid json`)
			return
		}
		out := map[string]json.RawMessage{}
		f.mu.Lock()
		for k, v := range items {
			out[k] = json.RawMessage(v)
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	default:
		http.NotFound(w, r)
	}
}

// roundTripper drives the same handler in-process, with no listener and no
// background goroutine. This is deliberate rather than a real httptest.Server:
// providertest's NoGoroutineLeak case runs goleak.VerifyNone with no ignore
// options, which a live server's accept goroutine can never satisfy, so the
// conformance suite (Task 5) needs a transport with no background goroutine at
// all. Using the same transport here too keeps one fake for every test rather
// than two.
type roundTripper struct{ f *fakeGC }

func (rt roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	// Honor cancellation explicitly. http.Client delegates context handling to
	// the transport, so a RoundTripper that ignores it would serve a request
	// whose context is already dead and hide a provider that forgot to thread
	// ctx through.
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	rec := httptest.NewRecorder()
	rt.f.handle(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// provider builds a Provider talking to this fake in-process, with no network
// listener. Extra options are applied last so a test can override the store.
func (f *fakeGC) provider(opts ...Option) *Provider {
	base := []Option{
		WithConnectionString(fmt.Sprintf("https://global-config.vercel.com/%s?token=%s", testStore, testToken)),
		WithHTTPClient(&http.Client{Transport: roundTripper{f: f}}),
	}
	return New(append(base, opts...)...)
}
