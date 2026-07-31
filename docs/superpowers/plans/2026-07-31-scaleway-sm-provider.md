# Scaleway Secret Manager provider implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `providers/scaleway-sm`, a mamori provider reading Scaleway Secret Manager over its REST API with the standard library only.

**Architecture:** One authenticated GET per resolve against the by-path access route. No cache, no `Watch`, and deliberately no `ResolveBatch`, because the API has no bulk endpoint and a loop pretending to be one would claim a saving that does not exist. This is the first provider in its stack that is genuinely secret-bearing, so `Value.Sensitive` is true, and the first whose `Value.Version` is a real backend revision rather than a content hash.

**Tech Stack:** Go 1.26, `net/http`, `encoding/json`, `net/url`, `hash/crc32` from the standard library. `github.com/xavidop/mamori` plus `providertest`. No third-party dependencies.

**Spec:** [2026-07-31-scaleway-secret-manager-provider-design.md](../specs/2026-07-31-scaleway-secret-manager-provider-design.md)

**Reference implementations:** `providers/vercel-gc` and `providers/cloudflare-kv`, both built and reviewed immediately before this one. Read both before starting. Where this plan is silent, follow them.

## The verified API contract

Every detail here was read out of `scaleway-sdk-go`'s own source, not inferred from prose. Do not "correct" it from memory.

```
GET {base}/secret-manager/v1beta1/regions/{region}/secrets-by-path/versions/{revision}/access
    ?secret_path={path}&secret_name={name}&project_id={project}
X-Auth-Token: {secret key}
```

Response:

```json
{"secret_id": "...", "revision": 3, "data": "<base64>", "data_crc32": 123, "type": "opaque"}
```

- `data` is base64 in JSON and unmarshals into a Go `[]byte` field automatically. **Do not add a manual decode step.**
- `revision` is a `uint32`, numbered from 1, incrementing on every write.
- `data_crc32` is a nullable `*uint32`, present only when a CRC was supplied at write time.
- `revision` in the path accepts a number, `latest`, or `latest_enabled`.
- Regions are `fr-par`, `nl-ams`, `pl-waw`.

## Global Constraints

- **Module path:** `github.com/xavidop/mamori/providers/scaleway-sm`. **Go package name:** `scalewaysm`.
- **Zero third-party dependencies.** `scaleway-sdk-go` exists; using it is a plan violation.
- **`go.mod` needs `replace github.com/xavidop/mamori => ../..`**.
- **Every module command runs with the workspace disabled:** `GOWORK=off go test ./...` from inside `providers/scaleway-sm`.
- **No `time`-based caching, no TTL, no clock.** An `http.Client` timeout is fine.
- **`Value.Sensitive` is always `true`.** This is a secret manager, unlike the two sibling modules in this stack.
- **`Value.Version` is the response's `revision` as a decimal string, never a content hash.** This is the point of the module.
- **`Value.Metadata` carries the region and revision only.** Never the secret id, project id, path, or value. A secret's location is itself information, and `Metadata` reaches the admin endpoint and the status report.
- **No credential, project ID, or resolved value may reach an error message or a log line.** Both sibling modules shipped a leak of this exact kind: `url.Parse` and `http.Client.Do` both return `*url.Error` whose message embeds the whole request URL, and this module's URL carries the project ID in its query string. Copy `sanitizeTransportError` from `providers/cloudflare-kv/resolve.go` and route **every** transport-error site through it. Error bodies are read through `io.LimitReader(resp.Body, 4096)` for diagnostics only.
- **Never use an em-dash character in any file.**
- **Must pass `GOWORK=off golangci-lint run --timeout=5m` with zero issues.** `unused` is on: every fake helper needs a caller.
- **Commit at the end of every task on the `xavier/scaleway-sm-provider` branch only.** Never `push`, `merge`, or `rebase`, never touch `main`. Conventional Commits ending with:

```
Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
```

Committing is explicitly authorized for this branch. Three implementers on this project declined to commit citing a general instruction against it; that instruction is overridden here.

---

### Task 1: Module scaffold, configuration, and ref parsing

**Files:**
- Create: `providers/scaleway-sm/go.mod`, `providers/scaleway-sm/scalewaysm.go`, `providers/scaleway-sm/scalewaysm_test.go`
- Modify: `go.work` (add `./providers/scaleway-sm` alphabetically, after `./providers/s3` and before `./providers/sops`)

