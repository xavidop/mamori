# Cloudflare Workers KV provider implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `providers/cloudflare-kv`, a mamori provider reading Cloudflare Workers KV over its REST API with the standard library only.

**Architecture:** A stateless provider. `Resolve` GETs one key and uses the raw response body as the value. `ResolveBatch` POSTs to the bulk endpoint, chunked at the API's 100-key ceiling and grouped by namespace. No snapshot cache and no `Watch`: Workers KV offers no digest, no ETag, and no streaming read, so there is nothing to gate a cache on and nothing to watch honestly.

**Tech Stack:** Go 1.26, `net/http`, `encoding/json`, `net/url` from the standard library. `github.com/xavidop/mamori` plus `providertest`. No third-party dependencies.

**Spec:** [2026-07-31-cloudflare-kv-provider-design.md](../specs/2026-07-31-cloudflare-kv-provider-design.md)

**Reference implementation:** `providers/vercel-gc` is the sibling this module should echo in structure, doc-comment style, and error wording. Read it before starting. Where this plan is silent, do what vercel-gc does.

## Global Constraints

- **Module path:** `github.com/xavidop/mamori/providers/cloudflare-kv`. **Go package name:** `cloudflarekv` (a Go package identifier cannot contain a hyphen).
- **Zero third-party dependencies.** The only non-indirect require is `github.com/xavidop/mamori`. Cloudflare publishes a Go SDK; using it is a plan violation.
- **`go.mod` needs `replace github.com/xavidop/mamori => ../..`**, like every other provider module.
- **Every module command runs with the workspace disabled:** `GOWORK=off go test ./...` from inside `providers/cloudflare-kv`.
- **No `time`-based caching, no TTL, no clock.** An `http.Client` timeout is fine; a cache expiry is not.
- **`Value.Sensitive` is always `false`.** Workers KV is a general-purpose store, and Cloudflare's own docs note that anyone with namespace read access sees values in plain text.
- **Never let a token, an account ID, or a resolved value reach an error message, a log line, or `Value.Metadata`.** This is a regression pin: the equivalent defect was found in `providers/vercel-gc`'s final review, where `url.Parse`'s `*url.Error` embedded a live token. Error bodies are read through `io.LimitReader(resp.Body, 4096)` for diagnostics only.
- **Never use an em-dash character in any file.** Use a hyphen or restructure.
- **The module must pass `GOWORK=off golangci-lint run --timeout=5m` with zero issues.** The `unused` linter is on: do not add a helper nothing calls.
- **Commit at the end of every task on the `xavier/cloudflare-kv-provider` branch only.** Never `push`, `merge`, or `rebase`, and never touch `main`. Conventional Commits, `feat(cloudflare-kv):` for code and `docs(cloudflare-kv):` for the docs task, each ending with this trailer on its own line:

```
Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
```

---

### Task 1: Module scaffold, configuration, and ref parsing

**Files:**
- Create: `providers/cloudflare-kv/go.mod`, `providers/cloudflare-kv/cloudflarekv.go`, `providers/cloudflare-kv/cloudflarekv_test.go`
- Modify: `go.work` (add `./providers/cloudflare-kv` in alphabetical position, after `./providers/azure` and before `./providers/configcat`)

**Interfaces produced:**
- `type Provider struct{...}`, `func New(opts ...Option) *Provider`, `func (p *Provider) Scheme() string`
- `type Option func(*Provider)`: `WithAPIToken`, `WithAccountID`, `WithNamespaceID`, `WithBaseURL`, `WithHTTPClient`
- `type settings struct { token, account, namespace string }`
- `func (p *Provider) settingsFor(ref mamori.Ref) (settings, error)`
- `func keyOf(ref mamori.Ref) (string, error)`

- [ ] **Step 1: Create the module and register it in the workspace**

`providers/cloudflare-kv/go.mod`:

```
module github.com/xavidop/mamori/providers/cloudflare-kv

go 1.26.0

require github.com/xavidop/mamori v0.1.0

replace github.com/xavidop/mamori => ../..
```

Add `	./providers/cloudflare-kv` to the `use (` block in `go.work`, keeping alphabetical order.

- [ ] **Step 2: Write the failing tests**

Create `providers/cloudflare-kv/cloudflarekv_test.go`. Note the key-with-slash cases: they are the whole reason the namespace is a query option rather than a path segment.

