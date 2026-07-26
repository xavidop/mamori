package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
)

// newTestServer builds and starts a *Server for a handler test, registering
// t.Cleanup to stop its upstream watches and release resources - the
// handler-test equivalent of resolve_test.go's inline New/start/defer Close
// pattern, factored out here since nearly every test in this file needs one.
func newTestServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	s, err := New(opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
	})
	return s
}

// doRequest sends one request straight through s.Handler(), bypassing any
// real network - the same in-process shortcut core's own handler tests use,
// appropriate for every route except /v1/watch, which needs a real
// connection to observe a forced disconnect (see the Watch tests below,
// which use httptest.NewServer instead).
func doRequest(t *testing.T, s *Server, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// fakeAuth is a scriptable mamori.Authenticator for tests that need a
// specific Authenticate outcome (a given Identity, a given error) rather
// than stubAuth's always-succeeds behavior (server_test.go).
type fakeAuth struct {
	id  mamori.Identity
	err error
}

func (f fakeAuth) Authenticate(r *http.Request) (mamori.Identity, error) { return f.id, f.err }

// challengingAuth is a fakeAuth that also implements mamori.Challenger, to
// exercise authenticate's WWW-Authenticate header path.
type challengingAuth struct {
	err       error
	challenge string
}

func (c challengingAuth) Authenticate(r *http.Request) (mamori.Identity, error) {
	return mamori.Identity{}, c.err
}
func (c challengingAuth) Challenge() string { return c.challenge }

// ---------------------------------------------------------------------
// GET /v1/values/{name}
// ---------------------------------------------------------------------

// TestHandleValueSuccessReturnsWireShape confirms a healthy binding's GET
// response matches the wire spec: the value's fields verbatim, base64 bytes
// (encoding/json's standard []byte handling), a normalized (never nil)
// metadata object, no "kind" field (this response is fresh, not stale), and
// Cache-Control: no-store. It also, incidentally, proves that leaving
// WithAudit unset (the default) does not panic or otherwise break a
// request - logAudit's nil check is exercised on every request this test
// makes.
func TestHandleValueSuccessReturnsWireShape(t *testing.T) {
	p := mamoritest.NewProvider("success")
	p.Set("k", "hunter2")

	s := newTestServer(t, WithPolicy(AllowAll()), NoAuth(), Bind("db-password", "success://k"), WithProvider(p))
	waitForLookup(t, s, "db-password", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "hunter2"
	})

	rec := doRequest(t, s, http.MethodGet, "/v1/values/db-password", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}

	var vb valueBody
	if err := json.Unmarshal(rec.Body.Bytes(), &vb); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if vb.Name != "db-password" {
		t.Fatalf("name = %q, want db-password", vb.Name)
	}
	if string(vb.Bytes) != "hunter2" {
		t.Fatalf("bytes = %q, want hunter2", vb.Bytes)
	}
	if vb.Version == "" {
		t.Fatal("version = \"\", want a non-empty version hash")
	}
	if vb.Kind != "" {
		t.Fatalf("kind = %q, want empty (fresh value, not stale)", vb.Kind)
	}
	if vb.Metadata == nil || len(vb.Metadata) != 0 {
		t.Fatalf("metadata = %v, want a non-nil empty object", vb.Metadata)
	}
	if vb.Error != nil {
		t.Fatalf("error = %+v, want nil", vb.Error)
	}

	// The bytes must actually be base64 on the wire (not, say, a raw string
	// or an array of numbers), matching the spec's own example
	// ("aHVudGVyMg==" for "hunter2").
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	wantB64 := base64.StdEncoding.EncodeToString([]byte("hunter2"))
	if raw["bytes"] != wantB64 {
		t.Fatalf("raw bytes field = %v, want base64 %q", raw["bytes"], wantB64)
	}
}

