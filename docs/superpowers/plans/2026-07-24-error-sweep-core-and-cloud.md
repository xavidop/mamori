# Error Mapping Sweep, Part 1: Core Built-ins and the Cloud Tier

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every resolve failure in the core built-in providers and the five cloud providers report an accurate `mamori.Kind`, so an operator sees `permission_denied` on an IAM problem instead of an opaque error string.

**Architecture:** Each provider gains one `classify(err) error` helper that maps its SDK's error vocabulary onto mamori's sentinels and wraps with `%w: %w` (preserving both `errors.Is` to the sentinel and `errors.As` to the original SDK error). Each test fake gains a `fails map[string]error` consulted by its serving path, plus `fail`/`clear` mutators, which lets `providertest`'s `ErrorClassification` case run instead of skip. Every mapping is additionally covered by a table test over real SDK error values, because no in-memory fake can produce a genuine IAM denial.

**Tech Stack:** Go 1.26. Per-module SDKs: `smithy-go` (AWS), `grpc/status` + `codes` (GCP), `azcore` (Azure), `vault/api` (Vault), `k8s.io/apimachinery/pkg/api/errors` (Kubernetes).

This is plan 2 of the spec `docs/superpowers/specs/2026-07-24-operational-layer-design.md`, covering the first slice of workstream A'. Plan 1 (complete) built the vocabulary.

## Scope

**In scope:** the core built-in providers (`file://`, `env:`, `dotenv://`, `exec:`) and five provider modules (`aws`, `gcp`, `azure`, `vault`, `k8s`). These are where permission and credential failures actually occur, which is the entire motivation for the feature.

**Explicitly deferred to later plans**, so the sweep is not misreported as finished:

| Group | Modules | Why separate |
|---|---|---|
| Datastores | postgres, mysql, mongodb, redis, etcd, consul, dynamodb | Uniform map-fakes, different error vocabularies (SQLSTATE, error numbers, gRPC, string prefixes) |
| Object stores | s3, gcs, azblob, cosmos | Uniform map-fakes, status-code based |
| Firebase | firestore, firebase-rc, firebase-rtdb | Mixed gRPC and HTTP |
| SaaS and flags | doppler, onepassword, launchdarkly, unleash, flagsmith, configcat, split, growthbook, flipt, goff | Mostly HTTP status; several have no error concept beyond not-found |
| Special cases | sqlite, sops | **No map-fake exists.** Both test against real files, so error injection needs a different mechanism (permission bits, a wrapping driver). Design these deliberately rather than by analogy. |

**Also deferred:** flipping `providertest.Config.Fail` from optional to required. That happens in the final plan of the sweep, once all 31 modules supply it. Doing it here would break the 26 modules this plan does not touch.

## Global Constraints

- **Core dependencies are frozen.** `github.com/xavidop/mamori` may import only stdlib, `validator/v10`, `mapstructure/v2`, `fsnotify`, `yaml.v3`, and `goleak` (test-only). Task 1 touches core and must add nothing. Provider modules may add SDK dependencies they already have transitively.
- **Do not run `git commit`.** The repo owner handles all git. Stage with `git add` and report the suggested message.
- **Work on `main`.** No branches, no worktrees.
- **`GOWORK=off` for every Go command.** Modules build independently, matching CI. Run provider commands from inside the module directory.
- **The tree stays green after every task.** `make test` must pass from the repo root at every stopping point.
- **No em-dash characters** in code, comments, docs, or commit messages.
- **The wrapping pattern is `fmt.Errorf("%w: %w", sentinel, err)`.** Both operands use `%w`. This is required, not stylistic: it keeps `errors.Is(err, mamori.ErrPermissionDenied)` working for mamori AND keeps `errors.As(err, &sdkErrType)` working for callers who already rely on reaching the SDK error. Using `%v` for the second operand silently breaks existing user code. See `site/src/pages/docs/writing-a-provider.md`.
- **`ErrNotFound` behavior must not change.** It is the only kind that triggers `default:` and `optional` handling. Every task preserves the existing not-found detection exactly; you are adding classifications alongside it, never rerouting it.
- **Do not treat a passing `ErrorClassification` case as proof your mapping is right.** The conformance case injects a mamori sentinel directly, which is not an SDK error type, so your `classify` function returns it unchanged and the case passes. It would pass even if your entire `classify` switch were deleted. What it actually proves is narrower and still worth having: that your provider's error path does not flatten an already-classified error. **The table test over real SDK error values is the only thing that validates your mapping.** Write it first and take it seriously.
- **Every provider task must include one `Resolve`-level classification test.** Discovered during Task 3: neither the table test nor the conformance case catches the `classify` call being removed from `Resolve`. The table test exercises the function directly, and the conformance case is vacuous for this (see the point above). So a provider can have a perfect classifier that is never actually called, with a fully green suite. The required test injects a REAL SDK error through the fake (a gRPC status, an `*azcore.ResponseError`, a `*StatusError`), calls `Resolve`, and asserts `mamori.ErrorKind` on the result. `providers/gcp/gcp_test.go`'s `TestResolveClassifiesNonNotFoundError` is the reference. Verify it is not vacuous by deleting the `classify` call and confirming it fails.
- **Never guess a mapping.** If an SDK condition does not clearly fit a kind, leave it unmapped so it reports `unknown`. A provider that reports `permission_denied` for a network timeout sends an operator down the wrong path, which is worse than reporting nothing.
- **`context.DeadlineExceeded` is already classified centrally** by `ErrorKind` as `KindUnavailable`, and `context.Canceled` deliberately stays `KindUnknown`. Do not map either one per-provider.

---

### Task 1: Core built-in providers

**Files:**
- Modify: `builtin_file.go` (`Resolve`, lines 24-42)
- Modify: `builtin_exec.go` (`Resolve`, lines 25-40)
- Modify: `builtin_dotenv.go` (`Resolve`)
- Test: `builtin_test.go`, `builtin_dotenv_test.go`

**Interfaces:**
- Consumes: `mamori.ErrNotFound`, `ErrPermissionDenied`, `ErrInvalid`, `ErrorKind` from plan 1.
- Produces: no new exported symbols. Establishes the wrapping pattern the five provider tasks follow.

**Why this task first:** it is the only one in core, it needs no fake, and `file://` today returns a bare unwrapped `err` on a permission failure, which is the single most common real-world built-in failure (a Kubernetes secret mounted with restrictive ownership).

- [ ] **Step 1: Write the failing tests**

Append to `builtin_test.go`. The package matches the existing file; follow it.

