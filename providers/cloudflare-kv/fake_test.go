package cloudflarekv

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

const (
	testToken     = "test-token"
	testAccount   = "test-account-id"
	testNamespace = "test-namespace-id"
)

// Path markers the fake uses to parse the URL Resolve (Task 2) and
// ResolveBatch (Task 3) build. pathAccountsPrefix is matched with
// strings.Index rather than strings.HasPrefix because defaultBaseURL itself
// carries a path ("/client/v4"), so the accounts segment does not start at
// the beginning of r.URL.Path.
const (
	pathAccountsPrefix  = "/accounts/"
	pathNamespaceMarker = "/storage/kv/namespaces/"
	pathValuesMarker    = "/values/"
	pathBulkGetSuffix   = "/bulk/get"
)

// fakeKV is an in-memory emulation of the Workers KV REST API. It serves the
// single-key value endpoint and the bulk endpoint, and records enough about
// each request for tests to assert against: the exact escaped path, the
// Authorization header, and per-endpoint request counts.
type fakeKV struct {
	mu sync.Mutex

	// values is namespace -> key -> stored bytes.
	values map[string]map[string][]byte
	// failCode is namespace -> status code to return for every request
	// against that namespace, until the test replaces the fake. There is no
	// clearFail: every test starts from a fresh newFake().
	failCode map[string]int

	getReq  int
	bulkReq int

	lastAuth string
	// lastPath is the exact request path as it appeared on the wire,
	// including any percent-escaping: this is what lets a test assert that a
	// key containing slashes travelled as one url.PathEscape'd segment
	// rather than as extra path segments.
	lastPath string

	// bulkSizes records len(keys) for every bulk request served, in request
	// order. Task 3's chunk-boundary tests read this to assert not just how
	// many bulk requests were made but exactly how many keys each one carried
	// - the off-by-one case at exactly 100/101 keys needs that precision.
	bulkSizes []int
	// lastBulkType records the "type" field of the most recent bulk request
	// body. Cloudflare's real bulk/get endpoint may JSON-parse values and
	// return objects, rather than the opaque strings this provider expects,
	// when "type" is omitted; this fake does not model that divergence (it
	// always returns strings), so lastBulkType exists to let a test pin that
	// ResolveBatch sends "type":"text" on the wire even though a fake that
	// ignored the field entirely would still "pass" on values alone.
	lastBulkType string
}

func newFake() *fakeKV {
	return &fakeKV{
		values:   map[string]map[string][]byte{},
		failCode: map[string]int{},
	}
}

// set stores value under (namespace, key), as a real Workers KV write would.
func (f *fakeKV) set(namespace, key string, value []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.values[namespace] == nil {
		f.values[namespace] = map[string][]byte{}
	}
	f.values[namespace][key] = value
}

// del removes a key, as a real Workers KV delete would. A read after del
// observes the same 404 an absent key has always produced: the fake keeps no
// separate "never written" state.
func (f *fakeKV) del(namespace, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.values[namespace], key)
}

// failStatus makes every request against namespace - single-key GET and bulk
// POST alike - return code, until the test replaces the fake with a new one.
func (f *fakeKV) failStatus(namespace string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCode[namespace] = code
}

// counts returns how many single-key GET and bulk POST requests have been
// served.
func (f *fakeKV) counts() (get, bulk int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getReq, f.bulkReq
}

// handle routes one request to the single-key GET endpoint or the bulk POST
// endpoint.
//
// Routing reads r.URL.Path, which net/http has already percent-decoded, so a
// key containing slashes reassembles into literal slashes by the time it
// reaches here; that is exactly why the key is taken as everything after the
// last marker rather than by splitting the whole path into a fixed number of
// segments. What actually went over the wire before that decoding - the
// escaped form Resolve is required to send - is captured separately as
// lastPath, from r.URL.EscapedPath().
func (f *fakeKV) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lastAuth = r.Header.Get("Authorization")
	f.lastPath = r.URL.EscapedPath()
	f.mu.Unlock()

	idx := strings.Index(r.URL.Path, pathAccountsPrefix)
	if idx < 0 {
		http.NotFound(w, r)
		return
	}
	rest := r.URL.Path[idx+len(pathAccountsPrefix):]
	_, rest, ok := strings.Cut(rest, pathNamespaceMarker)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch {
	case r.Method == http.MethodGet:
		namespace, key, ok := strings.Cut(rest, pathValuesMarker)
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.handleGet(w, namespace, key)
	case r.Method == http.MethodPost && strings.HasSuffix(rest, pathBulkGetSuffix):
		namespace := strings.TrimSuffix(rest, pathBulkGetSuffix)
		f.handleBulk(w, r, namespace)
	default:
		http.NotFound(w, r)
	}
}

