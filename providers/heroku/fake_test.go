package heroku

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/xavidop/mamori"
)

// The credentials and scope every test uses. They are passed as explicit
// options rather than left to the environment, so a developer who happens to
// have HEROKU_* set in their shell cannot change what these tests exercise.
const (
	testToken = "HRKU-test-api-token"
	testApp   = "mamori-test-app"
)

// fakeHeroku is an in-process emulation of the one Platform API endpoint this
// provider calls. It is driven through an http.RoundTripper with no listener
// and no background goroutine, deliberately rather than through an
// httptest.Server: providertest's NoGoroutineLeak case runs goleak.VerifyNone
// with no ignore options, which a live server's accept goroutine can never
// satisfy.
type fakeHeroku struct {
	mu sync.Mutex
	// apps maps an app identity to its config var document. A nil value models
	// the JSON null the vendor schema allows, distinct from an absent name.
	apps map[string]map[string]*string

	// failStatus fails every config-vars read until it is cleared. The
	// conformance kit reuses one fake for its whole run, so it must be
	// clearable or a failure injected for one case leaks into every later one.
	failStatus int

	reads      int
	readsByApp map[string]int

	lastPath   string
	lastAccept string
	lastAuth   string
}

// newFake returns a backend with one empty app.
func newFake() *fakeHeroku {
	return &fakeHeroku{
		apps:       map[string]map[string]*string{testApp: {}},
		readsByApp: map[string]int{},
	}
}

// set writes a config var into an app, creating the app if needed.
func (f *fakeHeroku) set(app, name, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.apps[app] == nil {
		f.apps[app] = map[string]*string{}
	}
	v := value
	f.apps[app][name] = &v
}

// setNull writes a config var whose value is JSON null, which the vendor schema
// permits and which must read as absent rather than as the four bytes "null".
func (f *fakeHeroku) setNull(app, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.apps[app] == nil {
		f.apps[app] = map[string]*string{}
	}
	f.apps[app][name] = nil
}

// del removes a config var, so a later read observes the same absence a never
// created name produces.
func (f *fakeHeroku) del(app, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.apps[app], name)
}

// fail makes every read answer status until clearFail.
func (f *fakeHeroku) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus = status
}

// clearFail cancels fail.
func (f *fakeHeroku) clearFail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus = 0
}

// counts returns how many reads have been served in total and per app, which is
// how a test observes that a batch of N refs cost one request.
func (f *fakeHeroku) counts() (total int, byApp map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	byApp = make(map[string]int, len(f.readsByApp))
	for k, v := range f.readsByApp {
		byApp[k] = v
	}
	return f.reads, byApp
}

// observed returns what the last request actually carried on the wire.
func (f *fakeHeroku) observed() (path, accept, auth string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPath, f.lastAccept, f.lastAuth
}

// transport returns an http.RoundTripper serving this backend.
func (f *fakeHeroku) transport() http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		// Honor cancellation explicitly. net/http enforces the request context
		// inside the Transport, not in Client.Do, and this in-process fake has
		// none of that machinery to fall back on. Without this check a
		// cancelled-context request would be answered as though it had
		// succeeded, and providertest's ContextCancel case would pass
		// vacuously against a provider that never threaded ctx through at all.
		if err := req.Context().Err(); err != nil {
			return nil, err
		}

		f.mu.Lock()
		f.lastPath = req.URL.EscapedPath()
		f.lastAccept = req.Header.Get("Accept")
		f.lastAuth = req.Header.Get("Authorization")
		f.mu.Unlock()

		// The version header is checked FIRST, before auth and before routing,
		// exactly as the real API documents it: "request failed, set Accept:
		// application/vnd.heroku+json; version=3 header and try again". Without
		// this branch a test asserting the header is sent could only inspect
		// what the fake recorded, and a provider that dropped it would still
		// resolve every value correctly here while failing against Heroku.
		if req.Header.Get("Accept") != acceptVersion {
			return errorResp(http.StatusNotAcceptable, "not_acceptable",
				"request failed, set `Accept: application/vnd.heroku+json; version=3` header and try again", nil), nil
		}
		if req.Header.Get("Authorization") != "Bearer "+testToken {
			return errorResp(http.StatusUnauthorized, "unauthorized", "Invalid credentials provided.", nil), nil
		}

		app, ok := configVarsApp(req)
		if req.Method != http.MethodGet || !ok {
			return errorResp(http.StatusNotFound, "not_found", "Not found.", nil), nil
		}
		return f.handleRead(req, app)
	})
}

