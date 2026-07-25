package mamori

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/mamori/secret"
	"go.uber.org/goleak"
)

// handlerSimpleConfig is a small healthy fixture shared by the tests in this
// file that do not need to drive an unhealthy state.
type handlerSimpleConfig struct {
	A string `source:"env:MAMORI_HANDLER_A"`
}

// newHandlerTestWatcher builds a healthy fixture watcher. It does not close
// the watcher itself: goleak.VerifyNone (deferred first, so it runs last)
// requires every goroutine gone by the time it checks, which means Close
// must be deferred by the caller after this returns, not registered here via
// t.Cleanup, since t.Cleanup fires after the test function's own defers
// (including a goleak check deferred inside the test) have already run.
func newHandlerTestWatcher(t *testing.T) *Watcher[handlerSimpleConfig] {
	t.Helper()
	t.Setenv("MAMORI_HANDLER_A", "alpha")
	w, err := Watch[handlerSimpleConfig](context.Background())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	return w
}

func TestHandlerRootReturnsStatusJSON(t *testing.T) {
	defer goleak.VerifyNone(t)
	w := newHandlerTestWatcher(t)
	defer func() { _ = w.Close() }()
	h := Handler(w)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var rep Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("body did not unmarshal into Report: %v; body=%s", err, rec.Body.String())
	}
	if !rep.Healthy {
		t.Fatalf("Report.Healthy = false, want true: %+v", rep)
	}
	if len(rep.Fields) != 1 || rep.Fields[0].Path != "A" {
		t.Fatalf("Report.Fields = %+v, want one field named A", rep.Fields)
	}
	if rep.Snapshot == 0 {
		t.Fatalf("Report.Snapshot = 0, want the initial snapshot version")
	}
}

func TestHandlerHealthzHealthyIsOK(t *testing.T) {
	defer goleak.VerifyNone(t)
	w := newHandlerTestWatcher(t)
	defer func() { _ = w.Close() }()
	h := Handler(w)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body healthzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body did not unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if body.Status != "ok" {
		t.Fatalf("status = %q, want ok", body.Status)
	}
	if len(body.Fields) != 0 {
		t.Fatalf("healthy /healthz carried field detail: %+v", body.Fields)
	}
}

func TestHandlerUnknownPathIs404(t *testing.T) {
	defer goleak.VerifyNone(t)
	w := newHandlerTestWatcher(t)
	defer func() { _ = w.Close() }()
	h := Handler(w)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nope status = %d, want 404", rec.Code)
	}
}

// TestHandlerRouteSetIsExactlyRootAndHealthz locks down the complete route
// set. Exactly / and /healthz may answer with anything other than 404; every
// other path, however plausible-sounding as a future admin endpoint, must
// 404. If a later change adds a value-bearing route (a /values endpoint, a
// per-field endpoint, anything), this test fails.
func TestHandlerRouteSetIsExactlyRootAndHealthz(t *testing.T) {
	defer goleak.VerifyNone(t)
	w := newHandlerTestWatcher(t)
	defer func() { _ = w.Close() }()
	h := Handler(w)

	cases := []struct {
		path         string
		wantNotFound bool
	}{
		{"/", false},
		{"/healthz", false},
		{"/values", true},
		{"/config", true},
		{"/debug", true},
		{"/status", true},
		{"/healthz/", true},
		{"/A", true},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		is404 := rec.Code == http.StatusNotFound
		if is404 != c.wantNotFound {
			t.Errorf("GET %s status = %d, wantNotFound=%v", c.path, rec.Code, c.wantNotFound)
		}
	}
}

// handlerSecretConfig carries one secret.String field so
// TestHandlerNeverServesAValue can seed a distinctive value and assert it
// never reaches a response body.
type handlerSecretConfig struct {
	Password secret.String `source:"env:MAMORI_HANDLER_SECRET"`
}

// TestHandlerNeverServesAValue seeds a distinctive secret into the watched
// config and asserts it appears in no response body from any route. The
// handler only ever serves w.Status(), whose Report redacts refs and omits
// values by construction, so this should hold no matter what route is hit;
// this test is the structural backstop for that guarantee, alongside
// TestHandlerRouteSetIsExactlyRootAndHealthz which guards against a
// value-bearing route being added in the first place.
func TestHandlerNeverServesAValue(t *testing.T) {
	defer goleak.VerifyNone(t)
	const distinctiveSecret = "SUPERSECRETVALUE"
	t.Setenv("MAMORI_HANDLER_SECRET", distinctiveSecret)
	w, err := Watch[handlerSecretConfig](context.Background())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	h := Handler(w)
	for _, path := range []string{"/", "/healthz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if strings.Contains(rec.Body.String(), distinctiveSecret) {
			t.Fatalf("GET %s leaked the secret value: %s", path, rec.Body.String())
		}
	}
}

func TestHandlerWithAuthRejectsMissingTokenWith401AndChallenge(t *testing.T) {
	defer goleak.VerifyNone(t)
	w := newHandlerTestWatcher(t)
	defer func() { _ = w.Close() }()
	h := Handler(w, WithAuth(BearerToken(secret.NewString("s3cr3t"))))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET / without token status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
	}
}

