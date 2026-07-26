# `providers/mamori` client (workstream I) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `providers/mamori`, a `mamori://` client provider that resolves binding names from a mamori config server (workstream H) over the v1 wire protocol, transparently reproducing the upstream error classification so a consumer cannot tell it is talking to a fan-out server.

**Architecture:** A new provider module like any other (`github.com/xavidop/mamori/providers/mamori`), implementing `Provider` (GET /v1/values/{name}), `BatchProvider` (POST /v1/values), and `WatchableProvider` (SSE `/v1/watch`) so `mamori://` is a native watch. The ref path is a binding NAME, never an upstream ref (the client-side half of decision D9). Classification passes through: the client reconstructs the sentinel from the wire `kind` so `errors.Is(err, mamori.ErrPermissionDenied)` holds exactly as against the real backend. Conformance runs `providertest` against a real in-process `server.Server` fronting a `mamoritest` upstream, over both a Unix socket and a TLS TCP listener, so the client, the wire protocol, and the server are all exercised by the same kit.

**Tech Stack:** Go 1.26. Standard library only for the module's non-test code (net/http, crypto/tls, encoding/json, encoding/base64 via json's []byte, bufio for SSE). Test-only dependencies: `github.com/xavidop/mamori/server`, `github.com/xavidop/mamori/mamoritest`, `github.com/xavidop/mamori/providertest`.

## Global Constraints

- **Do not run `git commit`.** Stage with `git add` on this task's files only, and report the suggested commit message. There is a large amount of pre-existing staged work on this tree from workstreams A-E and H (plan 11). Touch only your task's files, and NEVER run `git stash`, `git clean`, `git checkout -- <file>`, or `git reset` on this tree (a prior subagent nearly clobbered it with `git stash`; two data-loss events occurred).
- **Work on `main`.** No branches, no worktrees.
- **`GOWORK=off` for every Go command,** run from inside the module directory (`providers/mamori/`); `make test` from the repo root.
- **The tree stays green after every task.** Run `GOWORK=off go test -race ./...`, `go vet ./...`, and `gofmt -l .` (must print nothing) inside the module, then `make test` from the repo root, before reporting a task done.
- **No em-dash characters** anywhere (code, comments, docs, reports). Use commas, parentheses, or restructure.
- **Error wrapping is `fmt.Errorf("%w: %s", sentinel, message)`** where the message is the wire error message (a plain string, so `%s` not `%w`). Where an SDK-style error is wrapped, use `%w`. The `errors.Is` chain to the sentinel is a hard requirement; the workstream A conformance case tests it.
- **`ErrNotFound` must be reachable:** a wire `not_found` (or a 404 for an unbound name) maps to `mamori.ErrNotFound` so defaults and optional fields still apply.
- **Non-test code imports stdlib + `github.com/xavidop/mamori` only.** The server, mamoritest, and providertest modules are TEST-only dependencies.
- **The module go.mod uses `replace github.com/xavidop/mamori => ../..`** (two levels deep, like every other provider). The test-only server dep needs `replace github.com/xavidop/mamori/server => ../../server`.
- **Serial execution only.** Never run two edit agents in parallel on this tree.

## Wire protocol reference (from the server, workstream H, section 13.4 of the spec)

The client is the counterpart to the server's `server/handler.go` + `server/wire.go`. It MUST match these shapes exactly.

- `GET /v1/values/{name}` success body (status 200, `Cache-Control: no-store`):
  ```json
  {"name":"db-password","bytes":"aHVudGVyMg==","version":"v1","sensitive":true,"not_after":"2026-07-24T12:00:00Z","metadata":{},"kind":""}
  ```
  `bytes` is base64 (Go marshals `[]byte` as base64 automatically). `version`, `sensitive`, `not_after`, `bytes` are `omitempty`. `metadata` is always present (an object, possibly `{}`). `kind` is `omitempty`: EMPTY means a fresh value; a NON-EMPTY `kind` on a 200 success means "this is a last-known-good value being served while the upstream is CURRENTLY failing" (stale-but-serving), an annotation on a success, never a failure by itself. The client returns the value with a nil error in both cases.
