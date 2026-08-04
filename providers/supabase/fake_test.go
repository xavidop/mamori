package supabase

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// The project and credentials every test uses. They are passed as explicit
// options rather than left to the environment, so a developer who happens to
// have SUPABASE_* set in their shell cannot change what these tests exercise.
const (
	testProjectURL = "https://project.supabase.test"
	testServiceKey = "test-service-role-key-must-never-leak"
	testSchema     = "public"
	testView       = "decrypted_secrets"
)

// storedSecret is one secret and the timestamp of its last write. updatedAt
// advances on every write, which is what the real relation does and what lets
// the conformance kit's VersionMonotonic case exercise the updated_at path
// rather than the content-hash fallback.
type storedSecret struct {
	value     string
	updatedAt string
}

// fakeSupabase is an in-process emulation of the one PostgREST endpoint this
// provider calls. It is driven through an http.RoundTripper with no listener
// and no background goroutine, deliberately rather than through an
// httptest.Server: providertest's NoGoroutineLeak case runs goleak.VerifyNone
// with no ignore options, which a live server's accept goroutine can never
// satisfy.
type fakeSupabase struct {
	mu      sync.Mutex
	secrets map[string]*storedSecret
	// writes counts every set, so each write gets a distinct updated_at
	// without this fake depending on the wall clock.
	writes int

	// failStatus fails the read with an injected status, for the conformance
	// Fail hook and the status-to-kind table.
	failStatus int
	// duplicate makes the relation answer a matching name with TWO rows, the
	// misconfigured-relation case.
	duplicate bool
	// omitSecretColumn makes the relation answer without a decrypted_secret
	// column, the relation-built-wrong case.
	omitSecretColumn bool
	// omitUpdatedAt makes the relation answer without updated_at, which drives
	// versionOf's content-hash fallback.
	omitUpdatedAt bool

	reads int

	lastPath    string
	lastQuery   url.Values
	lastProfile string
	lastAPIKey  string
	lastAuth    string
}

// newFake returns an empty backend.
func newFake() *fakeSupabase {
	return &fakeSupabase{secrets: map[string]*storedSecret{}}
}

// set writes a value and advances its updated_at, so a repeated read observes a
// change the way the real relation reports one.
func (f *fakeSupabase) set(name, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	f.secrets[name] = &storedSecret{value: value, updatedAt: timestampFor(f.writes)}
}

// del removes a secret, so a later read observes the empty array that PostgREST
// answers an absent row with.
func (f *fakeSupabase) del(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.secrets, name)
}

// fail makes every read answer status until clear.
func (f *fakeSupabase) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus = status
}

// clear cancels fail. The conformance kit reuses one fake across its whole run,
// so a failure injected for one case must be undone before the next or it leaks
// into every later case.
func (f *fakeSupabase) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus = 0
}

// timestampFor renders a distinct, monotonically increasing PostgREST timestamp
// for the nth write. A counter rather than time.Now keeps the fake
// deterministic: two writes in the same microsecond would otherwise produce one
// timestamp and make a version test flaky.
func timestampFor(n int) string {
	return "2026-08-04T12:00:0" + string(rune('0'+n%10)) + ".000000+00:00"
}

// transport returns an http.RoundTripper serving this backend.
func (f *fakeSupabase) transport() http.RoundTripper {
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
		if req.Method != http.MethodGet {
			return errorResp(http.StatusMethodNotAllowed, "PGRST105", "method not allowed", nil), nil
		}
		return f.handleRead(req)
	})
}

