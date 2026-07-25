# Workstream B (HTTP): Admin endpoint and the Authenticator framework

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose a running `Watcher`'s health over HTTP, two ways (mount on your own mux, or let mamori run its own server), behind a pluggable `Authenticator` framework, serving metadata only and never values.

**Architecture:** `Handler[T]` returns an `http.Handler` with exactly two routes, `/` (the `Report` as JSON) and `/healthz`. There is no values route, ever, under any option: a config-wide exposure toggle is the kind of thing flipped during an incident and never flipped back, so it is a structural absence, not a default. Authentication is one small interface, `Authenticator`, mirroring the provider ecosystem: stdlib schemes ship in core, heavier ones (JWT, peer-credential) come later in their own module. `WithAdminHTTP` lets mamori bind and run the server itself, off by default, its lifetime tied to the `Watcher`.

**Tech Stack:** Go 1.26, stdlib only (`net/http`, `crypto/subtle`, `crypto/tls`, `encoding/json`). No new dependencies.

This implements spec sections 7.5 and 7.6 (`docs/superpowers/specs/2026-07-24-operational-layer-design.md`). It builds directly on the observability core (plan `2026-07-25-observability-core.md`): `Watcher.Status()`, `Health()`, `Report`, `FieldStatus`. Two schemes from the spec are deliberately deferred (see Scope).

## Scope

**In scope:** the `Handler`, its routes, `HandlerPrefix`, `WithAdminHTTP`/`WithAdminTLS`, the `Authenticator` interface and its stdlib schemes (`BasicAuth`, `BearerToken`, `APIKey`, `MTLS`, `AnyOf`, `AllOf`, plus their `Func` variants), `WithAuth`, `HandlerMiddleware`, and the detail-free `/healthz` exemption.

**Deferred, with the reason:**
- `PeerCred` (Unix-socket peer-credential auth) needs `golang.org/x/sys` promoted to a direct dependency plus platform build-tagged syscall code, and its natural home is the sidecar/config-server use case. It lands with the config server (workstream H), where it is the recommended auth.
- `x/authjwt` (JWT/JWKS) is a separate module and lands alongside the config server too.
- `WithAdminHTTP` serving over a Unix socket: the `Handler` and TLS TCP path are here; the UDS listener is a small addition that also fits better with the config server, which needs UDS regardless. This plan does TCP (with mandatory TLS guidance) and the mux-mounted handler.

## Global Constraints

- **Core dependencies are frozen.** Only stdlib, `validator/v10`, `mapstructure/v2`, `fsnotify`, `yaml.v3`, `goleak` (test-only). This plan adds nothing (all stdlib).
- **Do not run `git commit`.** Stage with `git add`, report the suggested message.
- **Work on `main`.** No branches, no worktrees.
- **`GOWORK=off` for every Go command;** `make test` from the repo root.
- **The tree stays green after every task.**
- **No em-dash characters** anywhere.
- **The admin endpoint serves metadata only.** No route returns a config value, and no option adds one. Two tests enforce this: the route set is exactly `/` and `/healthz`, and a distinctive secret seeded into the config appears in no response body.
- **Credentials are `secret.String`** so they redact in logs, and are compared with `crypto/subtle.ConstantTimeCompare` (including the username), so the endpoint leaks neither the secret through timing nor whether a username exists.
- Doc comments on every exported symbol, explaining the why.

---

### Task 1: The Authenticator interface

**Files:**
- Create: `auth.go`
- Create: `auth_test.go`

**Interfaces:**
- Produces: `Authenticator`, `Identity`, `Challenger`, `AuthFunc`, `ErrForbidden`. Task 2 implements the schemes against this; Task 3 consumes it in the handler.

**Design note.** `Authenticate` returns `(Identity, error)`, not just `error`. The admin endpoint ignores the `Identity`, but the config server (workstream H) authorizes on it, and one interface serving both surfaces beats two that drift. Returning it now costs nothing and avoids a breaking change later.