- Whole-request failure body (single-value route, and any 4xx/5xx before a value): `{"error":{"kind":"permission_denied","message":"..."}}`.
- `POST /v1/values` request `{"names":["a","b"]}`, response `{"values":[{...valueBody...},{...}]}` in request order. A per-name failure is one entry `{"name":"a","error":{"kind":"...","message":"..."}}`; the batch itself is still 200.
- `GET /v1/watch?name=a&name=b` is Server-Sent Events: `event: update` frames carry a valueBody `data:` line; `event: error` frames carry `{"name","error":{...}}`; lines beginning with `:` are comment heartbeats. The server polls its snapshot roughly every 200ms, so updates are near-real-time, not instant push.
- kind -> HTTP status (the client reads the `kind` field, not the status, but the two agree): `not_found`->404, `invalid`->400, `unauthenticated`->401, `permission_denied`->403, `rate_limited`->429, `unavailable`->503, `unknown`->500.

## Core types the client consumes (already exist, do NOT redefine)

- `mamori.Provider` = `Scheme() string` + `Resolve(ctx, Ref) (Value, error)`.
- `mamori.BatchProvider` = `Provider` + `ResolveBatch(ctx, []Ref) (map[string]Value, error)`. The map is keyed by each input `Ref.String()` (equivalently `Ref.Raw`). A not-found ref is OMITTED from the map (mamori applies the default), not an error.
- `mamori.WatchableProvider` = `Provider` + `Watch(ctx, Ref) (<-chan Update, error)`. The channel is closed when the watch ends (including ctx cancellation). Transient errors are delivered as `Update{Err: ...}` with the channel staying open; closure signals termination.
- `mamori.Value{Bytes []byte; Version string; Sensitive bool; NotAfter time.Time; Metadata map[string]string}`.
- `mamori.Ref{Scheme, Path, Key string; Opts url.Values; Raw string}`. For `mamori://db-password`, `Ref.Path == "db-password"`. `Ref.String()` renders back to the raw tag.
- `mamori.Update{Value Value; Err error}`.
- Sentinels: `mamori.ErrNotFound`, `mamori.ErrPermissionDenied`, `mamori.ErrUnauthenticated`, `mamori.ErrUnavailable`, `mamori.ErrRateLimited`, `mamori.ErrInvalid`. Kind constants: `mamori.KindNotFound` (`"not_found"`), `KindPermissionDenied` (`"permission_denied"`), `KindUnauthenticated` (`"unauthenticated"`), `KindUnavailable` (`"unavailable"`), `KindRateLimited` (`"rate_limited"`), `KindInvalid` (`"invalid"`), `KindUnknown` (`"unknown"`).
- `mamori.Register(p Provider)` registers a provider globally (called from `init`). `mamori.ParseRef(s string) (Ref, error)`.

## File Structure

- `providers/mamori/go.mod`, `go.sum` (new module).
- `providers/mamori/mamori.go`: `Config`, `Option`s, `New`, `Provider` struct, `Scheme`, endpoint parsing, the shared `http.Client`/transport, the shared request helper, `init` registration.
- `providers/mamori/resolve.go`: `Resolve` + the wire decode + `sentinelForKind` classification helper + the wire structs (`valueBody`, `errorBody`, `errorEnvelope`, `batchRequest`, `batchResponse`).
- `providers/mamori/batch.go`: `ResolveBatch`.
- `providers/mamori/watch.go`: `Watch` + the SSE reader + reconnect/backoff/jitter.
- `providers/mamori/*_test.go`: unit tests (httptest-canned) per file, plus `conformance_test.go` (the real-server providertest harness).
- `providers/mamori/README.md`.
- `site/src/pages/docs/providers/mamori.md`, nav in `site/src/layouts/DocsLayout.astro`, provider table in `site/src/pages/docs/providers/index.md`.

---

### Task 1: Module scaffold, Config, New, endpoint parsing

**Files:**
- Create: `providers/mamori/go.mod`, `providers/mamori/mamori.go`
- Test: `providers/mamori/mamori_test.go`

**Interfaces:**
- Produces: `mamoriprov.Config{Endpoint string; Auth Authenticator?; TLSConfig *tls.Config; InsecureNoTLS bool; HTTPClient *http.Client}` (see below for the auth shape decision), `func New(cfg Config) *Provider`, `func (p *Provider) Scheme() string` (returns `"mamori"`), and an unexported `func (p *Provider) do(ctx, method, path string, body io.Reader) (*http.Response, error)` request helper used by Tasks 2-4. Also an unexported `parseEndpoint(endpoint string, insecure bool) (baseURL string, transport *http.Transport, err error)`.

