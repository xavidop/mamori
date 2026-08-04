package infisical

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// The credentials and scope every test uses. They are passed as explicit
// options rather than left to the environment, so a developer who happens to
// have INFISICAL_* set in their shell cannot change what these tests exercise.
const (
	testClientID     = "test-client-id"
	testClientSecret = "test-client-secret"
	testAccessToken  = "test-access-token"
	testProject      = "test-project-id"
	testEnvironment  = "prod"
	testSecretPath   = "/"
)

// secretRef identifies one stored secret the way the API scopes it: a name is
// meaningful only inside a (project, environment, folder) triple.
type secretRef struct {
	project     string
	environment string
	path        string
	name        string
}

// storedSecret is one secret and its revision. version starts at 1 on the first
// write and advances on every later one, which is what the real backend does
// and what lets the conformance kit's VersionMonotonic case exercise the
// native-revision path rather than the content-hash fallback.
type storedSecret struct {
	value   string
	version int64
}

// fakeInfisical is an in-process emulation of the two Infisical endpoints this
// provider calls. It is driven through an http.RoundTripper with no listener
// and no background goroutine, deliberately rather than through an
// httptest.Server: providertest's NoGoroutineLeak case runs goleak.VerifyNone
// with no ignore options, which a live server's accept goroutine can never
// satisfy.
type fakeInfisical struct {
	mu      sync.Mutex
	secrets map[secretRef]*storedSecret

	// readFailStatus fails the SECRET READ path only, leaving login working.
	// That split is what the conformance Fail hook needs: it injects a mamori
	// sentinel per case and expects Resolve to report exactly that kind, which
	// a failure on the login leg would re-route through httpcore's authError
	// instead. It is also the honest model of the real backend, where a token
	// can be valid and still be refused one particular secret.
	readFailStatus int
	// loginFailStatus fails the LOGIN path only, for the tests that are about
	// the token exchange itself.
	loginFailStatus int

	logins int
	reads  int

	lastReadPath  string
	lastReadQuery url.Values
	lastReadAuth  string
	lastLoginBody []byte
}

// newFake returns an empty backend.
func newFake() *fakeInfisical {
	return &fakeInfisical{secrets: map[secretRef]*storedSecret{}}
}

// set writes a value and advances its version, so a repeated read observes a
// change the way the real backend reports one.
func (f *fakeInfisical) set(r secretRef, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.secrets[r]; ok {
		s.value, s.version = value, s.version+1
		return
	}
	f.secrets[r] = &storedSecret{value: value, version: 1}
}

// setUnversioned writes a value the backend reports with no version at all, to
// exercise the content-hash fallback in versionOf.
func (f *fakeInfisical) setUnversioned(r secretRef, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets[r] = &storedSecret{value: value, version: 0}
}

// del removes a secret, so a later read observes the same 404 an absent name
// has always produced.
func (f *fakeInfisical) del(r secretRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.secrets, r)
}

// failRead makes every secret read answer status until clearRead.
func (f *fakeInfisical) failRead(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readFailStatus = status
}

// clearRead cancels failRead. The conformance kit reuses one fake across its
// whole run, so a failure injected for one case must be undone before the next
// or it leaks into every later case.
func (f *fakeInfisical) clearRead() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readFailStatus = 0
}

// failLogin makes every login answer status.
func (f *fakeInfisical) failLogin(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loginFailStatus = status
}

// counts returns how many logins and reads have been served, which is how a
// test observes that the access token was cached rather than re-bought.
func (f *fakeInfisical) counts() (logins, reads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins, f.reads
}

// transport returns an http.RoundTripper serving this backend.
func (f *fakeInfisical) transport() http.RoundTripper {
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

		switch {
		case req.Method == http.MethodPost && req.URL.Path == loginPath:
			return f.handleLogin(req)
		// Routing reads URL.Path, the DECODED form, so a secret name whose
		// escaped %2F reassembles into a literal slash is matched and extracted
		// as one name rather than as extra path segments. What actually went
		// over the wire is captured separately, from EscapedPath, so a test can
		// still assert that the escape survived.
		case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, secretsPath):
			return f.handleRead(req)
		default:
			return jsonResp(http.StatusNotFound, `{"statusCode":404,"message":"route not found","error":"NotFound"}`), nil
		}
	})
}