**Interfaces produced:**
- `type Provider struct{...}`, `func New(opts ...Option) *Provider`, `func (p *Provider) Scheme() string`
- `type Option func(*Provider)`: `WithSecretKey`, `WithProjectID`, `WithRegion`, `WithBaseURL`, `WithHTTPClient`
- `type settings struct { secretKey, projectID, region string }`
- `func (p *Provider) settingsFor() (settings, error)`
- `func parseRef(ref mamori.Ref) (path, name, revision string, err error)`

- [ ] **Step 1: Module and workspace**

```
module github.com/xavidop/mamori/providers/scaleway-sm

go 1.26.0

require github.com/xavidop/mamori v0.1.0

replace github.com/xavidop/mamori => ../..
```

- [ ] **Step 2: Write the failing tests**

Cover, in `scalewaysm_test.go`:

**`parseRef`.** The last path segment is the secret name; everything before it is the path, which must be slash-prefixed. Cases:

| Ref | path | name | revision |
| --- | --- | --- | --- |
| `scaleway-sm://db-password` | `/` | `db-password` | `latest_enabled` |
| `scaleway-sm://prod/db-password` | `/prod` | `db-password` | `latest_enabled` |
| `scaleway-sm://a/b/c/secret` | `/a/b/c` | `secret` | `latest_enabled` |
| `scaleway-sm://db?revision=7` | `/` | `db` | `7` |
| `scaleway-sm://db?revision=latest` | `/` | `db` | `latest` |
| `scaleway-sm://db#user` | `/` | `db` | `latest_enabled` (fragment is not part of the name) |
| `scaleway-sm://` | error containing `requires a secret name` |
| `scaleway-sm://prod/` | error containing `requires a secret name` |

The default-revision case is the load-bearing one. Assert `latest_enabled` explicitly in more than one case, because a test that only ever checks an explicit `?revision=` would not catch the default silently becoming `latest`.

**Settings precedence**, for each of the three settings: explicit option beats `SCW_SECRET_KEY` / `SCW_DEFAULT_PROJECT_ID` / `SCW_DEFAULT_REGION`. Region defaults to `fr-par` when neither is set. Missing secret key and missing project ID each produce an error naming both the option and the environment variable.

**Credential safety:** an error produced while the secret key is set must not contain the secret key. Same for the project ID.

- [ ] **Step 3: Run to verify they fail, then implement**

Write the package doc comment in the style of `providers/cloudflare-kv/cloudflarekv.go`: backend, scheme, authentication, why there is no watch, and why there is no batch.

`parseRef` splits on the last slash. Note the deliberate divergence from `providers/cloudflare-kv`, whose entire ref path is one key because Workers KV keys may contain slashes. Scaleway's path is a real slash-delimited directory structure, so splitting is correct here. Say this in the doc comment, because a reader moving between the two modules will otherwise assume one is wrong.

Do **not** add `func init() { mamori.Register(New()) }` yet: `mamori.Register` needs the `Resolve` method that arrives in Task 2. If nothing in this file references the `mamori` package, add a blank import so `go mod tidy` keeps the require non-indirect; if `parseRef` takes a `mamori.Ref` then a normal import already exists and no blank import is needed. Check which applies rather than assuming.

- [ ] **Step 4: Verify and commit**

`GOWORK=off go mod tidy && GOWORK=off go test ./... && GOWORK=off go vet ./... && GOWORK=off golangci-lint run --timeout=5m`, all clean.

