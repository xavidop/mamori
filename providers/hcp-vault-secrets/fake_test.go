package hcpvaultsecrets

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
// have HCP_* set in their shell cannot change what these tests exercise.
const (
	testClientID     = "test-client-id"
	testClientSecret = "test-client-secret"
	testAccessToken  = "test-access-token"
	testOrganization = "11111111-1111-1111-1111-111111111111"
	testProject      = "22222222-2222-2222-2222-222222222222"
	testApp          = "test-app"

	testBaseURL  = "https://hcp-api.test"
	testTokenURL = "https://hcp-auth.test/oauth2/token"
	// tokenPath is the path half of testTokenURL, which is how the fake routes
	// a token exchange apart from a secret read.
	tokenPath = "/oauth2/token"
)

// secretRef identifies one stored secret the way the API scopes it: a name is
// meaningful only inside an (organization, project, application) triple.
type secretRef struct {
	organization string
	project      string
	app          string
	name         string
}

// storedSecret is one secret and its revision. version starts at 1 on the first
// write and advances on every later one, which is what the real backend does
// and what lets the conformance kit's VersionMonotonic case exercise the
// native-revision path rather than the content-hash fallback.
//
// kind is the secret's "type" field. A secret whose kind is not "kv" is served
// with no static_version at all, which is how the fake reproduces a rotating or
// dynamic secret without inventing its unpinned response shape.
type storedSecret struct {
	value   string
	version int64
	kind    string
}

// fakeHCP is an in-process emulation of the two HCP endpoints this provider
// calls: the identity provider's token exchange and the control plane's
// OpenAppSecret read. It is driven through an http.RoundTripper with no
// listener and no background goroutine, deliberately rather than through an
// httptest.Server: providertest's NoGoroutineLeak case runs goleak.VerifyNone
// with no ignore options, which a live server's accept goroutine can never
// satisfy.
type fakeHCP struct {
	mu      sync.Mutex
	secrets map[secretRef]*storedSecret

	// readFailStatus fails the SECRET READ path only, leaving the token
	// exchange working. That split is what the conformance Fail hook needs: it
	// injects a mamori sentinel per case and expects Resolve to report exactly
	// that kind, which a failure on the token leg would re-route through
	// httpcore's authError instead. It is also the honest model of the real
	// backend, where a token can be valid and still be refused one secret.
	readFailStatus int
	// tokenFailStatus fails the TOKEN path only, for the tests that are about
	// the exchange itself.
	tokenFailStatus int

	tokens int
	reads  int

	lastReadPath  string
	lastReadAuth  string
	lastTokenForm url.Values
}

// newFake returns an empty backend.
func newFake() *fakeHCP {
	return &fakeHCP{secrets: map[secretRef]*storedSecret{}}
}

// set writes a static secret and advances its version, so a repeated read
// observes a change the way the real backend reports one.
func (f *fakeHCP) set(r secretRef, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.secrets[r]; ok {
		s.value, s.version = value, s.version+1
		return
	}
	f.secrets[r] = &storedSecret{value: value, version: 1, kind: "kv"}
}

// setUnversioned writes a value the backend reports with no version at all, to
// exercise the content-hash fallback in versionOf.
func (f *fakeHCP) setUnversioned(r secretRef, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets[r] = &storedSecret{value: value, version: 0, kind: "kv"}
}

// setNonStatic stores a secret of a kind that carries no static_version, which
// is how a rotating or dynamic secret reaches this provider.
func (f *fakeHCP) setNonStatic(r secretRef, kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets[r] = &storedSecret{version: 1, kind: kind}
}

// del removes a secret, so a later read observes the same 404 an absent name
// has always produced.
func (f *fakeHCP) del(r secretRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.secrets, r)
}

// failRead makes every secret read answer status until clearRead.
func (f *fakeHCP) failRead(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readFailStatus = status
}

// clearRead cancels failRead. The conformance kit reuses one fake across its
// whole run, so a failure injected for one case must be undone before the next
// or it leaks into every later case.
func (f *fakeHCP) clearRead() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readFailStatus = 0
}

// failToken makes every token exchange answer status.
func (f *fakeHCP) failToken(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenFailStatus = status
}

// counts returns how many token exchanges and reads have been served, which is
// how a test observes that the access token was cached rather than re-bought.
func (f *fakeHCP) counts() (tokens, reads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens, f.reads
}

// lastRead returns the escaped path and Authorization header of the most recent
// read, so a test can assert what actually went over the wire.
func (f *fakeHCP) lastRead() (path, auth string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReadPath, f.lastReadAuth
}

// lastToken returns the decoded form of the most recent token exchange, so a
// test can assert the grant really carried the audience HCP requires.
func (f *fakeHCP) lastToken() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastTokenForm
}

// transport returns an http.RoundTripper serving this backend.
func (f *fakeHCP) transport() http.RoundTripper {
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
		case req.Method == http.MethodPost && req.URL.Path == tokenPath:
			return f.handleToken(req)
		// Routing reads URL.Path, the DECODED form, so a secret name whose
		// escaped %2F reassembles into a literal slash is matched and extracted
		// as one name rather than as extra path segments. What actually went
		// over the wire is captured separately, from EscapedPath, so a test can
		// still assert that the escape survived.
		case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/secrets/"):
			return f.handleRead(req)
		default:
			return errorResp(http.StatusNotFound, "route not found", nil), nil
		}
	})
}