- [ ] **Step 1: Write the failing test**

Create `auth_test.go`:

```go
package mamori

import (
	"errors"
	"net/http/httptest"
	"testing"
)

func TestAuthFuncAdaptsAFunction(t *testing.T) {
	called := false
	var a Authenticator = AuthFunc(func(r *http.Request) (Identity, error) {
		called = true
		return Identity{Subject: "svc"}, nil
	})
	req := httptest.NewRequest("GET", "/", nil)
	id, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	if !called {
		t.Fatal("AuthFunc did not invoke the wrapped function")
	}
	if id.Subject != "svc" {
		t.Fatalf("Identity.Subject = %q, want svc", id.Subject)
	}
}

func TestErrForbiddenIsDistinct(t *testing.T) {
	if errors.Is(ErrForbidden, ErrNotFound) {
		t.Fatal("ErrForbidden must be its own sentinel")
	}
}
```

Add `net/http` to the import block.

- [ ] **Step 2: Run, confirm failure**

```bash
GOWORK=off go test ./... -run 'TestAuthFunc|TestErrForbidden' -v
```

Expected: `undefined: Authenticator`, `undefined: AuthFunc`, `undefined: Identity`, `undefined: ErrForbidden`.

- [ ] **Step 3: Implement `auth.go`**

```go
package mamori

import (
	"errors"
	"net/http"
)

// Authenticator decides whether an HTTP request may proceed, and says who the
// caller is. A nil error allows the request; any error denies it.
//
// The returned Identity is ignored by the admin endpoint (which only serves
// metadata) and consumed by the config server, whose authorization policy is
// expressed in terms of it. It is one interface rather than two so an
// Authenticator written for one surface works unchanged on the other.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// Identity is the authenticated caller. Subject is a stable principal name;
// Attrs carries scheme-specific detail (certificate SANs, token claims, a peer
// uid). Both may be empty for schemes that authenticate without naming a
// principal, such as a shared bearer token.
type Identity struct {
	Subject string
	Attrs   map[string]string
}

// Challenger is optionally implemented by an Authenticator to supply the value
// of the WWW-Authenticate header sent with a 401. A scheme that does not
// implement it produces a bare 401.
type Challenger interface {
	Challenge() string
}

// AuthFunc adapts a plain function to Authenticator.
type AuthFunc func(r *http.Request) (Identity, error)

// Authenticate calls f.
func (f AuthFunc) Authenticate(r *http.Request) (Identity, error) { return f(r) }

// ErrForbidden, returned from Authenticate, produces a 403 rather than a 401.
// Use it when the caller is authenticated but not permitted. Any other error
// produces a 401.
var ErrForbidden = errors.New("mamori: forbidden")
```

- [ ] **Step 4: Run, confirm pass; stage**

```bash
GOWORK=off go test ./... -run 'TestAuthFunc|TestErrForbidden' -v
GOWORK=off go vet ./...
git add auth.go auth_test.go
```

```
feat(core): add the Authenticator interface for the admin endpoint

One small interface, mirroring the provider ecosystem: schemes plug in without
core shipping every scheme. Authenticate returns an Identity as well as an
error so the same interface serves both the admin endpoint (which ignores it)
and the config server (which authorizes on it).
```

---

### Task 2: The stdlib auth schemes

**Files:**
- Create: `authschemes.go`
- Create: `authschemes_test.go`

**Interfaces:**
- Consumes: `Authenticator`, `Identity`, `Challenger`, `ErrForbidden` (Task 1); `secret.String`.
- Produces: `BasicAuth`, `BasicAuthFunc`, `BearerToken`, `BearerTokenFunc`, `APIKey`, `APIKeyFunc`, `MTLS`, `MTLSOptions`, `AnyOf`, `AllOf`.