```go
func TestFileProviderClassifiesPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(path, []byte("hunter2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	ref, err := ParseRef("file://" + path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fileProvider{}.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("Resolve of an unreadable file returned nil error")
	}
	if got := ErrorKind(err); got != KindPermissionDenied {
		t.Fatalf("ErrorKind = %q, want %q (err: %v)", got, KindPermissionDenied, err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("wrapped error no longer satisfies errors.Is(err, fs.ErrPermission); "+
			"the %%w: %%w pattern must preserve the original: %v", err)
	}
}

func TestFileProviderStillReportsNotFound(t *testing.T) {
	ref, err := ParseRef("file:///nonexistent/path/to/nothing")
	if err != nil {
		t.Fatal(err)
	}
	_, err = fileProvider{}.Resolve(context.Background(), ref)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file must still report ErrNotFound, got %v", err)
	}
	if got := ErrorKind(err); got != KindNotFound {
		t.Fatalf("ErrorKind = %q, want %q", got, KindNotFound)
	}
}

func TestExecProviderClassifiesMissingBinary(t *testing.T) {
	ref, err := ParseRef("exec:mamori-no-such-binary-exists-anywhere")
	if err != nil {
		t.Fatal(err)
	}
	_, err = execProvider{}.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("Resolve of a nonexistent binary returned nil error")
	}
	if got := ErrorKind(err); got != KindNotFound {
		t.Fatalf("ErrorKind = %q, want %q (err: %v)", got, KindNotFound, err)
	}
}

func TestExecProviderClassifiesEmptyCommand(t *testing.T) {
	ref, err := ParseRef("exec:   ")
	if err != nil {
		t.Fatal(err)
	}
	_, err = execProvider{}.Resolve(context.Background(), ref)
	if got := ErrorKind(err); got != KindInvalid {
		t.Fatalf("ErrorKind = %q, want %q (err: %v)", got, KindInvalid, err)
	}
}

func TestExecProviderNonZeroExitStaysUnknown(t *testing.T) {
	// A command that runs and fails is a real failure, but mamori has no way to
	// know whether it was a permission problem, a missing value, or a bug in the
	// script. Reporting unknown is the honest answer.
	ref, err := ParseRef("exec:false")
	if err != nil {
		t.Fatal(err)
	}
	_, err = execProvider{}.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("Resolve of a failing command returned nil error")
	}
	if got := ErrorKind(err); got != KindUnknown {
		t.Fatalf("ErrorKind = %q, want %q (err: %v)", got, KindUnknown, err)
	}
}
```

Add `context`, `errors`, `io/fs`, `os`, and `path/filepath` to the test file's imports if absent.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
GOWORK=off go test ./... -run 'TestFileProviderClassifies|TestExecProviderClassifies|TestExecProviderNonZero' -v
```

Expected: `TestFileProviderClassifiesPermissionDenied` FAILs with `ErrorKind = "unknown", want "permission_denied"`. The exec tests fail similarly.

- [ ] **Step 3: Classify in the file provider**

In `builtin_file.go`, replace the two bare `return Value{}, err` branches in `Resolve`:

```go
func (fileProvider) Resolve(_ context.Context, ref Ref) (Value, error) {
	path := ref.Path
	info, err := os.Stat(path)
	if err != nil {
		return Value{}, classifyFileErr(path, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Value{}, classifyFileErr(path, err)
	}
	ver := fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano())
	return Value{Bytes: b, Version: ver}, nil
}

// classifyFileErr maps a filesystem error onto a mamori classification sentinel.
// A permission failure is the common real-world case: a secret mounted with
// restrictive ownership reads as "denied", which is actionable, where the bare
// syscall error was not.
//
// Both operands use %w so callers keep errors.As access to the underlying
// *fs.PathError alongside errors.Is on the sentinel.
func classifyFileErr(path string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ErrNotFound
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("mamori: file %q: %w: %w", path, ErrPermissionDenied, err)
	default:
		return fmt.Errorf("mamori: file %q: %w", path, err)
	}
}
```

Add `errors` and `io/fs` to the file's imports. Note the not-found branch returns the bare `ErrNotFound` sentinel exactly as before, preserving existing behavior.

- [ ] **Step 4: Classify in the exec provider**

In `builtin_exec.go`, update `Resolve`:

```go
	fields := strings.Fields(ref.Path)
	if len(fields) == 0 {
		return Value{}, fmt.Errorf("mamori: exec: %w: empty command", ErrInvalid)
	}
	cmd := exec.CommandContext(ctx, fields[0], fields[1:]...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// A binary that is not on PATH is a not-found condition. A binary that
		// ran and exited non-zero is a genuine failure, but mamori cannot tell
		// why, so it stays unclassified rather than being guessed at.
		if errors.Is(err, exec.ErrNotFound) {
			return Value{}, fmt.Errorf("mamori: exec %q: %w: %w", ref.Path, ErrNotFound, err)
		}
		if errors.Is(err, fs.ErrPermission) {
			return Value{}, fmt.Errorf("mamori: exec %q: %w: %w", ref.Path, ErrPermissionDenied, err)
		}
		return Value{}, fmt.Errorf("mamori: exec %q: %w: %s", ref.Path, err, strings.TrimSpace(stderr.String()))
	}
```

Add `errors` and `io/fs` to the imports.

- [ ] **Step 5: Route the dotenv provider through the same helper**

`builtin_dotenv.go`'s `Resolve` reads a file. Find its file-reading error branch and route it through `classifyFileErr` so `dotenv://` classifies identically to `file://`. Preserve its existing not-found and key-not-found behavior exactly.

- [ ] **Step 6: Run the tests to verify they pass**

```bash
GOWORK=off go test ./... -v
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
```

Expected: all PASS. The pre-existing `TestFileProviderResolve`, `TestFileProviderWatch`, `TestDotenv*`, and `TestExecProviderOptIn` must be unaffected.

- [ ] **Step 7: Confirm nothing regressed repo-wide**

```bash
make test
```

Expected: green across all modules.

- [ ] **Step 8: Stage and report**

```bash
git add builtin_file.go builtin_exec.go builtin_dotenv.go builtin_test.go builtin_dotenv_test.go
```

```
feat(core): classify built-in provider errors

file:// and dotenv:// now report permission_denied for an unreadable file
rather than an opaque syscall error, which is the common case when a mounted
secret has restrictive ownership. exec: reports not_found for a binary that
is not on PATH and invalid for an empty command; a non-zero exit stays
unknown, since mamori cannot tell why a script failed.

Not-found detection is unchanged, so defaults and optional handling behave
exactly as before.
```

---

### Task 2: AWS (`aws-sm`, `aws-ps`)

**Files:**
- Modify: `providers/aws/aws.go` (add the shared classifier)
- Modify: `providers/aws/sm.go` (`mapSMError`, lines 162-168)
- Modify: `providers/aws/ps.go` (`mapPSError`, lines 166-172)
- Modify: `providers/aws/go.mod` (promote `github.com/aws/smithy-go` from indirect to direct)
- Test: `providers/aws/aws_test.go`, new `providers/aws/errors_test.go`

**Interfaces:**
- Consumes: mamori sentinels from plan 1.
- Produces: `classifyAWS(err error) error` in package `aws`, used by both `mapSMError` and `mapPSError`. Tasks 3 to 6 mirror this shape with their own SDK.

**Key SDK fact:** both modeled types (`*smtypes.ResourceNotFoundException`) and unmodeled errors (`smithy.GenericAPIError`) satisfy `smithy.APIError`, so a single `ErrorCode()` switch covers both. Secrets Manager has no modeled `AccessDenied` type in this SDK version, so the code switch is the only way to catch it. `smithy-go` is already an indirect dependency; promote it to direct.