// TestHandleValueUnboundNameNotFound confirms an ALLOWED request for a name
// that simply is not a bound name reports 404/not_found - not 403 - which
// only makes sense once the ordering (authorize the name first, then check
// existence) is fixed: see TestPolicyDeniedTakesPriorityOverExistence below
// for the other half of that proof.
func TestHandleValueUnboundNameNotFound(t *testing.T) {
	s := newTestServer(t, WithPolicy(AllowAll()), NoAuth())

	rec := doRequest(t, s, http.MethodGet, "/v1/values/does-not-exist", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if mamori.Kind(body.Error.Kind) != mamori.KindNotFound {
		t.Fatalf("kind = %q, want not_found", body.Error.Kind)
	}
}

// TestPolicyDeniedTakesPriorityOverExistence is the ordering test the
// security brief calls out by name: authorize runs against the requested
// name BEFORE lookup ever checks whether it is a real binding, so a denied
// caller gets the exact same 403 whether the name exists or not. A Policy
// that denies literally everything is used here so the ONLY variable
// between the two requests is whether the name happens to be bound - if the
// ordering were reversed (existence checked first), the unbound name would
// answer 404 instead of 403, and the two response bodies below would
// differ.
func TestPolicyDeniedTakesPriorityOverExistence(t *testing.T) {
	p := mamoritest.NewProvider("denyall")
	p.Set("k", "v1")

	denyEverything := PolicyFunc(func(mamori.Identity, string) error { return ErrDenied })

	s := newTestServer(t, WithPolicy(denyEverything), NoAuth(), Bind("bound-and-denied", "denyall://k"), WithProvider(p))
	waitForLookup(t, s, "bound-and-denied", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	boundResp := doRequest(t, s, http.MethodGet, "/v1/values/bound-and-denied", nil)
	unboundResp := doRequest(t, s, http.MethodGet, "/v1/values/never-bound-at-all", nil)

	if boundResp.Code != http.StatusForbidden {
		t.Fatalf("bound-but-denied status = %d, want 403", boundResp.Code)
	}
	if unboundResp.Code != http.StatusForbidden {
		t.Fatalf("unbound-and-denied status = %d, want 403 (existence must not be checked before authorization)", unboundResp.Code)
	}
	if boundResp.Body.String() != unboundResp.Body.String() {
		t.Fatalf("denial bodies differ - a caller could infer existence from the response:\n  bound:   %s\n  unbound: %s",
			boundResp.Body.String(), unboundResp.Body.String())
	}
}

// TestNoAuthStillAppliesPolicy proves requirement 6: NoAuth() skips
// Authenticate entirely, but Policy.Allow still runs on every request. A
// denying Policy still yields 403 even with no Authenticator configured at
// all.
func TestNoAuthStillAppliesPolicy(t *testing.T) {
	p := mamoritest.NewProvider("noauthpolicy")
	p.Set("k", "v1")

	denyEverything := PolicyFunc(func(mamori.Identity, string) error { return ErrDenied })
	s := newTestServer(t, WithPolicy(denyEverything), NoAuth(), Bind("b", "noauthpolicy://k"), WithProvider(p))
	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	rec := doRequest(t, s, http.MethodGet, "/v1/values/b", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (Policy must still run under NoAuth)", rec.Code)
	}
}

// TestUnauthenticatedRequestGets401WithChallenge confirms authenticate's
// failure path: 401, with the Authenticator's Challenge() header attached
// when it implements mamori.Challenger, and a body whose kind is
// unauthenticated (round-trippable to mamori.ErrUnauthenticated).
func TestUnauthenticatedRequestGets401WithChallenge(t *testing.T) {
	auth := challengingAuth{err: errors.New("no credentials"), challenge: `Basic realm="mamori"`}
	s := newTestServer(t, WithPolicy(AllowAll()), WithAuth(auth))

	rec := doRequest(t, s, http.MethodGet, "/v1/values/anything", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != auth.challenge {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, auth.challenge)
	}
	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if mamori.Kind(body.Error.Kind) != mamori.KindUnauthenticated {
		t.Fatalf("kind = %q, want unauthenticated", body.Error.Kind)
	}
}

// TestAuthenticatorErrForbiddenGets403 confirms authenticate treats
// mamori.ErrForbidden as authenticated-but-forbidden (403, no
// WWW-Authenticate header) rather than a bare 401 - mirroring core's own
// admin handler (handler.go's authOK) exactly, per auth.go's ErrForbidden
// doc comment.
func TestAuthenticatorErrForbiddenGets403(t *testing.T) {
	s := newTestServer(t, WithPolicy(AllowAll()), WithAuth(fakeAuth{err: mamori.ErrForbidden}))

	rec := doRequest(t, s, http.MethodGet, "/v1/values/anything", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("WWW-Authenticate = %q, want empty for ErrForbidden", got)
	}
}

// TestStaleValueServedWithKindAnnotation confirms the "serving stale"
// contract lookup documents (resolve.go): once a binding has resolved
// successfully, a subsequent upstream failure still answers 200 with the
// last-known-good bytes, annotated with the current failure's kind on the
// wire so a client can tell fresh from stale-but-serving.
func TestStaleValueServedWithKindAnnotation(t *testing.T) {
	p := mamoritest.NewProvider("stale")
	p.Set("k", "v1")

	s := newTestServer(t, WithPolicy(AllowAll()), NoAuth(), Bind("b", "stale://k"), WithProvider(p))
	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	p.Fail("k", fmt.Errorf("backend down: %w", mamori.ErrUnavailable))
	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == mamori.KindUnavailable
	})

	rec := doRequest(t, s, http.MethodGet, "/v1/values/b", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale last-known-good is still served)", rec.Code)
	}
	var vb valueBody
	if err := json.Unmarshal(rec.Body.Bytes(), &vb); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(vb.Bytes) != "v1" {
		t.Fatalf("bytes = %q, want the last-known-good v1", vb.Bytes)
	}
	if mamori.Kind(vb.Kind) != mamori.KindUnavailable {
		t.Fatalf("kind = %q, want unavailable", vb.Kind)
	}
}