```bash
git add providers/scaleway-sm/go.mod providers/scaleway-sm/go.sum providers/scaleway-sm/scalewaysm.go providers/scaleway-sm/scalewaysm_test.go go.work
git commit -m "feat(scaleway-sm): module scaffold, configuration and ref parsing

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Resolve, value mapping, CRC verification, error classification, registration

**Files:**
- Create: `providers/scaleway-sm/resolve.go`, `providers/scaleway-sm/fake_test.go`, `providers/scaleway-sm/resolve_test.go`, `providers/scaleway-sm/errors_test.go`
- Modify: `providers/scaleway-sm/scalewaysm.go` (restore registration)

**Interfaces produced:**
- `func init() { mamori.Register(New()) }`
- `func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error)`
- `func classifyStatus(code int, statusErr error) error`
- `func sanitizeTransportError(err error) error`
- `func valueFor(resp accessResponse, ref mamori.Ref, region string) (mamori.Value, error)`

- [ ] **Step 0: Restore registration**

Add `func init() { mamori.Register(New()) }` and delete the deferral comment. Pin it with a test asserting the scheme reaches `mamori.RegisteredSchemes()`. Without it, `import _ ".../providers/scaleway-sm"` never wires the scheme up.

- [ ] **Step 1: The fake**

`providers/scaleway-sm/fake_test.go`, modelled on `providers/cloudflare-kv/fake_test.go`. It serves the access route, keyed by `(path, name, revision)`, and must:

- Support multiple revisions per secret, and per-revision enabled/disabled state, so the `latest_enabled` behavior can be tested for real rather than assumed.
- Return the documented envelope, with `data` base64-encoded and `data_crc32` optional.
- Record the exact request path and query, so tests can assert what went over the wire.
- Support failure injection by status, with the sentinel-honoring shape Task 3's conformance `Fail` hook needs.
- Be driven by an **in-process `RoundTripper`, never `httptest.NewServer`**. `providertest`'s `NoGoroutineLeak` runs `goleak.VerifyNone` with no ignore options. Copy the `roundTripper` from `providers/cloudflare-kv/fake_test.go` **including its `r.Context().Err()` check**, without which a context-cancellation test passes against code that never threads `ctx`.

- [ ] **Step 2: Write the failing tests**

`resolve_test.go` must cover:

- A secret resolves to its decoded payload, and the request URL carries `secret_path`, `secret_name`, and `project_id` as query parameters with the right values.
- The `X-Auth-Token` header is the secret key, and the key appears in no URL.
- **base64 fidelity:** a payload containing non-UTF8 bytes round-trips byte for byte.
- **`Value.Version` is the revision, not a hash.** The load-bearing test: two responses with **identical bytes** at revisions 3 and 4 must produce **different** `Version` values. A content-hash implementation cannot pass this, which is the whole point.
- **`Value.Sensitive` is true**, and a `secret.String` field wrapping it redacts under `fmt`.
- **`Metadata` contains region and revision, and contains none of** the secret id, the project id, the path, or the value.
- **CRC:** a matching `data_crc32` resolves; a mismatched one returns an error satisfying `mamori.ErrInvalid`; an absent one is not an error.
- **`latest_enabled` semantics:** with revision 4 disabled and 3 enabled, the default ref resolves revision 3, while `?revision=latest` resolves 4. Assert the revision reaching the URL and the `Version` returned.
- `#field` and `#/a/b` selection.
- An unknown secret returns `mamori.ErrNotFound`.
- A cancelled context returns `context.Canceled`.
- **Credential safety at every site:** removing `sanitizeTransportError` from any one call site must fail a test. Write one test per site, asserting the absence of **both** the secret key and the project id.

`errors_test.go` follows `providers/cloudflare-kv/errors_test.go`: an unmapped status (418) yields `mamori.KindUnknown`, `classifyStatus(code, nil)` returns nil, and the wrapped chain survives.

- [ ] **Step 3: Implement**

`Resolve` parses the ref, resolves settings, builds the URL, GETs, and maps the response.

The response type:

```go
// accessResponse mirrors the JSON returned by the by-path access route.
// Data is []byte deliberately: encoding/json base64-decodes into it for free,
// so there is no manual decode step and none should be added.
type accessResponse struct {
	SecretID  string  `json:"secret_id"`
	Revision  uint32  `json:"revision"`
	Data      []byte  `json:"data"`
	DataCrc32 *uint32 `json:"data_crc32"`
	Type      string  `json:"type"`
}
```

`valueFor` verifies the CRC when `DataCrc32` is non-nil, applies `mamori.SelectKey` when `ref.Key != ""`, and returns:

```go
mamori.Value{
	Bytes:     b,
	Version:   strconv.FormatUint(uint64(resp.Revision), 10),
	Sensitive: true,
	Metadata: map[string]string{
		"region":   region,
		"revision": strconv.FormatUint(uint64(resp.Revision), 10),
	},
}
```

Note `Version` is the revision even when a `#field` selection narrowed the bytes. That is correct and deliberate: the revision identifies the secret version the bytes came from, which is exactly the change-detection signal mamori wants, and it stays stable when an unrelated field of the same JSON secret changes only if the revision did not move. Say this in the doc comment.