```go
package cloudflarekv

import (
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

func ref(t *testing.T, tag string) mamori.Ref {
	t.Helper()
	r, err := mamori.ParseRef(tag)
	if err != nil {
		t.Fatalf("parsing ref %q: %v", tag, err)
	}
	return r
}

func TestKeyOf(t *testing.T) {
	tests := []struct {
		name    string
		tag     string
		want    string
		wantErr string
	}{
		{name: "simple key", tag: "cloudflare-kv://log-level", want: "log-level"},
		{name: "key containing slashes is one key", tag: "cloudflare-kv://config/prod/log-level", want: "config/prod/log-level"},
		{name: "namespace option is not part of the key", tag: "cloudflare-kv://log-level?namespace=ns123", want: "log-level"},
		{name: "fragment is not part of the key", tag: "cloudflare-kv://api-config#timeout", want: "api-config"},
		{name: "empty key", tag: "cloudflare-kv://", wantErr: "requires a key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := keyOf(ref(t, tc.tag))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got err %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSettingsPrecedence(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "env-token")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "env-account")
	t.Setenv("CLOUDFLARE_KV_NAMESPACE_ID", "env-namespace")

	got, err := New().settingsFor(ref(t, "cloudflare-kv://k"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.token != "env-token" || got.account != "env-account" || got.namespace != "env-namespace" {
		t.Fatalf("environment not read: %+v", got)
	}

	p := New(WithAPIToken("opt-token"), WithAccountID("opt-account"), WithNamespaceID("opt-namespace"))
	got, err = p.settingsFor(ref(t, "cloudflare-kv://k"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.token != "opt-token" || got.account != "opt-account" || got.namespace != "opt-namespace" {
		t.Fatalf("options must win over the environment: %+v", got)
	}
}

func TestSettingsRefNamespaceWins(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "t")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "a")
	t.Setenv("CLOUDFLARE_KV_NAMESPACE_ID", "default-ns")

	got, err := New().settingsFor(ref(t, "cloudflare-kv://k?namespace=ref-ns"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.namespace != "ref-ns" {
		t.Fatalf("got namespace %q, want the ref's %q", got.namespace, "ref-ns")
	}
}

func TestSettingsMissing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "no token", env: map[string]string{"CLOUDFLARE_ACCOUNT_ID": "a", "CLOUDFLARE_KV_NAMESPACE_ID": "n"}, wantErr: "CLOUDFLARE_API_TOKEN"},
		{name: "no account", env: map[string]string{"CLOUDFLARE_API_TOKEN": "t", "CLOUDFLARE_KV_NAMESPACE_ID": "n"}, wantErr: "CLOUDFLARE_ACCOUNT_ID"},
		{name: "no namespace", env: map[string]string{"CLOUDFLARE_API_TOKEN": "t", "CLOUDFLARE_ACCOUNT_ID": "a"}, wantErr: "CLOUDFLARE_KV_NAMESPACE_ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLOUDFLARE_API_TOKEN", "")
			t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
			t.Setenv("CLOUDFLARE_KV_NAMESPACE_ID", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := New().settingsFor(ref(t, "cloudflare-kv://k"))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got err %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

// A credential must never reach an error message. This pins the regression that
// providers/vercel-gc shipped and had to fix.
func TestSettingsErrorsNeverCarryCredentials(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "SUPER_SECRET_TOKEN")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	t.Setenv("CLOUDFLARE_KV_NAMESPACE_ID", "")

	_, err := New().settingsFor(ref(t, "cloudflare-kv://k"))
	if err == nil {
		t.Fatal("want an error when the account id is missing")
	}
	if strings.Contains(err.Error(), "SUPER_SECRET_TOKEN") {
		t.Fatalf("error leaked the API token: %v", err)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd providers/cloudflare-kv && GOWORK=off go test ./...`
Expected: build failure, `undefined: keyOf`, `undefined: New`.

- [ ] **Step 4: Implement**

Create `providers/cloudflare-kv/cloudflarekv.go`. Write the package doc comment in the style of `providers/vercel-gc/vercelgc.go`: what the backend is, the scheme, authentication, and why there is no watch.

The parts that carry decisions, and must be implemented exactly:

```go
const (
	scheme         = "cloudflare-kv"
	defaultBaseURL = "https://api.cloudflare.com/client/v4"
)

// keyOf returns the ref's key. The ENTIRE ref path is the key, deliberately.
//
// Workers KV keys may contain slashes: they are up to 512 bytes of any
// printable, non-whitespace characters, so "config/prod/log-level" is one
// ordinary key name. A segment-count rule like the one providers/vercel-gc
// uses to split "<store>/<key>" would silently misread that common shape as a
// namespace plus a shorter key, so the namespace is selected with the
// ?namespace= query option instead and never taken from the path.
func keyOf(ref mamori.Ref) (string, error) {
	if ref.Path == "" {
		return "", fmt.Errorf("mamori/cloudflare-kv: ref %q requires a key", ref.Raw)
	}
	return ref.Path, nil
}

// settingsFor resolves the credentials and namespace for one ref, reading the
// environment lazily so registering from init is safe with no credentials at
// process start. Precedence is the explicit option, then the environment; the
// namespace additionally lets the ref's ?namespace= override both.
func (p *Provider) settingsFor(ref mamori.Ref) (settings, error) {
	s := settings{
		token:     firstNonEmpty(p.apiToken, os.Getenv("CLOUDFLARE_API_TOKEN")),
		account:   firstNonEmpty(p.accountID, os.Getenv("CLOUDFLARE_ACCOUNT_ID")),
		namespace: firstNonEmpty(ref.Opt("namespace"), p.namespaceID, os.Getenv("CLOUDFLARE_KV_NAMESPACE_ID")),
	}
	// Each message names both the option and the environment variable that
	// would supply the value, and never echoes any credential that IS set.
	switch {
	case s.token == "":
		return settings{}, errors.New("mamori/cloudflare-kv: no API token; set CLOUDFLARE_API_TOKEN or use WithAPIToken")
	case s.account == "":
		return settings{}, errors.New("mamori/cloudflare-kv: no account id; set CLOUDFLARE_ACCOUNT_ID or use WithAccountID")
	case s.namespace == "":
		return settings{}, errors.New("mamori/cloudflare-kv: no namespace id; set CLOUDFLARE_KV_NAMESPACE_ID, use WithNamespaceID, or add ?namespace= to the ref")
	}
	return s, nil
}
```

Add `firstNonEmpty(vals ...string) string` as an unexported helper. Add the five `Option` funcs and `New` following vercel-gc's shape exactly. Do **not** add `func init() { mamori.Register(New()) }` yet: `mamori.Register` takes a `mamori.Provider`, which requires the `Resolve` method that arrives in Task 2. Leave a comment saying registration is deferred to the task that adds `Resolve`, and use a blank import `_ "github.com/xavidop/mamori"` so `go mod tidy` keeps the require non-indirect. This is not optional bookkeeping; it is what the sibling module had to do, and skipping it makes this task fail to compile.

- [ ] **Step 5: Verify and commit**

Run `cd providers/cloudflare-kv && GOWORK=off go mod tidy && GOWORK=off go test ./... && GOWORK=off go vet ./... && GOWORK=off golangci-lint run --timeout=5m`. All must be clean.