// configVarsApp extracts the app identity from a GET
// /apps/{app_id_or_name}/config-vars path, reporting false for any other path.
//
// It reads URL.Path, the DECODED form, so an app identity whose escaped %2F
// reassembles into a literal slash is seen as one identity rather than as extra
// path segments. What actually went over the wire is captured separately, from
// EscapedPath, so a test can still assert that the escape survived.
func configVarsApp(req *http.Request) (string, bool) {
	p := req.URL.Path
	if !strings.HasPrefix(p, appsPath) || !strings.HasSuffix(p, configVarsSuffix) {
		return "", false
	}
	app := strings.TrimSuffix(strings.TrimPrefix(p, appsPath), configVarsSuffix)
	if app == "" {
		return "", false
	}
	return app, true
}

// handleRead serves GET /apps/{app_id_or_name}/config-vars.
func (f *fakeHeroku) handleRead(req *http.Request, app string) (*http.Response, error) {
	f.mu.Lock()
	f.reads++
	f.readsByApp[app]++
	fail := f.failStatus
	vars, known := f.apps[app]
	// Copy under the lock: the response is marshalled outside it, and the
	// conformance kit's Concurrent case reads and writes at the same time.
	snapshot := make(map[string]*string, len(vars))
	for k, v := range vars {
		snapshot[k] = v
	}
	f.mu.Unlock()

	if fail != 0 {
		// The failure envelope deliberately ECHOES the API token the request
		// carried and every config var value the app holds, in a sibling field
		// beside the "id" a provider is allowed to surface. Real backends do
		// echo rejected input, and a fake whose error body contained nothing
		// secret would let "no credential and no value reaches an error" pass
		// against a provider that pasted the whole body into its message. This
		// is what makes that assertion falsifiable.
		echo := map[string]any{
			"authorization": req.Header.Get("Authorization"),
			"config_vars":   snapshot,
		}
		return errorResp(fail, "injected_failure", "injected failure for app "+app, echo), nil
	}
	if !known {
		// A 404 on this endpoint means the APP is absent or invisible to the
		// token. There is no per-config-var 404: an absent name is simply
		// absent from a successful document.
		return errorResp(http.StatusNotFound, "not_found", "Couldn't find that app.", nil), nil
	}

	payload, err := json.Marshal(snapshot)
	if err != nil {
		return errorResp(http.StatusInternalServerError, "internal_server_error", "encode failure", nil), nil
	}
	resp := jsonResp(http.StatusOK, string(payload))
	// The real endpoint sends both validators. They are set here so a test can
	// prove this provider does NOT derive Version from them (see
	// TestVersionIsPerVarNotTheDocumentETag).
	resp.Header.Set("ETag", `"`+mamori.VersionHash(payload)+`"`)
	resp.Header.Set("Last-Modified", "Sun, 01 Jan 2012 12:00:00 GMT")
	return resp, nil
}

// provider builds a Provider talking to this fake in-process. Extra options are
// applied last so a test can override any default.
func (f *fakeHeroku) provider(opts ...Option) *Provider {
	base := []Option{
		WithAPIKey(testToken),
		WithApp(testApp),
		WithBaseURL("https://api.heroku.test"),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
	}
	return New(append(base, opts...)...)
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// errorResp builds Heroku's documented error envelope - an "id" from a closed
// vocabulary, a human "message", and a "url" - plus whatever echo fields the
// caller wants alongside them to prove a provider is not surfacing the whole
// body.
func errorResp(status int, id, message string, echo map[string]any) *http.Response {
	env := map[string]any{
		"id":      id,
		"message": message,
		"url":     "https://devcenter.heroku.com/articles/platform-api-reference#error-responses",
	}
	if echo != nil {
		env["echo"] = echo
	}
	body, err := json.Marshal(env)
	if err != nil {
		return jsonResp(http.StatusInternalServerError, `{"id":"internal_server_error","message":"encode failure"}`)
	}
	return jsonResp(status, string(body))
}

// jsonResp builds a JSON response with the given status and body.
func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}
}

// clearEnv unsets every variable this provider reads, so a developer's own
// HEROKU_* shell settings cannot change what a test exercises. t.Setenv also
// makes the test refuse to run in parallel, which is the point: the provider
// reads the process environment.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{envAPIKey, envApp, envAppName} {
		t.Setenv(k, "")
	}
}

// mustRef parses a ref tag or fails the test.
func mustRef(t *testing.T, tag string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(tag)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", tag, err)
	}
	return ref
}
