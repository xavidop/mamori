# Error Mapping Sweep, Part 2: Object Stores and Cloud-Family Datastores

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend error classification to the six provider modules whose SDKs share an error vocabulary already mapped in part 1, so an S3 `AccessDenied` or a Cosmos 403 reports `permission_denied` instead of `unknown`.

**Architecture:** Each of these six uses an SDK whose error shape is already handled by a proven classifier in the repo. `s3` and `dynamodb` use `smithy.APIError` like `providers/aws`; `azblob` and `cosmos` use `*azcore.ResponseError` like `providers/azure`; `firestore` uses gRPC `status.Code` like `providers/gcp`; `gcs` uses a sentinel plus `*googleapi.Error`. Each module gets its own `classify*` function, because Go modules cannot share SDK-specific code through core (core is restricted to stdlib plus five dependencies).

**Tech Stack:** Go 1.26. `smithy-go`, `azcore`, `grpc/status`, `cloud.google.com/go/storage`, `google.golang.org/api/googleapi`.

Part 1 (`2026-07-24-error-sweep-core-and-cloud.md`, complete) covered the four core built-ins plus `aws`, `gcp`, `azure`, `vault`, `k8s`.

## Scope

**In scope, six modules:** `s3`, `gcs`, `azblob`, `cosmos`, `dynamodb`, `firestore`.

**Still deferred after this plan, twenty modules:**

| Group | Modules | Why separate |
|---|---|---|
| Datastores | postgres, mysql, mongodb, redis, etcd, consul | Genuinely distinct vocabularies: SQLSTATE codes, MySQL error numbers, Mongo command codes, Redis string prefixes, etcd `rpctypes` sentinels, Consul `api.StatusError`. Each needs its own research; none can copy an existing classifier. |
| Flags and SaaS | doppler, onepassword, launchdarkly, unleash, flagsmith, configcat, split, growthbook, flipt, goff | Several have **no error vocabulary beyond not-found**. `unleash` probes existence via a membership check, `configcat` via `GetAllKeys`, `split` via a literal `"control"` string. Forcing mappings here would violate the no-guessing rule; each needs a judgment call about whether there is anything honest to classify. |
| No-fake modules | sqlite, sops | Neither has a map-fake. Both test against real files, so error injection needs a different mechanism (permission bits, a wrapping driver). Design deliberately, not by analogy. |

**Also still deferred:** flipping `providertest.Config.Fail` to required. That is the final task of the last sweep plan, once all 35 providers supply it.

## Global Constraints

- **Do not run `git commit`.** The repo owner handles all git. Stage with `git add` and report the suggested message.
- **Work on `main`.** No branches, no worktrees.
- **`GOWORK=off` for every Go command,** run from inside the module directory. `make test` runs from the repo root.
- **The tree stays green after every task.** `make test` must pass at every stopping point.
- **No em-dash characters** in code, comments, docs, or commit messages.
- **The wrapping pattern is `fmt.Errorf("%w: %w", sentinel, err)`.** Both operands `%w`, so `errors.Is` reaches the sentinel AND `errors.As` reaches the SDK error. This applies to the not-found branch too: part 1 initially wrapped only the sentinel there, which made three READMEs' reachability claims false, and it had to be fixed. Do not repeat that.
- **`ErrNotFound` behavior must not change.** It is the only kind that triggers `default:` and `optional` handling. Preserve each module's existing not-found detection exactly; you are adding classifications alongside it.
- **Never guess a mapping.** If an SDK condition does not clearly fit a kind, leave it unmapped so it reports `unknown`. In part 1 I specified four AWS error codes that turned out to be fabricated or borrowed from other services, and they had to be removed before they propagated. Every code you add must be one you can point at in that service's documentation.
- **Never map a condition to `ErrNotFound` unless the referenced value is genuinely absent.** In part 1, mapping "binary not on PATH" to `ErrNotFound` silently made a missing binary trigger `default:`, so a deployment missing its agent came up on a dev fallback instead of failing. `ErrNotFound` changes resolution behavior; every other kind is diagnostic only. When in doubt, do not use it.
- **A passing `ErrorClassification` conformance case does not prove your mapping works.** It injects a mamori sentinel, which is not an SDK error, so your classifier returns it unchanged and the case passes even with your whole switch deleted. Two other tests carry the real weight, and both are mandatory: a table test over real SDK error values, and a `Resolve`-level test that injects a real SDK error through the fake and asserts the kind. `providers/gcp/gcp_test.go`'s `TestResolveClassifiesNonNotFoundError` is the reference for the second.
- **`context.DeadlineExceeded` is classified centrally** by `ErrorKind` as `KindUnavailable`; `context.Canceled` deliberately stays `KindUnknown`. Do not map either per-provider.