`classifyStatus` follows `providers/cloudflare-kv/resolve.go`'s function of the same name, including its closing "codes not listed report unknown rather than being guessed at". Handle 404 before classification. Its doc comment must carry the caveat that a 404 does not distinguish an unknown secret from a known secret whose requested revision does not exist, so `?revision=99` degrades silently to the field's default.

Drain the body with `io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))` before returning on the 404 branch, so the connection can be reused.

- [ ] **Step 4: Verify and commit**

Module tests, `-race`, `go vet`, `golangci-lint`, all clean.

```bash
git add providers/scaleway-sm/resolve.go providers/scaleway-sm/fake_test.go providers/scaleway-sm/resolve_test.go providers/scaleway-sm/errors_test.go providers/scaleway-sm/scalewaysm.go
git commit -m "feat(scaleway-sm): Resolve with real revisions, CRC verification and error classification

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Conformance kit and live integration test

**Files:**
- Create: `providers/scaleway-sm/conformance_test.go`, `providers/scaleway-sm/scalewaysm_integration_test.go`

- [ ] **Step 1: Wire `providertest.Run`**

Read `providertest/providertest.go` for the real `Config` semantics. Model on `providers/cloudflare-kv/conformance_test.go`.

- **Do not set `SkipWatch`.** The type genuinely has no `Watch`, so the watch cases must skip on their own.
- **Do not set `NoResolveErrors`.** This provider classifies status and has a failure seam.
- **`Fail` must honor the injected sentinel.** Map each injected `mamori` sentinel back to the HTTP status that produces it, so every case round-trips through the real `classifyStatus`. Hard-coding one status fails 4 of 5 cases; that mistake has already been made twice on this project.
- Wire `PointerRef` so `JSONPointerSelection` runs.
- `Seed` and `Mutate` must genuinely change fake state. Because `Version` is a real revision here, `Mutate` should **bump the revision**, which makes the version-monotonicity case meaningful rather than incidental.

- [ ] **Step 2: Add the not-batchable assertion**

The spec says this provider deliberately does not implement `BatchProvider`, because the API has no bulk endpoint and a loop would claim a saving that does not exist. Pin it:

```go
func TestProviderIsNotBatchable(t *testing.T) {
	if _, ok := any(New()).(mamori.BatchProvider); ok {
		t.Fatal("scaleway-sm must not implement BatchProvider: the API has no bulk access endpoint, so a ResolveBatch would be a loop claiming a round trip saving that does not exist")
	}
}
```

Also assert it does not satisfy `mamori.WatchableProvider`.

- [ ] **Step 3: Run the conformance suite verbosely**

`GOWORK=off go test -run TestConformance -v ./...`. Record which cases ran and which skipped, and why, in your report.

- [ ] **Step 4: Live integration test**

`//go:build integration`, skipping unless `SCW_SECRET_KEY`, `SCW_DEFAULT_PROJECT_ID`, and `SCALEWAY_SM_TEST_SECRET` are set. Resolve the test secret and assert a non-empty value and a numeric `Version`.

It must also **assert the `Version` is the revision**, by resolving `?revision=latest_enabled` and a pinned `?revision=<n>` and checking the returned versions differ appropriately. A fake can only prove the implementation agrees with the fake; this is the one check that the real API returns a revision where we expect one.

Never log a resolved value. Key name and byte count only.

- [ ] **Step 5: Verify and commit**

Module suite, `-race`, `golangci-lint`, then `make test` and `make vet` from the repo root.