func TestHandlerWithAuthAllowsCorrectToken(t *testing.T) {
	defer goleak.VerifyNone(t)
	w := newHandlerTestWatcher(t)
	defer func() { _ = w.Close() }()
	h := Handler(w, WithAuth(BearerToken(secret.NewString("s3cr3t"))))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer s3cr3t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / with correct token status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlerWithAuthForbiddenIs403(t *testing.T) {
	defer goleak.VerifyNone(t)
	w := newHandlerTestWatcher(t)
	defer func() { _ = w.Close() }()
	forbidden := AuthFunc(func(r *http.Request) (Identity, error) {
		return Identity{}, ErrForbidden
	})
	h := Handler(w, WithAuth(forbidden))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET / against an ErrForbidden authenticator status = %d, want 403", rec.Code)
	}
}

func TestHandlerWithAuthAppliedTwicePanics(t *testing.T) {
	w := newHandlerTestWatcher(t)
	defer func() { _ = w.Close() }()
	a := BearerToken(secret.NewString("x"))

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Handler(w, WithAuth(a), WithAuth(a)) did not panic")
		}
	}()
	_ = Handler(w, WithAuth(a), WithAuth(a))
}

// newUnhealthyHandlerWatcher builds a watcher over a single required field
// (no default, not optional) fed by the watchProvider fixture from
// watch_test.go, then drives it unhealthy with pushErr the same way
// notfound_health_test.go's TestHandleErrRequiredFieldBecomesUnhealthyOnNotFound
// does: a native-watch ErrNotFound delivered to a required field is recorded
// as KindNotFound and is terminal, so Health() and Status().Healthy flip
// without any staleness window to wait out. Like newHandlerTestWatcher, it
// does not close the watcher itself; see that function's comment for why.
func newUnhealthyHandlerWatcher(t *testing.T) *Watcher[handlerRequiredConfig] {
	t.Helper()
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("handlerunhealthy")
	wp.set("cfg/level", "info", "l1")

	w, err := Watch[handlerRequiredConfig](context.Background(), WithProvider(wp), WithClock(clk))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	wp.pushErr("cfg/level", ErrNotFound)
	waitUntil(t, 2*time.Second, "required field to go unhealthy", func() bool {
		return w.Health() != nil
	})
	return w
}

type handlerRequiredConfig struct {
	Level string `source:"handlerunhealthy://cfg/level"`
}

func TestHandlerHealthzUnauthenticatedOmitsDetailOn503(t *testing.T) {
	defer goleak.VerifyNone(t)
	w := newUnhealthyHandlerWatcher(t)
	defer func() { _ = w.Close() }()
	h := Handler(w, WithAuth(BearerToken(secret.NewString("s3cr3t"))))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz unauthenticated status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	var body healthzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body did not unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if body.Status != "unhealthy" {
		t.Fatalf("status = %q, want unhealthy", body.Status)
	}
	if len(body.Fields) != 0 {
		t.Fatalf("unauthenticated /healthz carried field detail: %+v", body.Fields)
	}
	if strings.Contains(rec.Body.String(), "Level") {
		t.Fatalf("unauthenticated /healthz leaked the failing field path: %s", rec.Body.String())
	}
}

func TestHandlerHealthzAuthenticatedIncludesDetailOn503(t *testing.T) {
	defer goleak.VerifyNone(t)
	w := newUnhealthyHandlerWatcher(t)
	defer func() { _ = w.Close() }()
	h := Handler(w, WithAuth(BearerToken(secret.NewString("s3cr3t"))))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer s3cr3t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz authenticated status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	var body healthzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body did not unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if body.Status != "unhealthy" {
		t.Fatalf("status = %q, want unhealthy", body.Status)
	}
	if len(body.Fields) != 1 || body.Fields[0].Path != "Level" {
		t.Fatalf("authenticated /healthz Fields = %+v, want one field named Level", body.Fields)
	}
}

// TestHandlerHealthzNeverReturns401 checks the healthz exemption directly:
// even with auth configured and no credential presented, /healthz must never
// answer 401 (unlike / under the same options).
func TestHandlerHealthzNeverReturns401(t *testing.T) {
	defer goleak.VerifyNone(t)
	w := newHandlerTestWatcher(t)
	defer func() { _ = w.Close() }()
	h := Handler(w, WithAuth(BearerToken(secret.NewString("s3cr3t"))))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("GET /healthz without credentials returned 401, must never 401")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz without credentials status = %d, want 200 (healthy watcher)", rec.Code)
	}
}

func TestHandlerPrefixStripsSubpath(t *testing.T) {
	defer goleak.VerifyNone(t)
	w := newHandlerTestWatcher(t)
	defer func() { _ = w.Close() }()
	h := Handler(w, HandlerPrefix("/admin"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/healthz status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /healthz without the mounted prefix status = %d, want 404", rec.Code)
	}
}

func TestHandlerMiddlewareRunsOutsideAuth(t *testing.T) {
	defer goleak.VerifyNone(t)
	w := newHandlerTestWatcher(t)
	defer func() { _ = w.Close() }()
	var called bool
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			called = true
			next.ServeHTTP(rw, r)
		})
	}
	h := Handler(w, WithAuth(BearerToken(secret.NewString("s3cr3t"))), HandlerMiddleware(mw))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Fatal("HandlerMiddleware did not run")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("middleware bypassed auth: status = %d, want 401", rec.Code)
	}
}