**Endpoint forms (spec section 14):**
- `unix:///path/to.sock`: a custom `http.Transport.DialContext` that dials the Unix socket for every request; the HTTP request URL uses a fixed dummy host (e.g. `http://unix`) with the real path appended. The `///` means `url.Parse` yields an empty host and `Path == "/path/to.sock"`.
- `https://host:port`: standard TLS transport; attach `cfg.TLSConfig` if set.
- `http://host:port`: REFUSED unless `cfg.InsecureNoTLS` is true (mirrors the server's `InsecureNoTLS`, named to be uncomfortable). Return a clear construction-surfaced error otherwise. Since `New` returns only `*Provider` (no error, to match the registration pattern), record the parse error on the Provider and return it from the first `Resolve`/`Watch`/`ResolveBatch` call (lazy-surface), OR expose the error. DECISION: store the endpoint error on the Provider and return it (wrapped, classified as `mamori.ErrInvalid`) from every method, so a misconfigured endpoint fails loudly at first use rather than panicking. Report this choice.

**Auth shape decision (make and report):** the spec shows `Endpoint` plus "with Auth and TLS". The server authenticates with a `mamori.Authenticator` on the SERVER side; the CLIENT must PRESENT a credential, which is the other half. The `mamori.Authenticator` interface authenticates an inbound request, so it is the wrong type for the client. Provide client-side credential attachment as a function option instead: `func WithRequestEditor(fn func(*http.Request)) Option` (or `WithHeader(key, value string)`), applied to every outbound request in `do`. This covers BearerToken/APIKey/BasicAuth (set the header) and leaves mTLS to `TLSConfig.Certificates`. PeerCred needs nothing on the client (the kernel supplies it over the UDS). Implement `WithRequestEditor` (and a `WithHeader` convenience wrapping it). Do NOT invent a client-side re-use of `mamori.Authenticator`.

- [ ] **Step 1: Write the failing tests** in `mamori_test.go`:
  - `TestParseEndpointUnix`: `parseEndpoint("unix:///run/x.sock", false)` yields a non-nil transport with a DialContext and a base URL whose host is the dummy host; no error.
  - `TestParseEndpointHTTPSOK`: `parseEndpoint("https://h:8443", false)` yields base URL `https://h:8443`, no error.
  - `TestParseEndpointHTTPRefusedWithoutInsecure`: `parseEndpoint("http://h:80", false)` returns a non-nil error that satisfies `errors.Is(err, mamori.ErrInvalid)`.
  - `TestParseEndpointHTTPAllowedWithInsecure`: `parseEndpoint("http://h:80", true)` returns no error.
  - `TestSchemeIsMamori`: `New(Config{Endpoint:"unix:///x.sock"}).Scheme() == "mamori"`.
  - `TestEndpointErrorSurfacesFromResolve`: `New(Config{Endpoint:"http://h"}).Resolve(ctx, ref)` returns an error satisfying `errors.Is(err, mamori.ErrInvalid)` (stub `Resolve` may just return the stored endpoint error for now; Task 2 fills it in).
- [ ] **Step 2: Run the tests, verify they fail** (compile error / undefined symbols): `GOWORK=off go test ./... 2>&1 | head`.
- [ ] **Step 3: Write `go.mod`** (module `github.com/xavidop/mamori/providers/mamori`, `go 1.26.0`, `require github.com/xavidop/mamori v0.1.0`, `replace github.com/xavidop/mamori => ../..`; add the server/mamoritest/providertest requires + replaces in Task 5 when first used). Run `GOWORK=off go mod tidy`.
- [ ] **Step 4: Implement `mamori.go`**: `Config`, `Option` (functional options, plus a `Config`-struct constructor `New(cfg Config)` per the spec's example), `parseEndpoint`, `New`, `Scheme`, `do`, `init() { mamori.Register(New(Config{})) }` (a registered default with an empty endpoint that lazily errors is acceptable, matching how other providers register a zero-config default; confirm the empty-endpoint error is classified `ErrInvalid`). A minimal `Resolve` stub returning the stored endpoint error (Task 2 replaces it).
- [ ] **Step 5: Run the tests, verify they pass;** `go vet ./...`; `gofmt -l .` (empty); `make test` from repo root.
- [ ] **Step 6: Stage** `git add providers/mamori/`.

---

### Task 2: `Resolve` and classification passthrough

**Files:**
- Create: `providers/mamori/resolve.go`
- Modify: `providers/mamori/mamori.go` (remove the stub `Resolve`)
- Test: `providers/mamori/resolve_test.go`

**Interfaces:**
- Consumes: `do` from Task 1, `mamori.Value`, the sentinels/kinds.
- Produces: `func (p *Provider) Resolve(ctx, mamori.Ref) (mamori.Value, error)`; unexported `sentinelForKind(kind string) error` (maps a wire kind string to the matching sentinel, `KindUnknown`/unrecognized -> nil so the caller wraps a bare classified error); the wire structs `valueBody`, `errorDetail`, `errorEnvelope`.

**Behavior:**
- The binding name is `ref.Path` (never an upstream ref: client-side D9). If `ref.Path == ""`, return `fmt.Errorf("%w: mamori:// ref %q has no binding name", mamori.ErrInvalid, ref.Raw)`.
- `GET /v1/values/{name}` with `name` path-escaped (`url.PathEscape`).
- On 200: decode `valueBody`, build `mamori.Value{Bytes, Version, Sensitive, NotAfter (from *time.Time, zero if nil), Metadata}`. A non-empty `kind` on a 200 is a stale annotation: still return the value with a NIL error (Task 5's stale conformance depends on this). Report whether you surface the stale kind anywhere (you may drop it; the value is still usable).
- On non-200: decode `errorEnvelope` (`{"error":{"kind","message"}}`); map `kind` to a sentinel via `sentinelForKind`; return `fmt.Errorf("%w: %s", sentinel, message)`. If the kind is unrecognized or `unknown`, return a bare error carrying the message with no sentinel (so `ErrorKind` reports `KindUnknown`, matching a real provider that could not classify). A `not_found` kind (or a 404 with an undecodable body) maps to `mamori.ErrNotFound`.
- Bounded body read for the error path (`io.LimitReader`, e.g. 8 KiB) so a hostile/broken server cannot stream unboundedly.

**The classification map** (put in resolve.go, greppable):
```go
var wireKindSentinel = map[mamori.Kind]error{
	mamori.KindNotFound:         mamori.ErrNotFound,
	mamori.KindPermissionDenied: mamori.ErrPermissionDenied,
	mamori.KindUnauthenticated:  mamori.ErrUnauthenticated,
	mamori.KindUnavailable:      mamori.ErrUnavailable,
	mamori.KindRateLimited:      mamori.ErrRateLimited,
	mamori.KindInvalid:          mamori.ErrInvalid,
}

func sentinelForKind(kind string) error { return wireKindSentinel[mamori.Kind(kind)] }
```

- [ ] **Step 1: Write failing tests** in `resolve_test.go` using `httptest.NewServer` returning canned wire bodies:
  - `TestResolveSuccessDecodesValue`: server returns the sample 200 body with base64 bytes, a version, `sensitive:true`, a `not_after`, and metadata; assert `Value.Bytes`, `Version`, `Sensitive`, `NotAfter` (parsed), and `Metadata` all round-trip.
  - `TestResolveStaleKindStillReturnsValueNilError`: 200 body with `"kind":"unavailable"` and real bytes; assert the value is returned and the error is nil.
  - `TestResolveClassificationPassthrough`: table over the five kinds (`permission_denied`, `unauthenticated`, `unavailable`, `rate_limited`, `invalid`) each returned as `{"error":{"kind":K,"message":"m"}}` with the matching status; assert `errors.Is(err, <sentinel>)` for each.
  - `TestResolveNotFoundIsErrNotFound`: 404 with `{"error":{"kind":"not_found","message":"m"}}`; assert `errors.Is(err, mamori.ErrNotFound)`.
  - `TestResolveUnknownKindReportsKindUnknown`: 500 `{"error":{"kind":"unknown","message":"m"}}`; assert `mamori.ErrorKind(err) == mamori.KindUnknown`.
  - `TestResolveEmptyNameIsInvalid`: a ref with an empty path; assert `errors.Is(err, mamori.ErrInvalid)`.
  - Point the provider at each `httptest.Server` via `New(Config{Endpoint: ts.URL, InsecureNoTLS: true})`.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement `resolve.go`;** delete the Task 1 stub.
- [ ] **Step 4: Run, verify pass;** `go vet`; `gofmt -l .`; repo-root `make test`.
- [ ] **Step 5: Stage** `git add providers/mamori/`.

---

### Task 3: `BatchProvider` via `POST /v1/values`

**Files:**
- Create: `providers/mamori/batch.go`
- Test: `providers/mamori/batch_test.go`

**Interfaces:**
- Consumes: `do`, `sentinelForKind`, the wire structs from Task 2.
- Produces: `func (p *Provider) ResolveBatch(ctx, []mamori.Ref) (map[string]mamori.Value, error)`; the wire structs `batchRequest{Names []string}`, `batchResponse{Values []valueBody}`.

**Behavior:**
- Build `batchRequest.Names` from each `ref.Path`, in input order. Reject any ref with an empty path (`mamori.ErrInvalid`).
- `POST /v1/values` with the JSON body. On a non-200 whole-request failure (malformed body, 401/403 before any name), decode `errorEnvelope` and return the classified error for the WHOLE batch (this is a transport/auth failure, not a per-name outcome).
- On 200, decode `batchResponse`. For each entry, match it back to the input ref by NAME (the response preserves request order, but match by `valueBody.Name` to be robust). Key the output map by the input `ref.String()` (the `Raw` tag), per the `BatchProvider` contract.
  - Entry with no `error` field: a value (fresh or stale-annotated); include it in the map.
  - Entry with `error.kind == "not_found"`: OMIT it from the map (mamori applies the default). Do NOT error.
  - Entry with any OTHER `error` kind (permission_denied, unavailable, ...): this is a hard per-name failure with no last-known-good; return the classified error for the whole `ResolveBatch` call (a single denied secret in a batch should fail loudly, matching what a per-ref `Resolve` would do; the alternative, silently dropping it, would let a consumer apply a default in place of a secret it is not allowed to read). Report this decision in your report; it is the one genuine judgment call in this task.
- A name present in the request but absent from the response entirely is treated as not-found (omitted).

- [ ] **Step 1: Write failing tests** in `batch_test.go` (httptest returning canned batch bodies):
  - `TestResolveBatchReturnsValuesKeyedByRawRef`: two names both resolved; assert the map has both, keyed by `ref.String()`, with correct bytes.
  - `TestResolveBatchOmitsNotFound`: one resolved, one `{"name":"b","error":{"kind":"not_found"}}`; assert the map has only the resolved one (no error).
  - `TestResolveBatchHardErrorFailsWholeBatch`: one resolved, one `{"name":"b","error":{"kind":"permission_denied"}}`; assert `ResolveBatch` returns an error satisfying `errors.Is(err, mamori.ErrPermissionDenied)`.
  - `TestResolveBatchWholeRequestFailure`: server returns 401 `{"error":{"kind":"unauthenticated"}}`; assert `errors.Is(err, mamori.ErrUnauthenticated)`.
  - `TestResolveBatchSingleRequest`: a request for 3 names issues exactly ONE HTTP request (count requests on the httptest server).
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement `batch.go`.**
- [ ] **Step 4: Run, verify pass;** `go vet`; `gofmt -l .`; repo-root `make test`.
- [ ] **Step 5: Stage** `git add providers/mamori/`.

---

### Task 4: `WatchableProvider` via SSE, with reconnect

**Files:**
- Create: `providers/mamori/watch.go`
- Test: `providers/mamori/watch_test.go`

**Interfaces:**
- Consumes: `do`, `sentinelForKind`, the wire structs.
- Produces: `func (p *Provider) Watch(ctx, mamori.Ref) (<-chan mamori.Update, error)`.

**Behavior:**
- `GET /v1/watch?name={name}` with `Accept: text/event-stream`. Return a buffered `chan mamori.Update` and spawn ONE goroutine that reads SSE frames and forwards `Update`s, closing the channel on ctx cancellation or unrecoverable teardown.
- SSE parsing (a small `bufio.Scanner`/reader): accumulate `event:` and `data:` lines; a blank line dispatches the frame. `event: update` -> decode the `data` as a `valueBody`, send `Update{Value: ...}`. `event: error` -> decode `{"name","error":{...}}`, send `Update{Err: fmt.Errorf("%w: %s", sentinelForKind(kind), message)}` (channel stays open, per the `WatchableProvider` contract). Lines beginning with `:` (heartbeat comments) are ignored.
- Reconnect: if the stream ends (EOF, read error, non-200 status) and ctx is not done, reconnect after a backoff with jitter (exponential from e.g. 100ms capped at e.g. 30s, plus jitter), then resubscribe (re-issue the GET). Do NOT close the caller's channel across reconnects; only close it when ctx is done. A reconnection failure is a transient `Update{Err: ...}` (classified as `mamori.ErrUnavailable`), not a channel close.
- Respect ctx: the request uses `http.NewRequestWithContext`; on ctx cancellation the read unblocks, the goroutine drains, closes the channel, and returns (goleak clean).
- Bound the per-line/per-frame read so a hostile server cannot exhaust memory.

- [ ] **Step 1: Write failing tests** in `watch_test.go` using an httptest server that speaks SSE (write `event:`/`data:` lines + `Flush`):
  - `TestWatchDeliversUpdate`: server sends one `event: update` frame with a value; assert the channel yields `Update{Value}` with correct bytes.
  - `TestWatchDeliversErrorFrameKeepsChannelOpen`: server sends an `event: error` frame (`permission_denied`) then an `event: update`; assert the first `Update.Err` satisfies `errors.Is(..., mamori.ErrPermissionDenied)` and the channel then still delivers the update (not closed).
  - `TestWatchReconnectsAfterDisconnect`: server closes the connection after the first frame; on reconnect it sends a second frame; assert both frames arrive on the same channel. (Use a request counter to confirm a second GET happened.)
  - `TestWatchClosesChannelOnContextCancel`: cancel the ctx; assert the channel closes; wrap with `goleak.VerifyNone` (add `go.uber.org/goleak` as a test dep) to prove no goroutine leak.
  - `TestWatchIgnoresHeartbeatComments`: server sends a `: heartbeat` line then an update; assert only the update is delivered.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement `watch.go`.**
- [ ] **Step 4: Run, verify pass** with `-race`; `go vet`; `gofmt -l .`; repo-root `make test`.
- [ ] **Step 5: Stage** `git add providers/mamori/`.

---

### Task 5: Conformance against a real in-process server (UDS and TLS TCP)

This is the strong conformance the spec calls for (section 14): `providertest` drives the CLIENT, which talks over the real wire to a real `server.Server`, which fans out to a `mamoritest` upstream. It validates the client, the wire protocol, and the server together.

**Files:**
- Create: `providers/mamori/conformance_test.go`
- Modify: `providers/mamori/go.mod` (add test-only requires + replaces for `server`, `mamoritest`, `providertest`; run `GOWORK=off go mod tidy`).
- Test: the file IS the test.

**The harness design (the crux; implement exactly, it resolves three real hazards):**

The server has a FIXED binding table set at `server.New`, but `providertest` generates some keys dynamically (`classify-<kind>-<uniq>`, `does-not-exist-<uniq>`). And the server serves last-known-good with a 200 + `kind` annotation for a binding that PREVIOUSLY resolved but is CURRENTLY failing, returning a hard error ONLY for a binding that has NEVER resolved successfully. So the harness normalizes keys onto a fixed binding set and controls resolution state precisely.

1. **One `mamoritest.Provider` upstream** under a private scheme, e.g. `up := mamoritest.NewProvider("up")`. The server binds each conformance name to `up://<name>`.
2. **`Config.Key` normalizes dynamic keys onto a fixed set** so the binding table is static:
   ```go
   Key: func(name string) string {
       switch {
       case strings.HasPrefix(name, "classify-"):
           return "conformance-classify"        // one reused slot; classify cases are sequential (Seed/Fail/Clear each)
       case strings.HasPrefix(name, "does-not-exist-"):
           return "conformance-absent"           // deliberately NOT bound -> server 404 not_found
       default:
           return "conformance-" + name          // scheme, resolve, ctxcancel, concurrent, version, watch, watchclose, leak
       }
   },
   ```
3. **The server is constructed once** with a binding for every fixed name EXCEPT `conformance-absent`:
   - Bind `conformance-scheme`, `conformance-resolve`, `conformance-ctxcancel`, `conformance-concurrent`, `conformance-version`, `conformance-watch`, `conformance-watchclose`, `conformance-leak`, and `conformance-classify`, each to `up://<same-name>`.
   - `conformance-absent` is NOT bound, so a GET for it returns 404 `not_found` (the client maps this to `ErrNotFound`, satisfying the not-found case).
   - Server options: `server.WithProvider(up)`, `server.WithPolicy(server.AllowAll())`, and for the UDS variant `server.NoAuth()` + `server.Unix(sockPath, 0600)`; for the TCP variant a self-signed `server.TCP(addr, server.TLS(cfg))` + an auth scheme (see below). Start with `go srv.Serve(ctx)`; `defer srv.Close()`.
4. **`Config.Ref`** = `func(key string) string { return "mamori://" + key }` (key is already normalized).
5. **`Config.Seed`** writes to the upstream AND synchronizes on the async watch fan-out:
   - For a `conformance-classify` key: do NOT store a good value (leave the binding unresolved so a later `Fail` yields a hard error, not stale-serving). Return nil. (providertest's classify case does not verify the seeded value; it only checks the post-Fail error.)
   - For any other key: `up.Set(key, val)`, then POLL the client (`p.Resolve` through the running server) until it returns the value (bounded, e.g. 3s), so the server's watch has propagated before the test proceeds. This makes `resolve`/`version`/`watch` deterministic despite the 200ms SSE poll and the watch fan-out latency.
6. **`Config.Mutate`** = `up.Set(key, val)` then poll until the client observes the new value (or new version). Needed for version-monotonicity and watch tests.
7. **`Config.Fail`** = `up.Fail(key, err)` (mamoritest publishes the error to the server's active Watch), then POLL until the client's `Resolve` returns an error of the matching kind (bounded), so the classify assertion sees the propagated failure. Because the classify slot was never seeded (step 5), the binding has `hasValue == false`, so the server returns a hard error with the kind rather than a stale 200.
8. **`Config.Clear`** = `up.Clear(key)` then poll until the client no longer errors for that key (or, for the classify slot which was never seeded, until it returns not_found again). Bounded.
9. **`EventuallyTimeout`**: set generously (e.g. 5s) to absorb the SSE poll interval.

Run the WHOLE `providertest.Run` twice via a sub-test helper `runConformance(t, endpoint, serverOpts)`: once over UDS (`unix://` + `NoAuth`), once over TLS TCP (`https://` + a `BearerToken` server auth and a `WithHeader("Authorization","Bearer ...")` client editor + a self-signed cert whose CA the client trusts via `TLSConfig.RootCAs`). This proves both transports and the auth-header path.

Auth for the TCP variant: generate a self-signed cert in-test (`crypto/tls`, `crypto/x509`); the server uses it via `server.TLS`; the client trusts it via `Config.TLSConfig.RootCAs`; the client presents a bearer token via `WithHeader`; the server validates it with `mamori.BearerToken(...)`. Report if the existing auth scheme constructors make this awkward and adjust.

- [ ] **Step 1: Write the harness** `conformance_test.go` per the design above, with `TestConformanceOverUnixSocket` and `TestConformanceOverTLSTCP` each calling `providertest.Run(t, cfg)` with the appropriate endpoint/options. Add the additional targeted tests the spec's row I lists:
  - `TestSensitiveSurvivesTheHop`: seed a value that is `Sensitive` upstream (mamoritest marks secret-bearing values; if it does not set Sensitive, set it via a binding on a secret field or assert the wire `sensitive` field round-trips by checking `Value.Sensitive` after a client Resolve of a binding the server marks sensitive). If mamoritest cannot mark a value sensitive, drive it through a real `mamori.Load` into a `secret.String` field and assert redaction; report which path you used.
  - `TestErrorsIsReachesSentinelForEveryKind`: for each of the five sentinels, `up.Fail(classifySlot, sentinel)`, resolve through the client, assert `errors.Is`.
  - `TestBatchIssuesOneRequestForMultiBindingStruct`: load a struct with several `mamori://` bindings and assert (via a request-counting wrapper `http.RoundTripper`, or a server-side audit count) that a single `POST /v1/values` was issued rather than N single gets. (If counting server-side is hard, count client-side by injecting a counting transport through `Config.HTTPClient`.)
  - `TestWatchReconnectsOnServerRestartMidWatch`: start a watch, `srv.Close()`, start a fresh server on the SAME socket path/addr, mutate, assert the client's watch channel eventually delivers the new value (reconnect + resubscribe). Bound with a timeout; keep it UDS to reuse the path deterministically.
- [ ] **Step 2: Run, verify fail** (harness not yet wired / server not imported): `GOWORK=off go test ./... 2>&1 | head`.
- [ ] **Step 3: Wire go.mod** test deps (`server`, `mamoritest`, `providertest`) with their `replace` directives; `GOWORK=off go mod tidy`.
- [ ] **Step 4: Iterate to green.** This is the integration surface; expect to tune the poll/synchronization. Run `GOWORK=off go test -race -count=2 ./...` until stable, `go vet`, `gofmt -l .`, then repo-root `make test`.
- [ ] **Step 5: Stage** `git add providers/mamori/`.

---

### Task 6: Documentation

**Files:**
- Create: `site/src/pages/docs/providers/mamori.md`, `providers/mamori/README.md`
- Modify: `site/src/layouts/DocsLayout.astro` (nav), `site/src/pages/docs/providers/index.md` (provider table)

- [ ] **Step 1: Write `site/src/pages/docs/providers/mamori.md`** with the standard frontmatter (`layout: ../../../layouts/DocsLayout.astro`, `title: mamori`). Document: what the `mamori://` client is (a provider that resolves binding NAMES from a config server, not upstream refs, the client half of D9); the `Config`/`New` usage with all three endpoint forms (`unix://`, `https://`, and `http://` only with `InsecureNoTLS`); client credential attachment (`WithHeader`/`WithRequestEditor` and mTLS via `TLSConfig`); that it is a NATIVE watch (SSE, with automatic reconnect/resubscribe and backoff); classification passthrough (`errors.Is(err, mamori.ErrPermissionDenied)` holds against the real backend through the hop, and `Doctor` reports the real upstream kind); that `Value.Sensitive` survives the hop and keeps redacting; and the batch behavior (one request for a multi-binding struct). Cross-reference the config-server page (`../server`). Match the depth/tone of an existing provider page such as `providers/doppler.md`.
- [ ] **Step 2: Add the nav entry** to `site/src/layouts/DocsLayout.astro`: `{ slug: "providers/mamori", title: "mamori (client)", indent: true }` in the Providers group, placed logically (e.g. right after `providers/exec` in the core-built-in cluster, or at the end of the built-ins; pick the clearer spot and report).
- [ ] **Step 3: Add the provider-table row** in `site/src/pages/docs/providers/index.md`: `mamori://` as a NATIVE watch with error classification. If the table has an error-classification column, mark it classified; if a watch-type column, mark native.
- [ ] **Step 4: Write `providers/mamori/README.md`**: one-paragraph description, a minimal usage example, the endpoint forms, and a pointer to the site docs and the config-server page.
- [ ] **Step 5: Build the site** to prove it compiles: `make site-build` from repo root (the site needs Node >= 22.12; if the default `node` is older, use the project's configured Node via nvm and report). Fix any MD/Astro errors.
- [ ] **Step 6: Stage** `git add site/ providers/mamori/README.md`.

---

## Self-Review

**Spec coverage.** Implements spec section 14 in full: the provider module (Task 1), `Provider` via GET (Task 2), `BatchProvider` via POST (Task 3), `WatchableProvider` via SSE with reconnect (Task 4), the unusually-strong conformance against a real in-process server over both transports (Task 5), and the docs including the native-watch and classification-passthrough claims (Task 6). Endpoint forms, the `http://`-refusal, classification passthrough, and `Value.Sensitive`-survives-the-hop are each covered and tested (spec testing-strategy row I).

**Placeholders.** None. Each task names its files, the exact wire shapes, the classification map, and the conformance harness verbatim, including the three real hazards (dynamic keys vs a static binding table, stale-serving vs hard-error, and async watch propagation) and their resolutions.

**Type consistency.** The client implements the core `Provider`/`BatchProvider`/`WatchableProvider` interfaces unchanged; the wire structs mirror `server/wire.go`'s `valueBody`/`errorEnvelope`/`batchRequest`/`batchResponse` field-for-field; the `kind` strings use `mamori.Kind` constants and reconstruct the exact sentinels via `wireKindSentinel`, the mirror of the server's `kindStatus`.

**Risk noted.** Task 5 is the integration crux and the most likely to need iteration: it depends on mamoritest's `Fail`/`Set` publishing to the active Watch stream (verified: `mamoritest.Fail` and `Set` both `publish` to subscriptions), on the server's stale-vs-hard-error rule (a never-resolved binding returns a hard error, which the classify harness relies on by never seeding the classify slot), and on synchronizing over the server's ~200ms SSE poll and watch fan-out (the harness polls the client after every Seed/Mutate/Fail/Clear). The one genuine per-task judgment call is Task 3's choice to fail the whole batch on a hard per-name error (rather than silently drop it, which would substitute a default for an unreadable secret); it is flagged for the implementer to confirm and report. Execute strictly serially; never run parallel edit agents on this tree.