## Per-task shape

Every task in this plan has the same seven steps. They are written out in full for Task 1; later tasks state only what differs, and name the exact file, function, and mapping table for that module.

1. Write the table test over real SDK error values (`errors_test.go`), run it, watch it fail with `undefined: classify<X>`.
2. Implement `classify<X>` in the provider file.
3. Route the existing error path through it, keeping the not-found branch first and double-wrapping it.
4. Add `fails map[string]error` plus `fail`/`clear` to the fake, consulted inside the existing lock in every serving method.
5. Wire `Fail`/`Clear` into `providertest.Run`, and confirm `ErrorClassification` RUNS rather than SKIPs.
6. Add the `Resolve`-level test, and prove it non-vacuous by temporarily removing the `classify` call and watching it fail.
7. Add the module README table, mirror it onto the docs-site page, flip the row in both coverage tables, run `make test`, stage.

**Step 7's coverage tables are not optional.** The provider table in the root `README.md` and the one in `site/src/pages/docs/providers/index.md` are hand-maintained and carry the project's entire honesty story about which providers classify errors. `site/src/pages/docs/writing-a-provider.md`'s acceptance checklist now requires updating both.

---

### Task 1: `providers/s3`

**Files:**
- Modify: `providers/s3/s3.go` (`mapError`, around lines 240-255)
- Modify: `providers/s3/go.mod` (promote `github.com/aws/smithy-go` to direct if it is indirect)
- Test: `providers/s3/errors_test.go` (new), `providers/s3/s3_test.go`
- Docs: `providers/s3/README.md`, `site/src/pages/docs/providers/s3.md`, `site/src/pages/docs/providers/index.md`, root `README.md`

**Interfaces:**
- Consumes: `mamori.ErrNotFound`, `ErrPermissionDenied`, `ErrUnauthenticated`, `ErrUnavailable`, `ErrRateLimited`, `ErrInvalid`, `mamori.ErrorKind`.
- Produces: `classifyS3(err error) error`, unexported, used by `mapError`.

**Reference:** `providers/aws/aws.go`'s `classifyAWS` uses the identical `smithy.APIError` mechanism. Read it for the function shape (nil in / nil out, unclassified pass-through, `%w: %w` wrap). Do not copy its code table; S3's error codes differ and are given below.

**Existing state:** `mapError` already detects `*s3types.NoSuchKey` and `*s3types.NoSuchBucket` via `errors.As`, with a `smithy.APIError` code fallback. Preserve that detection exactly.

**Mapping table.** These are real S3 REST error codes; every one is documented in the S3 API reference.

| S3 code | Sentinel |
|---|---|
| `NoSuchKey`, `NoSuchBucket`, `NoSuchVersion` | `ErrNotFound` (already handled; keep) |
| `AccessDenied`, `AllAccessDisabled` | `ErrPermissionDenied` |
| `InvalidAccessKeyId`, `SignatureDoesNotMatch`, `ExpiredToken`, `InvalidToken`, `TokenRefreshRequired` | `ErrUnauthenticated` |
| `SlowDown`, `ServiceUnavailable` | see note below |
| `InternalError` | `ErrUnavailable` |
| `InvalidRequest`, `InvalidArgument`, `MalformedXML` | `ErrInvalid` |
| anything else | unmapped, reports `unknown` |