// handleRead serves GET /rest/v1/{relation}?name=eq.{name}&select=...
func (f *fakeSupabase) handleRead(req *http.Request) (*http.Response, error) {
	q := req.URL.Query()
	apiKey := req.Header.Get("apikey")
	auth := req.Header.Get("Authorization")
	profile := req.Header.Get("Accept-Profile")

	f.mu.Lock()
	f.reads++
	f.lastPath = req.URL.EscapedPath()
	f.lastQuery = q
	f.lastProfile = profile
	f.lastAPIKey = apiKey
	f.lastAuth = auth
	fail := f.failStatus
	duplicate := f.duplicate
	omitSecret := f.omitSecretColumn
	omitUpdated := f.omitUpdatedAt
	f.mu.Unlock()

	// Both headers are required, and the real gateway distinguishes them: the
	// apikey header is what it authenticates, the bearer is what PostgREST
	// reads to pick a role. A provider that sent only one must not pass.
	if apiKey != testServiceKey {
		return errorResp(http.StatusUnauthorized, "PGRST301", "no API key found in request", nil), nil
	}
	if auth != "Bearer "+testServiceKey {
		return errorResp(http.StatusUnauthorized, "PGRST301", "invalid or missing Authorization bearer", nil), nil
	}

	// An unexposed schema is PGRST106, exactly what an operator who skipped the
	// setup, or who named the vault schema, actually gets.
	if profile != "" && profile != testSchema {
		return errorResp(http.StatusNotAcceptable, "PGRST106",
			`The schema must be one of the following: `+testSchema, nil), nil
	}
	// An unknown relation is a 404: the other thing a skipped setup produces.
	if strings.TrimPrefix(req.URL.Path, "/rest/v1/") != testView {
		return errorResp(http.StatusNotFound, "PGRST205",
			`Could not find the table in the schema cache`, nil), nil
	}

	name, ok := eqFilter(q.Get("name"))
	if !ok {
		return errorResp(http.StatusBadRequest, "PGRST100", "failed to parse filter", nil), nil
	}

	f.mu.Lock()
	s, found := f.secrets[name]
	var value, updatedAt string
	if found {
		value, updatedAt = s.value, s.updatedAt
	}
	f.mu.Unlock()

	if fail != 0 {
		// The failure envelope deliberately ECHOES the service-role key it was
		// sent AND the secret value it was asked for, in the "details" and
		// "hint" fields beside the "message" a provider is allowed to surface.
		// Real PostgREST puts the offending row in "details", and a fake whose
		// error body contained nothing secret would let "no credential and no
		// value reaches an error" pass against a provider that pasted the whole
		// body into its message. This is what makes those assertions
		// falsifiable.
		return errorResp(fail, "PGRSTINJ", "injected read failure", map[string]string{
			"details": "Failing row contains (" + value + ")",
			"hint":    "apikey was " + apiKey,
		}), nil
	}

	// The shape that matters: an absent row is an EMPTY ARRAY with 200, never a
	// 404. A fake that answered 404 here would let a provider that never
	// special-cased the empty array still pass every not-found test.
	if !found {
		return jsonResp(http.StatusOK, `[]`), nil
	}

	row := map[string]any{"name": name}
	if !omitSecret {
		row["decrypted_secret"] = value
	}
	if !omitUpdated {
		row["updated_at"] = updatedAt
	}
	rows := []map[string]any{row}
	if duplicate {
		rows = append(rows, row)
	}

	payload, err := json.Marshal(rows)
	if err != nil {
		return errorResp(http.StatusInternalServerError, "XX000", "encode failure", nil), nil
	}
	return jsonResp(http.StatusOK, string(payload)), nil
}

// eqFilter parses PostgREST's "eq.<value>" filter syntax, undoing the optional
// double quoting filterValue applies to a value with reserved characters.
func eqFilter(raw string) (string, bool) {
	v, ok := strings.CutPrefix(raw, "eq.")
	if !ok {
		return "", false
	}
	if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
		inner := v[1 : len(v)-1]
		r := strings.NewReplacer(`\"`, `"`, `\\`, `\`)
		return r.Replace(inner), true
	}
	return v, true
}

// provider builds a Provider talking to this fake in-process. Extra options are
// applied last so a test can override any default.
func (f *fakeSupabase) provider(opts ...Option) *Provider {
	base := []Option{
		WithProjectURL(testProjectURL),
		WithServiceKey(testServiceKey),
		WithSchema(testSchema),
		WithView(testView),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
	}
	return New(append(base, opts...)...)
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// errorResp builds PostgREST's four-field error envelope: a message and a code
// a provider may surface, plus the details and hint it must not.
func errorResp(status int, code, message string, extra map[string]string) *http.Response {
	env := map[string]any{"code": code, "message": message}
	env["details"] = extra["details"]
	env["hint"] = extra["hint"]
	body, err := json.Marshal(env)
	if err != nil {
		return jsonResp(http.StatusInternalServerError, `{"code":"XX000","message":"encode failure"}`)
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
