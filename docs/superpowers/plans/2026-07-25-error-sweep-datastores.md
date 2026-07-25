# Error Mapping Sweep, Part 3: Datastore Providers

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend error classification to the six datastore providers whose error vocabularies are genuinely distinct from anything mapped so far, so a Postgres `42501` or a Redis `NOAUTH` reports the right kind instead of `unknown`.

**Architecture:** Unlike the cloud tier, these six do not share an SDK error shape, so each gets its own `classify*` with its own mapping. Two lean on a proven structural model (`etcd` is gRPC `status.Code` like `classifyGCP`; `consul` is HTTP status like `classifyAzure`); the other four are new (`postgres` SQLSTATE strings, `mysql` error numbers, `redis` typed prefix helpers, `mongodb` numeric command codes). The function SHAPE stays identical across all six and matches the fifteen already done: nil in / nil out, unrecognized passes through unchanged, `fmt.Errorf("%w: %w", sentinel, err)` on a match.

**Tech Stack:** Go 1.26. `pgx/v5/pgconn`, `go-sql-driver/mysql`, `redis/go-redis/v9`, `mongo-driver/mongo`, `etcd/client/v3` with `grpc/status`, `hashicorp/consul/api`.

Parts 1 and 2 (complete) covered the four core built-ins plus `aws`, `gcp`, `azure`, `vault`, `k8s`, `s3`, `gcs`, `azblob`, `cosmos`, `dynamodb`, `firestore`. Fifteen of thirty-five providers classify today.

## Scope

**In scope, six modules:** `postgres`, `mysql`, `mongodb`, `redis`, `etcd`, `consul`.

**Still deferred after this plan, fourteen modules:** `sqlite` and `sops` (no map-fake; they test against real files, so error injection needs a driver wrapper or permission bits, designed deliberately in the final plan), and the twelve flag/SaaS modules `doppler`, `onepassword`, `launchdarkly`, `unleash`, `flagsmith`, `configcat`, `split`, `growthbook`, `flipt`, `goff`, `firebase-rc`, `firebase-rtdb`. Several of those have no error vocabulary beyond not-found and will need a judgment call about whether there is anything honest to classify.

**Also deferred:** flipping `providertest.Config.Fail` to required. Final task of the last sweep plan, once all 35 providers supply it.

## Global Constraints