**The security requirements, all testable, all mandatory:**
- Comparison is constant-time via `crypto/subtle.ConstantTimeCompare`, for both the username and the secret, so timing reveals neither the secret nor whether a username exists.
- A zero `secret.String` (from a `Func` variant that has not been populated yet) DENIES every request. It never falls through to an open endpoint and never panics.
- `BasicAuth` challenges with `Basic realm="mamori"`; `BearerToken` with `Bearer`. `APIKey` and `MTLS` implement no `Challenger` (bare 401).
- Authentication failures are never returned with the presented credential in the message.
- `AnyOf` evaluates every member even after one succeeds or fails, so its work does not depend on which member matched (avoids a timing oracle); it returns the first member's `Challenge`.

- [ ] **Step 1: Write the failing tests**

Create `authschemes_test.go`. Cover, at minimum:
- `BasicAuth` accepts the right user+pass, rejects wrong pass, rejects wrong user, rejects missing header. Assert the 401 path is signaled by a non-nil error (the handler turns it into a status; here you test the Authenticator directly).
- `BearerToken` accepts the right token, rejects wrong, rejects missing/malformed header.
- `APIKey` reads the named header, accepts right, rejects wrong/missing.
- A `Func` variant returning a zero `secret.String` denies (fail-closed).
- `AnyOf(a, b)` allows when either allows; `AllOf(a, b)` allows only when both allow.
- `BasicAuth`'s `Challenge()` returns `Basic realm="mamori"`; `BearerToken`'s returns `Bearer`.
- A constant-time-usage test: assert the code path calls `subtle.ConstantTimeCompare` by construction (verify by reading, and add a test that a right-length-wrong-value password is still rejected, which at least exercises the comparison).

Construct secrets with `secret.NewString("...")`. Build requests with `httptest.NewRequest` and set `req.SetBasicAuth`, `req.Header.Set("Authorization", "Bearer "+tok)`, etc.

- [ ] **Step 2: Run, confirm failure**

```bash
GOWORK=off go test ./... -run 'TestBasicAuth|TestBearer|TestAPIKey|TestAnyOf|TestAllOf|TestFailClosed' -v
```

- [ ] **Step 3: Implement `authschemes.go`**

Implement each scheme. The shape for `BasicAuth`:

```go
// BasicAuth authenticates HTTP Basic credentials. The username and password are
// both compared in constant time, so the endpoint discloses neither the
// password through response timing nor whether a username exists. The password
// is a secret.String so it redacts in logs.
func BasicAuth(user string, pass secret.String) Authenticator {
	return BasicAuthFunc(func() (string, secret.String) { return user, pass })
}

// BasicAuthFunc reads the expected credentials per request, so the admin
// credential can be rotated by a mamori Watcher rather than frozen at
// construction. A zero password denies every request (fail closed).
func BasicAuthFunc(fn func() (string, secret.String)) Authenticator {
	return basicAuth{fn: fn}
}

type basicAuth struct{ fn func() (string, secret.String) }

func (b basicAuth) Authenticate(r *http.Request) (Identity, error) {
	wantUser, wantPass := b.fn()
	if wantPass.IsZero() {
		return Identity{}, errors.New("mamori: basic auth not configured")
	}
	gotUser, gotPass, ok := r.BasicAuth()
	if !ok {
		return Identity{}, errors.New("mamori: missing basic credentials")
	}
	userOK := subtle.ConstantTimeCompare([]byte(gotUser), []byte(wantUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(gotPass), wantPass.RevealBytes()) == 1
	if !(userOK && passOK) {
		return Identity{}, errors.New("mamori: invalid basic credentials")
	}
	return Identity{Subject: wantUser}, nil
}

func (b basicAuth) Challenge() string { return `Basic realm="mamori"` }
```

Follow the same shape for `BearerToken`/`BearerTokenFunc` (parse the `Bearer ` prefix, constant-time compare, `Identity{Subject: "bearer"}`, `Challenge() == "Bearer"`), and `APIKey`/`APIKeyFunc` (`header string` plus the secret; read `r.Header.Get(header)`, constant-time compare, `Identity{Subject: "apikey"}`, no `Challenge`).