**Note on `SlowDown` and `ServiceUnavailable`:** S3 returns `SlowDown` (503) for throttling and `ServiceUnavailable` (503) for overload. Map `SlowDown` to `ErrRateLimited`, and `ServiceUnavailable` to `ErrUnavailable`. They share a status code but mean different things, and the code string separates them cleanly.

- [ ] **Step 1: Write the table test**

Create `providers/s3/errors_test.go`. Model its structure on `providers/aws/errors_test.go`, which you should read first. It must cover: every code in the table above, an unmapped code reporting `KindUnknown`, a plain non-API error reporting `KindUnknown`, `classifyS3(nil)` returning nil, and a preservation test asserting that a classified error still satisfies `errors.As` to `smithy.APIError` with the original code recoverable.

Construct test errors with `&smithy.GenericAPIError{Code: "AccessDenied"}`.

- [ ] **Step 2: Run it, confirm it fails**

```bash
cd providers/s3 && GOWORK=off go test ./... -run TestClassifyS3 -v
```

Expected: `undefined: classifyS3`.

- [ ] **Step 3: Implement `classifyS3`**

Add it to `providers/s3/s3.go`, following `classifyAWS`'s shape with the table above. The doc comment must explain why unmapped codes pass through unclassified rather than being guessed at.

- [ ] **Step 4: Route `mapError` through it, and fix the not-found wrap**

Keep the existing `*s3types.NoSuchKey` / `*s3types.NoSuchBucket` detection first. Change its wrap to double-wrap so the SDK error stays reachable:

```go
		return fmt.Errorf("s3: object %q not found: %w: %w", ref.Path, mamori.ErrNotFound, err)
```

Route the fallback through `classifyS3(err)`.

- [ ] **Step 5: Add `fail`/`clear` to the fake and wire the conformance case**

`fakeS3` in `s3_test.go` around line 33 has an `objects map[string]fakeObject` and serves `GetObject`. Add `fails map[string]error`, initialize it in the constructor, add `fail`/`clear` mutators, and consult the map at the top of `GetObject` inside the existing lock.

Wire `Fail`/`Clear` into `providertest.Run` at `s3_test.go:112`. **Confirm the key form matches what `Seed` uses**, or the injection never fires and the conformance case passes while testing nothing.

- [ ] **Step 6: Add the `Resolve`-level test and prove it non-vacuous**

Add `TestResolveClassifiesNonNotFoundError` to `errors_test.go`, injecting `&smithy.GenericAPIError{Code: "AccessDenied"}` through the fake, calling `Resolve`, and asserting `KindPermissionDenied`.

Then temporarily change `mapError`'s fallback to wrap the raw `err`, confirm the test FAILS, restore, and confirm `git diff providers/s3/s3.go` shows only the intended change. Report both results.

- [ ] **Step 7: Docs, verify, stage**

Add `## Error classification` to `providers/s3/README.md` listing every code in your switch grouped by kind, plus a line noting unlisted codes report `unknown`. Mirror it onto `site/src/pages/docs/providers/s3.md`. Flip the `s3` row in the coverage tables in root `README.md` and `site/src/pages/docs/providers/index.md`.

```bash
cd providers/s3 && GOWORK=off go test ./... -v && GOWORK=off go test -race ./... && GOWORK=off go vet ./... && GOWORK=off go mod tidy
make test
make site-build   # needs Node 22; run `nvm use 22` first if the engine check fails
git add providers/s3/ site/src/pages/docs/providers/s3.md site/src/pages/docs/providers/index.md README.md
```

```
feat(s3): classify object storage errors

An AccessDenied now reports permission_denied and expired credentials report
unauthenticated, instead of both surfacing as unknown. SlowDown maps to
rate_limited while ServiceUnavailable maps to unavailable; they share a 503
status but mean different things and the code string separates them.

Unmapped codes stay unknown rather than being guessed at.
```

---

### Task 2: `providers/dynamodb`