```bash
git add providers/cloudflare-kv/go.mod providers/cloudflare-kv/go.sum providers/cloudflare-kv/cloudflarekv.go providers/cloudflare-kv/cloudflarekv_test.go go.work
git commit -m "feat(cloudflare-kv): module scaffold, configuration and ref parsing

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Resolve, error classification, and provider registration

**Files:**
- Create: `providers/cloudflare-kv/resolve.go`, `providers/cloudflare-kv/fake_test.go`, `providers/cloudflare-kv/resolve_test.go`, `providers/cloudflare-kv/errors_test.go`
- Modify: `providers/cloudflare-kv/cloudflarekv.go` (restore registration)

**Interfaces produced:**
- `func init() { mamori.Register(New()) }` restored
- `func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error)`
- `func classifyStatus(code int, statusErr error) error`
- `func valueFor(b []byte, ref mamori.Ref, namespace string) (mamori.Value, error)`
- test helpers `newFake() *fakeKV` with `set`, `del`, `failStatus`, `counts`, `handle`, `provider`, and an in-process `roundTripper`

- [ ] **Step 0: Restore registration**

In `cloudflarekv.go`: change the blank import `_ "github.com/xavidop/mamori"` to a normal import, delete the deferral comments, and add `func init() { mamori.Register(New()) }` after `New`. Without this, `import _ ".../providers/cloudflare-kv"` never wires the scheme up and the documented usage is a lie. Pin it with a test asserting `scheme` appears in `mamori.RegisteredSchemes()`.

- [ ] **Step 1: Write the fake**

Create `providers/cloudflare-kv/fake_test.go`, modelled on `providers/vercel-gc/fake_test.go`. It must serve both endpoints:

- `GET /accounts/{acct}/storage/kv/namespaces/{ns}/values/{key}` returning the **raw stored bytes**, 404 when absent
- `POST /accounts/{acct}/storage/kv/namespaces/{ns}/bulk/get` returning `{"success":true,"result":{"values":{...}},"errors":[],"messages":[]}` with absent keys omitted

**Drive it through an in-process `RoundTripper`, never `httptest.NewServer`.** `providertest`'s `NoGoroutineLeak` case runs `goleak.VerifyNone` with no ignore options, and a live server's accept goroutine can never satisfy it. Copy the `roundTripper` from `providers/vercel-gc/fake_test.go`, including its context check: `http.Client` delegates cancellation to the transport, so a transport that ignores `r.Context().Err()` would let a context-cancellation test pass against code that never threads `ctx`.

The fake must record the **exact escaped path** it received, so Task 2's escaping test can assert what went over the wire, and must count requests per endpoint so Task 3 can assert chunking.

Give every helper a caller in this task or Task 3. The `unused` linter is on, and the sibling module had to delete dead fake helpers in a fix round.

- [ ] **Step 2: Write the failing tests**

Create `providers/cloudflare-kv/resolve_test.go` covering:

- a simple key resolves to its raw bytes
- **a key containing slashes** (`config/prod/log-level`) resolves, and the request path contains the escaped form `config%2Fprod%2Flog-level` rather than extra path segments. This is the load-bearing test of the ref-grammar decision.
- keys containing `:`, `%`, and a space round-trip
- `?namespace=` overrides the configured default, asserted by the namespace in the request path
- an absent key returns an error satisfying `mamori.ErrNotFound`
- `#field` and `#/a/b` selection work through `mamori.SelectKey`
- `Value.Sensitive` is `false`, `Value.Version` equals `mamori.VersionHash` of the bytes, `Metadata["namespace"]` is set and `Metadata` contains neither the value nor the account id
- the `Authorization` header is exactly `Bearer <token>` and the token appears in no URL
- a cancelled context produces `context.Canceled`

Create `providers/cloudflare-kv/errors_test.go` following `providers/vercel-gc/errors_test.go`: an unmapped status (418) yields `mamori.KindUnknown`, `classifyStatus(code, nil)` returns nil, and the wrapped chain survives so the original diagnostic text is reachable.

- [ ] **Step 3: Run the tests to verify they fail, then implement**

`Resolve` builds `{base}/accounts/{account}/storage/kv/namespaces/{namespace}/values/{url.PathEscape(key)}`, sends `Authorization: Bearer <token>`, and uses the **raw response body** as the value. Document in the code that this endpoint returns naked bytes rather than a JSON envelope, unlike the bulk endpoint, because that asymmetry is genuinely surprising.

`valueFor` applies `mamori.SelectKey(b, ref.Key)` when `ref.Key != ""`, then sets `Bytes`, `Version: mamori.VersionHash(...)`, `Sensitive: false`, and `Metadata{"namespace": ns}`. There is no unwrapping step: Workers KV stores opaque bytes, not JSON values.

`classifyStatus` follows `providers/vercel-gc/resolve.go`'s function of the same name exactly, including its structure and its closing "codes not listed report unknown rather than being guessed at". Handle 404 before classification, as the sibling does. Its doc comment must state the honest caveat: a 404 means either an absent key or an absent namespace, and the response does not reliably distinguish them, so a misconfigured namespace presents as every field silently falling back to its default.

- [ ] **Step 4: Verify and commit**

Run the module's tests, `-race`, `go vet`, and `golangci-lint`. All clean.