For the bearer prefix, use a constant-time-safe extraction: check the `Bearer ` prefix with `strings.HasPrefix` (the prefix is not secret), then compare the remainder in constant time.

`MTLS`:

```go
// MTLSOptions configures certificate-based authentication.
type MTLSOptions struct {
	// AllowedCNs, if non-empty, permits only these certificate common names.
	AllowedCNs []string
	// AllowedDNSNames, if non-empty, permits only these certificate DNS SANs.
	AllowedDNSNames []string
}

// MTLS authenticates a client by its verified TLS certificate. It requires the
// server to be configured with tls.RequireAndVerifyClientCert (see
// WithAdminTLS); on a non-TLS connection it denies every request. For an
// endpoint that reports on secret material inside a cluster, this is the
// strongest option that needs no dependency.
func MTLS(opts MTLSOptions) Authenticator { return mtls{opts: opts} }
```

`mtls.Authenticate` denies when `r.TLS == nil` or `len(r.TLS.PeerCertificates) == 0`, otherwise checks the leaf certificate's `Subject.CommonName` against `AllowedCNs` and its `DNSNames` against `AllowedDNSNames` (allow if either list matches; if both lists are empty, a verified peer cert is sufficient). Sets `Identity{Subject: leaf.Subject.CommonName}`.

`AnyOf`/`AllOf`:

```go
// AnyOf allows a request if any member allows it. It evaluates every member
// even after one succeeds, so its work does not depend on which member matched,
// and returns the first member's challenge on failure.
func AnyOf(as ...Authenticator) Authenticator { return anyOf(as) }

// AllOf allows a request only if every member allows it.
func AllOf(as ...Authenticator) Authenticator { return allOf(as) }
```

For `AnyOf`, iterate all members, record the first success (do not break early), and on total failure return an error; if any member is a `Challenger`, surface the first one's challenge via a wrapping type that implements `Challenger`. For `AllOf`, the first denial fails the whole (short-circuit is acceptable here since a partial failure is already a denial).

- [ ] **Step 4: Run, confirm pass**

```bash
GOWORK=off go test ./... -run 'TestBasicAuth|TestBearer|TestAPIKey|TestAnyOf|TestAllOf|TestFailClosed|TestMTLS' -v
GOWORK=off go vet ./...
```

- [ ] **Step 5: Stage**

```bash
git add authschemes.go authschemes_test.go
```

```
feat(core): add stdlib auth schemes (basic, bearer, api-key, mtls)

Constant-time credential comparison including the username, secret.String
credentials that redact in logs, and fail-closed on an unset credential. Func
variants read the credential per request so it can be rotated by a Watcher.
AnyOf and AllOf compose schemes. JWT and peer-credential auth land later.
```

---

### Task 3: The Handler, routes, and the healthz exemption

**Files:**
- Create: `handler.go`
- Create: `handler_test.go`

**Interfaces:**
- Consumes: `Watcher.Status()`/`Health()`, `Report` (observability core); `Authenticator`, `Challenger`, `ErrForbidden` (Tasks 1-2).
- Produces: `Handler[T]`, `HandlerOption`, `HandlerPrefix`, `WithAuth`, `HandlerMiddleware`.

**Routes, the complete set:**

| Path | Response |
|------|----------|
| `GET /` | `w.Status()` as JSON; 401/403 if auth configured and the request fails it |
| `GET /healthz` | 200 `{"status":"ok"}` or 503; failing-field detail only when authenticated |

Any other path is 404. There is no third route.

**The healthz exemption (spec decision D8):** `/healthz` answers without credentials, but an unauthenticated response carries only the bare status. The failing-field list (paths, redacted refs, kinds) is served only to an authenticated caller. So a Kubernetes probe needs no credentials while a port-scanner learns liveness but nothing about the config surface. When no auth is configured, `/healthz` returns the full detail, since there is no credential to distinguish callers by and the operator has accepted that posture.

- [ ] **Step 1: Write the failing tests**