// handleLogin serves POST /api/v1/auth/universal-auth/login.
func (f *fakeInfisical) handleLogin(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return jsonResp(http.StatusBadRequest, `{"statusCode":400,"message":"unreadable body","error":"BadRequest"}`), nil
	}

	f.mu.Lock()
	f.logins++
	f.lastLoginBody = body
	fail := f.loginFailStatus
	f.mu.Unlock()

	var lr loginRequest
	if err := json.Unmarshal(body, &lr); err != nil {
		return jsonResp(http.StatusBadRequest, `{"statusCode":400,"message":"login body is not JSON","error":"BadRequest"}`), nil
	}

	if fail != 0 {
		// The failure envelope deliberately ECHOES the client secret it was
		// sent, in a sibling field beside "message". Real backends do echo
		// rejected input, and a fake whose error body contained nothing secret
		// would let "the credential never reaches an error" pass against a
		// provider that pasted the whole body into its message. This is what
		// makes that assertion falsifiable.
		return errorResp(fail, "injected login failure", map[string]any{"clientSecret": lr.ClientSecret}), nil
	}

	if lr.ClientID != testClientID || lr.ClientSecret != testClientSecret {
		return jsonResp(http.StatusUnauthorized, `{"statusCode":401,"message":"invalid machine identity credentials","error":"Unauthorized"}`), nil
	}
	return jsonResp(http.StatusOK,
		`{"accessToken":"`+testAccessToken+`","expiresIn":3600,"accessTokenMaxTTL":2592000,"tokenType":"Bearer"}`), nil
}

// handleRead serves GET /api/v4/secrets/{secretName}.
func (f *fakeInfisical) handleRead(req *http.Request) (*http.Response, error) {
	q := req.URL.Query()

	f.mu.Lock()
	f.reads++
	f.lastReadPath = req.URL.EscapedPath()
	f.lastReadQuery = q
	f.lastReadAuth = req.Header.Get("Authorization")
	fail := f.readFailStatus
	f.mu.Unlock()

	if req.Header.Get("Authorization") != "Bearer "+testAccessToken {
		return jsonResp(http.StatusUnauthorized, `{"statusCode":401,"message":"missing or invalid access token","error":"Unauthorized"}`), nil
	}

	r := secretRef{
		project:     q.Get("projectId"),
		environment: q.Get("environment"),
		path:        q.Get("secretPath"),
		name:        strings.TrimPrefix(req.URL.Path, secretsPath),
	}

	f.mu.Lock()
	s, ok := f.secrets[r]
	var value string
	var version int64
	if ok {
		value, version = s.value, s.version
	}
	f.mu.Unlock()

	if fail != 0 {
		// As on the login path, the failure envelope echoes BOTH the access
		// token the request carried and the secret value it was asking for,
		// beside the "message" a provider is allowed to surface. Without that,
		// "no credential and no value reaches an error" would pass against a
		// provider that embedded the entire error body.
		return errorResp(fail, "injected read failure", map[string]any{
			"authorization": req.Header.Get("Authorization"),
			"secretValue":   value,
		}), nil
	}

	if !ok {
		return jsonResp(http.StatusNotFound,
			`{"statusCode":404,"message":"Secret with name '`+r.name+`' not found","error":"NotFound"}`), nil
	}

	payload, err := json.Marshal(map[string]any{
		"secret": map[string]any{
			"secretKey":   r.name,
			"secretValue": value,
			"version":     version,
		},
	})
	if err != nil {
		return jsonResp(http.StatusInternalServerError, `{"statusCode":500,"message":"encode failure","error":"Internal"}`), nil
	}
	return jsonResp(http.StatusOK, string(payload)), nil
}

// provider builds a Provider talking to this fake in-process. Extra options are
// applied last so a test can override any default.
func (f *fakeInfisical) provider(opts ...Option) *Provider {
	base := []Option{
		WithClientID(testClientID),
		WithClientSecret(testClientSecret),
		WithProjectID(testProject),
		WithEnvironment(testEnvironment),
		WithSecretPath(testSecretPath),
		WithBaseURL("https://infisical.test"),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
	}
	return New(append(base, opts...)...)
}

// conformanceRef is the (project, environment, folder) triple the fake
// provider addresses, so a test can seed a name the provider will actually
// look for.
func conformanceRef(name string) secretRef {
	return secretRef{project: testProject, environment: testEnvironment, path: testSecretPath, name: name}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// errorResp builds one of Infisical's error envelopes: a "message" a provider
// may surface, plus whatever echo fields the caller wants alongside it to prove
// a provider is not surfacing the whole body.
func errorResp(status int, message string, echo map[string]any) *http.Response {
	body, err := json.Marshal(map[string]any{
		"statusCode": status,
		"message":    message,
		"error":      "Injected",
		"echo":       echo,
	})
	if err != nil {
		return jsonResp(http.StatusInternalServerError, `{"statusCode":500,"message":"encode failure","error":"Internal"}`)
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