Same seven steps as Task 1. What differs:

**Files:** `providers/dynamodb/dynamodb.go` (`mapError`, around lines 296-302); `providers/dynamodb/errors_test.go` (new); `providers/dynamodb/dynamodb_test.go`; `providers/dynamodb/README.md`; `site/src/pages/docs/providers/dynamodb.md`.

**Produces:** `classifyDynamoDB(err error) error`.

**Reference:** `providers/aws/aws.go`'s `classifyAWS`, same `smithy.APIError` mechanism.

**Existing state:** `mapError` detects `*ddbtypes.ResourceNotFoundException`. There is also a separate not-found path at `dynamodb.go:188` for an empty successful `GetItem` (`len(out.Item) == 0`), which is a genuine not-found and must keep working unchanged. Preserve both.

**Mapping table.** All are real DynamoDB codes.

| Code | Sentinel |
|---|---|
| `ResourceNotFoundException` | `ErrNotFound` (already handled; keep, but double-wrap) |
| `AccessDeniedException` | `ErrPermissionDenied` |
| `UnrecognizedClientException`, `ExpiredTokenException`, `InvalidSignatureException` | `ErrUnauthenticated` |
| `ProvisionedThroughputExceededException`, `ThrottlingException`, `RequestLimitExceeded` | `ErrRateLimited` |
| `InternalServerError`, `ServiceUnavailable` | `ErrUnavailable` |
| `ValidationException` | `ErrInvalid` |
| anything else | unmapped, `unknown` |

Note `ProvisionedThroughputExceededException` genuinely belongs here. It was wrongly placed in the AWS Secrets Manager classifier in part 1 and removed from there; DynamoDB is its real home.

**Fake:** `fakeDDB` in `dynamodb_test.go` around line 26 already has a single blanket `errOnGet error` field used once at line 391. **Replace it with a keyed `fails map[string]error`** plus `fail`/`clear`, and update that existing line-391 usage to the new mechanism. A blanket switch cannot support `Clear` per key and would leak into later conformance subtests.

**`providertest.Run`** is at `dynamodb_test.go:120`.

---

### Task 3: `providers/azblob`

Same seven steps. What differs:

**Files:** `providers/azblob/azblob.go` (`Resolve` around lines 185-195, `isNotFound` around 297); `providers/azblob/errors_test.go` (new); `providers/azblob/azblob_test.go`; `providers/azblob/README.md`; `site/src/pages/docs/providers/azblob.md`.

**Produces:** `classifyAzblob(err error) error`.

**Reference:** `providers/azure/azure.go`'s `classifyAzure`, identical `*azcore.ResponseError` status mechanism.

**Existing state and a bug to fix:** `azblob.go:193` returns a **bare unwrapped `err`**, so any non-not-found failure reaches the user with no context at all. This task fixes that, as the equivalent fix in `providers/azure` did. Existing not-found detection uses `bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound)` with an `*azcore.ResponseError` 404 fallback; preserve both, and double-wrap the not-found branch.

**Mapping:** identical to `classifyAzure`'s status switch. 404 not-found, 403 permission_denied, 401 unauthenticated, 429 rate_limited, 5xx unavailable, 400 invalid, anything else unmapped. A plain error that is not an `*azcore.ResponseError` stays `unknown`, not `unavailable`: it could be a client bug as easily as a backend outage.

**Fake:** `fakeStore` in `azblob_test.go` around line 19, `data map[string][]string`, serving `Download`.

**`providertest.Run`** is at `azblob_test.go:259`.

---

### Task 4: `providers/cosmos`

Same seven steps. What differs:

**Files:** `providers/cosmos/cosmos.go` (`Resolve` around lines 215-225, `isNotFound` around 334); `providers/cosmos/errors_test.go` (new); `providers/cosmos/cosmos_test.go`; `providers/cosmos/README.md`; `site/src/pages/docs/providers/cosmos.md`.

**Produces:** `classifyCosmos(err error) error`.

**Reference:** `providers/azure/azure.go`'s `classifyAzure`.