Create `handler_test.go`. Use `httptest.NewServer(Handler(w, ...))` or call the handler with `httptest.NewRecorder()`. Cover:
- `GET /` returns 200 and a JSON body that unmarshals into `Report` with the expected fields, when no auth is configured.
- `GET /healthz` returns 200 with `{"status":"ok"}` for a healthy watcher.
- An unknown path returns 404.
- **The route-set lockdown test:** assert that exactly `/` and `/healthz` are served and every other tested path (`/values`, `/config`, `/debug`) is 404. This test must fail if anyone later adds a value-bearing route.
- **The no-values test:** seed a distinctive secret (e.g. a `secret.String` field whose value is `SUPERSECRETVALUE`) into the config, hit `/` and `/healthz`, and assert the string `SUPERSECRETVALUE` appears in NO response body.
- With `WithAuth(BearerToken(...))`: `GET /` without a token is 401 and sets `WWW-Authenticate: Bearer`; with the right token is 200; with `ErrForbidden` from a custom authenticator is 403.
- With auth configured: `GET /healthz` without a token still returns a status (200/503) but its body does NOT contain field paths; with a token, a 503 body DOES contain the failing field paths. Drive an unhealthy state to exercise the 503 detail (reuse the watch-test mechanism for injecting a failing field, or a Doctor-style report if simpler; if driving a live unhealthy watcher is hard, at minimum test the authenticated-vs-not detail difference on the healthy 200 path and note the 503-detail test is covered by a handler-unit test with an injected Report).
- Applying `WithAuth` twice panics (construction error).

- [ ] **Step 2: Run, confirm failure**

```bash
GOWORK=off go test ./... -run TestHandler -v
```

- [ ] **Step 3: Implement `handler.go`**

```go
package mamori

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

type handlerOptions struct {
	prefix     string
	auth       Authenticator
	authSet    bool
	middleware []func(http.Handler) http.Handler
}

// HandlerOption configures Handler and WithAdminHTTP.
type HandlerOption func(*handlerOptions)

// HandlerPrefix strips prefix from request paths before routing, so the handler
// can be mounted under a subpath of an existing mux.
func HandlerPrefix(prefix string) HandlerOption {
	return func(o *handlerOptions) { o.prefix = strings.TrimSuffix(prefix, "/") }
}

// WithAuth requires every request (except the liveness aspect of /healthz) to
// pass a. Applying it more than once is a construction error: compose schemes
// with AnyOf or AllOf instead.
func WithAuth(a Authenticator) HandlerOption {
	return func(o *handlerOptions) {
		if o.authSet {
			panic("mamori: WithAuth applied more than once; compose with AnyOf or AllOf")
		}
		o.auth = a
		o.authSet = true
	}
}

// HandlerMiddleware wraps the handler with a non-authentication concern such as
// request logging or rate limiting. It runs outside the Authenticator.
func HandlerMiddleware(mw func(http.Handler) http.Handler) HandlerOption {
	return func(o *handlerOptions) { o.middleware = append(o.middleware, mw) }
}

// Handler returns an http.Handler exposing the watcher's health. It serves
// exactly two routes, GET / (the Report) and GET /healthz, and never serves a
// configuration value under any option. Mount it on your own mux, or use
// WithAdminHTTP to have mamori run its own server.
func Handler[T any](w *Watcher[T], opts ...HandlerOption) http.Handler {
	o := &handlerOptions{}
	for _, opt := range opts {
		opt(o)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(rw http.ResponseWriter, r *http.Request) {
		if !authOK(rw, r, o) {
			return
		}
		writeJSON(rw, http.StatusOK, w.Status())
	})
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, r *http.Request) {
		serveHealthz(rw, r, w, o)
	})

	var h http.Handler = mux
	if o.prefix != "" {
		h = http.StripPrefix(o.prefix, h)
	}
	for i := len(o.middleware) - 1; i >= 0; i-- {
		h = o.middleware[i](h)
	}
	return h
}
```

