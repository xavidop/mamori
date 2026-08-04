package https

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"sync"
)

// fakeBackend is an in-process HTTP backend. An httptest.Server is deliberately
// not used: the conformance kit's NoGoroutineLeak case snapshots goroutines with
// goleak.IgnoreCurrent and a live server's accept goroutine does not survive it.
type fakeBackend struct {
	mu         sync.Mutex
	values     map[string][]byte
	etags      map[string]string
	failStatus int
	seq        int
	lastPath   string
	lastQuery  string
	lastHeader http.Header
}

// newFake returns an empty backend.
func newFake() *fakeBackend {
	return &fakeBackend{values: map[string][]byte{}, etags: map[string]string{}}
}

// set writes a value and advances its ETag, so a conditional GET sees a change.
func (f *fakeBackend) set(path string, val []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.values[path] = val
	f.etags[path] = `"v` + strconv.Itoa(f.seq) + `"`
}

// fail makes every request answer status until clearFail.
func (f *fakeBackend) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus = status
}

// clearFail cancels fail.
func (f *fakeBackend) clearFail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus = 0
}

// transport returns an http.RoundTripper serving this backend.
func (f *fakeBackend) transport() http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		// Honor cancellation explicitly. http.Client delegates context handling
		// to the transport (a real net/http.Transport notices a dead context
		// deep in its dial and connection-reuse machinery), and this in-process
		// fake has none of that machinery to fall back on. Skipping this check
		// would answer a cancelled-context request as if it had succeeded and
		// hide a provider that forgot to thread ctx through to the request it
		// builds.
		if err := req.Context().Err(); err != nil {
			return nil, err
		}

		f.mu.Lock()
		defer f.mu.Unlock()

		// EscapedPath, not Path: Path is the decoded form, so a percent-encoded
		// traversal would look like a real one here even though the wire never
		// carried it.
		f.lastPath = req.URL.EscapedPath()
		f.lastQuery = req.URL.RawQuery
		f.lastHeader = req.Header.Clone()

		if f.failStatus != 0 {
			return newResp(f.failStatus, nil, ""), nil
		}
		val, ok := f.values[req.URL.Path]
		if !ok {
			return newResp(http.StatusNotFound, nil, ""), nil
		}
		etag := f.etags[req.URL.Path]
		if req.Header.Get("If-None-Match") == etag {
			return newResp(http.StatusNotModified, nil, etag), nil
		}
		return newResp(http.StatusOK, val, etag), nil
	})
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// newResp builds a response with an optional ETag.
func newResp(status int, body []byte, etag string) *http.Response {
	h := http.Header{}
	if etag != "" {
		h.Set("ETag", etag)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// provider builds a Provider serving this backend under the endpoint name
// "test", with values rooted at /v1.
func (f *fakeBackend) provider() *Provider {
	p, err := New(Endpoint{
		Name:    "test",
		BaseURL: "https://api.test/v1",
		Client:  &http.Client{Transport: f.transport()},
	})
	if err != nil {
		panic("building fake provider: " + err.Error())
	}
	return p
}