```bash
git add providers/cloudflare-kv/resolve.go providers/cloudflare-kv/fake_test.go providers/cloudflare-kv/resolve_test.go providers/cloudflare-kv/errors_test.go providers/cloudflare-kv/cloudflarekv.go
git commit -m "feat(cloudflare-kv): Resolve, error classification and registration

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: ResolveBatch with chunking

**Files:**
- Modify: `providers/cloudflare-kv/resolve.go`
- Create: `providers/cloudflare-kv/batch_test.go`

**Interfaces produced:**
- `func (p *Provider) ResolveBatch(ctx context.Context, refs []mamori.Ref) (map[string]mamori.Value, error)` satisfying `mamori.BatchProvider`
- `const bulkMaxKeys = 100`

- [ ] **Step 1: Write the failing tests**

Create `providers/cloudflare-kv/batch_test.go` covering:

- **Chunking.** 250 refs against one namespace produce exactly **3** bulk requests, and all 250 values come back correct. Assert both the request count and the values. A silent truncation at 100 would drop 150 fields with no error at all, which is the failure this test exists to prevent.
- **Exactly 100 refs produce exactly 1 request**, and 101 produce 2. Off-by-one at the chunk boundary is the likeliest bug here.
- **Grouping by namespace.** Refs split across two namespaces via `?namespace=` produce one request per namespace, asserted by request count and by each value coming from the right namespace.
- **Absent keys are omitted, siblings survive.** This is a regression pin: the sibling `providers/vercel-gc` shipped a defect where a field-level not-found failed the entire batch and took its siblings with it.
- **`Resolve` and `ResolveBatch` agree.** For a ref whose `#field` is absent from a JSON value, `Resolve` must return an error satisfying `mamori.ErrNotFound` and `ResolveBatch` must omit that ref while still returning its siblings. The sibling module got this wrong; do not repeat it.
- **A 200 response carrying `success: false`** produces an error satisfying `mamori.ErrInvalid`.
- An empty batch makes zero requests and returns an empty map.
- `var _ mamori.BatchProvider = (*Provider)(nil)` compiles, and `New()` does **not** satisfy `mamori.WatchableProvider`.

- [ ] **Step 2: Implement**

Group refs by namespace, then chunk each namespace's keys into groups of at most `bulkMaxKeys`. POST `{"keys": [...], "type": "text"}` to `{base}/accounts/{account}/storage/kv/namespaces/{namespace}/bulk/get`.

`type: "text"` is required: without it Cloudflare may JSON-parse values and return objects rather than the strings this provider treats as opaque bytes.

Merge each chunk's `result.values` into the output, keyed by `ref.Raw` per the `BatchProvider` contract. A key absent from `values` is omitted so mamori applies the field's default.

Handle the `valueFor` error exactly as the sibling was corrected to:

```go
v, err := valueFor(b, r, ns)
if err != nil {
	if errors.Is(err, mamori.ErrNotFound) {
		continue // an absent selected field is still not-found; mamori applies the default
	}
	return nil, err
}
```

- [ ] **Step 3: Verify and commit**

Module tests, `-race`, `go vet`, `golangci-lint`, all clean.

```bash
git add providers/cloudflare-kv/resolve.go providers/cloudflare-kv/batch_test.go
git commit -m "feat(cloudflare-kv): ResolveBatch, chunked at the API's 100-key ceiling

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Conformance kit and live integration test

**Files:**
- Create: `providers/cloudflare-kv/conformance_test.go`, `providers/cloudflare-kv/cloudflarekv_integration_test.go`

- [ ] **Step 1: Wire `providertest.Run`**

Read `providertest/providertest.go` for the real `Config` field semantics rather than assuming. Model the wiring on `providers/vercel-gc/conformance_test.go`, which the reviewers judged more rigorous than doppler's.

Requirements the wiring must meet:

- Build the provider with the fake's in-process transport, never a live server.
- Do **not** set `SkipWatch`. This provider genuinely has no `Watch` method, so the watch cases must skip on their own. If they fail rather than skip, investigate and report rather than reaching for the flag.
- Do **not** set `NoResolveErrors`. This provider classifies HTTP status and has a failure-injection seam, so it owes the `ErrorClassification` case.
- **`Fail` must honor the injected sentinel.** The sibling's plan hard-coded a single status and ignored the sentinel, which failed 4 of 5 classification cases. Map each injected `mamori` sentinel back to the HTTP status that produces it, so every case round-trips through the real `classifyStatus` over the fake's HTTP layer rather than short-circuiting it.
- Wire `PointerRef` so the `JSONPointerSelection` case actually runs.
- `Seed` and `Mutate` must genuinely change the fake's state.

- [ ] **Step 2: Run the conformance suite verbosely**

Run: `cd providers/cloudflare-kv && GOWORK=off go test -run TestConformance -v ./...`
Record which cases ran and which skipped, and why, in your report.

- [ ] **Step 3: Live integration test**

Create `providers/cloudflare-kv/cloudflarekv_integration_test.go` behind `//go:build integration`, skipping unless `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_KV_NAMESPACE_ID`, and `CLOUDFLARE_KV_TEST_KEY` are all set. It resolves the test key and asserts a non-empty value.

It must also **exercise the bulk path against the real API**, resolving the same key through `ResolveBatch` and asserting the two agree. The single and bulk endpoints have different response shapes, so this is the only check that the bulk parsing matches production rather than matching the fake.

- [ ] **Step 4: Verify and commit**