Note the `GET /` pattern in Go 1.22+ `ServeMux` matches only the exact root plus any unmatched path; to make unknown paths 404 rather than falling into `GET /`, register `GET /{$}` for the exact root and let other paths 404. Read the Go 1.22 `ServeMux` pattern semantics and use `GET /{$}` for `/` so `/values` returns 404. Verify with the route-set lockdown test.

Implement `authOK` (returns true if allowed; on failure writes 401 with the `Challenger` header, or 403 for `ErrForbidden`, and returns false), `serveHealthz` (calls `w.Health()`; on nil writes 200 `{"status":"ok"}`; on error writes 503, including the failing-field detail only when the request authenticates, checked via the same authenticator but never returning 401 from healthz), and `writeJSON`. `serveHealthz` must not 401: if auth is configured and the request does not pass, it still returns the bare status, just without detail.

**Redaction reminder:** the `Report` from `w.Status()` already carries redacted refs and no values, so serving it as JSON is safe by construction. Do not add any field to the wire shape beyond what `Report` contains.

- [ ] **Step 4: Run, confirm pass; race**

```bash
GOWORK=off go test ./... -run TestHandler -v
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
```

- [ ] **Step 5: Full suite; stage**

```bash
make test
git add handler.go handler_test.go
```

```
feat(core): add the admin HTTP handler with metadata-only routes

Handler serves exactly GET / (the Report) and GET /healthz, and never a
config value under any option, enforced by a route-set test and a
no-secret-in-body test. Auth is pluggable via WithAuth; /healthz is exempt but
detail-free, so a readiness probe needs no credentials while an unauthenticated
caller learns liveness but nothing about the config surface.
```

---

### Task 4: WithAdminHTTP and WithAdminTLS

**Files:**
- Create: `adminhttp.go`
- Modify: `reconcile.go` (add `adminAddr`, `adminOpts`, `adminTLS` to `options`)
- Modify: `reconciler.go` (`Watch` binds the listener and starts the server; `Watcher.Close` shuts it down)
- Create: `adminhttp_test.go`

**Interfaces:**
- Consumes: `Handler` (Task 3), `Watcher`, `options`.
- Produces: `WithAdminHTTP(addr string, opts ...HandlerOption) Option`, `WithAdminTLS(cfg *tls.Config) Option`.

**The lifecycle contract, which is the whole point of this task:**
- Off by default. With no `WithAdminHTTP`, no listener is constructed, no port is bound, and no goroutine starts. A `goleak` test asserts no extra goroutine without the option.
- `WithAdminHTTP` binds the listener inside `Watch`, before `Watch` returns. A bind failure (port in use, permission denied) fails `Watch` with the bind error, rather than logging and leaving the caller believing the endpoint is up. This matches `Watch`'s existing fail-fast on the initial `Load`.
- `Watcher.Close` calls the server's `Shutdown` with a bounded grace period and waits, so `Close` returning means the port is released. A test rebinds the same address after `Close` to prove it.
- `Load` accepts the option type but has no watcher to report on, so it ignores `WithAdminHTTP`; document that.

- [ ] **Step 1: Write the failing tests**

