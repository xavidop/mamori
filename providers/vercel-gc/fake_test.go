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
	mu        sync.Mutex
	stores    map[string]map[string]jsonRaw // store id -> key -> raw JSON
	rev       map[string]int                // store id -> bumped on every edit, rendered as the digest
	failCode  map[string]int                // store id -> status to return until cleared
	digestReq int
	itemsReq  int
	lastAuth  string
}

func newFake() *fakeGC {
	return &fakeGC{
		stores:   map[string]map[string]jsonRaw{},
		rev:      map[string]int{},
		failCode: map[string]int{},
	}
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCode[store] = code
}

func (f *fakeGC) clearFail(store string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failCode, store)
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
	if code, ok := f.failCode[store]; ok {
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