// TestKindToStatusTableAndSentinelRoundTrip is both the kind->status table
// test and the round-trip proof (requirement 4): for a binding that has
// NEVER resolved (a hard failure, no last-known-good value), the wire
// error's kind must map to the status the spec's table lists, AND
// mamori.SentinelFor(kind) must recover the exact sentinel a real provider
// would have wrapped - proving a client on the other end of the wire can
// reconstruct errors.Is(err, mamori.ErrPermissionDenied) (etc.) from a value
// that only ever traveled as a JSON string.
func TestKindToStatusTableAndSentinelRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		kind     mamori.Kind
		sentinel error
		status   int
	}{
		{"not_found", mamori.KindNotFound, mamori.ErrNotFound, http.StatusNotFound},
		{"invalid", mamori.KindInvalid, mamori.ErrInvalid, http.StatusBadRequest},
		{"unauthenticated", mamori.KindUnauthenticated, mamori.ErrUnauthenticated, http.StatusUnauthorized},
		{"permission_denied", mamori.KindPermissionDenied, mamori.ErrPermissionDenied, http.StatusForbidden},
		{"rate_limited", mamori.KindRateLimited, mamori.ErrRateLimited, http.StatusTooManyRequests},
		{"unavailable", mamori.KindUnavailable, mamori.ErrUnavailable, http.StatusServiceUnavailable},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := fmt.Sprintf("kindtable%d", i)
			p := mamoritest.NewProvider(scheme)
			// Fail BEFORE start: mamoritest.Provider.Watch replays the
			// injected failure as the baseline update, so this binding
			// never has a last-known-good value at all (see
			// TestNeverResolvedBindingReturnsUpstreamErrorKind in
			// resolve_test.go for the same setup).
			p.Fail("missing", fmt.Errorf("injected: %w", tc.sentinel))

			s := newTestServer(t, WithPolicy(AllowAll()), NoAuth(), Bind("b", scheme+"://missing"), WithProvider(p))
			waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
				return found && k == tc.kind
			})

			rec := doRequest(t, s, http.MethodGet, "/v1/values/b", nil)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			var body errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if mamori.Kind(body.Error.Kind) != tc.kind {
				t.Fatalf("wire kind = %q, want %q", body.Error.Kind, tc.kind)
			}

			got := mamori.SentinelFor(mamori.Kind(body.Error.Kind))
			if got != tc.sentinel {
				t.Fatalf("SentinelFor(%q) = %v, want %v", body.Error.Kind, got, tc.sentinel)
			}
		})
	}
}

