# Workstream H: the config server (fan-out)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Execute STRICTLY SERIALLY: one edit agent at a time. Do NOT run edit agents in parallel; two prior data-loss events came from parallel edits on the shared tree. Reviews (read-only) may overlap.

**Goal:** One process fronts the backends for many: it holds the cloud credentials, maintains one upstream watch per published value, and serves that value to many local consumers over HTTP and Unix sockets, under a mandatory authorization policy, never serving a client-supplied ref.

**Architecture:** A new `server/` module depending only on core plus stdlib. It reuses core's `Authenticator` (workstream B) for authentication and adds a `Policy` for authorization. It serves a fixed table of operator-declared name-to-ref bindings, resolving each through the caller's linked providers via a newly-exported core `WatchRef`. The wire protocol is versioned JSON over HTTP, identical on TCP (mandatory TLS) and Unix sockets. This is the most security-sensitive component in the spec: it concentrates every backend credential, so its guarantees are structural (no client refs, mandatory auth, mandatory policy, mandatory TLS on TCP, no values in logs), not conventions.

**Tech Stack:** Go 1.26. Core plus stdlib (`net/http`, `crypto/tls`, `crypto/subtle`, `encoding/json`, `net`). `golang.org/x/sys` promoted to a direct core dependency for `PeerCred`. No cloud SDKs in `server/` (providers arrive through the operator's own binary, like `Load`/`Watch`).

This implements spec section 13. It builds on the `Authenticator` from `2026-07-25-admin-http-and-auth.md` and the error kinds from workstream A. The `mamori://` client (workstream I) is a separate following plan; together they validate the wire protocol end to end.

## Decisions locked with the operator

- **Authz:** `Policy` interface with `AllowAll()`, `PrefixPolicy(map[subject][]glob)`, `PolicyFunc`. Mandatory: `server.New` errors without a policy. No implicit default.
- **Auth:** reuse core's `Authenticator`; add `PeerCred` (UDS, kernel-verified uid) now. `x/authjwt` deferred to a later module. Auth is mandatory: `New` errors without an `Authenticator` unless `NoAuth()` is set, and `NoAuth` is refused on a TCP listener.
- **Bindings:** operator-declared name->ref only; never a client-supplied ref. `exec:` and `mamori:` bindings rejected unless `AllowExec()`/`AllowChaining()`.
- **Transports:** TCP (TLS mandatory unless `InsecureNoTLS()`) and Unix sockets, both serving the same handler; both may run at once.

## Global Constraints

- **Do not run `git commit`** unless the operator explicitly asks (they commit). Stage with `git add`, report the suggested message.
- **NEVER `git stash`/`checkout`/`reset`/`clean`.** Two prior agents nearly destroyed the tree with `git stash`. The tree is committed now, but still: only `git add <your files>`. If `make test` breaks outside your files, STOP and report; do not fix by stashing.
- **Work on the current branch** (`feat/introspection`). No new branches, no worktrees.
- **`GOWORK=off` for every Go command** in a module directory; `make test` from the repo root.
- **The tree stays green after every task.** The core changes in Task 1 must not regress any existing module.
- **No em-dash characters** anywhere.
- **The server serves metadata AND real values, but NEVER a value in a log or audit record.** A test seeds a distinctive secret and asserts it appears in no audit output.
- Doc comments on every exported symbol explaining the why, especially the security rationale.

---

### Task 1: Core prerequisites (WatchRef + PeerCred)

**Files:**
- Create: `watchref.go` (core), `watchref_test.go`
- Create: `authpeercred_linux.go`, `authpeercred_darwin.go`, `authpeercred_other.go`, `authpeercred_test.go` (core)
- Modify: `reconciler.go` (`engine.start` calls the extracted `WatchRef`)
- Modify: `go.mod` (promote `golang.org/x/sys` to direct)

**Interfaces produced:**
- `WatchRef(ctx context.Context, p Provider, ref Ref, opts ...Option) <-chan Update` — the watch-source selection (native `WatchableProvider.Watch` else `pollWatch`), extracted so the server can watch a single ref the same way the engine does.
- `PeerCred(opts PeerCredOptions) Authenticator`, `PeerCredOptions{UIDs []int, GIDs []int}`.

**WatchRef extraction.** The engine's `start` currently inlines: `if wp, isW := p.(WatchableProvider); isW { ch, werr := wp.Watch(ctx, ref); if werr != nil { src = pollWatch(...) } else { src = ch } } else { src = pollWatch(...) }`. Extract exactly that into `WatchRef`, and have `start` call it. This must be behavior-preserving: the full existing watch suite passes unchanged. `WatchRef` builds an `options` from `opts` for `pollWatch`'s clock/interval/jitter (read how `start` gets `e.o`; `WatchRef` needs the same, so it takes `...Option` and builds `defaultOptions()` + applies them, matching `Load`/`Watch`).

- [ ] **Step 1: Write the failing tests**

`watchref_test.go`: using `mamoritest`, `WatchRef` over a watchable provider emits the current value as a baseline and a new `Update` on `Set`; over a non-watchable provider it polls (drive with `FakeClock`). `authpeercred_test.go`: `PeerCred` denies on a non-UDS (`r.TLS`/no unix conn) request; on the unsupported-platform build it denies unconditionally (test via the `other` file's behavior). The uid-match happy path needs a real unix socket peer; if that is hard in a unit test, assert the deny paths (no unix conn -> deny) and leave the happy path to an integration-tagged test, reporting the choice.

- [ ] **Step 2-4: Implement, extract, verify**

Implement `WatchRef` and route `engine.start` through it (behavior-preserving; re-run the FULL existing suite + `-race` + `goleak`). Implement `PeerCred`:
- `authpeercred_linux.go` (`//go:build linux`): read `SO_PEERCRED` via `x/sys/unix` from the connection. The HTTP server exposes the underlying `net.Conn` via `http.Server.ConnContext`; the handler reads the peer creds stashed there. Design: the UDS listener wraps accepted conns to capture `*net.UnixConn`, and `ConnContext` puts the ucred into the request context; `PeerCred.Authenticate` reads it. Document this plumbing.
- `authpeercred_darwin.go` (`//go:build darwin`): `LOCAL_PEERCRED` via `x/sys/unix` `GetsockoptXucred`.
- `authpeercred_other.go` (`//go:build !linux && !darwin`): `Authenticate` always denies with a clear "peer credentials unsupported on this platform" error.
- `PeerCred` on a non-unix connection denies (not a fallthrough). Sets `Identity{Subject: "uid:<n>", Attrs: {"uid","gid","pid"}}`.

Promote `x/sys` to direct with `go mod tidy`. Report that the core dependency set grew by one `golang.org/x` module (documented in the spec 5.1).

**The `ConnContext` plumbing is the subtle part** and is server-specific, but `PeerCred` (the Authenticator) lives in core so it is reusable by the admin endpoint too. Put the conn-capturing listener wrapper in the `server/` module (Task 5) and have `PeerCred` read a well-known context key that both set. Define the context key and the `ucred` type in core so both sides agree. Report the exact seam.

- [ ] **Step 5: Stage**

```bash
git add watchref.go watchref_test.go reconciler.go authpeercred_*.go go.mod go.sum
```

```
feat(core): export WatchRef and add PeerCred authenticator

WatchRef extracts the engine's native-watch-or-poll source selection so the
config server can watch a single ref the same way the reconciler does;
engine.start now calls it, behavior-preserving. PeerCred authenticates a
Unix-socket client by kernel-verified uid/gid (SO_PEERCRED on Linux,
LOCAL_PEERCRED on Darwin, deny elsewhere), the strongest sidecar auth.
Promotes golang.org/x/sys to a direct dependency.
```

---

### Task 2: server module scaffold, bindings, Policy

**Files:**
- Create: `server/go.mod` (module `github.com/xavidop/mamori/server`, `replace github.com/xavidop/mamori => ../..`), `server/server.go`, `server/bindings.go`, `server/policy.go`, `server/policy_test.go`, `server/server_test.go`
- Modify: `go.work` (add `./server`), root `Makefile` (auto-discovers modules, so likely no change; confirm)

**Interfaces produced:** `server.New(opts ...Option) (*Server, error)`; `Option`; `Bind(name, ref string) Option`, `BindFile(path string) Option`; `WithAuth(mamori.Authenticator) Option`, `NoAuth() Option`; `WithPolicy(Policy) Option`; `WithAudit(*slog.Logger) Option`; `AllowExec() Option`, `AllowChaining() Option`; `Policy` interface + `AllowAll()`, `PrefixPolicy(map[string][]string)`, `PolicyFunc(func(mamori.Identity, string) error)`.

- [ ] **Step 1: Write failing tests**

`policy_test.go`: `AllowAll` permits any subject/name; `PrefixPolicy` permits only matching globs (`"svc": {"db-*"}` allows `db-password`, denies `api-key`), and a subject not in the map is denied; `PolicyFunc` delegates. A denied name returns an error indistinguishable from a nonexistent name (same error), so the policy is not a directory.

`server_test.go`: `New` errors when no policy is set; `New` errors when no auth and no `NoAuth()`; `New` with `NoAuth()` plus only a TCP listener errors (NoAuth refused on TCP); `Bind("x", "exec:...")` errors without `AllowExec()`; `Bind("y", "mamori://...")` errors without `AllowChaining()`; a duplicate binding name errors.

- [ ] **Step 2-4: Implement, verify, stage**

Implement the `Server`, the binding table (parse each ref with `mamori.ParseRef`; reject `exec`/`mamori` schemes unless the corresponding allow-opt is set), the `Policy` interface and its three constructors, and `New`'s validation (mandatory policy; mandatory auth-or-NoAuth; NoAuth+TCP is an error; but defer the actual listeners to Task 5). `PrefixPolicy` matches with `path.Match`-style globs on the name; document the grammar. `New` returns a `*Server` not yet serving (Task 3 wires resolution, Task 5 wires listeners).

```
feat(server): scaffold the config server with bindings and policy

New config-server module: operator-declared name-to-ref bindings (never a
client-supplied ref; exec and mamori schemes gated behind explicit opts), a
mandatory authorization Policy (AllowAll/PrefixPolicy/PolicyFunc), and New()
validation that refuses to start without a policy and without auth (NoAuth is
allowed only on a Unix socket). Depends only on core plus stdlib.
```

---

### Task 3: Upstream watching (fan-out core)

**Files:** `server/resolve.go`, `server/resolve_test.go`; modify `server/server.go`.

**Interfaces:** internal. The server maintains, per binding, a reconciled last-known value plus its error/staleness, fed by one `WatchRef` per binding.

- [ ] Implement: on `Serve` (or `New`), start one `mamori.WatchRef` per binding through the operator's providers (the server takes providers via `mamori.WithProvider`-style options threaded to `WatchRef`, OR resolves through the global registry like `Load` does; match how `Load` gets providers and report). Each binding holds an `atomic.Pointer` to its latest `mamori.Value` plus last error/kind. A binding whose upstream is failing serves its last-known-good value and reports the upstream error kind in the response metadata, so a client can tell fresh from stale-but-serving. One hundred consumers of a binding produce one upstream watch, verified by a test counting resolves against a `mamoritest` provider.

- [ ] Test with `mamoritest`: N concurrent reads of a binding produce one upstream watch; a `Set` upstream propagates to subsequent reads; a `Fail` upstream leaves the last-good served with the error kind in metadata. `-race`, `goleak` on `Close`. Stage.

```
feat(server): fan-out upstream watching, one watch per binding
```

---

### Task 4: Wire protocol handler (v1)

**Files:** `server/handler.go`, `server/wire.go` (the JSON types), `server/handler_test.go`; the auth/authz/audit middleware.

**Routes (spec 13.4), versioned `/v1/`:**

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/values/{name}` | resolve one binding |
| POST | `/v1/values` | batch, body `{"names":[...]}`, backs `BatchProvider` |
| GET | `/v1/watch?name=a&name=b` | SSE stream of updates |
| GET | `/v1/healthz` | liveness, no binding detail |

Value response maps one-to-one onto `mamori.Value`, `bytes` base64-encoded:
```json
{"name":"db-password","bytes":"aHVudGVyMg==","version":"...","sensitive":true,"not_after":"...","metadata":{}}
```
Errors carry the workstream-A kind, mapped to status: not_found->404, invalid->400, unauthenticated->401, permission_denied->403, rate_limited->429, unavailable->503, unknown->500:
```json
{"error":{"kind":"permission_denied","message":"..."}}
```
Every value response sets `Cache-Control: no-store`. SSE frames: `event: update` (Value body), `event: error` (error body), comment heartbeat on an interval.

- [ ] **The security-critical requirements, each tested:**
  - Every request is authenticated (via the `Authenticator`) and authorized (`Policy.Allow(identity, name)`) before any binding is read. A denied name returns 403 and is byte-identical to a nonexistent name's 404? No: denied is 403, nonexistent is 404, but a denied name must NOT reveal whether it exists (return 403 for both "exists but denied" and, per the policy, treat unknown-but-denied consistently; the simplest honest rule: authorize first against the requested name; if the policy denies, 403 regardless of existence; if allowed, then 404 if truly absent). Document and test this ordering so the policy is not a directory of what exists.
  - **No value ever appears in an audit log.** `WithAudit` logs identity subject, binding name, allow/deny, kind, latency; never `bytes`. A test seeds `LEAKME` and asserts it is in no audit record.
  - `/v1/healthz` reveals no binding names or values.
  - The wire `kind` round-trips through `mamori.ErrorKind`/`SentinelFor` so the client (workstream I) can reconstruct the sentinel.
- [ ] Tests: each route; the kind->status table; authz-denies-before-read; no-value-in-audit; base64 round-trip; SSE delivers an update and survives a forced disconnect (the client handles reconnect, not the server). Stage.

```
feat(server): v1 wire protocol handler with auth, authz, and audit
```

---

### Task 5: Transports and lifecycle

**Files:** `server/transport.go`, `server/transport_test.go`; the UDS conn-capturing listener for `PeerCred`; modify `server/server.go` (`Serve`, `Close`).

**Interfaces:** `Unix(path string, mode os.FileMode) Option`, `TCP(addr string, opts ...TCPOption) Option`, `TLS(cfg *tls.Config) TCPOption`, `InsecureNoTLS() TCPOption`; `(*Server).Serve(ctx) error`, `(*Server).Close() error`, `(*Server).Addrs() []net.Addr`.

- [ ] Implement:
  - `Unix(path, mode)`: remove a stale socket at path on start, create with `mode` (0600 recommended, documented), unlink on `Close`. Wrap the listener so accepted conns capture peer creds into `ConnContext` (the `PeerCred` seam from Task 1).
  - `TCP(addr, TLS(cfg))`: bind; wrap with `tls.NewListener` when TLS is set. `TCP` without `TLS` fails construction unless `InsecureNoTLS()` (named to be uncomfortable and greppable). `NoAuth()` is refused on a TCP listener (checked in `New`, Task 2; re-confirm at Serve).
  - Both listeners may run at once, serving identical bindings under one policy. `Serve` runs all listeners and returns on the first failure; `Close` shuts all down (graceful, bounded), unlinks sockets, waits (goleak clean).
  - Bind failures fail `Serve` (fail-fast), mirroring `WithAdminHTTP`.
- [ ] Tests: a UDS server serves a binding to a client over the socket; a TCP+TLS server serves over HTTPS and rejects plaintext; `TCP` without TLS and without `InsecureNoTLS` fails; both-transports-at-once; `Close` unlinks the socket and releases the port; `PeerCred` over the real UDS authenticates by uid (this is where the happy path from Task 1 gets its integration coverage, if a real unix socketpair is usable in-test; report). Stage.

```
feat(server): Unix and TLS-TCP transports with lifecycle and PeerCred plumbing
```

---

### Task 6: Documentation

**Files:** `site/src/pages/docs/server.md` (new), `site/src/pages/docs/auth.md` (PeerCred), `site/src/pages/docs/security.md` (blast radius), `site/src/layouts/DocsLayout.astro` (nav), `README.md`, `server/README.md`.

- [ ] Document: why fan-out (credentials in one place, N consumers -> one upstream watch, workloads without their own IAM role); bindings (and why never client refs: `file:///etc/shadow` and `exec:` would be reachable otherwise); the `Policy` model; transports (UDS + TLS TCP); the wire protocol reference; `PeerCred`; the audit log (never values); and, prominently, the **blast radius** (the server concentrates every backend credential, so compromising it is worse than any single consumer) and that `/v1/` freezes once external clients exist. Do NOT sell only the benefits. `make site-build`. Stage.

```
docs: document the config server, PeerCred, and its blast radius
```

---

## Self-Review

**Spec coverage.** Implements spec section 13 in full: 13.1 deployment model (module + operator's binary), 13.2 bindings, 13.3 upstream watching (via the new core `WatchRef`), 13.4 wire protocol, 13.5 transports, 13.6 authentication (reused `Authenticator` + `PeerCred`), 13.7 authorization (`Policy`), 13.8 audit. `x/authjwt` is deferred per the operator's decision.

**Placeholders.** None. Each task names its files, interfaces, and tests. Three genuine judgment calls are flagged for the implementer to make and report: how `WatchRef`/the server obtain the operator's providers (threaded option vs registry, matching `Load`), the `PeerCred` `ConnContext` seam between core and the server module, and whether the `PeerCred` uid happy-path is unit- or integration-testable.

**Type consistency.** The server reuses core's `Authenticator`/`Identity` (workstream B) unchanged; `Policy` consumes `Identity`. The wire `kind` uses `mamori.ErrorKind`/`SentinelFor` so the client can reconstruct it. `WatchRef` returns the same `<-chan Update` the engine uses.

**Risk noted.** This is the highest-blast-radius component in the project: it holds every backend credential. The mitigations are structural and each is tested: no client-supplied refs (delete the file-read/exec-injection class), mandatory policy, mandatory auth (NoAuth only on UDS), mandatory TLS on TCP, and no values in audit logs. The `PeerCred` `ConnContext` plumbing and the platform build tags are the fiddliest engineering; the unsupported-platform build denies rather than silently allowing. Execute strictly serially, and re-run the full existing suite after Task 1's `engine.start` change to hold the single-source watch invariant.