- [ ] **Step 1: Write the failing table test**

Create `providers/aws/errors_test.go`:

```go
package aws

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aws/smithy-go"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/xavidop/mamori"
)

func TestClassifyAWS(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"ModeledNotFound", &smtypes.ResourceNotFoundException{}, mamori.KindNotFound},
		{"AccessDenied", &smithy.GenericAPIError{Code: "AccessDeniedException"}, mamori.KindPermissionDenied},
		{"UnrecognizedClient", &smithy.GenericAPIError{Code: "UnrecognizedClientException"}, mamori.KindUnauthenticated},
		{"ExpiredToken", &smithy.GenericAPIError{Code: "ExpiredTokenException"}, mamori.KindUnauthenticated},
		{"Throttling", &smithy.GenericAPIError{Code: "ThrottlingException"}, mamori.KindRateLimited},
		{"TooManyRequests", &smithy.GenericAPIError{Code: "TooManyRequestsException"}, mamori.KindRateLimited},
		{"InternalService", &smithy.GenericAPIError{Code: "InternalServiceError"}, mamori.KindUnavailable},
		{"ServiceUnavailable", &smithy.GenericAPIError{Code: "ServiceUnavailable"}, mamori.KindUnavailable},
		{"InvalidParameter", &smithy.GenericAPIError{Code: "InvalidParameterException"}, mamori.KindInvalid},
		{"InvalidRequest", &smithy.GenericAPIError{Code: "InvalidRequestException"}, mamori.KindInvalid},
		{"ValidationException", &smithy.GenericAPIError{Code: "ValidationException"}, mamori.KindInvalid},
		{"UnmappedCode", &smithy.GenericAPIError{Code: "SomeFutureException"}, mamori.KindUnknown},
		{"PlainError", errors.New("connection reset"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifyAWS(tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyAWS(%v)) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyAWSPreservesSdkError(t *testing.T) {
	// Callers who already reach the SDK error with errors.As must keep working.
	orig := &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"}
	wrapped := classifyAWS(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var api smithy.APIError
	if !errors.As(wrapped, &api) {
		t.Fatalf("errors.As can no longer reach smithy.APIError: %v", wrapped)
	}
	if api.ErrorCode() != "AccessDeniedException" {
		t.Fatalf("recovered code = %q, want AccessDeniedException", api.ErrorCode())
	}
}

func TestClassifyAWSNilIsNil(t *testing.T) {
	if err := classifyAWS(nil); err != nil {
		t.Fatalf("classifyAWS(nil) = %v, want nil", err)
	}
}

func TestMapSMErrorStillReportsNotFound(t *testing.T) {
	ref, err := mamori.ParseRef("aws-sm://prod/db")
	if err != nil {
		t.Fatal(err)
	}
	got := mapSMError(ref, &smtypes.ResourceNotFoundException{})
	if !errors.Is(got, mamori.ErrNotFound) {
		t.Fatalf("mapSMError lost ErrNotFound: %v", got)
	}
	if !strings.Contains(got.Error(), "prod/db") {
		t.Errorf("error message no longer names the ref: %v", got)
	}
}
```