Create `adminhttp_test.go`. Cover:
- With `WithAdminHTTP("127.0.0.1:0")` (port 0 lets the OS choose; capture the actual address), an HTTP GET to `/healthz` on the bound address returns 200. (You will need to expose the chosen address; add an unexported field on the `Watcher` holding the bound `net.Addr` and a small test helper, or have `WithAdminHTTP` accept a concrete address and use a known-free one. Prefer port 0 plus an accessor; report your choice.)
- A bind failure fails `Watch`: bind one listener to an address, then `Watch` with `WithAdminHTTP` on the same address, and assert `Watch` returns a non-nil error and a nil watcher.
- `Close` releases the port: after `Close`, a fresh `net.Listen` on the same address succeeds.
- **No option means no goroutine:** wrap a `Watch` without `WithAdminHTTP` in a `goleak` check on `Close`, and additionally assert nothing is listening on a pre-chosen port.
- TLS: with `WithAdminTLS` and a self-signed cert (generate one in-test with `crypto/tls`+`crypto/x509`, or use `httptest`'s cert), an HTTPS GET succeeds and a plain HTTP GET to the TLS port fails.

- [ ] **Step 2: Run, confirm failure**

```bash
GOWORK=off go test ./... -run TestAdminHTTP -v
```

- [ ] **Step 3: Add the options**

In `reconcile.go`, add to `options`:

```go
	adminAddr string
	adminOpts []HandlerOption
	adminTLS  *tls.Config
```

Add the option constructors in `adminhttp.go`:

```go
// WithAdminHTTP makes the Watcher run its own HTTP server on addr, serving the
// same routes as Handler. It is off by default: with no WithAdminHTTP option no
// server is constructed, no port is bound, and no goroutine is started. The
// server's lifetime is the Watcher's; Close shuts it down gracefully and
// releases the port. Load ignores this option, since it has no watcher to
// report on.
//
// A bind failure fails Watch with the bind error rather than leaving the caller
// believing the endpoint is up.
func WithAdminHTTP(addr string, opts ...HandlerOption) Option {
	return func(o *options) { o.adminAddr = addr; o.adminOpts = opts }
}

// WithAdminTLS serves the admin endpoint over TLS. It carries the ClientCAs and
// ClientAuth settings that MTLS depends on. Basic auth over plaintext is not
// authentication; this exists so the shipped schemes can be used safely.
func WithAdminTLS(cfg *tls.Config) Option {
	return func(o *options) { o.adminTLS = cfg }
}
```

- [ ] **Step 4: Wire the lifecycle into Watch and Close**

In `reconciler.go`'s `Watch`, after the initial load succeeds and the `Watcher` is built but before returning, if `o.adminAddr != ""`:
1. `net.Listen("tcp", o.adminAddr)`; on error, cancel the watcher context and return the bind error (fail-fast). Wrap the TLS config if `o.adminTLS != nil` with `tls.NewListener`.
2. Build the handler: `Handler[T](w, o.adminOpts...)`.
3. Start `&http.Server{Handler: h}` on the listener in a goroutine tracked by `w.wg`, so `Close` waits for it.
4. Store the server and the bound `net.Addr` on the `Watcher` (unexported fields) so `Close` can shut it down and tests can read the address.

In `Close`, before or alongside the existing cancel+wait, call `server.Shutdown(ctx)` with a bounded timeout (e.g. 5s) if a server was started, then let the existing `wg.Wait()` proceed.

**A concurrency detail to get right:** the admin server goroutine must be registered with `w.wg` and must exit on `Shutdown`, so `goleak` stays clean. Read how the existing engine goroutines register with `w.wg` and match that pattern exactly.

- [ ] **Step 5: Run, race, full suite**

```bash
GOWORK=off go test ./... -run TestAdminHTTP -v
GOWORK=off go test -race ./...
make test
```

The `goleak` checks must pass: no goroutine leaks when the option is unused, and none after `Close` when it is used.

- [ ] **Step 6: Stage**

```bash
git add adminhttp.go reconcile.go reconciler.go adminhttp_test.go
```

```
feat(core): add WithAdminHTTP for a self-hosted admin endpoint

Off by default: no option means no listener, no bound port, no goroutine. When
set, Watch binds before returning so a bind failure fails Watch rather than
leaving a dead endpoint, and Close shuts the server down and releases the port.
WithAdminTLS serves it over TLS, which the shipped credential schemes require
to be safe.
```

---

### Task 5: Documentation

**Files:**
- Modify: `site/src/pages/docs/observability.md` (add the HTTP exposure section)
- Create: `site/src/pages/docs/auth.md`
- Modify: `site/src/pages/docs/security.md` (the two-surface model, unauthenticated default, bind-to-localhost guidance)
- Modify: `site/src/layouts/DocsLayout.astro` (nav entry for the auth page)
- Modify: `README.md` (mention the admin endpoint and auth)

**Interfaces:** consumes everything from Tasks 1-4.

- [ ] **Step 1: Document HTTP exposure in observability.md**

Add a section covering both modes: `Handler(w, opts...)` mounted on an existing mux, and `WithAdminHTTP(addr, opts...)` for a self-hosted server, with the lifecycle guarantees (off by default, bind fails Watch, Close releases the port). State plainly and repeatedly: **the endpoint serves metadata only and never a config value.** Show the two routes and the `/healthz` readiness-probe recipe. Verify every code example compiles against the real signatures.

- [ ] **Step 2: Write auth.md**

Create `site/src/pages/docs/auth.md` covering the `Authenticator` interface, the shipped schemes (`BasicAuth`, `BearerToken`, `APIKey`, `MTLS`, `AnyOf`, `AllOf`, and the `Func` variants for rotation), writing your own via the interface, and the credential-rotation pattern reading from a mamori-managed config. State that `PeerCred` and JWT land with the config server. Note constant-time comparison and fail-closed behavior.

- [ ] **Step 3: Update security.md**

Document the two-surface model (admin serves metadata and cannot serve values; the config server, later, serves values under policy). State that `WithAdminHTTP` is unauthenticated by default and should be bound to localhost or a non-ingress port, or fronted with `WithAuth` + `WithAdminTLS`. Reference the ref-redaction denylist.

- [ ] **Step 4: Nav, README, build**

Add `auth.md` to the nav in `DocsLayout.astro`. Add a short admin-endpoint mention to `README.md`.

```bash
make site-build   # Node 22; nvm use 22 if the engine check fails
```

- [ ] **Step 5: Stage**

```bash
git add site/src/pages/docs/observability.md site/src/pages/docs/auth.md site/src/pages/docs/security.md site/src/layouts/DocsLayout.astro README.md
```

```
docs: document the admin HTTP endpoint and the auth framework

Adds the HTTP exposure section (both modes, metadata-only, lifecycle), a new
auth page (the Authenticator interface, the shipped schemes, credential
rotation), and the two-surface security model. States that peer-credential and
JWT auth arrive with the config server.
```

---

## Self-Review

**Spec coverage.** Implements spec sections 7.5 (HTTP exposure) and 7.6 (Authenticator and the stdlib schemes). Deliberately deferred, with the reason stated in Scope: `PeerCred` (needs `golang.org/x/sys` promoted plus platform build tags, natural home is the config-server sidecar), `x/authjwt` (separate module, lands with the config server), and the `WithAdminHTTP`-over-UDS listener (also fits the config server, which needs UDS anyway).

**Placeholders.** None. Every task carries complete code for the load-bearing parts (the auth schemes, the handler routing, the option constructors) and precise specs for the rest. Three steps flag a judgment call and ask the implementer to report it: the `AnyOf` challenge-surfacing type (Task 2), the route-pattern choice for making unknown paths 404 (Task 3, `GET /{$}`), and the bound-address accessor for testing (Task 4).

**Type consistency.** `Authenticator` is defined once (Task 1) and consumed identically by every scheme (Task 2) and the handler (Task 3). `HandlerOption` is shared by `Handler` and `WithAdminHTTP`. The handler serves only `w.Status()`, whose `Report` already redacts refs and omits values, so the metadata-only guarantee holds by construction rather than by the handler remembering to redact.

**Risk noted.** Two areas carry the most risk. First, the metadata-only guarantee: it is enforced structurally (no values route exists) and by two tests (route-set lockdown, no-secret-in-body), so a future change that adds a value route fails CI. Second, the `WithAdminHTTP` goroutine lifecycle: the admin server must register with `w.wg` and exit on `Shutdown`, or `goleak` fails; the plan mandates a no-leak test both with and without the option. The constant-time-comparison and fail-closed requirements are the security core of Task 2 and each has a dedicated test.