- **Do not run `git commit`.** Stage with `git add` and report the suggested message.
- **Work on `main`.** No branches, no worktrees.
- **`GOWORK=off` for every Go command,** from inside the module directory. `make test` and `make site-build` run from the repo root. `make site-build` needs Node 22 (`nvm use 22`).
- **The tree stays green after every task.** `make test` must pass at every stopping point.
- **No em-dash characters** anywhere.
- **The wrapping pattern is `fmt.Errorf("%w: %w", sentinel, err)`.** Both operands `%w`. This applies wherever an SDK error exists to preserve. A not-found path that is derived purely from an empty result (etcd, consul, mongo's `ErrNoDocuments` sentinel) has no distinct SDK error and correctly uses a single `%w`; say so in your report rather than inventing a double-wrap.
- **`ErrNotFound` behavior must not change.** It is the only kind that triggers `default:` and `optional` handling. Preserve each module's existing not-found detection exactly.
- **Never map a condition to `ErrNotFound` unless the referenced value is genuinely absent.** In an earlier plan, mapping "binary not on PATH" to `ErrNotFound` made a missing binary silently trigger `default:`.
- **Never guess a code.** Earlier plans of mine shipped multiple codes that were fabricated or borrowed from a sibling service, caught only in review. Every code you add must be one you can point at in that database's documentation or its Go driver's source. The mapping tables below were built from reading the vendored driver source; still verify each before shipping it.
- **A passing `ErrorClassification` conformance case proves nothing about your mapping.** It injects a mamori sentinel, not an SDK error, so your classifier returns it unchanged and the case passes even with the whole switch deleted. Two other tests carry the weight, both mandatory: a table test over real SDK error values, and a `Resolve`-level test that injects a real SDK error through the fake and asserts the kind. `providers/gcp/gcp_test.go`'s `TestResolveClassifiesNonNotFoundError` is the reference.
- **`context.DeadlineExceeded` is classified centrally** by `ErrorKind` as `KindUnavailable`; `context.Canceled` stays `KindUnknown`. Do not map either per-provider.

## A fact that applies to all six

For every one of these providers, the SDK client is constructed lazily and does no network I/O, so **authentication and connection errors genuinely reach `Resolve`** at query time rather than failing at construction. That is what makes classifying `unauthenticated` and `unavailable` at the resolve boundary meaningful here. This was verified per module during research.

## Per-task shape

Same seven steps as the earlier sweep plans, written in full for Task 1, deltas only thereafter:

1. Table test over real SDK error values (`errors_test.go`); run it; watch it fail with `undefined`.
2. Implement `classify<X>` in the provider file.
3. Route the existing non-not-found error path through it; keep the not-found branch first.
4. Add `fails map[string]error` plus `fail`/`clear` to the fake, consulted inside the existing lock in the serving method.
5. Wire `Fail`/`Clear` into `providertest.Run`; confirm `ErrorClassification` RUNS not SKIPs.
6. Add the `Resolve`-level test; prove it non-vacuous by removing the `classify` call and watching it fail.
7. Module README table + docs-site page. **Do NOT touch the two shared coverage tables** (root `README.md`, `site/src/pages/docs/providers/index.md`); Task 7 owns those to keep parallel work conflict-free.

---

### Task 1: `providers/postgres`

**Files:** `providers/postgres/postgres.go` (`mapScanErr`, lines 351-358); `providers/postgres/errors_test.go` (new); `providers/postgres/postgres_test.go`; `providers/postgres/README.md`; `site/src/pages/docs/providers/postgres.md`.

**Produces:** `classifyPostgres(err error) error`.

**Detection:** `var pgErr *pgconn.PgError; errors.As(err, &pgErr)`, then switch on `pgErr.Code` (the SQLSTATE string). `github.com/jackc/pgx/v5/pgconn` is part of the already-required `pgx/v5` module; importing it needs no go.mod change, but run `go mod tidy` and confirm.

**Existing state:** `mapScanErr` detects `pgx.ErrNoRows` and produces `ErrNotFound`. Keep that branch first, unchanged. The `pgx.ErrNoRows` not-found path has no `*pgconn.PgError` to preserve, so keep its single `%w`.

**Mapping table** (SQLSTATE strings, all from the Postgres error-code appendix):

| SQLSTATE | Meaning | Sentinel |
|---|---|---|
| `42501` | insufficient_privilege | `ErrPermissionDenied` |
| `28P01` | invalid_password | `ErrUnauthenticated` |
| `28000` | invalid_authorization_specification | `ErrUnauthenticated` |
| `53300` | too_many_connections | `ErrUnavailable` |
| `57P03` | cannot_connect_now | `ErrUnavailable` |
| `08006` | connection_failure | `ErrUnavailable` |
| `08001` | sqlclient_unable_to_establish_sqlconnection | `ErrUnavailable` |
| `08004` | sqlserver_rejected_establishment_of_sqlconnection | `ErrUnavailable` |
| anything else | | unmapped, `unknown` |

Postgres has no rate-limit concept, so there is no `ErrRateLimited` row. Syntax errors (`42601`) are unreachable because the query is a fixed template with a bound `$1`, so no `ErrInvalid` row. Do not add either on speculation.

**Connection errors that are not `*pgconn.PgError`:** a raw dial failure (connection refused) may surface as a non-`PgError`. Do not try to catch it by type-guessing; leave it `unknown`. The SQLSTATE `08xxx` family covers the cases Postgres itself reports.

- [ ] **Step 1: Table test.** Create `providers/postgres/errors_test.go`. Cover every SQLSTATE in the table (construct with `&pgconn.PgError{Code: "42501"}`), an unmapped code (`"XX000"`) reporting `KindUnknown`, a plain error reporting `KindUnknown`, `classifyPostgres(nil)` returning nil, and a preservation test asserting a classified error still satisfies `errors.As` to `*pgconn.PgError` with `Code` recoverable. Model structure on `providers/aws/errors_test.go`.

- [ ] **Step 2: Run it.** `cd providers/postgres && GOWORK=off go test ./... -run TestClassifyPostgres -v`. Expect `undefined: classifyPostgres`.

- [ ] **Step 3: Implement `classifyPostgres`** in `postgres.go`, following the fifteen existing classifiers' shape. Doc comment must explain why unmapped SQLSTATEs pass through unclassified, and note that Postgres has no rate-limit class.

- [ ] **Step 4: Route `mapScanErr` through it.** Keep the `pgx.ErrNoRows` branch first and unchanged. Route the fallback (line 357) through `classifyPostgres(err)`.

- [ ] **Step 5: Add `fail`/`clear` to the fake.** `fakeBackend` in `postgres_test.go` line 27 has `rows map[string]fakeRowData` and serves `QueryRow` returning `*fakeRow` or `errRow{err}`. Add `fails map[string]error`, a `fail`/`clear` mutator, and consult the map at the top of `QueryRow`, returning `errRow{err: injected}`. Wire `Fail`/`Clear` into `providertest.Run` (around `postgres_test.go:150`). Leave `postgres_integration_test.go` alone. Confirm the key form matches `Seed`.

- [ ] **Step 6: `Resolve`-level test.** Add `TestResolveClassifiesNonNotFoundError` injecting `&pgconn.PgError{Code: "42501"}` and asserting `KindPermissionDenied`. Prove non-vacuous by temporarily removing the `classifyPostgres` call, watching it fail, restoring, and confirming `git diff postgres.go` is clean.

- [ ] **Step 7: Docs, verify, stage.** README table + `site/src/pages/docs/providers/postgres.md`. Do not touch the coverage tables.

```bash
cd providers/postgres && GOWORK=off go test ./... -v && GOWORK=off go test -race ./... && GOWORK=off go vet ./... && GOWORK=off go mod tidy
make test
git add providers/postgres/ site/src/pages/docs/providers/postgres.md
```

```
feat(postgres): classify SQLSTATE errors

An insufficient-privilege 42501 now reports permission_denied and a bad
password 28P01 reports unauthenticated, instead of both surfacing as unknown.
Connection-family 08xxx and too-many-connections 53300 report unavailable.
Postgres has no rate-limit class, so none is mapped, and unrecognized
SQLSTATEs stay unknown rather than being guessed at.
```

---

### Task 2: `providers/mysql`

Same seven steps. Deltas:

**Files:** `providers/mysql/mysql.go` (`Resolve` error branch around line 257); `providers/mysql/errors_test.go` (new); `providers/mysql/mysql_test.go`; `providers/mysql/README.md`; `site/src/pages/docs/providers/mysql.md`.

**Produces:** `classifyMySQL(err error) error`.

**Detection:** the module already imports `mysqldriver "github.com/go-sql-driver/mysql"`. Use `var me *mysqldriver.MySQLError; errors.As(err, &me)`, then switch on `me.Number` (a `uint16`). Note `MySQLError.SQLState` is a `[5]byte` array, not a method; switch on `Number`, not SQLState.

**Existing state:** the `sql.ErrNoRows` not-found branch (line 254) stays first and unchanged; it has no `*MySQLError` to preserve, so single `%w`.

**Mapping table** (MySQL server error numbers):

| Number | Meaning | Sentinel |
|---|---|---|
| `1044` | ER_DBACCESS_DENIED_ERROR | `ErrPermissionDenied` |
| `1142` | ER_TABLEACCESS_DENIED_ERROR | `ErrPermissionDenied` |
| `1045` | ER_ACCESS_DENIED_ERROR | `ErrUnauthenticated` |
| `1040` | ER_CON_COUNT_ERROR (too many connections) | `ErrUnavailable` |
| `1203` | ER_TOO_MANY_USER_CONNECTIONS | `ErrUnavailable` |
| anything else | | unmapped, `unknown` |

MySQL has no rate-limit concept, so no `ErrRateLimited` row. Syntax error `1064` is unreachable (fixed query template), so no `ErrInvalid` row.

**A deliberate limitation to state in the README:** a refused TCP connection (client errors 2002 / 2003) is NOT a `*MySQLError`; it is a net-level error, and its exact wrapped type is not stable enough to match reliably, so it reports `unknown`. Do not type-guess it. This means "database is down" reports `unknown` for a hard dial failure while reporting `unavailable` for a connection-limit rejection. Document this honestly rather than papering over it.

**Fake:** `fakeDB` in `mysql_test.go` line 23, `rows map[string]fakeRowData`, serving `QueryRowContext` -> `*fakeRow` whose `Scan` returns `sql.ErrNoRows` or a stored `ctxErr`. Add an `injectedErr`-style keyed mechanism checked in `Scan`. `providertest.Run` is around `mysql_test.go:414` (note this module sets `SkipWatch: true`; keep it).

---

### Task 3: `providers/redis`

Same seven steps. Deltas:

**Files:** `providers/redis/redis.go` (`resolveWith` error branch around line 244); `providers/redis/errors_test.go` (new); `providers/redis/redis_test.go`; `providers/redis/README.md`; `site/src/pages/docs/providers/redis.md`.

**Produces:** `classifyRedis(err error) error`.

**This one uses typed helpers, not a code switch.** go-redis v9 exports predicate functions (confirmed in the vendored `error.go`). Use them in this order:

```go
switch {
case redis.IsAuthError(err):        // NOAUTH, WRONGPASS, "unauthenticated"
    sentinel = mamori.ErrUnauthenticated
case redis.IsPermissionError(err):  // NOPERM (ACL)
    sentinel = mamori.ErrPermissionDenied
case redis.IsLoadingError(err), redis.IsClusterDownError(err), redis.IsMasterDownError(err):
    sentinel = mamori.ErrUnavailable
default:
    return err
}
```

**Existing state:** the `goredis.Nil` not-found branch (line 241) stays first and unchanged; single `%w`. Note the module aliases the import as `goredis`; use whatever alias the file already uses, not `redis`, in your code.

Redis has no rate-limit error surfaced through this path, so no `ErrRateLimited` row. Syntax is not applicable.

**A dial failure** (connection refused) does not satisfy any `redis.Is*Error` predicate, because it is not a `redis.Error`. You MAY add a final check mapping a `net.Error` (or `errors.Is(err, syscall.ECONNREFUSED)`) to `ErrUnavailable`, since a dial failure genuinely means the backend is unreachable and that detection is stable, unlike MySQL's. If you do, cover it with a table-test case using a real `&net.OpError{...}`; if you are not confident the detection is robust, leave it `unknown` and say so. This is a judgment call; report which you chose and why.

**Fake:** `fakeRedis` in `redis_test.go` line 18, `data map[string]string`, serving `Get`. Add `fails map[string]error` checked at the top of `Get`. `providertest.Run` is around `redis_test.go:182`.

**Note for the table test:** construct real go-redis errors. A `NOAUTH`/`NOPERM` error is `proto.RedisError("NOAUTH Authentication required.")` from `github.com/redis/go-redis/v9/internal/proto`, or more simply whatever the vendored `IsAuthError` matches; read `error.go` to see exactly what string form the predicate accepts and construct a matching `redis.Error`. Verify each predicate fires on your constructed value before trusting the table.

---

### Task 4: `providers/mongodb`

Same seven steps. Deltas:

**Files:** `providers/mongodb/mongodb.go` (`FindDoc` error branch around line 379); `providers/mongodb/errors_test.go` (new); `providers/mongodb/mongodb_test.go`; `providers/mongodb/README.md`; `site/src/pages/docs/providers/mongodb.md`.

**Produces:** `classifyMongo(err error) error`.

**Detection:** `var ce mongo.CommandError; errors.As(err, &ce)`, then use `ce.HasErrorCode(n)`. MongoDB server error codes are numeric with no named Go constants, so hardcode the numbers with a comment naming each.

**Mapping table** (MongoDB server error codes):

| Code | Name | Sentinel |
|---|---|---|
| `18` | AuthenticationFailed | `ErrUnauthenticated` |
| `13` | Unauthorized | `ErrPermissionDenied` |
| anything else | | unmapped, `unknown` |

**Keep this deliberately small.** Only codes 13 and 18 are rock-solid for a read path and unambiguous. MongoDB has dozens of codes for replica-set and write conditions (`NotWritablePrimary`, `PrimarySteppedDown`, and so on) whose mapping to `unavailable` is arguable and whose reachability on a `FindOne` is not certain. Per the no-guessing rule, leave them unmapped. State in the README that only auth and authorization are classified today and the rest report `unknown`, a deliberate choice pending confirmation of which codes a read genuinely surfaces.

**Existing state:** the `mongo.ErrNoDocuments` not-found branch (line 376) stays first and unchanged; single `%w`. The `WatchDoc` open-failure path at line 390 also wraps a bare err; route it through `classifyMongo` too, since mongo has a native watch and its watch errors deserve the same classification. Read both sites and report which you covered.

**Fake:** `fakeBackend` in `mongodb_test.go` line 24, `colls map[string]map[string]bson.M`, serving `FindDoc`. Add `fails map[string]error`. `providertest.Run` is around `mongodb_test.go:175`. Leave `mongodb_integration_test.go` alone.

---

### Task 5: `providers/etcd`

Same seven steps. Deltas:

**Files:** `providers/etcd/etcd.go` (`Resolve` error branch around line 162, and the `Watch` stream-error path around line 201); `providers/etcd/errors_test.go` (new); `providers/etcd/etcd_test.go`; `providers/etcd/README.md`; `site/src/pages/docs/providers/etcd.md`.

**Produces:** `classifyEtcd(err error) error`.

**Reference:** `providers/gcp/gcp.go`'s `classifyGCP`. etcd is gRPC, so use `status.Code(err)` the same way. `google.golang.org/grpc/status` and `codes` are in the module graph via the etcd client; confirm with `go mod tidy`.

**Mapping table** (gRPC codes):

| Code | Sentinel |
|---|---|
| `codes.PermissionDenied` | `ErrPermissionDenied` |
| `codes.Unauthenticated` | `ErrUnauthenticated` |
| `codes.Unavailable`, `codes.DeadlineExceeded` | `ErrUnavailable` |
| `codes.ResourceExhausted` | `ErrRateLimited` |
| anything else | unmapped, `unknown` |

**THE TRAP, do not miss this.** etcd's bad-username/password error (`rpctypes.ErrGRPCAuthFailed`) is gRPC `codes.InvalidArgument`, NOT `codes.Unauthenticated`. So you must NOT add a `codes.InvalidArgument -> ErrUnauthenticated` row, and you must NOT add `codes.InvalidArgument -> ErrInvalid` either, because for etcd that code is genuinely ambiguous between "bad credentials" and "malformed request". Leave `InvalidArgument` unmapped so it reports `unknown`. This is the honest outcome: mapping it either way would be wrong half the time. Note this explicitly in the README and in the classifier's doc comment. (In practice `ErrGRPCAuthFailed` fires at client construction when username/password are set, which this provider does not currently do, so it rarely reaches `Resolve` anyway, but the classifier must still not misclassify it.)

`status.Code(err)` returns `codes.Unknown` for a plain non-status error, which stays unclassified; do not add a `codes.Unknown` row. `codes.NotFound` is not used by etcd for a missing key (an empty `Kvs` slice is), so the existing local not-found path at lines 226/235 stays exactly as is, single `%w`, and is not derived from any SDK error.

**Two error sites:** `Resolve` at line 162 and `Watch`'s stream error at line 201. Route both through `classifyEtcd`. Report which you covered.

**Fake:** `fakeClient` in `etcd_test.go` line 22, `data map[string]*mvccpb.KeyValue`, serving `Get`. Add `fails map[string]error`. `providertest.Run` around `etcd_test.go:151`. Leave `etcd_integration_test.go` alone.

---

### Task 6: `providers/consul`

Same seven steps. Deltas:

**Files:** `providers/consul/consul.go` (`Resolve` error branch around line 162, `Watch` around line 206); `providers/consul/errors_test.go` (new); `providers/consul/consul_test.go`; `providers/consul/README.md`; `site/src/pages/docs/providers/consul.md`.

**Produces:** `classifyConsul(err error) error`.

**Reference:** `providers/azure/azure.go`'s `classifyAzure`. Consul surfaces non-2xx responses as `*api.StatusError{Code int, Body string}` where `Code` is the raw HTTP status. Detect with `var se api.StatusError; errors.As(err, &se)` (check whether the client returns it by value or pointer; read `api.go` and match).

**Mapping table** (HTTP status):

| Status | Sentinel |
|---|---|
| 403 | `ErrPermissionDenied` |
| 401 | `ErrUnauthenticated` |
| 429 | `ErrRateLimited` |
| 5xx (>= 500) | `ErrUnavailable` |
| 400 | `ErrInvalid` |
| anything else | unmapped, `unknown` |

These are standard HTTP semantics applied to a real HTTP status code, which is defensible even where Consul's KV endpoint may not commonly emit a given status. But say in the README that 403 (ACL denial) is the confirmed common case, and that 401 and 429 are mapped on HTTP-standard grounds and may or may not be emitted by the KV endpoint depending on Consul configuration. Do not overstate.

**A critical existing-state fact: for Consul, not-found is NOT an error.** A missing key returns `(nil pair, meta, nil error)`, and the provider produces `ErrNotFound` from `pair == nil` at `consul.go:253-254`, with no SDK error involved. Do NOT route that path through the classifier, and do not touch it. `classifyConsul` only ever sees the bare error at line 162, which is never a not-found. This means there is no not-found double-wrap question for this module; state that in your report.

**Two error sites:** `Resolve` at line 162 and `Watch` at line 206. Route both. Report which you covered.

**Fake:** `fakeKV` in `consul_test.go` line 20, `data map[string]*api.KVPair`, serving `Get`. Add `fails map[string]error`. `providertest.Run` around `consul_test.go:120`. Leave `consul_integration_test.go` alone.

---

### Task 7: Coverage tables and cross-check

**Files:** root `README.md`, `site/src/pages/docs/providers/index.md`, and any page touched by Tasks 1 to 6.

- [ ] **Step 1: Update both coverage tables.** After this plan, twenty-one providers classify beyond not-found: the four core built-ins, the eleven cloud-tier modules from parts 1 and 2, plus the six here (`postgres`, `mysql`, `mongodb`, `redis`, `etcd`, `consul`). Fourteen do not: `sqlite`, `sops`, `doppler`, `onepassword`, `launchdarkly`, `unleash`, `flagsmith`, `configcat`, `split`, `growthbook`, `flipt`, `goff`, `firebase-rc`, `firebase-rtdb`. Mark exactly the six new ones in both tables. Update any prose count, and phrase it the way part 2's fix did (count providers and module rows unambiguously, since core is one row for four providers). Keep the "sweep in progress" framing.

- [ ] **Step 2: Cross-check every table against the code.** For each of the six, open its `classify*` and confirm every README and site-page row corresponds to a real branch, with no branch missing. Report per module. This is where part 2 found three READMEs promising reachability the code did not deliver, so take it seriously. Confirm in particular that etcd's README documents the `InvalidArgument`-stays-unknown decision and consul's documents that not-found is not an error.

- [ ] **Step 3: Verify and stage.**

```bash
make test && make vet && make site-build
gofmt -l $(find . -name '*.go' -not -path './**/testdata/*' -not -path './site/*')
git add README.md site/src/pages/docs/providers/
```

```
docs: mark datastore providers as classifying errors

Six more providers classify beyond not-found: postgres, mysql, mongodb,
redis, etcd, and consul. Twenty-one of thirty-five are now covered; the
coverage tables and prose continue to state that the rest report unknown
while the sweep continues.
```

---

## Self-Review

**Spec coverage.** Third slice of spec section 6.2's per-provider mapping, for six of the fourteen modules parts 1 and 2 deferred. The Scope section names all fourteen still remaining, so completeness is not overstated.

**Placeholders.** None. Each task names its exact files, its `classify` function, its full mapping table built from the vendored driver source, its fake type and serving method, and its `providertest.Run` line. Where a mapping involves a genuine judgment call, the task states it and tells the implementer to report their choice rather than guess: MySQL's un-typeable dial failure (Task 2), Redis's optional net-error branch (Task 3), MongoDB's deliberately minimal code set (Task 4), etcd's `InvalidArgument` trap (Task 5), and Consul's not-found-is-not-an-error and defensively-mapped 401/429 (Task 6).

**Type consistency.** Every task produces `classify<Module>(err error) error` with identical semantics and the `%w: %w` match wrap. `fail`/`clear` are uniform across all six fakes and map onto `providertest.Config.Fail`/`Clear` identically.

**Risk noted.** These six vocabularies are all distinct, so unlike part 2 none is a copy of a proven table; each mapping was built from reading driver source and must be verified, not trusted. The two highest-risk traps are etcd's `InvalidArgument`-is-not-unauthenticated (a naive gRPC switch gets it wrong) and Consul's not-found-is-not-an-error (touching that path would break `default:`/`optional`). Both are called out at the point of use. MySQL and Redis both have a connection-failure case that cannot be reliably typed, and the honest outcome there is `unknown`, documented rather than guessed.