Imports for this file: `errors`, `strings`, `testing`, `github.com/aws/smithy-go`, `smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"`, and `github.com/xavidop/mamori`.

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd providers/aws && GOWORK=off go test ./... -run TestClassifyAWS -v
```

Expected: compile failure, `undefined: classifyAWS`.

- [ ] **Step 3: Add the shared classifier**

Add to `providers/aws/aws.go`:

```go
// classifyAWS maps an AWS SDK error onto a mamori classification sentinel.
//
// Both modeled service exceptions and unmodeled API errors satisfy
// smithy.APIError, so a single ErrorCode switch covers both. That matters for
// Secrets Manager in particular, which has no modeled AccessDenied type in this
// SDK version, so the code string is the only way to detect a denial.
//
// Unrecognized codes are returned wrapped but unclassified, reporting as
// unknown. Guessing at a code's meaning would send an operator down the wrong
// path, which is worse than admitting mamori does not know.
func classifyAWS(err error) error {
	if err == nil {
		return nil
	}
	var api smithy.APIError
	if !errors.As(err, &api) {
		return err
	}
	var sentinel error
	switch api.ErrorCode() {
	case "ResourceNotFoundException", "ParameterNotFound", "SecretNotFoundException":
		sentinel = mamori.ErrNotFound
	case "AccessDeniedException", "AccessDenied", "AuthorizationError":
		sentinel = mamori.ErrPermissionDenied
	case "UnrecognizedClientException", "ExpiredTokenException", "InvalidSignatureException",
		"MissingAuthenticationToken", "IncompleteSignature":
		sentinel = mamori.ErrUnauthenticated
	case "ThrottlingException", "Throttling", "TooManyRequestsException",
		"RequestLimitExceeded", "ProvisionedThroughputExceededException":
		sentinel = mamori.ErrRateLimited
	case "InternalServiceError", "InternalServerError", "InternalFailure",
		"ServiceUnavailable", "ServiceUnavailableException":
		sentinel = mamori.ErrUnavailable
	case "InvalidParameterException", "InvalidRequestException", "ValidationException",
		"InvalidParameters", "InvalidParameterValue":
		sentinel = mamori.ErrInvalid
	default:
		return err
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}
```

Add `errors`, `fmt`, `github.com/aws/smithy-go`, and `github.com/xavidop/mamori` to the file's imports as needed.

- [ ] **Step 4: Route both mappers through it**

In `sm.go`, `mapSMError`'s fallback branch (line 167) currently wraps generically. Change it so the generic wrap is applied to the classified error, preserving the existing modeled not-found branch verbatim:

```go
func mapSMError(ref mamori.Ref, err error) error {
	var nf *smtypes.ResourceNotFoundException
	if errors.As(err, &nf) {
		return fmt.Errorf("aws-sm: secret %q not found: %w", ref.Path, mamori.ErrNotFound)
	}
	return fmt.Errorf("aws-sm: resolve %q: %w", ref.Path, classifyAWS(err))
}
```

Apply the identical shape to `mapPSError` in `ps.go`, keeping its `*ssmtypes.ParameterNotFound` branch verbatim.

- [ ] **Step 5: Promote smithy-go to a direct dependency**

```bash
cd providers/aws && GOWORK=off go mod tidy
```

Confirm `github.com/aws/smithy-go` moved out of the `// indirect` block in `providers/aws/go.mod`.

- [ ] **Step 6: Add fail/clear to both fakes and wire the conformance case**

In `providers/aws/aws_test.go`, add a `fails map[string]error` field to both `fakeSM` (line 33 area) and `fakeSSM`, initialize it in their constructors, and add mutators to each:

```go
func (f *fakeSM) fail(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[id] = err
}

func (f *fakeSM) clear(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fails, id)
}
```

Consult it at the top of every serving method (`GetSecretValue`, `BatchGetSecretValue` for `fakeSM`; `GetParameter`, `GetParameters` for `fakeSSM`), inside the existing lock:

```go
	if err, ok := f.fails[id]; ok {
		return nil, err
	}
```

Then add `Fail` and `Clear` to both `providertest.Run` calls (`aws_test.go:219` and `:229`):

```go
		Fail:  func(_ context.Context, key string, err error) error { fake.fail(key, err); return nil },
		Clear: func(_ context.Context, key string) error { fake.clear(key); return nil },
```

Note the injected error passes straight through `classifyAWS` unchanged, since a mamori sentinel is not a `smithy.APIError`. That is exactly what the conformance case tests: the classification must survive the provider's error path.

- [ ] **Step 7: Run the tests**

```bash
cd providers/aws && GOWORK=off go test ./... -v
cd providers/aws && GOWORK=off go test -race ./...
cd providers/aws && GOWORK=off go vet ./...
```

Expected: all PASS. The `ErrorClassification` subtest must now RUN rather than SKIP for both conformance tests. Confirm that in the verbose output and quote it in your report.

- [ ] **Step 8: Add the README error table**

Add a section to `providers/aws/README.md` near the existing "What is verified" bullets:

```markdown
## Error classification

Failures are classified so `mamori.ErrorKind` can distinguish them:

| AWS error code | mamori kind |
|---|---|
| `ResourceNotFoundException`, `ParameterNotFound` | `not_found` |
| `AccessDeniedException` | `permission_denied` |
| `UnrecognizedClientException`, `ExpiredTokenException` | `unauthenticated` |
| `ThrottlingException`, `RequestLimitExceeded` | `rate_limited` |
| `InternalServiceError`, `ServiceUnavailable` | `unavailable` |
| `InvalidParameterException`, `ValidationException` | `invalid` |
| anything else | `unknown` |

The original SDK error remains reachable with `errors.As`, so existing code
matching on `*smtypes.ResourceNotFoundException` keeps working.
```

- [ ] **Step 9: Confirm repo-wide green, then stage**

```bash
make test
git add providers/aws/
```

```
feat(aws): classify Secrets Manager and Parameter Store errors

A denied IAM policy now reports permission_denied instead of an opaque SDK
error, and expired credentials report unauthenticated. Classification goes
through smithy.APIError, which covers both modeled service exceptions and
unmodeled API errors, since Secrets Manager has no modeled AccessDenied type
in this SDK version.

Unrecognized codes stay unknown rather than being guessed at. The original
SDK error is still reachable with errors.As.
```

---

### Task 3: GCP (`gcp-sm`)

**Files:**
- Modify: `providers/gcp/gcp.go` (`Resolve` error branch, lines 149-154)
- Test: `providers/gcp/gcp_test.go`, new `providers/gcp/errors_test.go`

**Interfaces:**
- Consumes: mamori sentinels. Mirrors Task 2's `classifyAWS` shape as `classifyGCP`.

**Key SDK fact:** this is the gRPC `secretmanager/apiv1` client, so `status.Code(err)` gives a clean one-to-one mapping. `google.golang.org/grpc/codes` and `.../status` are already directly imported at `gcp.go:31-32`. No dependency change needed.

- [ ] **Step 1: Write the failing table test**

Create `providers/gcp/errors_test.go`:

```go
package gcp

import (
	"errors"
	"testing"

	"github.com/xavidop/mamori"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestClassifyGCP(t *testing.T) {
	cases := []struct {
		code codes.Code
		want mamori.Kind
	}{
		{codes.NotFound, mamori.KindNotFound},
		{codes.PermissionDenied, mamori.KindPermissionDenied},
		{codes.Unauthenticated, mamori.KindUnauthenticated},
		{codes.Unavailable, mamori.KindUnavailable},
		{codes.DeadlineExceeded, mamori.KindUnavailable},
		{codes.ResourceExhausted, mamori.KindRateLimited},
		{codes.InvalidArgument, mamori.KindInvalid},
		{codes.Internal, mamori.KindUnknown},
		{codes.Unimplemented, mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.code.String(), func(t *testing.T) {
			err := status.Error(tc.code, "boom")
			if got := mamori.ErrorKind(classifyGCP(err)); got != tc.want {
				t.Fatalf("ErrorKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyGCPPreservesStatus(t *testing.T) {
	orig := status.Error(codes.PermissionDenied, "caller lacks secretmanager.versions.access")
	wrapped := classifyGCP(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	if got := status.Code(wrapped); got != codes.PermissionDenied {
		t.Fatalf("status.Code(wrapped) = %v, want PermissionDenied; the %%w: %%w "+
			"pattern must keep the gRPC status reachable", got)
	}
}

func TestClassifyGCPNilAndPlain(t *testing.T) {
	if err := classifyGCP(nil); err != nil {
		t.Fatalf("classifyGCP(nil) = %v, want nil", err)
	}
	plain := errors.New("dial tcp: connection refused")
	if got := mamori.ErrorKind(classifyGCP(plain)); got != mamori.KindUnknown {
		t.Fatalf("plain error kind = %q, want unknown", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd providers/gcp && GOWORK=off go test ./... -run TestClassifyGCP -v
```

Expected: `undefined: classifyGCP`.

- [ ] **Step 3: Implement the classifier**

Add to `providers/gcp/gcp.go`:

```go
// classifyGCP maps a gRPC status onto a mamori classification sentinel. The
// Secret Manager v1 client is gRPC-based, so codes map one to one and no
// string matching is needed.
//
// DeadlineExceeded maps to unavailable because a request that timed out is a
// backend that did not respond in time, which is what unavailable denotes.
// Codes with no clear mamori meaning (Internal, Unimplemented, Aborted) are
// returned unclassified rather than guessed at.
func classifyGCP(err error) error {
	if err == nil {
		return nil
	}
	var sentinel error
	switch status.Code(err) {
	case codes.NotFound:
		sentinel = mamori.ErrNotFound
	case codes.PermissionDenied:
		sentinel = mamori.ErrPermissionDenied
	case codes.Unauthenticated:
		sentinel = mamori.ErrUnauthenticated
	case codes.Unavailable, codes.DeadlineExceeded:
		sentinel = mamori.ErrUnavailable
	case codes.ResourceExhausted:
		sentinel = mamori.ErrRateLimited
	case codes.InvalidArgument:
		sentinel = mamori.ErrInvalid
	default:
		return err
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}
```

**Important:** `status.Code` returns `codes.Unknown` for a plain non-status error, which falls to `default` and stays unclassified. That is why the test above asserts a plain error reports `unknown`.

- [ ] **Step 4: Route Resolve through it**

In `gcp.go`, keep the existing `codes.NotFound` branch verbatim and change only the fallback (line 153):

```go
		return mamori.Value{}, fmt.Errorf("gcp-sm: accessing %q: %w", ref.Path, classifyGCP(err))
```

Also classify the local malformed-path error at `gcp.go:133-135` as `mamori.ErrInvalid`, since a ref that is not `project/secret` is exactly what `invalid` denotes:

```go
		return mamori.Value{}, fmt.Errorf("gcp-sm: ref %q must be project/secret: %w", ref.Path, mamori.ErrInvalid)
```

- [ ] **Step 5: Add fail/clear and wire the conformance case**

In `providers/gcp/gcp_test.go`, add `fails map[string]error` to `fakeSM` (line 25 area), initialize it, add `fail`/`clear` mutators following Task 2's shape, and consult the map at the top of `AccessSecretVersion` inside the existing lock. Then add `Fail` and `Clear` to the `providertest.Run` call at `gcp_test.go:97`.

The fake keys by `project/secret`; make sure `fail(key, err)` uses the same key form `Seed` does, so the conformance case's `Seed` then `Fail` on one key targets the same entry.

- [ ] **Step 6: Run the tests**

```bash
cd providers/gcp && GOWORK=off go test ./... -v
cd providers/gcp && GOWORK=off go test -race ./...
```

Expected: all PASS, `ErrorClassification` RUNS rather than skips. Quote that line in your report.

- [ ] **Step 7: Add the README error table**

Add to `providers/gcp/README.md`, near the "Verified vs. needs a live backend" section:

```markdown
## Error classification

| gRPC code | mamori kind |
|---|---|
| `NotFound` | `not_found` |
| `PermissionDenied` | `permission_denied` |
| `Unauthenticated` | `unauthenticated` |
| `Unavailable`, `DeadlineExceeded` | `unavailable` |
| `ResourceExhausted` | `rate_limited` |
| `InvalidArgument` | `invalid` |
| anything else | `unknown` |

A malformed ref (not `project/secret`) reports `invalid` without a round trip.
The gRPC status stays reachable through `status.Code`.
```

- [ ] **Step 8: Confirm repo-wide green, then stage**

```bash
make test
git add providers/gcp/
```

```
feat(gcp): classify Secret Manager errors from gRPC status codes

An IAM denial now reports permission_denied and missing ADC reports
unauthenticated, instead of both surfacing as an opaque error. Codes with no
clear mamori meaning stay unknown. A malformed ref reports invalid without a
round trip to the API.
```

---

### Task 4: Azure (`azure-kv`)

**Files:**
- Modify: `providers/azure/azure.go` (`Resolve` error branch at line 137, `isNotFound` at 211-217)
- Test: `providers/azure/azure_test.go`, new `providers/azure/errors_test.go`

**Key SDK fact:** `*azcore.ResponseError` carries `StatusCode`, already imported and used by `isNotFound`. Classification is an HTTP status switch. Note `azure.go:137` currently returns a **bare unwrapped `err`**, so this task also fixes a missing-context bug.

- [ ] **Step 1: Write the failing table test**

Create `providers/azure/errors_test.go`:

```go
package azure

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/xavidop/mamori"
)

func TestClassifyAzure(t *testing.T) {
	cases := []struct {
		status int
		want   mamori.Kind
	}{
		{http.StatusNotFound, mamori.KindNotFound},
		{http.StatusForbidden, mamori.KindPermissionDenied},
		{http.StatusUnauthorized, mamori.KindUnauthenticated},
		{http.StatusTooManyRequests, mamori.KindRateLimited},
		{http.StatusInternalServerError, mamori.KindUnavailable},
		{http.StatusBadGateway, mamori.KindUnavailable},
		{http.StatusServiceUnavailable, mamori.KindUnavailable},
		{http.StatusBadRequest, mamori.KindInvalid},
		{http.StatusConflict, mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			err := &azcore.ResponseError{StatusCode: tc.status, ErrorCode: "Test"}
			if got := mamori.ErrorKind(classifyAzure(err)); got != tc.want {
				t.Fatalf("status %d: ErrorKind = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestClassifyAzurePreservesResponseError(t *testing.T) {
	orig := &azcore.ResponseError{StatusCode: http.StatusForbidden, ErrorCode: "Forbidden"}
	wrapped := classifyAzure(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var respErr *azcore.ResponseError
	if !errors.As(wrapped, &respErr) {
		t.Fatalf("errors.As can no longer reach *azcore.ResponseError: %v", wrapped)
	}
	if respErr.ErrorCode != "Forbidden" {
		t.Fatalf("recovered ErrorCode = %q, want Forbidden", respErr.ErrorCode)
	}
}

func TestClassifyAzureNonResponseErrorIsUnavailable(t *testing.T) {
	// A transport failure never becomes an *azcore.ResponseError. It is a real
	// "could not reach the backend" condition, which is what unavailable means.
	plain := errors.New("dial tcp 10.0.0.1:443: i/o timeout")
	if got := mamori.ErrorKind(classifyAzure(plain)); got != mamori.KindUnknown {
		t.Fatalf("plain transport error kind = %q, want unknown", got)
	}
}
```

Note the last test asserts `unknown`, not `unavailable`. A plain error could be anything, including a bug in the provider, so it is not classified. Do not "fix" this by mapping every non-response error to unavailable.

- [ ] **Step 2: Run to verify it fails**

```bash
cd providers/azure && GOWORK=off go test ./... -run TestClassifyAzure -v
```

Expected: `undefined: classifyAzure`.

- [ ] **Step 3: Implement the classifier**

Add to `providers/azure/azure.go`:

```go
// classifyAzure maps a Key Vault HTTP status onto a mamori classification
// sentinel. Key Vault returns 403 both for a token that lacks an access policy
// and for a firewall rejection; both are correctly permission_denied.
//
// A transport failure is not an *azcore.ResponseError at all and is left
// unclassified, since it could equally be a provider bug as a backend outage.
func classifyAzure(err error) error {
	if err == nil {
		return nil
	}
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) {
		return err
	}
	var sentinel error
	switch code := respErr.StatusCode; {
	case code == http.StatusNotFound:
		sentinel = mamori.ErrNotFound
	case code == http.StatusForbidden:
		sentinel = mamori.ErrPermissionDenied
	case code == http.StatusUnauthorized:
		sentinel = mamori.ErrUnauthenticated
	case code == http.StatusTooManyRequests:
		sentinel = mamori.ErrRateLimited
	case code >= 500:
		sentinel = mamori.ErrUnavailable
	case code == http.StatusBadRequest:
		sentinel = mamori.ErrInvalid
	default:
		return err
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}
```

- [ ] **Step 4: Fix the bare return and route through the classifier**

In `azure.go`, keep the `isNotFound` branch verbatim and replace the bare `return mamori.Value{}, err` at line 137:

```go
		return mamori.Value{}, fmt.Errorf("azure-kv: resolve %q: %w", ref.Path, classifyAzure(err))
```

Also classify the local malformed-ref error at lines 121-124 as `mamori.ErrInvalid`, matching Task 3.

- [ ] **Step 5: Add fail/clear and wire the conformance case**

In `providers/azure/azure_test.go`, add `fails map[string]error` to `fakeVault` (line 28 area), initialize it, add `fail`/`clear`, consult it at the top of `GetSecret`, and add `Fail`/`Clear` to `providertest.Run` at `azure_test.go:189`.

- [ ] **Step 6: Run the tests**

```bash
cd providers/azure && GOWORK=off go test ./... -v
cd providers/azure && GOWORK=off go test -race ./...
```

Expected: all PASS, `ErrorClassification` RUNS. Quote it.

- [ ] **Step 7: Add the README error table**

Add to `providers/azure/README.md`, near "Verified vs. needs a live backend":

```markdown
## Error classification

| HTTP status | mamori kind |
|---|---|
| 404 | `not_found` |
| 403 | `permission_denied` |
| 401 | `unauthenticated` |
| 429 | `rate_limited` |
| 5xx | `unavailable` |
| 400 | `invalid` |
| anything else | `unknown` |

A transport failure (no HTTP response at all) stays `unknown`, since it could
be a client problem rather than a backend one. `*azcore.ResponseError` stays
reachable with `errors.As`.
```

- [ ] **Step 8: Confirm repo-wide green, then stage**

```bash
make test
git add providers/azure/
```

```
feat(azure): classify Key Vault errors by HTTP status

A missing access policy now reports permission_denied and an expired AAD
token reports unauthenticated. Also fixes a bare unwrapped error return that
gave a non-404 failure no context at all, and reports a malformed ref as
invalid without a round trip.
```

---

### Task 5: Vault

**Files:**
- Modify: `providers/vault/vault.go` (`Resolve` error branch at line 190, `isNotFound` at 254-263)
- Test: `providers/vault/vault_test.go`, new `providers/vault/errors_test.go`

**Key SDK fact and the one genuine ambiguity in this plan:** Vault returns HTTP 403 with body `permission denied` for BOTH a missing or invalid token AND a valid token denied by policy. The status code alone cannot separate them.

**Decision:** map 403 to `permission_denied`. It is the more common cause in a running service (a policy that does not cover the path), and it is the honest choice given the data available. Document the limitation in the README so an operator investigating an `permission_denied` on Vault knows to also check token validity. **Do not** attempt to distinguish them by matching on the error body text, which is brittle across Vault versions.

- [ ] **Step 1: Write the failing table test**

Create `providers/vault/errors_test.go`:

```go
package vault

import (
	"errors"
	"net/http"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/xavidop/mamori"
)

func respErr(status int) error {
	return &vaultapi.ResponseError{StatusCode: status, Errors: []string{"test"}}
}

func TestClassifyVault(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"NotFoundStatus", respErr(http.StatusNotFound), mamori.KindNotFound},
		{"NotFoundSentinel", vaultapi.ErrSecretNotFound, mamori.KindNotFound},
		{"Forbidden", respErr(http.StatusForbidden), mamori.KindPermissionDenied},
		{"BadRequest", respErr(http.StatusBadRequest), mamori.KindInvalid},
		{"TooManyRequests", respErr(http.StatusTooManyRequests), mamori.KindRateLimited},
		{"Sealed", respErr(http.StatusServiceUnavailable), mamori.KindUnavailable},
		{"InternalError", respErr(http.StatusInternalServerError), mamori.KindUnavailable},
		{"Teapot", respErr(http.StatusTeapot), mamori.KindUnknown},
		{"PlainError", errors.New("connection refused"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mamori.ErrorKind(classifyVault(tc.err)); got != tc.want {
				t.Fatalf("ErrorKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyVaultPreservesResponseError(t *testing.T) {
	orig := &vaultapi.ResponseError{StatusCode: http.StatusForbidden, Errors: []string{"permission denied"}}
	wrapped := classifyVault(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var re *vaultapi.ResponseError
	if !errors.As(wrapped, &re) {
		t.Fatalf("errors.As can no longer reach *vaultapi.ResponseError: %v", wrapped)
	}
	if re.StatusCode != http.StatusForbidden {
		t.Fatalf("recovered StatusCode = %d, want 403", re.StatusCode)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd providers/vault && GOWORK=off go test ./... -run TestClassifyVault -v
```

Expected: `undefined: classifyVault`.

- [ ] **Step 3: Implement the classifier**

Add to `providers/vault/vault.go`:

```go
// classifyVault maps a Vault API error onto a mamori classification sentinel.
//
// 403 maps to permission_denied. Vault returns 403 with "permission denied"
// for both a policy that does not cover the path AND a missing, invalid, or
// expired token, and the status code cannot separate them. permission_denied
// is the more common cause in a running service. Matching on the response body
// text to distinguish them was considered and rejected as brittle across Vault
// versions; the README documents the ambiguity so an operator knows to also
// check the token.
//
// 503 is Vault's sealed-or-standby response, which is genuinely unavailable.
func classifyVault(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, vaultapi.ErrSecretNotFound) {
		return fmt.Errorf("%w: %w", mamori.ErrNotFound, err)
	}
	var re *vaultapi.ResponseError
	if !errors.As(err, &re) {
		return err
	}
	var sentinel error
	switch code := re.StatusCode; {
	case code == http.StatusNotFound:
		sentinel = mamori.ErrNotFound
	case code == http.StatusForbidden:
		sentinel = mamori.ErrPermissionDenied
	case code == http.StatusTooManyRequests:
		sentinel = mamori.ErrRateLimited
	case code >= 500:
		sentinel = mamori.ErrUnavailable
	case code == http.StatusBadRequest:
		sentinel = mamori.ErrInvalid
	default:
		return err
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}
```

- [ ] **Step 4: Fix the bare return and route through it**

In `vault.go`, keep the `isNotFound` branch verbatim and replace the bare `return mamori.Value{}, err` at line 190:

```go
		return mamori.Value{}, fmt.Errorf("vault: resolve %q: %w", ref.Path, classifyVault(err))
```

Also classify the local `splitMountPath` validation failure (lines 158-169) as `mamori.ErrInvalid`.

- [ ] **Step 5: Add fail/clear and wire the conformance case**

In `providers/vault/vault_test.go`, add `fails map[string]error` to `fakeVault` (line 41 area), initialize it, add `fail`/`clear`, consult it at the top of `Get`, and add `Fail`/`Clear` to `providertest.Run` at `vault_test.go:111`.

Leave `vault_integration_test.go` alone. It is build-tagged and runs against a live Vault where injecting an arbitrary error is not possible.

- [ ] **Step 6: Run the tests**

```bash
cd providers/vault && GOWORK=off go test ./... -v
cd providers/vault && GOWORK=off go test -race ./...
```

Expected: all PASS, `ErrorClassification` RUNS. Quote it.

- [ ] **Step 7: Add the README error table**

Add to `providers/vault/README.md`, alongside the existing "Verified vs. needs a live backend" table:

```markdown
## Error classification

| Vault response | mamori kind |
|---|---|
| 404, `api.ErrSecretNotFound` | `not_found` |
| 403 | `permission_denied` |
| 429 | `rate_limited` |
| 5xx (including a sealed vault's 503) | `unavailable` |
| 400 | `invalid` |
| anything else | `unknown` |

**One known ambiguity:** Vault answers 403 for both a policy that does not
cover the path and a missing or expired token. The status code cannot separate
them, so both report `permission_denied`. If you are investigating one, check
token validity as well as policy. mamori deliberately does not match on the
response body text to guess, because that string has changed between Vault
versions.
```

- [ ] **Step 8: Confirm repo-wide green, then stage**

```bash
make test
git add providers/vault/
```

```
feat(vault): classify Vault API errors by response status

A policy denial reports permission_denied, a sealed vault reports
unavailable, and request-rate quotas report rate_limited. Also fixes a bare
unwrapped error return that gave non-404 failures no context.

403 covers both a policy denial and a bad token, which Vault does not
distinguish by status. It maps to permission_denied and the README documents
the ambiguity rather than guessing from response body text.
```

---

### Task 6: Kubernetes (`k8s-secret`, `k8s-cm`)

**Files:**
- Modify: `providers/k8s/k8s.go` (`mapGetError` at 378-383, `Watch` error path at 227, `consume` at 290)
- Test: `providers/k8s/k8s_test.go`, new `providers/k8s/errors_test.go`

**Interfaces:**
- Consumes: mamori sentinels.
- Produces: `classifyK8s(err error) error`.

**Why this task is last and different:** the other four modules use hand-rolled map fakes. Kubernetes uses the real generated fake clientset (`k8s.io/client-go/kubernetes/fake.NewSimpleClientset`), which has no map to inject into. Error injection uses `PrependReactor` instead.

**Design for the fake:** do NOT call `PrependReactor` from `Fail`, because reactors accumulate and cannot be individually removed, so `Clear` would be impossible. Instead install ONE reactor at fake-construction time that consults a `map[string]error`, and have `fail`/`clear` mutate that map. This keeps the same mental model as every other module.

- [ ] **Step 1: Write the failing table test**

Create `providers/k8s/errors_test.go`:

```go
package k8s

import (
	"errors"
	"testing"

	"github.com/xavidop/mamori"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var testGR = schema.GroupResource{Group: "", Resource: "secrets"}

func TestClassifyK8s(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"NotFound", apierrors.NewNotFound(testGR, "db"), mamori.KindNotFound},
		{"Forbidden", apierrors.NewForbidden(testGR, "db", errors.New("rbac")), mamori.KindPermissionDenied},
		{"Unauthorized", apierrors.NewUnauthorized("bad token"), mamori.KindUnauthenticated},
		{"TooManyRequests", apierrors.NewTooManyRequests("slow down", 1), mamori.KindRateLimited},
		{"ServiceUnavailable", apierrors.NewServiceUnavailable("no endpoints"), mamori.KindUnavailable},
		{"Timeout", apierrors.NewTimeoutError("timed out", 1), mamori.KindUnavailable},
		{"BadRequest", apierrors.NewBadRequest("malformed"), mamori.KindInvalid},
		{"Conflict", apierrors.NewConflict(testGR, "db", errors.New("conflict")), mamori.KindUnknown},
		{"PlainError", errors.New("connection refused"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mamori.ErrorKind(classifyK8s(tc.err)); got != tc.want {
				t.Fatalf("ErrorKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyK8sPreservesStatusError(t *testing.T) {
	orig := apierrors.NewForbidden(testGR, "db", errors.New("rbac denied"))
	wrapped := classifyK8s(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	if !apierrors.IsForbidden(wrapped) {
		t.Fatalf("apierrors.IsForbidden no longer recognizes the wrapped error; "+
			"the %%w: %%w pattern must keep the *StatusError reachable: %v", wrapped)
	}
	var se *apierrors.StatusError
	if !errors.As(wrapped, &se) {
		t.Fatalf("errors.As can no longer reach *apierrors.StatusError: %v", wrapped)
	}
	if se.ErrStatus.Reason != metav1.StatusReasonForbidden {
		t.Fatalf("recovered Reason = %q, want Forbidden", se.ErrStatus.Reason)
	}
}
```

The `apierrors.IsForbidden(wrapped)` assertion is the important one. Kubernetes users routinely call these helpers, and they must keep working through mamori's wrapping.

- [ ] **Step 2: Run to verify it fails**

```bash
cd providers/k8s && GOWORK=off go test ./... -run TestClassifyK8s -v
```

Expected: `undefined: classifyK8s`.

- [ ] **Step 3: Implement the classifier**

Add to `providers/k8s/k8s.go`:

```go
// classifyK8s maps a Kubernetes API error onto a mamori classification
// sentinel, using the apierrors predicates rather than raw status codes so it
// stays correct across API versions.
//
// The %w: %w wrapping keeps the underlying *StatusError reachable, so callers
// can still use apierrors.IsForbidden and friends on an error that has passed
// through mamori.
func classifyK8s(err error) error {
	if err == nil {
		return nil
	}
	var sentinel error
	switch {
	case apierrors.IsNotFound(err):
		sentinel = mamori.ErrNotFound
	case apierrors.IsForbidden(err):
		sentinel = mamori.ErrPermissionDenied
	case apierrors.IsUnauthorized(err):
		sentinel = mamori.ErrUnauthenticated
	case apierrors.IsTooManyRequests(err):
		sentinel = mamori.ErrRateLimited
	case apierrors.IsServiceUnavailable(err), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		sentinel = mamori.ErrUnavailable
	case apierrors.IsBadRequest(err), apierrors.IsInvalid(err):
		sentinel = mamori.ErrInvalid
	default:
		return err
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}
```

- [ ] **Step 4: Route the error paths through it**

In `k8s.go`, keep `mapGetError`'s existing `IsNotFound` branch verbatim and classify the fallback (line 382):

```go
	return fmt.Errorf("%s: get %s/%s: %w", scheme, namespace, name, classifyK8s(err))
```

Also classify the watch-start failure at line 227, which currently sends a raw unclassified error:

```go
		ch <- mamori.Update{Err: fmt.Errorf("%s: watch %s/%s: %w", scheme, namespace, name, classifyK8s(werr))}
```

Match the surrounding code's exact variable names and format-string style; read the function before editing.

Leave the `watch.Error` event handling at line 290 alone. Changing its re-list behavior is out of scope for this plan.

- [ ] **Step 5: Add error injection to the fake clientset**

In `providers/k8s/k8s_test.go`, add a helper that wraps fake-clientset construction with a failure map and a single persistent reactor:

```go
// failInjector adds error injection to a fake clientset. A single reactor
// installed at construction consults a map, so failures can be both added and
// removed. Calling PrependReactor per injection would be simpler but reactors
// cannot be removed individually, which would make Clear impossible.
type failInjector struct {
	mu    sync.Mutex
	fails map[string]error
}

func newFailInjector(cs *fake.Clientset) *failInjector {
	fi := &failInjector{fails: map[string]error{}}
	cs.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(k8stesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		fi.mu.Lock()
		defer fi.mu.Unlock()
		if err, found := fi.fails[ga.GetName()]; found {
			return true, nil, err
		}
		return false, nil, nil
	})
	return fi
}

func (f *failInjector) fail(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[name] = err
}

func (f *failInjector) clear(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fails, name)
}
```

Imports needed: `sync`, `k8s.io/apimachinery/pkg/runtime`, `k8stesting "k8s.io/client-go/testing"`, and the existing `fake` import.

**Important detail:** the conformance kit's keys are namespaced paths, while the reactor sees only the object name. Read how the existing `Ref` function at `k8s_test.go:113` builds a ref from a key, and make `fail`/`clear` key on whatever component the reactor can actually see. If the key contains a namespace prefix, strip it in `Fail`/`Clear` before calling `fi.fail`. Verify by running the conformance case and confirming the injected error actually surfaces; if `ErrorClassification` passes without the reactor firing, the test is vacuous and the key form is wrong.

- [ ] **Step 6: Wire the conformance case**

Add `Fail` and `Clear` to both `providertest.Run` calls (`k8s_test.go:113` and `:141`), constructing a `failInjector` alongside the fake clientset in each.

- [ ] **Step 7: Run the tests and prove the injection works**

```bash
cd providers/k8s && GOWORK=off go test ./... -v
cd providers/k8s && GOWORK=off go test -race ./...
```

Expected: all PASS, `ErrorClassification` RUNS for both schemes.

**Then prove it is not vacuous.** Temporarily change `classifyK8s` to `return err` unconditionally and re-run. `ErrorClassification` must FAIL. Restore the function and confirm the file is byte-identical afterwards. Report both results; a passing conformance case that would also pass with the classifier removed proves nothing.

- [ ] **Step 8: Add the README error table**

Add to `providers/k8s/README.md`, near the "What is verified" bullets:

```markdown
## Error classification

| Kubernetes condition | mamori kind |
|---|---|
| `IsNotFound` | `not_found` |
| `IsForbidden` (RBAC) | `permission_denied` |
| `IsUnauthorized` | `unauthenticated` |
| `IsTooManyRequests` | `rate_limited` |
| `IsServiceUnavailable`, `IsTimeout`, `IsServerTimeout` | `unavailable` |
| `IsBadRequest`, `IsInvalid` | `invalid` |
| anything else | `unknown` |

Detection uses the `apierrors` predicates rather than raw status codes, so it
stays correct across API versions. The underlying `*StatusError` remains
reachable, so `apierrors.IsForbidden` still works on an error that has passed
through mamori.
```

- [ ] **Step 9: Confirm repo-wide green, then stage**

```bash
make test
git add providers/k8s/
```

```
feat(k8s): classify Kubernetes API errors

An RBAC denial now reports permission_denied rather than an opaque error,
and a watch that cannot start reports a classified error instead of a raw
one. Detection uses the apierrors predicates so it stays correct across API
versions, and the underlying StatusError stays reachable so callers can still
use apierrors.IsForbidden on a mamori error.
```

---

### Task 7: Documentation for the swept providers

**Files:**
- Modify: `site/src/pages/docs/providers/aws.md`, `gcp.md`, `azure.md`, `vault.md`, `kubernetes.md`
- Modify: `site/src/pages/docs/providers/env.md`, `file.md`, `dotenv.md`, `exec.md`
- Modify: `site/src/pages/docs/providers/index.md`
- Modify: `README.md` (root)

**Interfaces:**
- Consumes: the mapping tables written into each module README in Tasks 1 to 6.

- [ ] **Step 1: Mirror each module's error table onto its site page**

For each of `aws`, `gcp`, `azure`, `vault`, `kubernetes`, copy the `## Error classification` table from that module's `README.md` onto its docs-site page, adapting heading level and voice to the page. The tables must match; a reader should not find two different answers for the same provider.

For `env`, `file`, `dotenv`, and `exec`, add a short section from Task 1's behavior:

```markdown
## Error classification

| Condition | mamori kind |
|---|---|
| File or variable absent | `not_found` |
| File exists but is unreadable | `permission_denied` |
| Malformed ref | `invalid` |

`exec:` additionally reports `not_found` when the binary is not on PATH. A
command that runs and exits non-zero reports `unknown`, because mamori cannot
tell whether it failed from a missing value, a permission problem, or a bug in
the script.
```

Adapt per page: `env:` cannot produce `permission_denied`, so its table has only the not-found row. Do not paste rows that cannot occur for that provider.

- [ ] **Step 2: Mark swept providers in the provider index**

In `site/src/pages/docs/providers/index.md`, add a column or marker to the provider table showing which providers classify errors beyond not-found. Mark exactly the nine covered here (the four built-ins plus aws, gcp, azure, vault, k8s) and state plainly that the remaining modules currently report `unknown` for anything that is not not-found, with the sweep in progress. Do not imply broader coverage than exists.

- [ ] **Step 3: Update the root README provider table**

In `README.md`, add the same marker to the provider table so the repository's front page agrees with the docs site.

- [ ] **Step 4: Verify the site builds**

```bash
make site-build
```

Astro requires Node 22 or newer. If the default Node is older, run `nvm use 22` first. Expected: build succeeds, no broken links.

- [ ] **Step 5: Cross-check every table against the code**

For each of the nine providers, open its classifier and confirm every row in both its module README and its site page corresponds to a real branch, and that no branch is missing from the tables. Report the check per provider. A table that claims a mapping the code does not implement is worse than no table, because the 26 remaining sweep tasks will treat these as the reference.

- [ ] **Step 6: Stage and report**

```bash
git add site/src/pages/docs/providers/ README.md
```

```
docs: document error classification for the swept providers

Adds mapping tables to the site pages for the core built-ins and the five
cloud providers, mirroring each module README. The provider index and root
README now mark which providers classify beyond not-found, and state plainly
that the rest still report unknown while the sweep continues.
```

---

## Self-Review

**Spec coverage.** This plan implements the first slice of spec section 6.2 half 2 ("per provider" SDK mapping tables) and the per-provider README tables from section 12.3, for 9 of the 35 providers (4 core built-ins plus 5 modules).

**Explicitly not covered, with the reason stated in Scope above:** the 26 remaining provider modules, and flipping `providertest.Config.Fail` to required. Both belong to later plans in the sweep. The Scope table names every deferred module so the sweep cannot be mistaken for complete.

**Placeholders.** None. Every code step carries complete, valid Go. Two steps direct the executor to read surrounding code before editing rather than supplying a full replacement (Task 1 Step 5's dotenv routing, Task 6 Step 4's watch-path format string), because both must match local variable names the plan cannot see without reproducing the whole function; each names the exact file, function, and line range.

**Type consistency.** Each task defines `classify<Provider>(err error) error` with an identical signature and identical nil handling. All five return the input unchanged when they cannot classify, so an unmapped error reports `unknown` rather than being wrapped in a misleading sentinel. Every classifier uses `fmt.Errorf("%w: %w", sentinel, err)`, matching the corrected guidance in `site/src/pages/docs/writing-a-provider.md`. `fail(key string, err error)` and `clear(key string)` have the same signature in all five modules, and map onto `providertest.Config.Fail` and `Clear` identically.

**Risk noted for the executor.** Task 6's key-form question (what the fake clientset's reactor can see versus what the conformance kit passes as a key) is the one place where a wrong guess produces a test that passes without testing anything. Step 7 mandates a mutation check specifically to catch that.
