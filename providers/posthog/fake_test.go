package posthog

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sync"
)

// fakeBackend is an in-process PostHog flag endpoint. An httptest.Server is
// deliberately not used: the conformance kit's NoGoroutineLeak case runs
// goleak.VerifyNone, and a live server's accept goroutine does not survive it.
type fakeBackend struct {
	mu    sync.Mutex
	flags map[string]flagResult

	failStatus   int
	quotaLimited []string
	computeError bool

	// Observed request state, so tests can assert what actually reached the
	// wire rather than what the provider meant to send.
	lastMethod string
	lastPath   string
	lastQuery  url.Values
	lastHeader http.Header
	lastBody   flagsRequest
	// lastRawBody is the undecoded body, for asserting on field names rather
	// than on the struct they decode into.
	lastRawBody []byte
	calls       int
}

// newFake returns a backend with no flags, which answers every evaluation with
// an empty flags object exactly as PostHog does for an unknown key.
func newFake() *fakeBackend {
	return &fakeBackend{flags: map[string]flagResult{}}
}

// setBool seeds a boolean flag: enabled state, no variant.
func (f *fakeBackend) setBool(key string, enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flags[key] = flagResult{Key: key, Enabled: enabled}
}

// setVariant seeds a multivariate flag: enabled with a variant key.
func (f *fakeBackend) setVariant(key, variant string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flags[key] = flagResult{Key: key, Enabled: true, Variant: variant}
}

// setPayload seeds an enabled flag whose payload is the JSON-encoded string
// PostHog documents, which is the shape the conformance kit reads back.
func (f *fakeBackend) setPayload(key, payload string) {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic("marshalling fake payload: " + err.Error())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fr := flagResult{Key: key, Enabled: true}
	fr.Metadata.Payload = raw
	f.flags[key] = fr
}

// setRawPayload seeds a flag whose payload is the given raw JSON, for the
// non-string payload case.
func (f *fakeBackend) setRawPayload(key, rawJSON string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fr := flagResult{Key: key, Enabled: true}
	fr.Metadata.Payload = json.RawMessage(rawJSON)
	f.flags[key] = fr
}

// fail makes every evaluation answer status until clearFail.
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

// setQuotaLimited makes the backend answer 200 with an empty flags object and
// the given quotaLimited array, which is how PostHog reports a billing pause.
func (f *fakeBackend) setQuotaLimited(resources ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quotaLimited = resources
}

// setComputeError sets errorsWhileComputingFlags on every response.
func (f *fakeBackend) setComputeError(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.computeError = v
}

// observed returns a copy of the last request the backend saw.
func (f *fakeBackend) observed() (method, path string, q url.Values, h http.Header, body flagsRequest, raw []byte, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastMethod, f.lastPath, f.lastQuery, f.lastHeader, f.lastBody, f.lastRawBody, f.calls
}

// transport returns an http.RoundTripper serving this backend.
func (f *fakeBackend) transport() http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		// Honor cancellation explicitly. net/http enforces the request context
		// in the Transport, not in Client.Do, and this in-process fake has none
		// of a real Transport's dial and connection-reuse machinery to notice a
		// dead context in. Without this check the ContextCancel conformance
		// case would pass vacuously, hiding a provider that never threaded ctx
		// into the request it builds.
		if err := req.Context().Err(); err != nil {
			return nil, err
		}

		var raw []byte
		if req.Body != nil {
			b, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			_ = req.Body.Close()
			raw = b
		}

		f.mu.Lock()
		defer f.mu.Unlock()

		f.calls++
		f.lastMethod = req.Method
		f.lastPath = req.URL.EscapedPath()
		f.lastQuery = req.URL.Query()
		f.lastHeader = req.Header.Clone()
		f.lastRawBody = raw
		f.lastBody = flagsRequest{}
		_ = json.Unmarshal(raw, &f.lastBody)

		if f.failStatus != 0 {
			return newResp(f.failStatus, []byte(`{"error":"injected"}`)), nil
		}

		env := flagsEnvelope{
			Flags:                     map[string]flagResult{},
			ErrorsWhileComputingFlags: f.computeError,
			QuotaLimited:              f.quotaLimited,
		}
		// A quota pause answers 200 with no flags at all, whatever is seeded.
		if len(f.quotaLimited) == 0 {
			for k, v := range f.flags {
				env.Flags[k] = v
			}
		}
		out, err := json.Marshal(env)
		if err != nil {
			return nil, err
		}
		return newResp(http.StatusOK, out), nil
	})
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// newResp builds a JSON response.
func newResp(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// provider builds a Provider served by this backend.
func (f *fakeBackend) provider(opts ...Option) *Provider {
	base := []Option{
		WithProjectAPIKey("phc_fake_project_key"),
		WithHost("https://posthog.test"),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
	}
	return New(append(base, opts...)...)
}