**Mapping:** the same status switch as Task 3, with one Cosmos-specific addition. Cosmos DB returns **429 with a `x-ms-retry-after-ms` header for request-unit throttling**, which is its most common operational failure. It maps to `ErrRateLimited` like any 429; call it out in the README because an operator seeing `rate_limited` on Cosmos should know to look at provisioned RU/s rather than at a rate limiter.

**Existing state:** `sdkReader.ReadItem` at `cosmos.go:324` returns the bare `mamori.ErrNotFound` sentinel by design; that is an internal signal, not an SDK error, so leave it. `Resolve`'s not-found branch at 221 should be double-wrapped only if it has an underlying SDK error to preserve; read it and judge. If there is no SDK error at that point, a single `%w` is correct and you should say so in your report rather than inventing one.

**Fake:** `fakeStore` in `cosmos_test.go` around line 20, `items map[string]*fakeItem`, serving `ReadItem`.

**`providertest.Run`** is at `cosmos_test.go:305`.

---

### Task 5: `providers/firestore`

Same seven steps. What differs:

**Files:** `providers/firestore/firestore.go` (`Resolve` around line 217, `valueFor` around 289); `providers/firestore/errors_test.go` (new); `providers/firestore/firestore_test.go`; `providers/firestore/README.md`; `site/src/pages/docs/providers/firestore.md`.

**Produces:** `classifyFirestore(err error) error`.

**Reference:** `providers/gcp/gcp.go`'s `classifyGCP`, identical gRPC `status.Code` mechanism.

**Mapping:** identical to `classifyGCP`. `NotFound`, `PermissionDenied`, `Unauthenticated`, `Unavailable` and `DeadlineExceeded` to `ErrUnavailable`, `ResourceExhausted` to `ErrRateLimited`, `InvalidArgument` to `ErrInvalid`. Codes with no clear meaning (`Internal`, `Unimplemented`, `Aborted`, `FailedPrecondition`) fall through to `unknown`. `status.Code` returns `codes.Unknown` for a plain non-status error, which must stay unclassified; do not map `codes.Unknown`.

**Two error sites, not one.** `valueFor` at line 289 is used by BOTH `Resolve` and `Watch`, so classifying there covers the watch path as well. Read both call sites and make sure the classification reaches each; note in your report which paths you covered.

**Fake:** `fakeStore` in `firestore_test.go` around line 21, `docs map[string]fakeDoc` plus a `waiters` channel for the native watch, serving `Get` and `Snapshots`.

**`providertest.Run`** is at `firestore_test.go:140`.

---

### Task 6: `providers/gcs`

Same seven steps. What differs, and this one needs the most care:

**Files:** `providers/gcs/gcs.go` (`Resolve` around lines 170-176); `providers/gcs/errors_test.go` (new); `providers/gcs/gcs_test.go`; `providers/gcs/README.md`; `site/src/pages/docs/providers/gcs.md`.

**Produces:** `classifyGCS(err error) error`.

**Why this differs from Task 5 despite both being Google:** the GCS Go client is REST-based, not gRPC. It surfaces `storage.ErrObjectNotExist` and `storage.ErrBucketNotExist` as sentinels, and other failures as `*googleapi.Error` carrying an HTTP `Code int`. So this is a status switch like Azure's, not a gRPC code switch like GCP's.

**Check the dependency first.** `google.golang.org/api` may currently be an indirect dependency of this module. If importing `google.golang.org/api/googleapi` promotes it to direct, that is expected and fine; run `go mod tidy` and report the change. If for some reason it is not available, stop and tell me rather than working around it.

**Mapping table:**

| Condition | Sentinel |
|---|---|
| `storage.ErrObjectNotExist`, `storage.ErrBucketNotExist`, or `*googleapi.Error` with `Code == 404` | `ErrNotFound` (existing sentinel detection stays first) |
| `*googleapi.Error` `Code == 403` | `ErrPermissionDenied` |
| `*googleapi.Error` `Code == 401` | `ErrUnauthenticated` |
| `*googleapi.Error` `Code == 429` | `ErrRateLimited` |
| `*googleapi.Error` `Code >= 500` | `ErrUnavailable` |
| `*googleapi.Error` `Code == 400` | `ErrInvalid` |
| anything else, including a plain error | unmapped, `unknown` |