// TestUnknownKindMapsTo500 completes the kind->status table with its final
// row: a failure ErrorKind cannot classify at all (KindUnknown) answers 500,
// the honest "this server cannot say what went wrong" status.
func TestUnknownKindMapsTo500(t *testing.T) {
	p := mamoritest.NewProvider("unknownkind")
	p.Fail("missing", errors.New("totally unclassified failure"))

	s := newTestServer(t, WithPolicy(AllowAll()), NoAuth(), Bind("b", "unknownkind://missing"), WithProvider(p))
	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == mamori.KindUnknown
	})

	rec := doRequest(t, s, http.MethodGet, "/v1/values/b", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestAuditNeverLogsValueBytes is the single most important test in this
// file. It seeds a binding whose resolved value is a recognizable secret
// (LEAKME12345), configures WithAudit to write into an in-memory buffer,
// issues a request that successfully reads the value (confirmed by checking
// the RESPONSE contains it, so this test cannot pass vacuously), and then
// asserts the secret - in raw form AND base64-encoded - appears nowhere in
// the captured audit output, while the fields the audit contract DOES
// promise (subject, name, decision) do appear.
func TestAuditNeverLogsValueBytes(t *testing.T) {
	const secret = "LEAKME12345"

	p := mamoritest.NewProvider("auditleak")
	p.Set("k", secret)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	s := newTestServer(t, WithPolicy(AllowAll()), NoAuth(),
		Bind("secret-binding", "auditleak://k"), WithProvider(p), WithAudit(logger))
	waitForLookup(t, s, "secret-binding", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == secret
	})

	rec := doRequest(t, s, http.MethodGet, "/v1/values/secret-binding", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	if !strings.Contains(rec.Body.String(), encoded) {
		t.Fatalf("sanity check failed: the HTTP response itself does not contain the secret; this test would pass vacuously. body=%s", rec.Body.String())
	}

	logged := logBuf.String()
	if strings.Contains(logged, secret) {
		t.Fatalf("audit log contains the raw secret value:\n%s", logged)
	}
	if strings.Contains(logged, encoded) {
		t.Fatalf("audit log contains the base64-encoded secret value:\n%s", logged)
	}
	if !strings.Contains(logged, "secret-binding") {
		t.Fatalf("expected audit log to record the binding name, got:\n%s", logged)
	}
	if !strings.Contains(logged, `"decision":"allow"`) {
		t.Fatalf("expected audit log to record an allow decision, got:\n%s", logged)
	}
}

// ---------------------------------------------------------------------
// GET /v1/healthz
// ---------------------------------------------------------------------

// TestHealthzExemptFromAuthAndRevealsNoBindingDetail proves requirement 3:
// /v1/healthz always answers 200, even with an Authenticator that fails
// every request and a Policy that denies everything - and its body never
// mentions a configured binding's name.
func TestHealthzExemptFromAuthAndRevealsNoBindingDetail(t *testing.T) {
	p := mamoritest.NewProvider("healthzexempt")
	p.Set("k", "v1")

	denyEverything := PolicyFunc(func(mamori.Identity, string) error { return ErrDenied })
	s := newTestServer(t, WithPolicy(denyEverything), WithAuth(fakeAuth{err: errors.New("always fails")}),
		Bind("very-secret-binding-name", "healthzexempt://k"), WithProvider(p))

	rec := doRequest(t, s, http.MethodGet, "/v1/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with a failing Authenticator and Policy", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "very-secret-binding-name") {
		t.Fatalf("healthz body leaked a binding name: %s", rec.Body.String())
	}
	var body healthzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("status field = %q, want ok", body.Status)
	}
}

// ---------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------