// handleGet serves the single-key value endpoint. Unlike handleBulk, a
// success response body is the raw stored bytes, not a JSON envelope: this
// mirrors the real API's asymmetry that resolve.go documents at its call
// site.
func (f *fakeKV) handleGet(w http.ResponseWriter, namespace, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getReq++

	if code, failing := f.failCode[namespace]; failing {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":0,"message":"injected failure"}],"messages":[]}`)
		return
	}
	val, ok := f.values[namespace][key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":10009,"message":"key not found"}],"messages":[]}`)
		return
	}
	_, _ = w.Write(val)
}

// bulkGetRequest is the body Task 3's ResolveBatch POSTs. Type is captured
// (not just Keys) so a test can pin that ResolveBatch sends "type":"text";
// see lastBulkType.
type bulkGetRequest struct {
	Keys []string `json:"keys"`
	Type string   `json:"type"`
}

// bulkGetResponse is the envelope the bulk endpoint wraps every value in,
// unlike the single-key GET endpoint's raw bytes.
type bulkGetResponse struct {
	Success  bool              `json:"success"`
	Result   bulkGetResult     `json:"result"`
	Errors   []bulkGetErrorMsg `json:"errors"`
	Messages []bulkGetErrorMsg `json:"messages"`
}

type bulkGetResult struct {
	Values map[string]string `json:"values"`
}

type bulkGetErrorMsg struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// handleBulk serves the bulk endpoint Task 3's ResolveBatch reads. It is
// implemented here, alongside handleGet, so Task 3 needs no further routing
// changes to this fake: only its own test callers.
func (f *fakeKV) handleBulk(w http.ResponseWriter, r *http.Request, namespace string) {
	f.mu.Lock()
	f.bulkReq++
	code, failing := f.failCode[namespace]
	f.mu.Unlock()

	if failing {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":0,"message":"injected failure"}],"messages":[]}`)
		return
	}

	var body bulkGetRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":0,"message":"malformed request body"}],"messages":[]}`)
		return
	}

	f.mu.Lock()
	f.bulkSizes = append(f.bulkSizes, len(body.Keys))
	f.lastBulkType = body.Type
	values := map[string]string{}
	for _, k := range body.Keys {
		if v, ok := f.values[namespace][k]; ok {
			values[k] = string(v)
		}
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(bulkGetResponse{
		Success:  true,
		Result:   bulkGetResult{Values: values},
		Errors:   []bulkGetErrorMsg{},
		Messages: []bulkGetErrorMsg{},
	})
}

// roundTripper drives fakeKV's handler in-process, with no listener and no
// background goroutine. This is deliberate rather than a real
// httptest.Server: providertest's NoGoroutineLeak case (Task 4) runs
// goleak.VerifyNone with no ignore options, which a live server's accept
// goroutine can never satisfy.
type roundTripper struct{ f *fakeKV }

func (rt roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	// Honor cancellation explicitly. http.Client delegates context handling
	// to the transport, so a RoundTripper that ignores it would serve a
	// request whose context is already dead and hide a provider that forgot
	// to thread ctx through.
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	rec := httptest.NewRecorder()
	rt.f.handle(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// provider builds a Provider talking to this fake in-process, with no
// network listener. Extra options are applied last so a test can override
// any default.
func (f *fakeKV) provider(opts ...Option) *Provider {
	base := []Option{
		WithAPIToken(testToken),
		WithAccountID(testAccount),
		WithNamespaceID(testNamespace),
		WithHTTPClient(&http.Client{Transport: roundTripper{f: f}}),
	}
	return New(append(base, opts...)...)
}