Run the module suite, `-race`, `golangci-lint`, then `make test` and `make vet` from the repo root.

```bash
git add providers/cloudflare-kv/conformance_test.go providers/cloudflare-kv/cloudflarekv_integration_test.go providers/cloudflare-kv/go.mod providers/cloudflare-kv/go.sum
git commit -m "test(cloudflare-kv): providertest conformance and live integration test

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

Note `go.mod` is in the add list: `providertest` pulls `go.uber.org/goleak` in as an indirect test dependency, exactly as it does for the other 35 provider modules.

---

### Task 5: Documentation

**Files:**
- Create: `providers/cloudflare-kv/README.md`, `site/src/pages/docs/providers/cloudflare-kv.md`
- Modify: `site/src/layouts/DocsLayout.astro`, `site/src/pages/docs/providers/index.md`, `README.md`, `skills/mamori/references/providers.md`

- [ ] **Step 1: Read the code, then write the module README**

Follow `providers/vercel-gc/README.md`'s structure. **Every factual claim must be true of the code**; read `providers/cloudflare-kv/*.go` line by line against your own prose before committing.

State these explicitly, since each is a decision a reader would otherwise assume went the other way:

- The entire ref path is the key, and why: Workers KV keys may contain slashes, so the namespace is a `?namespace=` option rather than a path segment.
- A `#` cannot appear in a key expressible as a ref, because mamori's grammar claims `#` for field selection.
- The single-key GET returns raw bytes while the bulk endpoint returns a JSON envelope.
- `ResolveBatch` chunks at 100 keys, the API's documented ceiling.
- `Value.Sensitive` is `false`, and why: Cloudflare's own docs note anyone with namespace read access sees values in plain text. Wrapping in `secret.String` still redacts.
- `Value.Version` is a content hash, because Workers KV exposes no revision or ETag.
- No native watch, and why: no streaming read, no blocking read, no digest to gate a cache on. Every `Resolve` fetches; compose `middleware.Cache` for coalescing.
- The 404 caveat: an absent key and an absent namespace are indistinguishable, so a misconfigured namespace presents as every field silently defaulting. Tell readers to check `Status()`.
- **Cloudflare Secrets Store is not supported and cannot be**, because its secrets are write-only by design: they bind to Workers and the API will not read a value back. Say this so nobody files it as a gap.

- [ ] **Step 2: Site page and navigation**

Create `site/src/pages/docs/providers/cloudflare-kv.md` with front matter matching a sibling exactly:

```markdown
---
layout: ../../../layouts/DocsLayout.astro
title: Cloudflare Workers KV
---
```

Add to the "KV & config" group in `site/src/layouts/DocsLayout.astro`, after the `providers/vercel-gc` entry:

```js
      { slug: "providers/cloudflare-kv", title: "Cloudflare Workers KV", indent: true },
```

- [ ] **Step 3: The three tables**

The root README table and the site provider matrix have **different column sets**. Check each table's header before writing its row.

Site matrix, `site/src/pages/docs/providers/index.md` (`Scheme | Page | Sensitive | Watch | Errors`):

```markdown
| `cloudflare-kv://` | Cloudflare Workers KV | no | poll | ✅ |
```

Root `README.md` (`Module | Schemes | Watch | Errors classified beyond not-found`):

```markdown
| `providers/cloudflare-kv` | `cloudflare-kv://` | poll | ✅ |
```

Plus the install line under `## Install`, and a row in `skills/mamori/references/providers.md` matching that file's existing column format.

If the site matrix carries a prose count of providers or modules, update it and verify the new number by counting.

- [ ] **Step 4: Verify and commit**

Run `make site-linkcheck` from the repo root (the site build needs Node 22; use `nvm use 22` if the default is older), then `make build && make test && make vet`.

```bash
git add providers/cloudflare-kv/README.md site/src/pages/docs/providers/cloudflare-kv.md site/src/layouts/DocsLayout.astro site/src/pages/docs/providers/index.md README.md skills/mamori/references/providers.md
git commit -m "docs(cloudflare-kv): module README, site page, provider matrix and skill reference

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Final verification

- [ ] `make all` passes from the repo root.
- [ ] `cd providers/cloudflare-kv && GOWORK=off go test -race ./...` passes.
- [ ] `providers/cloudflare-kv/go.mod` has exactly one non-indirect require.
- [ ] No em-dash in the module or its docs.
- [ ] The provider appears in the root README table, the site matrix, the site nav, and the agent-skill reference.
- [ ] No test starts a real `httptest.Server`.