// TestUnknownPathIs404 confirms the exact-path ServeMux patterns Handler
// registers do not fall back to a subtree match for anything outside the
// four documented routes.
func TestUnknownPathIs404(t *testing.T) {
	s := newTestServer(t, WithPolicy(AllowAll()), NoAuth())

	for _, path := range []string{"/", "/v1", "/v1/", "/v1/unknown", "/values/x", "/healthz"} {
		rec := doRequest(t, s, http.MethodGet, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

// ---------------------------------------------------------------------
// POST /v1/values (batch)
// ---------------------------------------------------------------------

// TestBatchMixedOutcomes exercises the chosen batch semantics (see
// handleBatch's doc comment in handler.go): a batch mixing an allowed
// resolvable name, a denied name, and an unbound name answers 200 overall,
// with each name's own outcome carried in its own entry.
func TestBatchMixedOutcomes(t *testing.T) {
	p := mamoritest.NewProvider("batchmixed")
	p.Set("allowed-key", "v1")

	policy := PolicyFunc(func(id mamori.Identity, name string) error {
		if name == "denied-name" {
			return ErrDenied
		}
		return nil
	})

	s := newTestServer(t, WithPolicy(policy), NoAuth(), Bind("allowed-name", "batchmixed://allowed-key"), WithProvider(p))
	waitForLookup(t, s, "allowed-name", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	body := `{"names":["allowed-name","denied-name","missing-name"]}`
	rec := doRequest(t, s, http.MethodPost, "/v1/values", strings.NewReader(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("batch status = %d, want 200 (a per-name denial must not fail the whole batch)", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}

	var resp batchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Values) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(resp.Values), resp.Values)
	}

	byName := map[string]valueBody{}
	for _, v := range resp.Values {
		byName[v.Name] = v
	}

	if allowed := byName["allowed-name"]; allowed.Error != nil || string(allowed.Bytes) != "v1" {
		t.Fatalf("allowed-name entry = %+v, want a resolved value with bytes=v1", allowed)
	}
	denied := byName["denied-name"]
	if denied.Error == nil || mamori.Kind(denied.Error.Kind) != mamori.KindPermissionDenied {
		t.Fatalf("denied-name entry = %+v, want a permission_denied error", denied)
	}
	missing := byName["missing-name"]
	if missing.Error == nil || mamori.Kind(missing.Error.Kind) != mamori.KindNotFound {
		t.Fatalf("missing-name entry = %+v, want a not_found error", missing)
	}
}

// TestBatchMalformedBodyBadRequest confirms a body that is not valid JSON
// fails the whole request with 400/invalid, before any name is looked at.
func TestBatchMalformedBodyBadRequest(t *testing.T) {
	s := newTestServer(t, WithPolicy(AllowAll()), NoAuth())

	rec := doRequest(t, s, http.MethodPost, "/v1/values", strings.NewReader("{not valid json"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if mamori.Kind(body.Error.Kind) != mamori.KindInvalid {
		t.Fatalf("kind = %q, want invalid", body.Error.Kind)
	}
}

// TestBatchUnauthenticatedGets401BeforeAnyNameIsRead confirms authentication
// is whole-request for the batch route: a failing Authenticator ends the
// request with 401 before the body is even decoded, regardless of what
// names it lists.
func TestBatchUnauthenticatedGets401BeforeAnyNameIsRead(t *testing.T) {
	s := newTestServer(t, WithPolicy(AllowAll()), WithAuth(fakeAuth{err: errors.New("nope")}))

	rec := doRequest(t, s, http.MethodPost, "/v1/values", strings.NewReader(`{"names":["a","b"]}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// ---------------------------------------------------------------------
// GET /v1/watch (SSE)
// ---------------------------------------------------------------------

// sseFrame is one parsed Server-Sent Event: its event type and its data
// line's payload (assumed to be exactly one line of JSON, which is all this
// package's writeSSEValue/writeSSEError ever produce).
type sseFrame struct {
	event string
	data  string
}

// readSSEFrame reads lines from r until a complete frame (an "event:" line,
// a "data:" line, and the blank line that terminates it) has been read,
// skipping SSE comment lines (heartbeats) along the way.
func readSSEFrame(r *bufio.Reader) (sseFrame, error) {
	var f sseFrame
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return f, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if f.event != "" || f.data != "" {
				return f, nil
			}
		case strings.HasPrefix(line, ":"):
			// A heartbeat/comment line - not a real frame, keep reading.
		case strings.HasPrefix(line, "event: "):
			f.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			f.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// readSSEFrameWithTimeout runs readSSEFrame on a goroutine and fails the
// test if no frame arrives within timeout, so a stream that unexpectedly
// stops sending frames fails fast rather than hanging the test suite.
func readSSEFrameWithTimeout(t *testing.T, r *bufio.Reader, timeout time.Duration) sseFrame {
	t.Helper()
	type result struct {
		frame sseFrame
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := readSSEFrame(r)
		ch <- result{f, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("reading SSE frame: %v", res.err)
		}
		return res.frame
	case <-time.After(timeout):
		t.Fatal("timed out waiting for an SSE frame")
		return sseFrame{}
	}
}

// TestWatchDeliversUpdateAndEndsCleanlyOnDisconnect covers both halves of
// the wire spec's /v1/watch test requirement: the stream delivers an
// "update" frame for the binding's current value, delivers a SECOND
// "update" frame after the value changes upstream, and then - once the
// client forces a disconnect by canceling its request context - the
// server's handleWatch goroutine actually exits, proven by the httptest
// server shutting down within a bounded time instead of hanging on a
// leaked handler. Reconnecting after a disconnect is explicitly the
// CLIENT's responsibility (see handleWatch's doc comment in handler.go),
// not something this server attempts, so this test does not exercise it.
func TestWatchDeliversUpdateAndEndsCleanlyOnDisconnect(t *testing.T) {
	p := mamoritest.NewProvider("watchupdate")
	p.Set("k", "v1")

	s := newTestServer(t, WithPolicy(AllowAll()), NoAuth(), Bind("w", "watchupdate://k"), WithProvider(p))
	waitForLookup(t, s, "w", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	httpSrv := httptest.NewServer(s.Handler())
	defer httpSrv.Close()

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, httpSrv.URL+"/v1/watch?name=w", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}

	reader := bufio.NewReader(resp.Body)

	first := readSSEFrameWithTimeout(t, reader, 3*time.Second)
	if first.event != "update" {
		t.Fatalf("first frame event = %q, want update", first.event)
	}
	var firstBody valueBody
	if err := json.Unmarshal([]byte(first.data), &firstBody); err != nil {
		t.Fatalf("decode first frame: %v", err)
	}
	if firstBody.Name != "w" || string(firstBody.Bytes) != "v1" {
		t.Fatalf("first frame = %+v, want name=w bytes=v1", firstBody)
	}

	p.Set("k", "v2")

	second := readSSEFrameWithTimeout(t, reader, 3*time.Second)
	if second.event != "update" {
		t.Fatalf("second frame event = %q, want update", second.event)
	}
	var secondBody valueBody
	if err := json.Unmarshal([]byte(second.data), &secondBody); err != nil {
		t.Fatalf("decode second frame: %v", err)
	}
	if string(secondBody.Bytes) != "v2" {
		t.Fatalf("second frame bytes = %q, want v2 (the stream must deliver the update)", secondBody.Bytes)
	}

	// Forced disconnect.
	cancelReq()

	closed := make(chan struct{})
	go func() {
		httpSrv.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down after client disconnect; handleWatch may have leaked")
	}
}

// TestWatchAuthorizesEachNameSeparately confirms handleWatch's per-name
// authorization: subscribing to one denied name and one allowed name in the
// same connection yields an "error" frame for the denied name and an
// "update" frame for the allowed one, in the order requested, rather than
// the whole connection failing because one of several names was denied.
func TestWatchAuthorizesEachNameSeparately(t *testing.T) {
	p := mamoritest.NewProvider("watchauth")
	p.Set("k", "v1")

	policy := PolicyFunc(func(id mamori.Identity, name string) error {
		if name == "denied" {
			return ErrDenied
		}
		return nil
	})

	s := newTestServer(t, WithPolicy(policy), NoAuth(), Bind("allowed", "watchauth://k"), WithProvider(p))
	waitForLookup(t, s, "allowed", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	httpSrv := httptest.NewServer(s.Handler())
	defer httpSrv.Close()

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, httpSrv.URL+"/v1/watch?name=denied&name=allowed", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	reader := bufio.NewReader(resp.Body)

	first := readSSEFrameWithTimeout(t, reader, 3*time.Second)
	if first.event != "error" {
		t.Fatalf("first frame event = %q, want error (for the denied name)", first.event)
	}
	var firstBody valueBody
	if err := json.Unmarshal([]byte(first.data), &firstBody); err != nil {
		t.Fatalf("decode first frame: %v", err)
	}
	if firstBody.Name != "denied" || firstBody.Error == nil || mamori.Kind(firstBody.Error.Kind) != mamori.KindPermissionDenied {
		t.Fatalf("first frame = %+v, want a permission_denied error for \"denied\"", firstBody)
	}

	second := readSSEFrameWithTimeout(t, reader, 3*time.Second)
	if second.event != "update" {
		t.Fatalf("second frame event = %q, want update (for the allowed name)", second.event)
	}
	var secondBody valueBody
	if err := json.Unmarshal([]byte(second.data), &secondBody); err != nil {
		t.Fatalf("decode second frame: %v", err)
	}
	if secondBody.Name != "allowed" || string(secondBody.Bytes) != "v1" {
		t.Fatalf("second frame = %+v, want name=allowed bytes=v1", secondBody)
	}
}

// TestWatchRequiresAtLeastOneName confirms a subscription with no ?name=
// query parameter at all fails fast with 400/invalid rather than opening a
// stream with nothing to watch.
func TestWatchRequiresAtLeastOneName(t *testing.T) {
	s := newTestServer(t, WithPolicy(AllowAll()), NoAuth())

	rec := doRequest(t, s, http.MethodGet, "/v1/watch", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestWatchUnauthenticatedGets401 confirms /v1/watch runs the same
// authenticate gate as every other non-healthz route: a failing
// Authenticator ends the request with 401 before any name is even parsed
// from the query string.
func TestWatchUnauthenticatedGets401(t *testing.T) {
	s := newTestServer(t, WithPolicy(AllowAll()), WithAuth(fakeAuth{err: errors.New("nope")}))

	rec := doRequest(t, s, http.MethodGet, "/v1/watch?name=anything", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