**Existing state:** `gcs.go:172` wraps not-found via `errors.Is(err, storage.ErrObjectNotExist)`. Preserve that first. The bare `ctx.Err()` and `SelectKey` passthroughs at lines 156 and 192 are not SDK errors; leave them alone.

**Fake:** `fakeGCS` in `gcs_test.go` around line 25, `objects map[string]*fakeObject`, serving `read()`.

**`providertest.Run`** is at `gcs_test.go:85`.

---

### Task 7: Coverage tables and cross-check

**Files:** root `README.md`, `site/src/pages/docs/providers/index.md`, and every page touched by Tasks 1 to 6.

- [ ] **Step 1: Confirm the coverage tables are exactly right**

Fifteen providers now classify beyond not-found: the four core built-ins, plus `aws`, `gcp`, `azure`, `vault`, `k8s` from part 1, plus `s3`, `gcs`, `azblob`, `cosmos`, `dynamodb`, `firestore` from this plan.

Twenty do NOT: postgres, mysql, sqlite, mongodb, redis, etcd, consul, doppler, onepassword, sops, launchdarkly, unleash, flagsmith, configcat, split, growthbook, flipt, goff, firebase-rc, firebase-rtdb.

Verify both coverage tables mark exactly those fifteen and no others, and that the surrounding prose still says plainly that the rest report `unknown` and the sweep is in progress. If any task forgot to flip its row, fix it here.

- [ ] **Step 2: Cross-check every table against its code**

For each of the six new modules, open its `classify*` function and confirm every row in both its module README and its site page corresponds to a real branch, and that no branch is missing from the tables. Report the check per module.

This is not busywork: in part 1 the final review found three READMEs promising `errors.As` reachability that the code did not deliver. Tables drift from code silently.

- [ ] **Step 3: Verify and stage**

```bash
make test && make vet && make site-build
gofmt -l $(find . -name '*.go' -not -path './**/testdata/*' -not -path './site/*')
git add README.md site/src/pages/docs/providers/
```

```
docs: mark object stores and cloud datastores as classifying errors

Six more providers classify beyond not-found: s3, gcs, azblob, cosmos,
dynamodb, and firestore. Fifteen of thirty-five are now covered; the coverage
tables and prose continue to state plainly that the rest report unknown while
the sweep continues.
```

---

## Self-Review

**Spec coverage.** Implements the second slice of spec section 6.2's per-provider mapping work, for six of the twenty-six modules that part 1 deferred. The Scope table names all twenty modules still remaining, so the sweep cannot be mistaken for complete.

**Placeholders.** None. Each task names its exact files, its `classify` function name, its full mapping table with real service error codes, its fake type and serving method, and its `providertest.Run` line. Tasks 2 through 6 state only what differs from Task 1's seven steps, which are written out in full; the steps are identical by construction, so repeating them would invite the copy-drift this plan exists to avoid. Where a task needs a judgment call (Task 4's not-found wrap, Task 6's dependency promotion), it says so explicitly and tells the implementer to report rather than guess.

**Type consistency.** Every task produces `classify<Module>(err error) error` with identical semantics: nil in / nil out, unclassified pass-through, `fmt.Errorf("%w: %w", sentinel, err)` on a match. `fail(key string, err error)` and `clear(key string)` are uniform across all six fakes and map onto `providertest.Config.Fail` and `Clear` identically.

**Risk noted.** Task 2's `fakeDDB` already has a blanket `errOnGet` field with one existing usage. Replacing it with a keyed map is the only change in this plan that modifies existing test behavior rather than adding to it, so that task carries the highest chance of breaking a passing test. Task 6 is the only one whose mapping is not already proven elsewhere in the repo.