// handleToken serves the OAuth2 client-credentials exchange.
func (f *fakeHCP) handleToken(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return errorResp(http.StatusBadRequest, "unreadable body", nil), nil
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return errorResp(http.StatusBadRequest, "token body is not a form", nil), nil
	}

	f.mu.Lock()
	f.tokens++
	f.lastTokenForm = form
	fail := f.tokenFailStatus
	f.mu.Unlock()

	if fail != 0 {
		// The failure envelope deliberately ECHOES the client secret it was
		// sent, in a sibling field beside "message". Real backends do echo
		// rejected input, and a fake whose error body contained nothing secret
		// would let "the credential never reaches an error" pass against a
		// provider that pasted the whole body into its message. This is what
		// makes that assertion falsifiable.
		return errorResp(fail, "injected token failure", map[string]any{
			"client_secret": form.Get("client_secret"),
		}), nil
	}

	if form.Get("grant_type") != "client_credentials" {
		return errorResp(http.StatusBadRequest, "unsupported grant_type", nil), nil
	}
	if form.Get("client_id") != testClientID || form.Get("client_secret") != testClientSecret {
		return errorResp(http.StatusUnauthorized, "invalid service principal credentials", nil), nil
	}
	return jsonResp(http.StatusOK,
		`{"access_token":"`+testAccessToken+`","token_type":"Bearer","expires_in":3600}`), nil
}

// handleRead serves GET on the OpenAppSecret path.
func (f *fakeHCP) handleRead(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.reads++
	f.lastReadPath = req.URL.EscapedPath()
	f.lastReadAuth = req.Header.Get("Authorization")
	fail := f.readFailStatus
	f.mu.Unlock()

	if req.Header.Get("Authorization") != "Bearer "+testAccessToken {
		return errorResp(http.StatusUnauthorized, "missing or invalid access token", nil), nil
	}

	r, ok := parseSecretPath(req.URL.Path)
	if !ok {
		return errorResp(http.StatusNotFound, "route not found", nil), nil
	}

	f.mu.Lock()
	s, found := f.secrets[r]
	var value, kind string
	var version int64
	if found {
		value, version, kind = s.value, s.version, s.kind
	}
	f.mu.Unlock()

	if fail != 0 {
		// As on the token path, the failure envelope echoes BOTH the access
		// token the request carried and the secret value it was asking for,
		// beside the "message" a provider is allowed to surface. Without that,
		// "no credential and no value reaches an error" would pass against a
		// provider that embedded the entire error body.
		return errorResp(fail, "injected read failure", map[string]any{
			"authorization": req.Header.Get("Authorization"),
			"value":         value,
		}), nil
	}

	if !found {
		return errorResp(http.StatusNotFound, "secret "+r.name+" not found", nil), nil
	}

	secret := map[string]any{
		"name":           r.name,
		"type":           kind,
		"latest_version": version,
	}
	// A non-kv secret carries no static_version, exactly as a rotating or
	// dynamic secret does on the real backend.
	if kind == "kv" {
		secret["static_version"] = map[string]any{
			"version": version,
			"value":   value,
		}
	}
	payload, err := json.Marshal(map[string]any{"secret": secret})
	if err != nil {
		return errorResp(http.StatusInternalServerError, "encode failure", nil), nil
	}
	return jsonResp(http.StatusOK, string(payload)), nil
}

// parseSecretPath pulls the (organization, project, application, name) triple
// back out of an OpenAppSecret path, which is how the fake proves the provider
// built the path the API documents rather than merely a path that round-trips.
//
// The expected DECODED shape is:
//
//	/secrets/{version}/organizations/{org}/projects/{proj}/apps/{app}/secrets/{name}:open
func parseSecretPath(p string) (secretRef, bool) {
	rest, ok := strings.CutSuffix(p, ":open")
	if !ok {
		return secretRef{}, false
	}
	// SplitN with a limit of 9 keeps a name containing a literal slash whole in
	// the final element, so an escaped %2F that decoded back into a slash is
	// still read as one name.
	parts := strings.SplitN(strings.TrimPrefix(rest, "/"), "/", 9)
	if len(parts) != 9 {
		return secretRef{}, false
	}
	if parts[0] != "secrets" || parts[1] != apiVersion ||
		parts[2] != "organizations" || parts[4] != "projects" ||
		parts[6] != "apps" || parts[8] == "" {
		return secretRef{}, false
	}
	// parts[8] is "secrets/{name}"; split once so the name keeps any slash.
	tail, ok := strings.CutPrefix(parts[8], "secrets/")
	if !ok || tail == "" {
		return secretRef{}, false
	}
	return secretRef{organization: parts[3], project: parts[5], app: parts[7], name: tail}, true
}

// provider builds a Provider talking to this fake in-process. Extra options are
// applied last so a test can override any default.
func (f *fakeHCP) provider(opts ...Option) *Provider {
	base := []Option{
		WithClientID(testClientID),
		WithClientSecret(testClientSecret),
		WithOrganizationID(testOrganization),
		WithProjectID(testProject),
		WithAppName(testApp),
		WithBaseURL(testBaseURL),
		WithTokenURL(testTokenURL),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
	}
	return New(append(base, opts...)...)
}

// conformanceRef is the (organization, project, application) triple the fake
// provider addresses, so a test can seed a name the provider will look for.
func conformanceRef(name string) secretRef {
	return secretRef{organization: testOrganization, project: testProject, app: testApp, name: name}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// errorResp builds HCP's googlerpcStatus error envelope: a "message" a provider
// may surface, plus whatever echo fields the caller wants alongside it to prove
// a provider is not surfacing the whole body.
func errorResp(status int, message string, echo map[string]any) *http.Response {
	body, err := json.Marshal(map[string]any{
		"code":    status,
		"message": message,
		"details": []any{},
		"echo":    echo,
	})
	if err != nil {
		return jsonResp(http.StatusInternalServerError, `{"code":500,"message":"encode failure"}`)
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