```bash
git add providers/scaleway-sm/conformance_test.go providers/scaleway-sm/scalewaysm_integration_test.go providers/scaleway-sm/go.mod providers/scaleway-sm/go.sum
git commit -m "test(scaleway-sm): providertest conformance and live integration test

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

`go.mod` is in the list: `providertest` pulls `go.uber.org/goleak` in as an indirect test dependency, as it does for every other provider module.

---

### Task 4: Documentation

**Files:**
- Create: `providers/scaleway-sm/README.md`, `site/src/pages/docs/providers/scaleway-sm.md`
- Modify: `site/src/layouts/DocsLayout.astro`, `site/src/pages/docs/providers/index.md`, `README.md`, `skills/mamori/references/providers.md`, `.github/dependabot.yml`

- [ ] **Step 1: Read the code, then write the module README**

Follow `providers/vercel-gc/README.md`'s structure. **Every factual claim must be true of the code**; check your prose against `providers/scaleway-sm/*.go` line by line.

State these explicitly:

- The ref grammar: last segment is the name, everything before is the path. **Say why it differs from `providers/cloudflare-kv`**, whose whole path is one key, so a reader moving between them does not assume one is wrong.
- Access by secret ID is **not** supported, and that this is a decision (UUIDs in struct tags are unreadable) rather than a gap.
- **`?revision=` defaults to `latest_enabled`, not `latest`**, and why: disabling a revision is how an operator revokes a leaked credential, so defaulting to `latest` would keep serving a secret that was explicitly disabled. Show the disabled-revision behavior.
- **`Value.Version` is the real backend revision**, not a content hash, and what that buys: correct change detection that does not depend on comparing bytes. This is the first provider in the repo's recent additions that can do this.
- `Value.Sensitive` is **true**. Contrast with the two sibling modules in this stack, which are not secret-bearing.
- `Metadata` carries region and revision only, deliberately excluding the secret id, project id, and path, because a secret's location is itself information and `Metadata` reaches the admin endpoint.
- CRC verification when `data_crc32` is present, and that its absence is normal.
- No native watch, and **no `ResolveBatch`**, with the reason: no bulk endpoint exists, so a batch would be a loop claiming a saving it does not deliver.
- The 404 caveat: an unknown secret and a nonexistent pinned revision are indistinguishable, so `?revision=99` degrades silently to the default.
- Auth via `X-Auth-Token`, and that the environment variable names are Scaleway's own so an existing `scw` or Terraform setup works unchanged.

- [ ] **Step 2: Site page and navigation**

`site/src/pages/docs/providers/scaleway-sm.md` with front matter matching a sibling exactly:

```markdown
---
layout: ../../../layouts/DocsLayout.astro
title: Scaleway Secret Manager
---
```

Add to the **"Secret managers"** group in `site/src/layouts/DocsLayout.astro`, not "KV & config", since this is a secret manager. Place it after `providers/doppler`.

- [ ] **Step 3: The three tables and Dependabot**

The root README table and the site provider matrix have **different column sets**; check each header. Note the site matrix has a "Sensitive" column and this provider is the first of the three in this stack for which it is **yes**.

Site matrix (`Scheme | Page | Sensitive | Watch | Errors`):

```markdown
| `scaleway-sm://` | Scaleway Secret Manager | yes | poll | ✅ |
```

Root README (`Module | Schemes | Watch | Errors classified beyond not-found`):

```markdown
| `providers/scaleway-sm` | `scaleway-sm://` | poll | ✅ |
```

Plus the install line, the `skills/mamori/references/providers.md` row, and if the site matrix carries a prose count of providers or modules, update it and verify by counting.

**Dependabot, which CI enforces.** `.github/scripts/check_dependabot_coverage.py` fails the build without an entry:

```yaml
  - package-ecosystem: gomod
    directory: "/providers/scaleway-sm"
    schedule: { interval: weekly }
    labels: [dependencies, go, provider:scaleway-sm]
```

Place it among the other secret-manager providers. Verify locally with the same script CI runs:

```bash
MODS=$(find . -name go.mod -not -path './site/*' -not -path '*/testdata/*' -exec dirname {} \; | sort \
  | python3 -c "import sys,json; print(json.dumps([l.strip() for l in sys.stdin]))")
python3 .github/scripts/check_dependabot_coverage.py "$MODS"
```

- [ ] **Step 4: Verify and commit**

`make site-linkcheck` from the repo root (needs Node 22; `nvm use 22`), then `make build && make test && make vet`. Revert incidental churn to `go.work.sum` or `site/package-lock.json`.

```bash
git add providers/scaleway-sm/README.md site/src/pages/docs/providers/scaleway-sm.md site/src/layouts/DocsLayout.astro site/src/pages/docs/providers/index.md README.md skills/mamori/references/providers.md .github/dependabot.yml
git commit -m "docs(scaleway-sm): module README, site page, provider matrix and skill reference

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Final verification

- [ ] `make all` passes from the repo root.
- [ ] `cd providers/scaleway-sm && GOWORK=off go test -race ./...` passes.
- [ ] `providers/scaleway-sm/go.mod` has exactly one non-indirect require.
- [ ] No em-dash in the module or its docs.
- [ ] The provider appears in the root README table, the site matrix, the site nav under "Secret managers", the agent-skill reference, and `.github/dependabot.yml`.
- [ ] No test starts a real `httptest.Server`.
- [ ] `Value.Sensitive` is true and `Value.Version` is the revision, both pinned by tests.
