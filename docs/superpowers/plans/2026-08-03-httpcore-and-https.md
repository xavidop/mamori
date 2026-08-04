# httpcore and https Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** ship `providers/httpcore`, a reusable HTTP resolve core for mamori provider authors, and `providers/https`, a generic provider built on it that resolves refs against operator-declared endpoints.

**Architecture:** `httpcore` is a standard-library-only module exposing four bounded units: `Client` (one round trip, bounded and drained body, no retry), `Authenticator` (credential injection), `ClassifyStatus` (the single HTTP-status-to-mamori-sentinel table), and `Revalidator` (per-ref ETag cache turning a poll into a conditional GET). `providers/https` is a thin consumer: it maps a ref's authority to a registered `Endpoint`, issues one `Revalidator.Get`, and hands the payload to `mamori.SelectKey`.

**Tech Stack:** Go 1.26, standard library only (`net/http`, `net/url`, `encoding/json`), `github.com/xavidop/mamori` for error sentinels and helpers, `github.com/xavidop/mamori/providertest` for the conformance kit.

## Global Constraints

- Go 1.26.0 in every new `go.mod`; `go.work` is on `go 1.26.5`.
- `httpcore` and `https` depend on **no third-party packages**. Only the standard library and `github.com/xavidop/mamori`.
- Both modules carry `replace github.com/xavidop/mamori => ../..`. `https` also carries `replace github.com/xavidop/mamori/providers/httpcore => ../httpcore`.
- Run module-local commands with the workspace disabled: `GOWORK=off go test ./...`.
- **Error wrapping uses two `%w` verbs**: `fmt.Errorf("%w: %w", mamori.ErrPermissionDenied, err)`. A single `%w` with `%v` for the cause flattens the chain and breaks `errors.As`. See `errors.go:148-156`.
- **Never log, wrap, or embed a resolved payload.** Error messages carry the ref, the status, and a caller-chosen detail string, never response bytes.
- Conformance tests use an in-process `http.RoundTripper` fake, never `httptest.Server`. The kit's `NoGoroutineLeak` case snapshots goroutines with `goleak.IgnoreCurrent` and a live server's accept goroutine does not survive it.
- Conventional Commits. `feat(httpcore):`, `feat(https):`, `docs:`.
- Every exported identifier carries a doc comment. This repo documents heavily; match the density of `providers/cloudflare-kv/cloudflarekv.go`.

## File Structure

**`providers/httpcore/`** (module `github.com/xavidop/mamori/providers/httpcore`)

| File | Responsibility |
| --- | --- |
| `httpcore.go` | Package doc, `Config`, `Client`, `New`, defaults |
| `classify.go` | `ClassifyStatus`, the status-to-sentinel table |
| `auth.go` | `Authenticator` interface, `Bearer`, `HeaderAuth`, `BasicAuth`, `QueryAuth` |
| `do.go` | `Request`, `Response`, `Client.Do` |
| `version.go` | `Version` |
| `revalidate.go` | `Revalidator`, LRU-bounded conditional-GET cache |
| `oauth2.go` | `OAuth2Config`, `OAuth2ClientCredentials`, token cache |
| `fake_test.go` | `roundTripFunc` and the shared test fake |
| `README.md` | Module documentation |

**`providers/https/`** (module `github.com/xavidop/mamori/providers/https`)

| File | Responsibility |
| --- | --- |
| `https.go` | Package doc, `Endpoint`, `Provider`, `New`, `Scheme` |
| `resolve.go` | `Resolve`, endpoint lookup, path join, `SelectKey` handoff |
| `fake_test.go` | `RoundTripper` fake with per-endpoint status injection |
| `conformance_test.go` | `providertest.Run` |
| `https_integration_test.go` | `//go:build integration` live test |
| `README.md` | Module documentation |

---

### Task 1: httpcore module scaffold and ClassifyStatus

`ClassifyStatus` first because it is a pure function with no dependencies, so it establishes the module and its test loop in one pass.

**Files:**
- Create: `providers/httpcore/go.mod`
- Create: `providers/httpcore/classify.go`
- Create: `providers/httpcore/httpcore.go`
- Test: `providers/httpcore/classify_test.go`
- Modify: `go.work`

**Interfaces:**
- Consumes: `mamori.ErrNotFound`, `mamori.ErrInvalid`, `mamori.ErrUnauthenticated`, `mamori.ErrPermissionDenied`, `mamori.ErrRateLimited`, `mamori.ErrUnavailable`, `mamori.Kind`, `mamori.ErrorKind` (all from `errors.go`)
- Produces:
  - `func ClassifyStatus(status int, detail string) error`
  - `func StatusForKind(k mamori.Kind) int`
  - the package itself

- [ ] **Step 1: Create the module**

```bash
mkdir -p providers/httpcore
cat > providers/httpcore/go.mod <<'EOF'
module github.com/xavidop/mamori/providers/httpcore

go 1.26.0

require github.com/xavidop/mamori v0.1.0

replace github.com/xavidop/mamori => ../..
EOF
```

- [ ] **Step 2: Add the module to the workspace**

Edit `go.work`, inserting `./providers/httpcore` into the `use (` block in alphabetical order, between `./providers/growthbook` and `./providers/k8s`.

- [ ] **Step 3: Write the failing test**

Create `providers/httpcore/classify_test.go`:

```go
package httpcore

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

func TestClassifyStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   mamori.Kind
	}{
		{"ok", http.StatusOK, ""},
		{"created", http.StatusCreated, ""},
		{"not modified", http.StatusNotModified, ""},
		{"bad request", http.StatusBadRequest, mamori.KindInvalid},
		{"unauthorized", http.StatusUnauthorized, mamori.KindUnauthenticated},
		{"forbidden", http.StatusForbidden, mamori.KindPermissionDenied},
		{"not found", http.StatusNotFound, mamori.KindNotFound},
		{"request timeout", http.StatusRequestTimeout, mamori.KindRateLimited},
		{"too many requests", http.StatusTooManyRequests, mamori.KindRateLimited},
		{"internal error", http.StatusInternalServerError, mamori.KindUnavailable},
		{"bad gateway", http.StatusBadGateway, mamori.KindUnavailable},
		{"teapot", http.StatusTeapot, mamori.KindUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ClassifyStatus(tt.status, "")
			if tt.want == "" {
				if err != nil {
					t.Fatalf("ClassifyStatus(%d) = %v, want nil", tt.status, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ClassifyStatus(%d) = nil, want %s", tt.status, tt.want)
			}
			if got := mamori.ErrorKind(err); got != tt.want {
				t.Fatalf("ClassifyStatus(%d) kind = %s, want %s", tt.status, got, tt.want)
			}
		})
	}
}

// TestClassifyStatusIncludesDetail proves the caller-supplied detail reaches the
// message, since that is the only channel a provider has for a vendor's error
// text.
func TestClassifyStatusIncludesDetail(t *testing.T) {
	err := ClassifyStatus(http.StatusForbidden, "token lacks secrets:read")
	if err == nil {
		t.Fatal("ClassifyStatus returned nil")
	}
	if !strings.Contains(err.Error(), "token lacks secrets:read") {
		t.Fatalf("detail missing from %q", err.Error())
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("errors.Is(ErrPermissionDenied) = false for %v", err)
	}
}

// TestStatusForKindRoundTrips is the reason StatusForKind is exported rather
// than hand-rolled per provider: it pins the two tables as exact inverses, so
// neither can drift without this failing. A drifted inverse makes a conformance
// Fail hook inject a status that maps to a different kind, which silently
// exercises one classification case five times instead of five cases once.
func TestStatusForKindRoundTrips(t *testing.T) {
	kinds := []mamori.Kind{
		mamori.KindInvalid,
		mamori.KindUnauthenticated,
		mamori.KindPermissionDenied,
		mamori.KindNotFound,
		mamori.KindRateLimited,
		mamori.KindUnavailable,
	}
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			status := StatusForKind(k)
			err := ClassifyStatus(status, "")
			if err == nil {
				t.Fatalf("StatusForKind(%s) = %d, which ClassifyStatus treats as success", k, status)
			}
			if got := mamori.ErrorKind(err); got != k {
				t.Fatalf("round trip %s -> %d -> %s, want %s", k, status, got, k)
			}
		})
	}
}

// TestStatusForKindUnknown pins the fallback. ClassifyStatus never produces
// KindUnknown, so the inverse has no exact answer and must pick a status that
// is at least an honest failure.
func TestStatusForKindUnknown(t *testing.T) {
	if got := StatusForKind(mamori.KindUnknown); got < 500 {
		t.Fatalf("StatusForKind(unknown) = %d, want a 5xx", got)
	}
}
```

- [ ] **Step 4: Run the test to verify it fails**

Run: `cd providers/httpcore && GOWORK=off go test ./... 2>&1 | head -20`
Expected: FAIL, `undefined: ClassifyStatus`

- [ ] **Step 5: Write the package doc file**

Create `providers/httpcore/httpcore.go`:

```go
// Package httpcore is the shared HTTP resolve core for mamori providers whose
// backend is a REST API.
//
// Sixteen of mamori's providers speak HTTP, and before this package each one
// hand-rolled request building, credential injection, status classification,
// and response body hygiene. That duplication is what issue #107 surfaced as
// inconsistent body draining. This package exists so a provider author writes
// the part that is actually specific to their backend and inherits the rest.
//
// # What it does not do
//
// It does not retry. mamori's reconciler already backs off and retries a failed
// resolve (see backoff.go in the core module), and a second retry layer inside
// the provider would multiply against it, turning a configured five attempts
// into twenty-five.
//
// It does not parse vendor error envelopes. [ClassifyStatus] takes the detail
// string from its caller because a response body can contain the resolved value
// itself, and only the provider knows which field of its backend's error shape
// is safe to surface.
//
// # Units
//
//   - [Client] performs one round trip with a bounded, always-drained body.
//   - [Authenticator] injects credentials.
//   - [ClassifyStatus] maps an HTTP status onto a mamori error sentinel.
//   - [Revalidator] turns a repeated poll into a conditional GET.
package httpcore
```

- [ ] **Step 6: Implement ClassifyStatus**

Create `providers/httpcore/classify.go`:

```go
package httpcore

import (
	"fmt"
	"net/http"

	"github.com/xavidop/mamori"
)

// ClassifyStatus maps an HTTP status code onto a wrapped mamori error sentinel.
// It returns nil for any 2xx and for 304 Not Modified, which is a successful
// conditional GET rather than a failure.
//
// The mapping is:
//
//	400            -> mamori.ErrInvalid
//	401            -> mamori.ErrUnauthenticated
//	403            -> mamori.ErrPermissionDenied
//	404            -> mamori.ErrNotFound
//	408, 429       -> mamori.ErrRateLimited
//	anything else  -> mamori.ErrUnavailable
//
// 404 maps to mamori.ErrNotFound because that is the only kind that drives
// mamori's behavior: it is what makes a field's default: or optional handling
// apply instead of failing the whole snapshot.
//
// detail is an optional, caller-chosen string appended to the message. Pass a
// vendor error code or message only after deciding it cannot contain the
// resolved value; pass "" when in doubt. ClassifyStatus never reads a response
// body itself.
func ClassifyStatus(status int, detail string) error {
	if status >= 200 && status < 300 {
		return nil
	}
	if status == http.StatusNotModified {
		return nil
	}

	var sentinel error
	switch status {
	case http.StatusBadRequest:
		sentinel = mamori.ErrInvalid
	case http.StatusUnauthorized:
		sentinel = mamori.ErrUnauthenticated
	case http.StatusForbidden:
		sentinel = mamori.ErrPermissionDenied
	case http.StatusNotFound:
		sentinel = mamori.ErrNotFound
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		sentinel = mamori.ErrRateLimited
	default:
		sentinel = mamori.ErrUnavailable
	}

	if detail == "" {
		return fmt.Errorf("http %d %s: %w", status, http.StatusText(status), sentinel)
	}
	return fmt.Errorf("http %d %s: %s: %w", status, http.StatusText(status), detail, sentinel)
}

// StatusForKind is the inverse of ClassifyStatus: it returns an HTTP status that
// ClassifyStatus maps back to k.
//
// It exists for conformance tests. providertest's ErrorClassification case
// injects a mamori sentinel, but an HTTP backend's fake can only fail a request
// with a status code, so the test has to turn the sentinel back into the status
// that produces it. Injecting one fixed status instead would exercise a single
// classification case five times rather than five cases once, and the test would
// still pass.
//
// Exporting it keeps the table and its inverse in one file, where a change to
// one that is not mirrored in the other fails TestStatusForKindRoundTrips
// immediately, rather than silently weakening the conformance test of every
// provider that copied an older version of the mapping.
//
// mamori.KindUnknown has no exact inverse, because ClassifyStatus never produces
// it. It maps to 500, which is at least an honest failure.
func StatusForKind(k mamori.Kind) int {
	switch k {
	case mamori.KindInvalid:
		return http.StatusBadRequest
	case mamori.KindUnauthenticated:
		return http.StatusUnauthorized
	case mamori.KindPermissionDenied:
		return http.StatusForbidden
	case mamori.KindNotFound:
		return http.StatusNotFound
	case mamori.KindRateLimited:
		return http.StatusTooManyRequests
	case mamori.KindUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `cd providers/httpcore && GOWORK=off go mod tidy && GOWORK=off go test ./...`
Expected: PASS

- [ ] **Step 8: Verify the workspace still builds**

Run: `go build ./... && go vet ./providers/httpcore/...`
Expected: no output

- [ ] **Step 9: Commit**

```bash
git add providers/httpcore go.work
git commit -m "feat(httpcore): status classification and module scaffold"
```

---

### Task 2: Authenticator and the four static schemes

**Files:**
- Create: `providers/httpcore/auth.go`
- Test: `providers/httpcore/auth_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks
- Produces:
  - `type Authenticator interface { Apply(ctx context.Context, req *http.Request) error }`
  - `func Bearer(token string) Authenticator`
  - `func HeaderAuth(name, value string) Authenticator`
  - `func BasicAuth(user, pass string) Authenticator`
  - `func QueryAuth(name, value string) Authenticator`

- [ ] **Step 1: Write the failing test**

Create `providers/httpcore/auth_test.go`:

```go
package httpcore

import (
	"context"
	"net/http"
	"testing"
)

func newTestRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://example.test/v1/cfg", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

func TestBearer(t *testing.T) {
	req := newTestRequest(t)
	if err := Bearer("tok123").Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok123" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer tok123")
	}
}

func TestHeaderAuth(t *testing.T) {
	req := newTestRequest(t)
	if err := HeaderAuth("X-Api-Key", "k9").Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("X-Api-Key"); got != "k9" {
		t.Fatalf("X-Api-Key = %q, want %q", got, "k9")
	}
}

func TestBasicAuth(t *testing.T) {
	req := newTestRequest(t)
	if err := BasicAuth("u", "p").Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	user, pass, ok := req.BasicAuth()
	if !ok || user != "u" || pass != "p" {
		t.Fatalf("BasicAuth = (%q, %q, %v), want (u, p, true)", user, pass, ok)
	}
}

func TestQueryAuth(t *testing.T) {
	req := newTestRequest(t)
	if err := QueryAuth("access_token", "t7").Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.URL.Query().Get("access_token"); got != "t7" {
		t.Fatalf("access_token = %q, want %q", got, "t7")
	}
}

// TestQueryAuthPreservesExistingQuery proves the authenticator adds to the query
// rather than replacing it, since Endpoint.Query is merged in before auth runs.
func TestQueryAuthPreservesExistingQuery(t *testing.T) {
	req := newTestRequest(t)
	q := req.URL.Query()
	q.Set("env", "prod")
	req.URL.RawQuery = q.Encode()

	if err := QueryAuth("access_token", "t7").Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.URL.Query().Get("env"); got != "prod" {
		t.Fatalf("env = %q, want prod", got)
	}
	if got := req.URL.Query().Get("access_token"); got != "t7" {
		t.Fatalf("access_token = %q, want t7", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd providers/httpcore && GOWORK=off go test ./... 2>&1 | head -20`
Expected: FAIL, `undefined: Bearer`

- [ ] **Step 3: Implement the authenticators**

Create `providers/httpcore/auth.go`:

```go
package httpcore

import (
	"context"
	"net/http"
)

// Authenticator injects credentials into an outbound request. Implementations
// must be safe for concurrent use, since one Client serves every resolve for
// its backend.
//
// An Authenticator must never log the credential it carries, and must not put
// one anywhere mamori might surface it: not in an error message, not in a
// Value's Metadata, not in a URL path that appears in a ref.
type Authenticator interface {
	Apply(ctx context.Context, req *http.Request) error
}

// AuthenticatorFunc adapts a function to the Authenticator interface.
type AuthenticatorFunc func(ctx context.Context, req *http.Request) error

// Apply calls f.
func (f AuthenticatorFunc) Apply(ctx context.Context, req *http.Request) error {
	return f(ctx, req)
}

// Bearer authenticates with an RFC 6750 bearer token in the Authorization
// header. This is what most REST config and secret backends want.
func Bearer(token string) Authenticator {
	return AuthenticatorFunc(func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	})
}

// HeaderAuth authenticates with a fixed header, for backends that use a named
// API-key header such as X-Api-Key rather than Authorization.
func HeaderAuth(name, value string) Authenticator {
	return AuthenticatorFunc(func(_ context.Context, req *http.Request) error {
		req.Header.Set(name, value)
		return nil
	})
}

// BasicAuth authenticates with HTTP Basic credentials.
func BasicAuth(user, pass string) Authenticator {
	return AuthenticatorFunc(func(_ context.Context, req *http.Request) error {
		req.SetBasicAuth(user, pass)
		return nil
	})
}

// QueryAuth authenticates with a query parameter, for the small number of
// backends that accept no header form.
//
// Prefer any of the header-based authenticators where the backend allows one: a
// query parameter travels in the request line, so it reaches proxy logs and
// server access logs that a header would not.
//
// Existing query parameters are preserved, so an Endpoint's fixed Query survives.
func QueryAuth(name, value string) Authenticator {
	return AuthenticatorFunc(func(_ context.Context, req *http.Request) error {
		q := req.URL.Query()
		q.Set(name, value)
		req.URL.RawQuery = q.Encode()
		return nil
	})
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd providers/httpcore && GOWORK=off go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add providers/httpcore/auth.go providers/httpcore/auth_test.go
git commit -m "feat(httpcore): Authenticator with bearer, header, basic and query schemes"
```

---

### Task 3: Client and Do

**Files:**
- Create: `providers/httpcore/do.go`
- Modify: `providers/httpcore/httpcore.go` (append `Config`, `Client`, `New`)
- Test: `providers/httpcore/do_test.go`
- Test: `providers/httpcore/fake_test.go`

**Interfaces:**
- Consumes: `Authenticator` (Task 2), `ClassifyStatus` (Task 1)
- Produces:
  - `type Config struct { BaseURL string; HTTPClient *http.Client; Auth Authenticator; MaxBody int64; UserAgent string }`
  - `func New(cfg Config) (*Client, error)`
  - `type Request struct { Method, Path string; Query url.Values; Header http.Header; Body []byte; IfNoneMatch, IfModifiedSince string }`
  - `type Response struct { Status int; Body []byte; ETag, LastModified string; NotModified bool }`
  - `func (c *Client) Do(ctx context.Context, r Request) (*Response, error)`
  - `const DefaultMaxBody int64 = 1 << 20`
  - `const DefaultTimeout = 30 * time.Second`

- [ ] **Step 1: Write the shared test fake**

Create `providers/httpcore/fake_test.go`:

```go
package httpcore

import (
	"bytes"
	"io"
	"net/http"
)

// roundTripFunc adapts a function to http.RoundTripper so tests drive a Client
// entirely in process. An httptest.Server is deliberately not used anywhere in
// this repo's provider tests: the conformance kit's NoGoroutineLeak case
// snapshots goroutines and a live server's accept goroutine does not survive it.
type roundTripFunc func(req *http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// fakeClient returns an *http.Client whose transport is f.
func fakeClient(f roundTripFunc) *http.Client { return &http.Client{Transport: f} }

// recordingBody is a ReadCloser that records whether it was closed, so tests
// can prove Do drains and closes every response.
type recordingBody struct {
	io.Reader
	closed bool
}

// Close marks the body closed.
func (b *recordingBody) Close() error {
	b.closed = true
	return nil
}

// newResponse builds an *http.Response with a recording body.
func newResponse(status int, body []byte, header http.Header) (*http.Response, *recordingBody) {
	rb := &recordingBody{Reader: bytes.NewReader(body)}
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       rb,
	}, rb
}
```

- [ ] **Step 2: Write the failing test**

Create `providers/httpcore/do_test.go`:

```go
package httpcore

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// bodyMarker is the fake response body used by the tests that assert a payload
// never reaches an error message. It must not be a word that appears in any
// mamori sentinel's own text: mamori.ErrPermissionDenied renders as "mamori:
// permission denied", so a body of "denied" would make the assertion trip on
// the sentinel rather than on a real leak.
const bodyMarker = "s3cr3t-body-marker-9f2a"

func TestNewRejectsEmptyBaseURL(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with empty BaseURL returned nil error")
	}
}

func TestNewRejectsUnparsableBaseURL(t *testing.T) {
	if _, err := New(Config{BaseURL: "://nope"}); err == nil {
		t.Fatal("New with unparsable BaseURL returned nil error")
	}
}

func TestDoJoinsPathAndQuery(t *testing.T) {
	var gotURL string
	c, err := New(Config{
		BaseURL: "https://api.test/v1",
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			gotURL = req.URL.String()
			resp, _ := newResponse(http.StatusOK, []byte(`{"a":1}`), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Do(context.Background(), Request{
		Path:  "cfg/main",
		Query: url.Values{"env": {"prod"}},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if want := "https://api.test/v1/cfg/main?env=prod"; gotURL != want {
		t.Fatalf("URL = %q, want %q", gotURL, want)
	}
}

func TestDoAppliesAuthAndUserAgent(t *testing.T) {
	var gotAuth, gotUA string
	c, err := New(Config{
		BaseURL:   "https://api.test",
		UserAgent: "test-agent/1",
		Auth:      Bearer("tok"),
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			gotAuth = req.Header.Get("Authorization")
			gotUA = req.Header.Get("User-Agent")
			resp, _ := newResponse(http.StatusOK, nil, nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Do(context.Background(), Request{Path: "x"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotUA != "test-agent/1" {
		t.Fatalf("User-Agent = %q", gotUA)
	}
}

func TestDoReturnsValidators(t *testing.T) {
	h := http.Header{}
	h.Set("ETag", `W/"abc"`)
	h.Set("Last-Modified", "Wed, 21 Oct 2026 07:28:00 GMT")

	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusOK, []byte("payload"), h)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := c.Do(context.Background(), Request{Path: "x"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.ETag != `W/"abc"` {
		t.Fatalf("ETag = %q", resp.ETag)
	}
	if resp.LastModified != "Wed, 21 Oct 2026 07:28:00 GMT" {
		t.Fatalf("LastModified = %q", resp.LastModified)
	}
	if string(resp.Body) != "payload" {
		t.Fatalf("Body = %q", resp.Body)
	}
}

func TestDoSendsConditionalHeaders(t *testing.T) {
	var inm, ims string
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			inm = req.Header.Get("If-None-Match")
			ims = req.Header.Get("If-Modified-Since")
			resp, _ := newResponse(http.StatusNotModified, nil, nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := c.Do(context.Background(), Request{
		Path:            "x",
		IfNoneMatch:     `W/"abc"`,
		IfModifiedSince: "Wed, 21 Oct 2026 07:28:00 GMT",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if inm != `W/"abc"` || ims != "Wed, 21 Oct 2026 07:28:00 GMT" {
		t.Fatalf("conditional headers = (%q, %q)", inm, ims)
	}
	if !resp.NotModified {
		t.Fatal("NotModified = false, want true")
	}
}

func TestDoClassifiesStatus(t *testing.T) {
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusForbidden, []byte(bodyMarker), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Do(context.Background(), Request{Path: "x"})
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	// The body must not leak into the message: it can contain the value itself.
	if strings.Contains(err.Error(), bodyMarker) {
		t.Fatalf("response body leaked into error %q", err.Error())
	}
}

func TestDoBoundsBody(t *testing.T) {
	big := strings.Repeat("x", 5000)
	c, err := New(Config{
		BaseURL: "https://api.test",
		MaxBody: 100,
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusOK, []byte(big), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Do(context.Background(), Request{Path: "x"})
	if err == nil {
		t.Fatal("Do with oversized body returned nil error")
	}
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestDoClosesBodyOnEveryPath(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusNotModified} {
		var rb *recordingBody
		c, err := New(Config{
			BaseURL: "https://api.test",
			HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
				var resp *http.Response
				resp, rb = newResponse(status, []byte("body"), nil)
				return resp, nil
			}),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, _ = c.Do(context.Background(), Request{Path: "x"})
		if rb == nil || !rb.closed {
			t.Fatalf("status %d: body not closed", status)
		}
	}
}

func TestDoWrapsTransportError(t *testing.T) {
	sentinel := errors.New("dial tcp: connection refused")
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			return nil, sentinel
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Do(context.Background(), Request{Path: "x"})
	if !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	// Two %w verbs must preserve the cause for errors.As and errors.Is.
	if !errors.Is(err, sentinel) {
		t.Fatalf("cause lost from %v", err)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `cd providers/httpcore && GOWORK=off go test ./... 2>&1 | head -20`
Expected: FAIL, `undefined: New`

- [ ] **Step 4: Append Config, Client and New to httpcore.go**

`providers/httpcore/httpcore.go` currently holds only the package doc and the
`package httpcore` clause, so the import block below goes directly after that
clause and the declarations follow it. Append all of this:

```go
import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/xavidop/mamori"
)

// DefaultMaxBody is the response size ceiling applied when Config.MaxBody is not
// set. A configuration value that does not fit in a megabyte is a mistake, and
// the ceiling is what stops a broken or hostile backend from exhausting memory.
const DefaultMaxBody int64 = 1 << 20

// DefaultTimeout is the per-request timeout applied when Config.HTTPClient is
// not supplied.
const DefaultTimeout = 30 * time.Second

// Config constructs a Client.
type Config struct {
	// BaseURL is the root every Request.Path is joined onto. Required.
	BaseURL string
	// HTTPClient performs the round trips. When nil, a client with
	// DefaultTimeout is used. Supply one to control transport, proxy, or TLS.
	HTTPClient *http.Client
	// Auth injects credentials. When nil, requests are sent unauthenticated.
	Auth Authenticator
	// MaxBody caps how many response bytes are read. Zero or negative selects
	// DefaultMaxBody.
	MaxBody int64
	// UserAgent sets the User-Agent header. Empty leaves Go's default.
	UserAgent string
}

// Client performs bounded, classified HTTP round trips against one backend. It
// is safe for concurrent use.
//
// Client does not retry. See the package documentation for why.
type Client struct {
	base      *url.URL
	http      *http.Client
	auth      Authenticator
	maxBody   int64
	userAgent string
}

// New validates cfg and returns a Client. It fails when BaseURL is empty or
// cannot be parsed, so a misconfiguration surfaces at construction rather than
// at the first resolve.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("httpcore: BaseURL is required: %w", mamori.ErrInvalid)
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("httpcore: BaseURL %q is not a URL: %w: %w", cfg.BaseURL, mamori.ErrInvalid, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("httpcore: BaseURL %q needs a scheme and host: %w", cfg.BaseURL, mamori.ErrInvalid)
	}

	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	maxBody := cfg.MaxBody
	if maxBody <= 0 {
		maxBody = DefaultMaxBody
	}

	return &Client{
		base:      base,
		http:      hc,
		auth:      cfg.Auth,
		maxBody:   maxBody,
		userAgent: cfg.UserAgent,
	}, nil
}
```

- [ ] **Step 5: Implement Do**

Create `providers/httpcore/do.go`:

```go
package httpcore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/xavidop/mamori"
)

// Request is one call against a Client's backend.
type Request struct {
	// Method defaults to GET when empty.
	Method string
	// Path is joined onto the Client's BaseURL.
	Path string
	// Query is merged into the URL.
	Query url.Values
	// Header is merged into the request headers, before Auth is applied.
	Header http.Header
	// Body is the request payload, for the backends whose read path is a POST.
	Body []byte
	// IfNoneMatch sets If-None-Match, making this a conditional GET.
	IfNoneMatch string
	// IfModifiedSince sets If-Modified-Since, making this a conditional GET.
	IfModifiedSince string
}

// Response is a completed round trip.
type Response struct {
	// Status is the HTTP status code.
	Status int
	// Body is the response payload, read up to the Client's MaxBody. It is nil
	// when NotModified is true.
	Body []byte
	// ETag is the response ETag, for the next conditional GET.
	ETag string
	// LastModified is the response Last-Modified, for the next conditional GET.
	LastModified string
	// NotModified reports a 304, meaning the caller's cached copy is current.
	NotModified bool
}

// Do performs one round trip. It applies Auth, bounds and always drains the
// response body, and classifies the status through ClassifyStatus.
//
// A 304 returns a Response with NotModified set and a nil Body, and no error: a
// successful conditional GET is not a failure.
//
// The response body never appears in a returned error. A body can contain the
// resolved value, so classification carries the status only.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	req, err := c.build(ctx, r)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpcore: %s %s: %w: %w",
			req.Method, redactURL(req.URL), mamori.ErrUnavailable, redactTransportError(err, req.URL))
	}
	// Drain and close on every path so the connection is reused rather than
	// abandoned. This is the hygiene issue #107 found missing across providers.
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	out := &Response{
		Status:       resp.StatusCode,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		NotModified:  resp.StatusCode == http.StatusNotModified,
	}

	if err := ClassifyStatus(resp.StatusCode, ""); err != nil {
		return nil, fmt.Errorf("httpcore: %s %s: %w", req.Method, redactURL(req.URL), err)
	}
	if out.NotModified {
		return out, nil
	}

	body, err := readBounded(resp.Body, c.maxBody)
	if err != nil {
		return nil, fmt.Errorf("httpcore: %s %s: %w", req.Method, redactURL(req.URL), err)
	}
	out.Body = body
	return out, nil
}

// build assembles the *http.Request, applying headers, query, and Auth in that
// order so an Authenticator can see and override anything set before it.
func (c *Client) build(ctx context.Context, r Request) (*http.Request, error) {
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}

	u := *c.base
	u.Path = joinPath(c.base.Path, r.Path)
	if len(r.Query) > 0 {
		q := u.Query()
		for k, vs := range r.Query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		u.RawQuery = q.Encode()
	}

	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("httpcore: building %s %s: %w: %w", method, redactURL(&u), mamori.ErrInvalid, err)
	}

	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if r.IfNoneMatch != "" {
		req.Header.Set("If-None-Match", r.IfNoneMatch)
	}
	if r.IfModifiedSince != "" {
		req.Header.Set("If-Modified-Since", r.IfModifiedSince)
	}
	if c.auth != nil {
		if err := c.auth.Apply(ctx, req); err != nil {
			return nil, fmt.Errorf("httpcore: applying credentials: %w: %w", mamori.ErrUnauthenticated, err)
		}
	}
	return req, nil
}

// joinPath joins a base path and a request path with exactly one separator.
func joinPath(base, p string) string {
	switch {
	case p == "":
		return base
	case base == "" || base == "/":
		return "/" + strings.TrimPrefix(p, "/")
	default:
		return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(p, "/")
	}
}

// readBounded reads at most max bytes, returning ErrInvalid if the body is
// larger. Reading max+1 is what distinguishes "exactly at the ceiling", which is
// fine, from "over the ceiling", which is truncation and must not be silent.
func readBounded(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w: %w", mamori.ErrUnavailable, err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("response exceeds %d byte limit: %w", max, mamori.ErrInvalid)
	}
	return b, nil
}

// redactURL renders a URL for an error message with its query and userinfo
// stripped, because QueryAuth puts a credential in the query and a URL can
// carry userinfo.
func redactURL(u *url.URL) string {
	c := *u
	c.RawQuery = ""
	c.User = nil
	return c.String()
}

// redactTransportError strips the credential that net/http leaks into the error
// it wraps a transport failure in.
//
// http.Client.Do returns a *url.Error whose URL field is stripPassword(req.URL),
// and stripPassword masks only a userinfo password: the query string survives
// intact. A QueryAuth credential therefore reaches err.Error() through the
// wrapped cause, bypassing redactURL entirely, and renders as:
//
//	Get "https://api/cfg?access_token=SUPERSECRET": dial tcp: connection refused
//
// The wrapper is rebuilt rather than discarded so the chain stays whole:
// errors.As still reaches *url.Error, and errors.Is still reaches the underlying
// net.OpError, timeout, or TLS verification error.
func redactTransportError(err error, u *url.URL) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	return &url.Error{Op: ue.Op, URL: redactURL(u), Err: ue.Err}
}
```

`do.go` needs `"errors"` in its import block for `errors.As`.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd providers/httpcore && GOWORK=off go test ./...`
Expected: PASS

- [ ] **Step 7: Run vet and the race detector**

Run: `cd providers/httpcore && GOWORK=off go vet ./... && GOWORK=off go test -race ./...`
Expected: no output from vet, PASS from test

- [ ] **Step 8: Commit**

```bash
git add providers/httpcore
git commit -m "feat(httpcore): Client.Do with bounded, drained bodies and classified status"
```

---

### Task 4: Version and Revalidator

**Files:**
- Create: `providers/httpcore/version.go`
- Create: `providers/httpcore/revalidate.go`
- Test: `providers/httpcore/version_test.go`
- Test: `providers/httpcore/revalidate_test.go`

**Interfaces:**
- Consumes: `Client.Do`, `Request`, `Response` (Task 3)
- Produces:
  - `func Version(resp *Response, body []byte) string`
  - `func NewRevalidator(c *Client, maxEntries int) *Revalidator`
  - `func (rv *Revalidator) Get(ctx context.Context, key string, r Request) (*Response, error)`
  - `const DefaultRevalidatorEntries = 512`

- [ ] **Step 1: Write the failing tests**

Create `providers/httpcore/version_test.go`:

```go
package httpcore

import (
	"testing"

	"github.com/xavidop/mamori"
)

func TestVersionPrefersETag(t *testing.T) {
	got := Version(&Response{ETag: `W/"abc"`, LastModified: "Wed, 21 Oct 2026 07:28:00 GMT"}, []byte("x"))
	if got != `W/"abc"` {
		t.Fatalf("Version = %q, want the ETag", got)
	}
}

func TestVersionFallsBackToLastModified(t *testing.T) {
	got := Version(&Response{LastModified: "Wed, 21 Oct 2026 07:28:00 GMT"}, []byte("x"))
	if got != "Wed, 21 Oct 2026 07:28:00 GMT" {
		t.Fatalf("Version = %q, want the Last-Modified", got)
	}
}

func TestVersionFallsBackToHash(t *testing.T) {
	body := []byte("payload")
	got := Version(&Response{}, body)
	if want := mamori.VersionHash(body); got != want {
		t.Fatalf("Version = %q, want %q", got, want)
	}
}

func TestVersionHandlesNilResponse(t *testing.T) {
	body := []byte("payload")
	if got, want := Version(nil, body), mamori.VersionHash(body); got != want {
		t.Fatalf("Version(nil) = %q, want %q", got, want)
	}
}
```

Create `providers/httpcore/revalidate_test.go`:

```go
package httpcore

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// newCountingClient returns a Client whose transport records every request and
// answers 200 with body, then 304 once the caller sends a matching
// If-None-Match.
//
// The recording is mutex-guarded because TestRevalidatorConcurrentGets drives
// this transport from many goroutines at once. The callers read calls and
// conditionals only after wg.Wait(), so the lock is needed for the writes alone.
func newCountingClient(t *testing.T, etag string, body []byte) (*Client, *int, *[]string) {
	t.Helper()
	calls := 0
	var conditionals []string
	var mu sync.Mutex
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			calls++
			inm := req.Header.Get("If-None-Match")
			conditionals = append(conditionals, inm)
			mu.Unlock()
			h := http.Header{}
			h.Set("ETag", etag)
			if inm == etag {
				resp, _ := newResponse(http.StatusNotModified, nil, h)
				return resp, nil
			}
			resp, _ := newResponse(http.StatusOK, body, h)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, &calls, &conditionals
}

func TestRevalidatorSendsValidatorOnSecondGet(t *testing.T) {
	c, calls, conditionals := newCountingClient(t, `"v1"`, []byte("payload"))
	rv := NewRevalidator(c, 8)

	first, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if string(first.Body) != "payload" || first.NotModified {
		t.Fatalf("first = %+v", first)
	}

	second, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("calls = %d, want 2", *calls)
	}
	if (*conditionals)[0] != "" {
		t.Fatalf("first request sent If-None-Match %q, want none", (*conditionals)[0])
	}
	if (*conditionals)[1] != `"v1"` {
		t.Fatalf("second request sent If-None-Match %q, want \"v1\"", (*conditionals)[1])
	}
	// The caller gets the cached body back, not an empty one.
	if string(second.Body) != "payload" {
		t.Fatalf("second body = %q, want the cached payload", second.Body)
	}
	if !second.NotModified {
		t.Fatal("second NotModified = false, want true")
	}
}

func TestRevalidatorKeysSeparately(t *testing.T) {
	c, calls, conditionals := newCountingClient(t, `"v1"`, []byte("payload"))
	rv := NewRevalidator(c, 8)

	if _, err := rv.Get(context.Background(), "a", Request{Path: "cfg"}); err != nil {
		t.Fatalf("Get a: %v", err)
	}
	if _, err := rv.Get(context.Background(), "b", Request{Path: "cfg"}); err != nil {
		t.Fatalf("Get b: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("calls = %d, want 2", *calls)
	}
	for i, inm := range *conditionals {
		if inm != "" {
			t.Fatalf("request %d sent If-None-Match %q, want none for a distinct key", i, inm)
		}
	}
}

func TestRevalidatorEvictsBeyondMaxEntries(t *testing.T) {
	c, _, _ := newCountingClient(t, `"v1"`, []byte("payload"))
	rv := NewRevalidator(c, 2)

	for _, k := range []string{"a", "b", "c"} {
		if _, err := rv.Get(context.Background(), k, Request{Path: "cfg"}); err != nil {
			t.Fatalf("Get %s: %v", k, err)
		}
	}
	if got := rv.len(); got != 2 {
		t.Fatalf("entries = %d, want 2", got)
	}
}

func TestRevalidatorDropsEntryOnError(t *testing.T) {
	var fail bool
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			if fail {
				resp, _ := newResponse(http.StatusInternalServerError, nil, nil)
				return resp, nil
			}
			h := http.Header{}
			h.Set("ETag", `"v1"`)
			resp, _ := newResponse(http.StatusOK, []byte("payload"), h)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rv := NewRevalidator(c, 8)

	if _, err := rv.Get(context.Background(), "k", Request{Path: "cfg"}); err != nil {
		t.Fatalf("seed Get: %v", err)
	}
	fail = true
	if _, err := rv.Get(context.Background(), "k", Request{Path: "cfg"}); err == nil {
		t.Fatal("failing Get returned nil error")
	}
	// A failed revalidation must not leave a stale validator behind, or the next
	// success would be answered from a cache entry the backend never confirmed.
	if got := rv.len(); got != 0 {
		t.Fatalf("entries = %d after failure, want 0", got)
	}
}

// TestRevalidatorKeepsCachedValidatorsOn304 pins that a 304 hit reports the
// validators the cache holds, not whatever the 304 response happened to carry.
//
// RFC 7232 says a backend should repeat ETag on a 304, but real backends, CDNs
// and proxies especially, sometimes omit it. Copying the 304's own empty ETag
// makes Version fall back to a body hash, so a genuinely unmodified poll reports
// a changed Version and mamori runs a spurious update.
//
// newCountingClient cannot express this, because it sets ETag on every response
// and so cannot distinguish "validator from the cache" from "validator from the
// response". That is exactly why this test builds its own backend.
func TestRevalidatorKeepsCachedValidatorsOn304(t *testing.T) {
	calls := 0
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Header.Get("If-None-Match") == `"v1"` {
				// Deliberately omit the validators, as a non-compliant backend does.
				resp, _ := newResponse(http.StatusNotModified, nil, nil)
				return resp, nil
			}
			h := http.Header{}
			h.Set("ETag", `"v1"`)
			h.Set("Last-Modified", "Wed, 21 Oct 2026 07:28:00 GMT")
			resp, _ := newResponse(http.StatusOK, []byte("payload"), h)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rv := NewRevalidator(c, 8)

	first, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	second, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if second.ETag != first.ETag {
		t.Fatalf("ETag on the 304 = %q, want the cached %q", second.ETag, first.ETag)
	}
	if second.LastModified != first.LastModified {
		t.Fatalf("LastModified on the 304 = %q, want the cached %q", second.LastModified, first.LastModified)
	}
	if got, want := Version(second, second.Body), Version(first, first.Body); got != want {
		t.Fatalf("Version changed across an unmodified poll: %q then %q", want, got)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

// TestRevalidatorDoesNotAliasCachedBody pins that a caller cannot corrupt the
// cache by writing into a body it was handed. Without a copy on both sides, the
// Revalidator and every caller share one backing array, so one caller decoding
// in place silently changes what the next poll returns.
func TestRevalidatorDoesNotAliasCachedBody(t *testing.T) {
	c, _, _ := newCountingClient(t, `"v1"`, []byte("payload"))
	rv := NewRevalidator(c, 8)

	first, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	first.Body[0] = 'X'

	second, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if string(second.Body) != "payload" {
		t.Fatalf("cached body = %q, want payload; a caller's write reached the cache", second.Body)
	}
}

// TestRevalidatorRetriesWhenEntryEvictedDuringRequest covers the gap between
// reading the validators and writing the response back. Get releases the lock
// for the network call, so another caller can evict the entry in between. The
// backend then answers 304 for a validator this Revalidator no longer holds,
// and there is no cached body to return, so the fallback must retry
// unconditionally rather than hand back an empty one.
//
// The eviction is forced from inside the RoundTripper, which IS the window
// under test: it runs after validators() and before store(). Get holds no lock
// there, so calling drop from the transport cannot deadlock.
func TestRevalidatorRetriesWhenEntryEvictedDuringRequest(t *testing.T) {
	var rv *Revalidator
	evicted := false
	calls := 0

	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			calls++
			h := http.Header{}
			h.Set("ETag", `"v1"`)
			if req.Header.Get("If-None-Match") == `"v1"` {
				if !evicted {
					evicted = true
					rv.drop("k")
				}
				resp, _ := newResponse(http.StatusNotModified, nil, h)
				return resp, nil
			}
			resp, _ := newResponse(http.StatusOK, []byte("payload"), h)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rv = NewRevalidator(c, 8)

	if _, err := rv.Get(context.Background(), "k", Request{Path: "cfg"}); err != nil {
		t.Fatalf("seed Get: %v", err)
	}
	got, err := rv.Get(context.Background(), "k", Request{Path: "cfg"})
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if string(got.Body) != "payload" {
		t.Fatalf("Body = %q, want the payload recovered by the unconditional retry", got.Body)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3: the seed, the 304 whose entry vanished, and the retry", calls)
	}
}

func TestRevalidatorConcurrentGets(t *testing.T) {
	c, _, _ := newCountingClient(t, `"v1"`, []byte("payload"))
	rv := NewRevalidator(c, 64)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i%4)
			if _, err := rv.Get(context.Background(), key, Request{Path: "cfg"}); err != nil {
				t.Errorf("Get: %v", err)
			}
		}(i)
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd providers/httpcore && GOWORK=off go test ./... 2>&1 | head -20`
Expected: FAIL, `undefined: Version`, `undefined: NewRevalidator`

- [ ] **Step 3: Implement Version**

Create `providers/httpcore/version.go`:

```go
package httpcore

import "github.com/xavidop/mamori"

// Version derives a mamori Value.Version from a response, preferring the
// strongest validator the backend supplied.
//
// The order is ETag, then Last-Modified, then a hash of the body. ETag comes
// first because it is exact: Last-Modified has one-second resolution, so two
// writes inside the same second are indistinguishable, and a body hash costs a
// full read the validators avoid.
//
// resp may be nil, in which case the body hash is used.
func Version(resp *Response, body []byte) string {
	if resp != nil {
		if resp.ETag != "" {
			return resp.ETag
		}
		if resp.LastModified != "" {
			return resp.LastModified
		}
	}
	return mamori.VersionHash(body)
}
```

- [ ] **Step 4: Implement Revalidator**

Create `providers/httpcore/revalidate.go`:

```go
package httpcore

import (
	"bytes"
	"container/list"
	"context"
	"sync"
)

// DefaultRevalidatorEntries is the entry ceiling used when NewRevalidator is
// given a non-positive maxEntries.
const DefaultRevalidatorEntries = 512

// Revalidator turns a repeated poll of the same ref into a conditional GET.
//
// mamori polls any provider without a native watch, which means the same value
// is fetched on every tick. Remembering the last ETag and body lets the next
// poll send If-None-Match and take a 304 with an empty body instead of the full
// payload, which is the difference between a poll that costs a megabyte and one
// that costs a few hundred bytes.
//
// Entries are bounded and evicted least-recently-used, so a large config cannot
// grow the cache without limit. Revalidator is safe for concurrent use.
type Revalidator struct {
	client     *Client
	maxEntries int

	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List // front is most recently used
}

// cacheEntry is one remembered response. It is stored by pointer in the LRU
// list, so key must travel with it for eviction.
type cacheEntry struct {
	key          string
	etag         string
	lastModified string
	body         []byte
}

// NewRevalidator returns a Revalidator over c holding at most maxEntries
// entries. A non-positive maxEntries selects DefaultRevalidatorEntries.
func NewRevalidator(c *Client, maxEntries int) *Revalidator {
	if maxEntries <= 0 {
		maxEntries = DefaultRevalidatorEntries
	}
	return &Revalidator{
		client:     c,
		maxEntries: maxEntries,
		entries:    make(map[string]*list.Element, maxEntries),
		lru:        list.New(),
	}
}

// Get performs r as a conditional GET keyed by key, which should be the ref's
// raw string so two fields reading the same ref share one entry.
//
// On a 304 the returned Response carries the cached body with NotModified set,
// so a caller can treat it exactly like a 200 and still know nothing changed.
//
// A failed request drops the entry, so a later success is never answered from a
// validator the backend has not confirmed.
func (rv *Revalidator) Get(ctx context.Context, key string, r Request) (*Response, error) {
	if etag, lastMod, ok := rv.validators(key); ok {
		if r.IfNoneMatch == "" {
			r.IfNoneMatch = etag
		}
		if r.IfModifiedSince == "" {
			r.IfModifiedSince = lastMod
		}
	}

	resp, err := rv.client.Do(ctx, r)
	if err != nil {
		rv.drop(key)
		return nil, err
	}

	if resp.NotModified {
		etag, lastMod, body, ok := rv.cached(key)
		if !ok {
			// The backend answered 304 for a validator we no longer hold, which
			// means the entry was evicted between the two halves of this call.
			// Retry unconditionally rather than returning an empty body.
			r.IfNoneMatch, r.IfModifiedSince = "", ""
			resp, err = rv.client.Do(ctx, r)
			if err != nil {
				rv.drop(key)
				return nil, err
			}
			rv.store(key, resp)
			return resp, nil
		}
		out := *resp
		out.Body = body
		// Report the validators the cache holds, not the ones the 304 carried.
		// RFC 7232 says a 304 should repeat them, but real backends, CDNs and
		// proxies especially, sometimes omit them. Copying an empty ETag makes
		// Version fall back to a body hash, so a genuinely unmodified poll
		// reports a changed Version and mamori runs a spurious update: a
		// needless PreApply, a needless OnChange, and for a rotating credential
		// a needless reconnect.
		out.ETag = etag
		out.LastModified = lastMod
		return &out, nil
	}

	rv.store(key, resp)
	return resp, nil
}

// validators returns the cached validators for key, marking it recently used.
func (rv *Revalidator) validators(key string) (etag, lastModified string, ok bool) {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	el, ok := rv.entries[key]
	if !ok {
		return "", "", false
	}
	rv.lru.MoveToFront(el)
	e := el.Value.(*cacheEntry)
	return e.etag, e.lastModified, true
}

// cached returns the entry's validators and a private copy of its body, marking
// it recently used.
//
// The body is copied because the caller receives it. Without the copy the
// Revalidator and every caller share one backing array, so a caller that decodes
// or trims in place silently changes what the next poll returns.
func (rv *Revalidator) cached(key string) (etag, lastModified string, body []byte, ok bool) {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	el, ok := rv.entries[key]
	if !ok {
		return "", "", nil, false
	}
	rv.lru.MoveToFront(el)
	e := el.Value.(*cacheEntry)
	return e.etag, e.lastModified, bytes.Clone(e.body), true
}

// store records resp under key, evicting the least recently used entry when the
// cache is over its ceiling.
func (rv *Revalidator) store(key string, resp *Response) {
	if resp.ETag == "" && resp.LastModified == "" {
		// Nothing to revalidate with next time; caching the body would only
		// consume memory for a validator that will never be sent.
		rv.drop(key)
		return
	}
	rv.mu.Lock()
	defer rv.mu.Unlock()

	// The body is copied on the way in for the same reason cached copies it on
	// the way out: the cache must own its bytes outright, or the caller that
	// received this same 200 response can write into what the cache will serve.
	if el, ok := rv.entries[key]; ok {
		e := el.Value.(*cacheEntry)
		e.etag, e.lastModified, e.body = resp.ETag, resp.LastModified, bytes.Clone(resp.Body)
		rv.lru.MoveToFront(el)
		return
	}
	el := rv.lru.PushFront(&cacheEntry{
		key:          key,
		etag:         resp.ETag,
		lastModified: resp.LastModified,
		body:         bytes.Clone(resp.Body),
	})
	rv.entries[key] = el

	for rv.lru.Len() > rv.maxEntries {
		oldest := rv.lru.Back()
		if oldest == nil {
			break
		}
		rv.lru.Remove(oldest)
		delete(rv.entries, oldest.Value.(*cacheEntry).key)
	}
}

// drop removes key's entry.
func (rv *Revalidator) drop(key string) {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	if el, ok := rv.entries[key]; ok {
		rv.lru.Remove(el)
		delete(rv.entries, key)
	}
}

// len reports the number of cached entries. It exists for tests.
func (rv *Revalidator) len() int {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	return len(rv.entries)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd providers/httpcore && GOWORK=off go test -race ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add providers/httpcore
git commit -m "feat(httpcore): Version and the conditional-GET Revalidator"
```

---

### Task 5: OAuth2 client-credentials authenticator

PR3 (HCP Vault Secrets) needs this, and building it here keeps every credential path in one module.

**Files:**
- Create: `providers/httpcore/oauth2.go`
- Test: `providers/httpcore/oauth2_test.go`

**Interfaces:**
- Consumes: `Authenticator` (Task 2), `Client`, `Config`, `New`, `Request` (Task 3), `ClassifyStatus` (Task 1)
- Produces:
  - `type OAuth2Config struct { TokenURL, ClientID, ClientSecret string; Scopes []string; Audience string; HTTPClient *http.Client; Leeway time.Duration; Now func() time.Time }`
  - `func OAuth2ClientCredentials(cfg OAuth2Config) (Authenticator, error)`
  - `const DefaultOAuth2Leeway = 30 * time.Second`

- [ ] **Step 1: Write the failing test**

Create `providers/httpcore/oauth2_test.go`:

```go
package httpcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// clientSecretMarker is the client secret used by every test in this file, and
// the needle the leak assertions search for. It is deliberately long and
// distinctive: a short secret like "sec" can match incidentally in unrelated
// text, and would not catch a leak that truncated the value.
const clientSecretMarker = "cs3cr3t-oauth-marker-4d1b"

// tokenServer returns a RoundTripper answering the token endpoint with a token
// valid for expiresIn seconds, counting how many exchanges it performed.
func tokenServer(t *testing.T, expiresIn int, exchanges *int) roundTripFunc {
	t.Helper()
	return func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/token" {
			resp, _ := newResponse(http.StatusOK, []byte("resource"), nil)
			return resp, nil
		}
		*exchanges++
		if err := req.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if got := req.PostForm.Get("grant_type"); got != "client_credentials" {
			t.Errorf("grant_type = %q", got)
		}
		body, _ := json.Marshal(map[string]any{
			"access_token": "at-" + req.PostForm.Get("client_id"),
			"token_type":   "Bearer",
			"expires_in":   expiresIn,
		})
		resp, _ := newResponse(http.StatusOK, body, nil)
		return resp, nil
	}
}

func TestOAuth2ClientCredentialsSetsBearer(t *testing.T) {
	exchanges := 0
	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		HTTPClient:   fakeClient(tokenServer(t, 3600, &exchanges)),
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}

	req := newTestRequest(t)
	if err := auth.Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at-cid" {
		t.Fatalf("Authorization = %q", got)
	}
	if exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1", exchanges)
	}
}

func TestOAuth2CachesToken(t *testing.T) {
	exchanges := 0
	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		HTTPClient:   fakeClient(tokenServer(t, 3600, &exchanges)),
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}
	for range 5 {
		if err := auth.Apply(context.Background(), newTestRequest(t)); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	if exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1 (token should be cached)", exchanges)
	}
}

func TestOAuth2RefreshesBeforeExpiry(t *testing.T) {
	exchanges := 0
	now := time.Unix(1_700_000_000, 0)
	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		Leeway:       30 * time.Second,
		HTTPClient:   fakeClient(tokenServer(t, 60, &exchanges)),
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}

	if err := auth.Apply(context.Background(), newTestRequest(t)); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1", exchanges)
	}

	// 40s later the token has 20s left, inside the 30s leeway, so it refreshes.
	now = now.Add(40 * time.Second)
	if err := auth.Apply(context.Background(), newTestRequest(t)); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if exchanges != 2 {
		t.Fatalf("exchanges = %d, want 2 (leeway should force a refresh)", exchanges)
	}
}

func TestOAuth2ClassifiesTokenFailure(t *testing.T) {
	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusUnauthorized, []byte(`{"error":"invalid_client"}`), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}
	err = auth.Apply(context.Background(), newTestRequest(t))
	if !errors.Is(err, mamori.ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
	// The client secret must never reach an error message.
	if strings.Contains(err.Error(), clientSecretMarker) {
		t.Fatalf("client secret leaked into %q", err.Error())
	}
}

// TestOAuth2FetchesLazily pins that constructing the authenticator performs no
// token exchange.
//
// Building a provider must never block on the network, and a misconfigured
// identity provider must surface as a classified resolve error rather than as a
// constructor failure at process start. Without this test, moving the exchange
// into OAuth2ClientCredentials would pass every other test in this file.
func TestOAuth2FetchesLazily(t *testing.T) {
	exchanges := 0
	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		HTTPClient:   fakeClient(tokenServer(t, 3600, &exchanges)),
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}
	if exchanges != 0 {
		t.Fatalf("exchanges = %d before the first Apply, want 0: construction must not touch the network", exchanges)
	}
	if err := auth.Apply(context.Background(), newTestRequest(t)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if exchanges != 1 {
		t.Fatalf("exchanges = %d after the first Apply, want 1", exchanges)
	}
}

func TestOAuth2RejectsMissingFields(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  OAuth2Config
	}{
		{"no token url", OAuth2Config{ClientID: "c", ClientSecret: clientSecretMarker}},
		{"no client id", OAuth2Config{TokenURL: "https://idp.test/token", ClientSecret: clientSecretMarker}},
		{"no client secret", OAuth2Config{TokenURL: "https://idp.test/token", ClientID: "c"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := OAuth2ClientCredentials(tt.cfg); err == nil {
				t.Fatal("OAuth2ClientCredentials returned nil error")
			}
		})
	}
}

// TestOAuth2WaiterReleasedByOwnContext pins that a caller blocked on someone
// else's in-flight exchange leaves when its OWN context expires.
//
// A plain mutex held across the network call would serialize correctly but
// ignore the waiter's ctx entirely, and mamori's reconciler is single-goroutine:
// one Apply wedged behind a hung identity provider would stall reconciliation
// for every field. The test hangs the token endpoint until released, so the
// waiter can only return by honouring its own deadline.
func TestOAuth2WaiterReleasedByOwnContext(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)

	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			resp, _ := newResponse(http.StatusOK,
				[]byte(`{"access_token":"at","token_type":"Bearer","expires_in":3600}`), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}

	// Owner: starts the exchange and blocks in the transport.
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		_ = auth.Apply(context.Background(), newTestRequest(t))
	}()
	<-entered

	// Waiter: arrives while the exchange is in flight, with a context that dies.
	ctx, cancel := context.WithCancel(context.Background())
	waiterErr := make(chan error, 1)
	go func() { waiterErr <- auth.Apply(ctx, newTestRequest(t)) }()
	cancel()

	select {
	case err := <-waiterErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not return after its own context was cancelled; it is blocked on the exchange")
	}

	close(release)
	<-ownerDone
}

// TestOAuth2DoesNotExposeSecretByReflection pins that the client secret is not
// reachable through fmt's reflection walk.
//
// The four authenticators in auth.go are immune because they capture their
// credentials in closures. This one holds state, so it must reach the same
// result deliberately: a secret in a struct field would print in cleartext from
// any %+v debug dump or panic trace, and fmt cannot call a String method on a
// value reached through an unexported field, so a redaction method would not
// save it.
func TestOAuth2DoesNotExposeSecretByReflection(t *testing.T) {
	exchanges := 0
	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		HTTPClient:   fakeClient(tokenServer(t, 3600, &exchanges)),
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if dump := fmt.Sprintf(verb, auth); strings.Contains(dump, clientSecretMarker) {
			t.Fatalf("client secret reachable via %s: %q", verb, dump)
		}
	}
}

func TestOAuth2ConcurrentApply(t *testing.T) {
	exchanges := 0
	var mu sync.Mutex
	auth, err := OAuth2ClientCredentials(OAuth2Config{
		TokenURL:     "https://idp.test/token",
		ClientID:     "cid",
		ClientSecret: clientSecretMarker,
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			return tokenServer(t, 3600, &exchanges)(req)
		}),
	})
	if err != nil {
		t.Fatalf("OAuth2ClientCredentials: %v", err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := auth.Apply(context.Background(), newTestRequest(t)); err != nil {
				t.Errorf("Apply: %v", err)
			}
		}()
	}
	wg.Wait()
	if exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1 under concurrency", exchanges)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd providers/httpcore && GOWORK=off go test ./... 2>&1 | head -20`
Expected: FAIL, `undefined: OAuth2ClientCredentials`

- [ ] **Step 3: Implement the OAuth2 authenticator**

Create `providers/httpcore/oauth2.go`:

```go
package httpcore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/mamori"
)

// DefaultOAuth2Leeway is how far before its stated expiry a cached token is
// treated as expired, so a request is not sent with a token that dies in flight.
const DefaultOAuth2Leeway = 30 * time.Second

// OAuth2Config configures the client-credentials grant.
type OAuth2Config struct {
	// TokenURL is the token endpoint. Required.
	TokenURL string
	// ClientID is the OAuth2 client identifier. Required.
	ClientID string
	// ClientSecret is the OAuth2 client secret. Required.
	ClientSecret string
	// Scopes is the optional scope list.
	Scopes []string
	// Audience is the optional audience parameter, which several providers
	// require.
	Audience string
	// HTTPClient performs the token exchange. When nil, a client with
	// DefaultTimeout is used.
	HTTPClient *http.Client
	// Leeway overrides DefaultOAuth2Leeway.
	Leeway time.Duration
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// oauth2Auth caches one access token and refreshes it before expiry.
//
// It deliberately does NOT retain the OAuth2Config. fmt's %v, %+v and %#v walk
// unexported struct fields through reflection, and cannot call a String method
// on a value reached that way, so a ClientSecret held in a field here would
// print in cleartext from any debug dump or panic trace. The four authenticators
// in auth.go are immune because they capture their credentials in closures, and
// this one does the same: the encoded form body, secret included, lives only
// inside the form closure, which reflection renders as a function pointer.
type oauth2Auth struct {
	client   *Client
	form     func() []byte
	clientID string
	leeway   time.Duration
	now      func() time.Time

	mu        sync.Mutex
	token     string
	expiresAt time.Time
	inflight  *tokenFetch
}

// tokenFetch is one in-flight token exchange, shared by every caller that
// arrives while it runs. done is closed once token and err are final, so a
// waiter that reads them after receiving from done sees settled values.
type tokenFetch struct {
	done  chan struct{}
	token string
	err   error
}

// tokenResponse is the subset of RFC 6749 section 5.1 this needs.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

// OAuth2ClientCredentials returns an Authenticator that performs the RFC 6749
// client-credentials grant, caches the access token, and refreshes it before it
// expires.
//
// The token is fetched lazily on the first Apply rather than at construction,
// so building a provider never blocks on the network and a misconfigured
// identity provider surfaces as a classified resolve error rather than a
// constructor failure.
//
// The client secret is never logged and never appears in a returned error.
func OAuth2ClientCredentials(cfg OAuth2Config) (Authenticator, error) {
	switch {
	case cfg.TokenURL == "":
		return nil, fmt.Errorf("httpcore: OAuth2 TokenURL is required: %w", mamori.ErrInvalid)
	case cfg.ClientID == "":
		return nil, fmt.Errorf("httpcore: OAuth2 ClientID is required: %w", mamori.ErrInvalid)
	case cfg.ClientSecret == "":
		return nil, fmt.Errorf("httpcore: OAuth2 ClientSecret is required: %w", mamori.ErrInvalid)
	}

	client, err := New(Config{BaseURL: cfg.TokenURL, HTTPClient: cfg.HTTPClient})
	if err != nil {
		return nil, fmt.Errorf("httpcore: OAuth2 TokenURL: %w", err)
	}

	leeway := cfg.Leeway
	if leeway <= 0 {
		leeway = DefaultOAuth2Leeway
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	// Encode the form once, here, so the client secret is captured by the
	// closure below and never stored in a struct field where reflection could
	// reach it. See the note on oauth2Auth.
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
	}
	if len(cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	if cfg.Audience != "" {
		form.Set("audience", cfg.Audience)
	}
	encoded := form.Encode()

	return &oauth2Auth{
		client:   client,
		form:     func() []byte { return []byte(encoded) },
		clientID: cfg.ClientID,
		leeway:   leeway,
		now:      now,
	}, nil
}

// Apply sets the Authorization header, exchanging for a new token when the
// cached one is missing or within Leeway of expiry.
func (a *oauth2Auth) Apply(ctx context.Context, req *http.Request) error {
	tok, err := a.tokenFor(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

// tokenFor returns a live token. Concurrent callers perform ONE exchange rather
// than one each, and a caller waiting on someone else's exchange is released by
// its own context.
//
// The lock is never held across the network call. A plain mutex would serialize
// callers correctly, which is what golang.org/x/oauth2 does, but sync.Mutex has
// no context-aware Lock, so a waiter is not woken by its own ctx expiring. That
// matters here more than it does for a general OAuth2 client: mamori's
// reconciler is single-goroutine, so an Apply wedged behind a hung identity
// provider would stall reconciliation for every field, not just the one being
// resolved. Instead the first caller publishes an inflight tokenFetch and the
// rest select on it against their own ctx.
func (a *oauth2Auth) tokenFor(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.token != "" && a.now().Add(a.leeway).Before(a.expiresAt) {
		tok := a.token
		a.mu.Unlock()
		return tok, nil
	}
	if f := a.inflight; f != nil {
		a.mu.Unlock()
		select {
		case <-f.done:
			return f.token, f.err
		case <-ctx.Done():
			return "", fmt.Errorf("httpcore: OAuth2 token exchange for client %q: %w: %w",
				a.clientID, mamori.ErrUnavailable, ctx.Err())
		}
	}
	f := &tokenFetch{done: make(chan struct{})}
	a.inflight = f
	a.mu.Unlock()

	tok, expires, err := a.exchange(ctx)

	a.mu.Lock()
	a.inflight = nil
	if err == nil {
		a.token, a.expiresAt = tok, expires
	}
	a.mu.Unlock()

	// Settle the result before closing, so a waiter reading after <-f.done sees
	// final values rather than racing these writes.
	f.token, f.err = tok, err
	close(f.done)
	return tok, err
}

// exchange performs one client-credentials round trip and returns the token with
// the instant it expires. It touches no shared state, so it runs outside the lock.
func (a *oauth2Auth) exchange(ctx context.Context) (token string, expiresAt time.Time, err error) {
	resp, err := a.client.Do(ctx, Request{
		Method: http.MethodPost,
		Header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}},
		Body:   a.form(),
	})
	if err != nil {
		// Do already classified the status. Restate it as an authentication
		// failure without repeating the cause, so the message cannot accumulate
		// anything derived from the secret.
		return "", time.Time{}, fmt.Errorf("httpcore: OAuth2 token exchange for client %q failed: %w", a.clientID, err)
	}

	var tr tokenResponse
	if err := json.Unmarshal(resp.Body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("httpcore: OAuth2 token response is not JSON: %w: %w", mamori.ErrInvalid, err)
	}
	if tr.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("httpcore: OAuth2 token response carried no access_token: %w", mamori.ErrUnauthenticated)
	}

	if tr.ExpiresIn > 0 {
		return tr.AccessToken, a.now().Add(time.Duration(tr.ExpiresIn) * time.Second), nil
	}
	// No expires_in means the server did not commit to a lifetime. Treat it as
	// good for two leeway windows so it is re-fetched often rather than cached
	// forever.
	return tr.AccessToken, a.now().Add(a.leeway * 2), nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd providers/httpcore && GOWORK=off go test -race ./...`
Expected: PASS

- [ ] **Step 5: Verify the token exchange error does not carry the 401 body**

Run: `cd providers/httpcore && GOWORK=off go test -run TestOAuth2ClassifiesTokenFailure -v ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add providers/httpcore/oauth2.go providers/httpcore/oauth2_test.go
git commit -m "feat(httpcore): OAuth2 client-credentials authenticator with token caching"
```

---

### Task 6: httpcore README

**Files:**
- Create: `providers/httpcore/README.md`

**Interfaces:**
- Consumes: every exported symbol from Tasks 1 through 5
- Produces: nothing consumed by code

- [ ] **Step 1: Write the README**

Create `providers/httpcore/README.md`. Follow the structure of `providers/cloudflare-kv/README.md`: a one-paragraph intro, the import line, then these sections.

Required sections and their content:

1. **Intro.** One paragraph: this is the shared HTTP resolve core for mamori providers whose backend is a REST API, extracted because sixteen providers hand-rolled it and issue #107 was that duplication surfacing as a bug.
2. **`## Install`** with `go get github.com/xavidop/mamori/providers/httpcore`.
3. **`## Client`** showing `New` and `Do` with a complete, compiling example that resolves one value.
4. **`## Authenticators`** with a table of `Bearer`, `HeaderAuth`, `BasicAuth`, `QueryAuth`, `OAuth2ClientCredentials`, each with one line on when to use it. State that `QueryAuth` puts the credential in the request line where proxy logs can see it, and to prefer a header form.
5. **`## Error classification`** with the exact status table from `classify.go`, and one paragraph on why `detail` is caller-supplied: a response body can contain the resolved value, so `httpcore` never reads one into an error. Document `StatusForKind` here as the exported inverse, stating that it exists so a provider's conformance `Fail` hook turns an injected sentinel back into a status, and that hand-rolling it per provider is what lets the two tables drift.
6. **`## Conditional GET`** explaining `Revalidator`, the LRU bound, and that a 304 returns the cached body with `NotModified` set.
7. **`## What this package does not do`**: no retry (mamori's reconciler owns it, and a second layer multiplies), no vendor error-envelope parsing, no SSE (planned separately).
8. **`## Writing a provider on httpcore`** with a complete example implementing `mamori.Provider` end to end, including `Scheme`, `Resolve`, `mamori.SelectKey`, and `Version`.
9. **`## Development`** with `GOWORK=off go mod tidy && GOWORK=off go test ./...`.

Every code block must compile. Do not write a snippet with elided imports.

- [ ] **Step 2: Verify every example compiles**

Create `providers/httpcore/example_test.go` holding one `Example` function per
README code block, each body copied verbatim from the README. A compiling
`example_test.go` is what proves the README is not aspirational, and `go vet`
checks the naming.

```go
package httpcore_test

import (
	"context"
	"fmt"

	"github.com/xavidop/mamori/providers/httpcore"
)

// ExampleNew is the README's "Client" block, verbatim.
func ExampleNew() {
	c, err := httpcore.New(httpcore.Config{
		BaseURL: "https://api.example.com/v1",
		Auth:    httpcore.Bearer("token"),
	})
	if err != nil {
		panic(err)
	}
	_ = c
	fmt.Println("ready")
	// Output: ready
}
```

Add one such function per block, then run:

Run: `cd providers/httpcore && GOWORK=off go test ./... && GOWORK=off go vet ./...`
Expected: PASS, no vet output. If a block does not compile, fix the README, not the code.

- [ ] **Step 3: Commit**

```bash
git add providers/httpcore/README.md
git commit -m "docs(httpcore): module README"
```

---

### Task 7: https module scaffold, Endpoint and New

**Files:**
- Create: `providers/https/go.mod`
- Create: `providers/https/https.go`
- Test: `providers/https/https_test.go`
- Modify: `go.work`

**Interfaces:**
- Consumes: `httpcore.Authenticator`, `httpcore.Client`, `httpcore.Config`, `httpcore.New`, `httpcore.NewRevalidator`, `httpcore.Revalidator`
- Produces:
  - `type Endpoint struct { Name, BaseURL string; Auth httpcore.Authenticator; Query url.Values; Header http.Header; Client *http.Client; MaxBody int64; Sensitive, AllowInsecure bool }`
  - `type Provider struct{ ... }`
  - `func New(endpoints ...Endpoint) (*Provider, error)`
  - `func (p *Provider) Scheme() string` returning `"https"`

- [ ] **Step 1: Create the module**

```bash
mkdir -p providers/https
cat > providers/https/go.mod <<'EOF'
module github.com/xavidop/mamori/providers/https

go 1.26.0

require (
	github.com/xavidop/mamori v0.1.0
	github.com/xavidop/mamori/providers/httpcore v0.1.0
)

replace github.com/xavidop/mamori => ../..

replace github.com/xavidop/mamori/providers/httpcore => ../httpcore
EOF
```

- [ ] **Step 2: Add the module to the workspace**

Edit `go.work`, inserting `./providers/https` into the `use (` block. Both new
modules share the `http` prefix, and the fifth byte decides the order: `c` in
`httpcore` sorts before `s` in `https`. So the block reads

```text
	./providers/growthbook
	./providers/httpcore
	./providers/https
	./providers/k8s
```

- [ ] **Step 3: Write the failing test**

Create `providers/https/https_test.go`:

```go
package https

import (
	"errors"
	"testing"

	"github.com/xavidop/mamori"
)

func TestNewRejectsNoEndpoints(t *testing.T) {
	if _, err := New(); err == nil {
		t.Fatal("New with no endpoints returned nil error")
	}
}

func TestNewRejectsUnnamedEndpoint(t *testing.T) {
	_, err := New(Endpoint{BaseURL: "https://api.test"})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestNewRejectsDuplicateNames(t *testing.T) {
	_, err := New(
		Endpoint{Name: "a", BaseURL: "https://one.test"},
		Endpoint{Name: "a", BaseURL: "https://two.test"},
	)
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestNewRejectsNameWithSlash(t *testing.T) {
	// The name is the ref authority, so a slash would make the ref ambiguous
	// with the path that follows it.
	_, err := New(Endpoint{Name: "a/b", BaseURL: "https://api.test"})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// TestNewRejectsUnparsableBaseURL covers New's url.Parse branch.
//
// url.Parse is permissive, so most malformed input slips through it and is
// caught later by httpcore.New's scheme and host checks instead. An invalid
// percent escape is one of the few things it genuinely rejects, which makes it
// the input that actually exercises this branch rather than a neighbouring one.
func TestNewRejectsUnparsableBaseURL(t *testing.T) {
	_, err := New(Endpoint{Name: "a", BaseURL: "https://%zz"})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// TestNewRejectsNonHTTPScheme pins that the scheme is checked against a closed
// set, not merely tested for http://.
//
// An ftp:// typo or a ws:// paste satisfies both this provider's insecure-scheme
// gate and httpcore.New's scheme-and-host check, so without this the endpoint
// constructs cleanly and then fails on every resolve with net/http's
// "unsupported protocol scheme". That is the resolve-time failure New exists to
// prevent.
func TestNewRejectsNonHTTPScheme(t *testing.T) {
	for _, base := range []string{"ftp://api.test", "ws://api.test", "file:///etc/config"} {
		t.Run(base, func(t *testing.T) {
			_, err := New(Endpoint{Name: "a", BaseURL: base})
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("New(%q) err = %v, want ErrInvalid", base, err)
			}
		})
	}
}

// TestAllowInsecureDoesNotRescueOtherSchemes pins the scope of AllowInsecure: it
// permits cleartext http, and nothing else. Reading it as a general "skip the
// scheme check" switch would reopen exactly the hole TestNewRejectsNonHTTPScheme
// closes.
func TestAllowInsecureDoesNotRescueOtherSchemes(t *testing.T) {
	_, err := New(Endpoint{Name: "a", BaseURL: "ftp://api.test", AllowInsecure: true})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid; AllowInsecure rescued a non-http scheme", err)
	}
}

// TestNewRejectsEmptyBaseURL pins that an omitted BaseURL fails at construction
// with this provider's own message, rather than reaching httpcore.New.
func TestNewRejectsEmptyBaseURL(t *testing.T) {
	_, err := New(Endpoint{Name: "a"})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestNewRejectsInsecureBaseURL(t *testing.T) {
	_, err := New(Endpoint{Name: "a", BaseURL: "http://api.test"})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestNewAllowsInsecureWhenOptedIn(t *testing.T) {
	if _, err := New(Endpoint{Name: "a", BaseURL: "http://api.test", AllowInsecure: true}); err != nil {
		t.Fatalf("New with AllowInsecure: %v", err)
	}
}

func TestScheme(t *testing.T) {
	p, err := New(Endpoint{Name: "a", BaseURL: "https://api.test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.Scheme(); got != "https" {
		t.Fatalf("Scheme = %q, want https", got)
	}
}

// TestProviderIsNotWatchable pins the deliberate absence of a native watch, so
// removing that decision cannot happen silently.
//
// This test only discriminates once Resolve exists, which Task 8 adds:
// WatchableProvider embeds Provider, so before Resolve is written the type
// cannot satisfy the interface no matter what else is added to it, and adding a
// Watch method alone would not fail this. Re-run the mutation after Task 8.
func TestProviderIsNotWatchable(t *testing.T) {
	p, err := New(Endpoint{Name: "a", BaseURL: "https://api.test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := any(p).(mamori.WatchableProvider); ok {
		t.Fatal("Provider implements WatchableProvider; a generic HTTP endpoint has no push channel, so mamori must poll it")
	}
}
```

- [ ] **Step 4: Run the test to verify it fails**

Run: `cd providers/https && GOWORK=off go test ./... 2>&1 | head -20`
Expected: FAIL, `undefined: New`

- [ ] **Step 5: Implement https.go**

Create `providers/https/https.go`:

```go
// Package https implements a generic mamori provider for configuration and
// secrets served over HTTP by an endpoint you declare.
//
// # Scheme
//
//	https://<endpoint>/<path>[#<key>][?<opts>]
//
// <endpoint> is not a hostname. It is the Name of an Endpoint registered with
// [New], and a ref naming an unregistered endpoint fails with mamori.ErrInvalid
// so `mamori doctor` catches the typo before deployment.
//
// Three problems forced that design, and it solves all three.
//
// A ref cannot carry target query parameters. mamori's grammar is
// scheme://path[#key][?opts] with the fragment BEFORE the query, and ?opts is
// mamori's own namespace for decode and debounce. A ref written
// "https://api.example.com/cfg?env=prod#/db/pass" does not fail loudly: ParseRef
// splits on the first '?', so Key comes out empty and Opts comes out as
// env="prod#/db/pass". Fixed query parameters therefore live on the Endpoint.
//
// A ref cannot carry credentials, because a struct tag is source code. Auth
// lives on the Endpoint, where it can be read from the environment at startup.
//
// A provider that fetched an arbitrary URL would make every struct tag a
// potential SSRF. Restricting refs to declared endpoints matches the posture the
// rest of mamori takes: the config server serves a fixed, operator-declared
// binding table and never a client-supplied ref, and exec: is opt-in for the
// same class of reason.
//
// # Watching
//
// A generic HTTP endpoint exposes no push channel, so this provider
// deliberately does not implement mamori.WatchableProvider and mamori wraps it
// in the polling adapter. Each poll goes through httpcore.Revalidator, so it is
// a conditional GET: an unchanged value costs a 304 rather than a full payload.
package https

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// scheme is the ref scheme this provider registers.
const scheme = "https"

// Endpoint is one operator-declared backend. Its Name is what a ref's authority
// selects.
type Endpoint struct {
	// Name is the ref authority, e.g. "billing" in https://billing/cfg. It must
	// be non-empty and must not contain '/'. Required.
	Name string
	// BaseURL is the root every ref path is joined onto. Required. An http://
	// BaseURL is rejected unless AllowInsecure is set.
	BaseURL string
	// Auth injects credentials. Nil sends requests unauthenticated.
	Auth httpcore.Authenticator
	// Query is merged into every request to this endpoint. It exists because a
	// ref cannot carry target query parameters; see the package documentation.
	Query url.Values
	// Header is merged into every request to this endpoint.
	Header http.Header
	// Client performs the round trips. Nil selects httpcore's default.
	Client *http.Client
	// MaxBody caps the response size. Zero selects httpcore.DefaultMaxBody.
	MaxBody int64
	// Sensitive marks every Value from this endpoint as secret, driving
	// redaction downstream. It is per-endpoint because a generic HTTP endpoint
	// may serve either secrets or plain configuration and mamori cannot infer
	// which.
	Sensitive bool
	// AllowInsecure permits an http:// BaseURL. Fetching configuration in
	// cleartext exposes it to anything on the path, so it must be opted into.
	AllowInsecure bool
}

// endpoint is a validated Endpoint with its client and revalidator built.
type endpoint struct {
	name      string
	query     url.Values
	header    http.Header
	sensitive bool
	reval     *httpcore.Revalidator
}

// Provider resolves https:// refs against registered endpoints. It is safe for
// concurrent use.
type Provider struct {
	endpoints map[string]*endpoint
}

// New validates endpoints and returns a Provider. Register it with
// mamori.WithProvider or mamori.Register.
//
// It fails when no endpoint is supplied, when a Name is empty, duplicated, or
// contains '/', when a BaseURL is missing or unparsable, or when a BaseURL is
// http:// without AllowInsecure. Every one of those is a startup failure rather
// than a resolve failure, so a misconfiguration cannot reach production as an
// intermittent error.
func New(endpoints ...Endpoint) (*Provider, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("https: at least one Endpoint is required: %w", mamori.ErrInvalid)
	}

	out := make(map[string]*endpoint, len(endpoints))
	for _, e := range endpoints {
		switch {
		case e.Name == "":
			return nil, fmt.Errorf("https: Endpoint.Name is required: %w", mamori.ErrInvalid)
		case strings.Contains(e.Name, "/"):
			return nil, fmt.Errorf("https: Endpoint.Name %q must not contain '/', it is the ref authority: %w", e.Name, mamori.ErrInvalid)
		}
		if _, dup := out[e.Name]; dup {
			return nil, fmt.Errorf("https: duplicate Endpoint.Name %q: %w", e.Name, mamori.ErrInvalid)
		}

		if e.BaseURL == "" {
			return nil, fmt.Errorf("https: endpoint %q BaseURL is required: %w", e.Name, mamori.ErrInvalid)
		}
		u, err := url.Parse(e.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("https: endpoint %q BaseURL %q is not a URL: %w: %w", e.Name, e.BaseURL, mamori.ErrInvalid, err)
		}
		// The scheme is checked against a closed set rather than only rejecting
		// http://. Anything else, an ftp:// typo or a ws:// paste, otherwise
		// passes here AND passes httpcore.New, which only requires a scheme and
		// a host, and then fails on every single resolve with net/http's
		// "unsupported protocol scheme". New exists precisely so a
		// misconfiguration cannot reach production as a resolve-time failure.
		switch u.Scheme {
		case "https":
		case "http":
			if !e.AllowInsecure {
				return nil, fmt.Errorf("https: endpoint %q BaseURL is http://, which sends configuration in cleartext; set AllowInsecure to accept that: %w", e.Name, mamori.ErrInvalid)
			}
		default:
			return nil, fmt.Errorf("https: endpoint %q BaseURL scheme %q is not http or https: %w", e.Name, u.Scheme, mamori.ErrInvalid)
		}

		client, err := httpcore.New(httpcore.Config{
			BaseURL:    e.BaseURL,
			HTTPClient: e.Client,
			Auth:       e.Auth,
			MaxBody:    e.MaxBody,
			UserAgent:  "mamori-https",
		})
		if err != nil {
			return nil, fmt.Errorf("https: endpoint %q: %w", e.Name, err)
		}

		out[e.Name] = &endpoint{
			name:      e.Name,
			query:     e.Query,
			header:    e.Header,
			sensitive: e.Sensitive,
			reval:     httpcore.NewRevalidator(client, 0),
		}
	}
	return &Provider{endpoints: out}, nil
}

// Scheme returns "https".
func (p *Provider) Scheme() string { return scheme }
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd providers/https && GOWORK=off go mod tidy && GOWORK=off go test ./...`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add providers/https go.work
git commit -m "feat(https): module scaffold with operator-declared endpoints"
```

---

### Task 8: https Resolve

**Files:**
- Create: `providers/https/resolve.go`
- Test: `providers/https/resolve_test.go`
- Test: `providers/https/fake_test.go`

**Interfaces:**
- Consumes: `Provider`, `endpoint` (Task 7), `httpcore.Request`, `httpcore.Version`, `mamori.SelectKey`, `mamori.Value`, `mamori.Ref`
- Produces: `func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error)`

- [ ] **Step 1: Write the fake**

Create `providers/https/fake_test.go`:

```go
package https

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"sync"
)

// fakeBackend is an in-process HTTP backend. An httptest.Server is deliberately
// not used: the conformance kit's NoGoroutineLeak case snapshots goroutines with
// goleak.IgnoreCurrent and a live server's accept goroutine does not survive it.
type fakeBackend struct {
	mu         sync.Mutex
	values     map[string][]byte
	etags      map[string]string
	failStatus int
	seq        int
	lastPath   string
	lastQuery  string
	lastHeader http.Header
}

// newFake returns an empty backend.
func newFake() *fakeBackend {
	return &fakeBackend{values: map[string][]byte{}, etags: map[string]string{}}
}

// set writes a value and advances its ETag, so a conditional GET sees a change.
func (f *fakeBackend) set(path string, val []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.values[path] = val
	f.etags[path] = `"v` + strconv.Itoa(f.seq) + `"`
}

// fail makes every request answer status until clearFail.
func (f *fakeBackend) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus = status
}

// clearFail cancels fail.
func (f *fakeBackend) clearFail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus = 0
}

// transport returns an http.RoundTripper serving this backend.
func (f *fakeBackend) transport() http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()

		// EscapedPath, not Path: Path is the decoded form, so a percent-encoded
		// traversal would look like a real one here even though the wire never
		// carried it.
		f.lastPath = req.URL.EscapedPath()
		f.lastQuery = req.URL.RawQuery
		f.lastHeader = req.Header.Clone()

		if f.failStatus != 0 {
			return newResp(f.failStatus, nil, ""), nil
		}
		val, ok := f.values[req.URL.Path]
		if !ok {
			return newResp(http.StatusNotFound, nil, ""), nil
		}
		etag := f.etags[req.URL.Path]
		if req.Header.Get("If-None-Match") == etag {
			return newResp(http.StatusNotModified, nil, etag), nil
		}
		return newResp(http.StatusOK, val, etag), nil
	})
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// newResp builds a response with an optional ETag.
func newResp(status int, body []byte, etag string) *http.Response {
	h := http.Header{}
	if etag != "" {
		h.Set("ETag", etag)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

```

- [ ] **Step 2: Write the failing test**

Create `providers/https/resolve_test.go`:

```go
package https

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// newTestProvider builds a Provider over f with the given endpoint options.
func newTestProvider(t *testing.T, f *fakeBackend, mutate func(*Endpoint)) *Provider {
	t.Helper()
	e := Endpoint{
		Name:    "billing",
		BaseURL: "https://api.test/v1",
		Client:  &http.Client{Transport: f.transport()},
	}
	if mutate != nil {
		mutate(&e)
	}
	p, err := New(e)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// mustRef parses a ref or fails the test.
func mustRef(t *testing.T, s string) mamori.Ref {
	t.Helper()
	r, err := mamori.ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return r
}

func TestResolveReturnsBody(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte(`{"region":"eu-west-1"}`))
	p := newTestProvider(t, f, nil)

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != `{"region":"eu-west-1"}` {
		t.Fatalf("Bytes = %q", v.Bytes)
	}
	if v.Version == "" {
		t.Fatal("Version is empty; change detection needs one")
	}
}

func TestResolveSelectsWithPointer(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte(`{"db":{"pass":"s3cr3t"}}`))
	p := newTestProvider(t, f, nil)

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg#/db/pass"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "s3cr3t" {
		t.Fatalf("Bytes = %q, want s3cr3t", v.Bytes)
	}
}

func TestResolveSelectsTopLevelKey(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte(`{"log.level":"debug"}`))
	p := newTestProvider(t, f, nil)

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg#log.level"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("Bytes = %q, want debug", v.Bytes)
	}
}

func TestResolveUnknownEndpoint(t *testing.T) {
	f := newFake()
	p := newTestProvider(t, f, nil)

	_, err := p.Resolve(context.Background(), mustRef(t, "https://nope/cfg"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	// An unknown endpoint must NOT be ErrNotFound: that would silently apply the
	// field's default instead of reporting the misconfiguration.
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("unknown endpoint reported as ErrNotFound, which would hide it behind a default")
	}
}

func TestResolveMissingValueIsNotFound(t *testing.T) {
	f := newFake()
	p := newTestProvider(t, f, nil)

	_, err := p.Resolve(context.Background(), mustRef(t, "https://billing/absent"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveMergesEndpointQueryAndHeader(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte(`{}`))
	p := newTestProvider(t, f, func(e *Endpoint) {
		e.Query = url.Values{"env": {"prod"}}
		e.Header = http.Header{"X-Tenant": {"acme"}}
	})

	if _, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if f.lastQuery != "env=prod" {
		t.Fatalf("query = %q, want env=prod", f.lastQuery)
	}
	if got := f.lastHeader.Get("X-Tenant"); got != "acme" {
		t.Fatalf("X-Tenant = %q, want acme", got)
	}
}

func TestResolveMarksSensitive(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte("s3cr3t"))
	p := newTestProvider(t, f, func(e *Endpoint) { e.Sensitive = true })

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Fatal("Sensitive = false, want true for a Sensitive endpoint")
	}
}

func TestResolveUsesConditionalGetOnSecondCall(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte("payload"))
	p := newTestProvider(t, f, nil)

	first, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	second, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if f.lastHeader.Get("If-None-Match") == "" {
		t.Fatal("second Resolve sent no If-None-Match; the poll is not conditional")
	}
	if string(second.Bytes) != string(first.Bytes) {
		t.Fatalf("second Bytes = %q, want the cached %q", second.Bytes, first.Bytes)
	}
	if second.Version != first.Version {
		t.Fatalf("Version changed across an unmodified poll: %q then %q", first.Version, second.Version)
	}
}

// TestResolveRejectsDotSegments pins that a ref path cannot escape the path
// prefix its endpoint declares.
//
// This is reachable, not theoretical: expandRefVars substitutes ${VAR} from
// WithRefVars, whose values the application supplies at runtime, so a ref of
// https://billing/${TENANT}/cfg carries whatever TENANT holds. For an endpoint
// scoped to a tenant prefix, "../.." reaches another tenant's configuration
// without ever leaving the declared host, so the endpoint check that exists to
// contain exactly this never fires.
func TestResolveRejectsDotSegments(t *testing.T) {
	paths := []string{
		"../secrets", "a/../../b", "./cfg", "a/./b",
		// Backslash separators. Splitting on '/' alone leaves these as one
		// segment matching neither "." nor "..", so the check passes and the
		// request goes out with the backslashes percent encoded as %5C. IIS and
		// ASP.NET decode that and honour '\' as a directory separator, which is
		// the classic backslash traversal bypass, and BaseURL is operator
		// supplied with no platform restriction.
		`..\secrets`, `a\..\..\secrets`, `a/..\b`,
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			f := newFake()
			p := newTestProvider(t, f, nil)

			_, err := p.Resolve(context.Background(), mustRef(t, "https://billing/"+path))
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("Resolve(%q) err = %v, want ErrInvalid", path, err)
			}
			if errors.Is(err, mamori.ErrNotFound) {
				t.Fatalf("Resolve(%q) reported ErrNotFound, which would hide the traversal behind a field default", path)
			}
		})
	}
}

// TestResolveAllowsBackslashInAnOrdinaryKey pins the scope of the backslash
// rule: a backslash is treated as a separator when looking for dot segments,
// but a key that merely contains one is still an ordinary key. Rejecting every
// backslash outright would be simpler and would break a legitimate
// Windows-style key name on a generic HTTP backend.
func TestResolveAllowsBackslashInAnOrdinaryKey(t *testing.T) {
	f := newFake()
	f.set(`/v1/a\b`, []byte("payload"))
	p := newTestProvider(t, f, nil)

	v, err := p.Resolve(context.Background(), mustRef(t, `https://billing/a\b`))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "payload" {
		t.Fatalf("Bytes = %q, want payload", v.Bytes)
	}
}

// TestResolveDoesNotDecodeEscapedDotSegments pins the other half: a
// percent-encoded traversal needs no separate check, because ParseRef does not
// decode escapes and url.URL.String re-encodes the percent sign. The request
// must reach the backend as an ordinary, non-traversing path.
func TestResolveDoesNotDecodeEscapedDotSegments(t *testing.T) {
	f := newFake()
	p := newTestProvider(t, f, nil)

	// Not found is the expected outcome: the point is that it is treated as a
	// literal key rather than resolved into a parent directory.
	_, err := p.Resolve(context.Background(), mustRef(t, "https://billing/%2e%2e/secrets"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if strings.Contains(f.lastPath, "../") {
		t.Fatalf("request path %q traversed; the escape was decoded somewhere", f.lastPath)
	}
}

// TestResolvePassesThroughUnknownOptions pins the DecodeOption conformance
// requirement: decoding is core's job, so the provider must not touch ?decode=.
func TestResolvePassesThroughUnknownOptions(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte("cGF5bG9hZA=="))
	p := newTestProvider(t, f, nil)

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg?decode=base64"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "cGF5bG9hZA==" {
		t.Fatalf("Bytes = %q; the provider decoded the value, which is core's job", v.Bytes)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd providers/https && GOWORK=off go test ./... 2>&1 | head -20`
Expected: FAIL, `p.Resolve undefined`

- [ ] **Step 4: Implement Resolve**

Create `providers/https/resolve.go`:

```go
package https

import (
	"context"
	"fmt"
	"strings"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// Resolve fetches ref's value from its endpoint.
//
// The ref path's first segment is the endpoint name and the remainder is the
// request path. An unknown endpoint wraps mamori.ErrInvalid rather than
// mamori.ErrNotFound: ErrNotFound is the one kind that makes mamori apply a
// field's default, and a misconfigured ref must never be hidden behind one.
//
// ref.Key is handed to mamori.SelectKey, so a fragment is either an RFC 6901
// JSON Pointer or a literal top-level key, identically to every other provider.
//
// Query options are passed through untouched. Decoding a resolved value is
// core's job, not a provider's.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	name, path := splitEndpoint(ref.Path)
	ep, ok := p.endpoints[name]
	if !ok {
		return mamori.Value{}, fmt.Errorf("https: no endpoint named %q is registered (ref %q): %w", name, ref.Raw, mamori.ErrInvalid)
	}
	if hasDotSegment(path) {
		return mamori.Value{}, fmt.Errorf("https: ref %q path contains a dot segment, which would escape endpoint %q's BaseURL: %w", ref.Raw, name, mamori.ErrInvalid)
	}

	resp, err := ep.reval.Get(ctx, ref.Raw, httpcore.Request{
		Path:   path,
		Query:  ep.query,
		Header: ep.header,
	})
	if err != nil {
		return mamori.Value{}, fmt.Errorf("https: endpoint %q: %w", name, err)
	}

	body, err := mamori.SelectKey(resp.Body, ref.Key)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("https: endpoint %q: %w", name, err)
	}

	return mamori.Value{
		Bytes:     body,
		Version:   httpcore.Version(resp, resp.Body),
		Sensitive: ep.sensitive,
	}, nil
}

// splitEndpoint splits a ref path into its endpoint name and the remaining
// request path. "billing/cfg/main" yields ("billing", "cfg/main"), and
// "billing" alone yields ("billing", "").
func splitEndpoint(refPath string) (name, path string) {
	trimmed := strings.TrimPrefix(refPath, "/")
	name, path, _ = strings.Cut(trimmed, "/")
	return name, path
}

// hasDotSegment reports whether p contains a "." or ".." path segment.
//
// Such a segment is rejected rather than cleaned. httpcore's joinPath does not
// resolve dot segments, so "../.." in a ref path would reach outside the path
// prefix its endpoint declares: for an endpoint scoped to
// https://api/v1/tenants/acme, that is another tenant's configuration, reached
// without ever leaving the declared host and therefore without tripping the
// endpoint check that exists to contain exactly this.
//
// It is reachable rather than theoretical, because a ref path is not only what
// the struct tag says. expandRefVars substitutes ${VAR} from WithRefVars
// (decode.go), whose values the application supplies at runtime, so a ref of
// https://billing/${TENANT}/cfg carries whatever TENANT holds.
//
// Rejecting is the loud option. Cleaning silently would change which value a ref
// names, and a ref that quietly means something other than it says is worse than
// one that fails. mamori doctor resolves every ref before deployment, so this
// surfaces there rather than in production.
//
// Backslash counts as a separator here, not only '/'. Splitting on '/' alone
// leaves `a\..\..\secrets` as a single segment that matches neither "." nor
// "..", so the check passes and the request goes out. url.URL.String percent
// encodes the backslashes, so the wire carries "%5C", which most backends treat
// as an ordinary character. IIS and ASP.NET are the well known exceptions: they
// decode it and honour '\' as a directory separator, which is the classic
// backslash traversal bypass. Endpoint.BaseURL is operator supplied with no
// platform restriction, so this package cannot assume the backend is not one of
// them. Splitting on both keeps `a\b` usable as an ordinary key while refusing
// `a\..\b`.
//
// The percent-encoded form needs no separate check: mamori's ParseRef does not
// decode escapes, so "%2e%2e" stays literal, and url.URL.String re-encodes the
// percent sign, leaving the backend with "%252e%252e" rather than a traversal.
// TestResolveRejectsDotSegments pins both halves of that.
//
// Segments of three or more dots are deliberately not matched. RFC 3986 section
// 5.2.4 defines dot-segment removal over exactly "." and "..", so "..." is an
// ordinary segment name rather than a traversal.
func hasDotSegment(p string) bool {
	isSep := func(r rune) bool { return r == '/' || r == '\\' }
	for _, seg := range strings.FieldsFunc(p, isSep) {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd providers/https && GOWORK=off go test -race ./...`
Expected: PASS

- [ ] **Step 6: Confirm the Version stays stable across a 304**

The `TestResolveUsesConditionalGetOnSecondCall` assertion on `Version` is the one that catches a regression where `Revalidator` returns the cached body but a fresh `Response` without its ETag, which would make mamori see a change on every poll.

Run: `cd providers/https && GOWORK=off go test -run TestResolveUsesConditionalGetOnSecondCall -v ./...`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add providers/https
git commit -m "feat(https): resolve refs against registered endpoints"
```

---

### Task 9: Conformance, error classification and integration tests

**Files:**
- Create: `providers/https/conformance_test.go`
- Create: `providers/https/errors_test.go`
- Create: `providers/https/https_integration_test.go`
- Modify: `providers/https/fake_test.go` (add `provider()` helper)

**Interfaces:**
- Consumes: `providertest.Run`, `providertest.Config`, `Provider`, `fakeBackend`, `httpcore.StatusForKind` (Task 1), `mamori.ErrorKind`
- Produces: nothing consumed by code

- [ ] **Step 1: Add the provider helper to the fake**

Append to `providers/https/fake_test.go`:

```go
// provider builds a Provider serving this backend under the endpoint name
// "test", with values rooted at /v1.
func (f *fakeBackend) provider() *Provider {
	p, err := New(Endpoint{
		Name:    "test",
		BaseURL: "https://api.test/v1",
		Client:  &http.Client{Transport: f.transport()},
	})
	if err != nil {
		panic("building fake provider: " + err.Error())
	}
	return p
}
```

- [ ] **Step 2: Write the conformance test**

Create `providers/https/conformance_test.go`:

```go
package https

import (
	"context"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
	"github.com/xavidop/mamori/providertest"
)

// TestConformance runs the shared providertest kit against this provider,
// built through fakeBackend's in-process RoundTripper rather than a real
// httptest.Server: the NoGoroutineLeak case runs goleak.VerifyNone with no
// ignore options, which a live server's accept goroutine could never satisfy.
//
// SkipWatch is deliberately left unset. This provider genuinely has no Watch
// method (see the package doc on Watching), so the watch cases must skip
// because the type assertion to mamori.WatchableProvider fails, not because
// this test told them to.
//
// NoResolveErrors is likewise left unset: Resolve classifies HTTP status
// through httpcore.ClassifyStatus and Fail/Clear give the kit a real seam, so
// this provider owes the ErrorClassification case.
func TestConformance(t *testing.T) {
	f := newFake()

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return f.provider() },
		Ref: func(key string) string { return "https://test/" + key },
		Seed: func(_ context.Context, key, val string) error {
			f.set("/v1/"+key, []byte(val))
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set("/v1/"+key, []byte(val))
			return nil
		},
		// The backend surfaces a failure as a status code, not as a mamori
		// error, so the injected sentinel is turned back into the status that
		// produces it via httpcore.StatusForKind, the exported inverse of
		// ClassifyStatus. Injecting one fixed status instead would exercise a
		// single classification case five times rather than five cases once.
		Fail: func(_ context.Context, _ string, err error) error {
			f.fail(httpcore.StatusForKind(mamori.ErrorKind(err)))
			return nil
		},
		Clear: func(_ context.Context, _ string) error {
			f.clearFail()
			return nil
		},
		PointerRef: func(key, frag string) string {
			return "https://test/" + key + frag
		},
	})
}
```

- [ ] **Step 3: Run the conformance test**

Run: `cd providers/https && GOWORK=off go test -run TestConformance -v ./... 2>&1 | tail -40`
Expected: every subtest PASS or SKIP. The watch subtests must SKIP.

If `NoGoroutineLeak` fails, the cause is a `Revalidator` or `Client` holding a goroutine; neither should. Do not add a goleak ignore option to make it pass.

- [ ] **Step 4: Write the error classification table test**

Create `providers/https/errors_test.go`:

```go
package https

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// TestStatusToKind maps every status this provider can receive onto the kind it
// must report. This is the requirement CONTRIBUTING calls out separately from
// the conformance kit: the kit proves a classified error survives transit, this
// proves the classification exists at all.
func TestStatusToKind(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   mamori.Kind
	}{
		{"bad request", http.StatusBadRequest, mamori.KindInvalid},
		{"unauthorized", http.StatusUnauthorized, mamori.KindUnauthenticated},
		{"forbidden", http.StatusForbidden, mamori.KindPermissionDenied},
		{"not found", http.StatusNotFound, mamori.KindNotFound},
		{"request timeout", http.StatusRequestTimeout, mamori.KindRateLimited},
		{"too many requests", http.StatusTooManyRequests, mamori.KindRateLimited},
		{"internal error", http.StatusInternalServerError, mamori.KindUnavailable},
		{"bad gateway", http.StatusBadGateway, mamori.KindUnavailable},
		{"gateway timeout", http.StatusGatewayTimeout, mamori.KindUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.set("/v1/cfg", []byte("payload"))
			f.fail(tt.status)
			p := newTestProvider(t, f, nil)

			_, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
			if err == nil {
				t.Fatalf("status %d produced no error", tt.status)
			}
			if got := mamori.ErrorKind(err); got != tt.want {
				t.Fatalf("status %d kind = %s, want %s", tt.status, got, tt.want)
			}
		})
	}
}

// TestResolveErrorCarriesNoPayload proves a failing resolve does not put the
// response body into the error, since a body can be the secret itself.
func TestResolveErrorCarriesNoPayload(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte("s3cr3t-value"))
	f.fail(http.StatusForbidden)
	p := newTestProvider(t, f, nil)

	_, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	if got := err.Error(); strings.Contains(got, "s3cr3t-value") {
		t.Fatalf("payload leaked into error %q", got)
	}
}
```

- [ ] **Step 5: Run the error tests**

Run: `cd providers/https && GOWORK=off go test -run 'TestStatusToKind|TestResolveErrorCarriesNoPayload' -v ./...`
Expected: PASS

- [ ] **Step 6: Write the integration test**

Create `providers/https/https_integration_test.go`:

```go
//go:build integration

// Package https live integration tests hit a real HTTP endpoint you nominate
// and are excluded from the default build. Run them against an endpoint that
// already serves a JSON document:
//
//	export MAMORI_HTTPS_BASE_URL=https://api.example.com/v1
//	export MAMORI_HTTPS_PATH=config
//	export MAMORI_HTTPS_TOKEN=...          # optional bearer token
//	export MAMORI_HTTPS_POINTER=/db/host   # optional JSON Pointer
//	GOWORK=off go test -tags integration -run Integration ./...
//
// BASE_URL and PATH are required; the tests skip if either is unset. Nothing
// here ever logs a token or a resolved value, only a byte count.
package https

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// liveConfig reads the environment the integration tests require, skipping the
// calling test if the two required variables are unset.
func liveConfig(t *testing.T) (baseURL, path, token, pointer string) {
	t.Helper()
	baseURL = os.Getenv("MAMORI_HTTPS_BASE_URL")
	path = os.Getenv("MAMORI_HTTPS_PATH")
	if baseURL == "" || path == "" {
		t.Skip("set MAMORI_HTTPS_BASE_URL and MAMORI_HTTPS_PATH to run the live tests")
	}
	return baseURL, path, os.Getenv("MAMORI_HTTPS_TOKEN"), os.Getenv("MAMORI_HTTPS_POINTER")
}

// liveProvider builds a Provider against the nominated endpoint.
func liveProvider(t *testing.T, baseURL, token string) *Provider {
	t.Helper()
	e := Endpoint{Name: "live", BaseURL: baseURL}
	if token != "" {
		e.Auth = httpcore.Bearer(token)
	}
	p, err := New(e)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestIntegrationResolve(t *testing.T) {
	baseURL, path, token, _ := liveConfig(t)
	p := liveProvider(t, baseURL, token)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("https://live/" + path)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(v.Bytes) == 0 {
		t.Fatal("Resolve returned an empty value")
	}
	if v.Version == "" {
		t.Fatal("Resolve returned no Version")
	}
	t.Logf("resolved %d bytes", len(v.Bytes))
}

// TestIntegrationConditionalGet proves the second poll revalidates rather than
// re-downloading, which is the whole reason this provider uses a Revalidator.
func TestIntegrationConditionalGet(t *testing.T) {
	baseURL, path, token, _ := liveConfig(t)
	p := liveProvider(t, baseURL, token)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("https://live/" + path)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	first, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	second, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if first.Version != second.Version {
		t.Logf("version changed between polls (%q then %q); the endpoint may not send a stable ETag", first.Version, second.Version)
	}
	if len(second.Bytes) != len(first.Bytes) {
		t.Fatalf("second Resolve returned %d bytes, want %d", len(second.Bytes), len(first.Bytes))
	}
}

func TestIntegrationPointerSelection(t *testing.T) {
	baseURL, path, token, pointer := liveConfig(t)
	if pointer == "" {
		t.Skip("set MAMORI_HTTPS_POINTER to run the pointer selection test")
	}
	p := liveProvider(t, baseURL, token)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("https://live/" + path + "#" + pointer)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	t.Logf("selected %d bytes at %s", len(v.Bytes), pointer)
}
```

- [ ] **Step 7: Verify the integration build compiles**

Run: `cd providers/https && GOWORK=off go vet -tags integration ./...`
Expected: no output. CI runs this same check.

- [ ] **Step 8: Run the full module suite with race and vet**

Run: `cd providers/https && GOWORK=off go test -race ./... && GOWORK=off go vet ./...`
Expected: PASS, no vet output

- [ ] **Step 9: Commit**

```bash
git add providers/https
git commit -m "test(https): conformance, error classification and live integration tests"
```

---

### Task 10: Documentation

The repo's standing rule is that documentation ships with the feature, covering the module README, the docs site, both coverage tables, and the agent skill.

**Files:**
- Create: `providers/https/README.md`
- Create: `site/src/pages/docs/providers/https.md`
- Modify: `site/src/pages/docs/providers/index.md`
- Modify: `README.md`
- Modify: `skills/mamori/references/providers.md`

**Interfaces:**
- Consumes: every exported symbol from Tasks 7 and 8
- Produces: nothing consumed by code

- [ ] **Step 1: Read the models before writing**

Run: `cat providers/cloudflare-kv/README.md && cat site/src/pages/docs/providers/cloudflare-kv.md`

Match their structure exactly. Do not invent a new section layout.

- [ ] **Step 2: Write the module README**

Create `providers/https/README.md` with these sections:

1. **Intro.** One paragraph: a generic provider for configuration and secrets served over HTTP by an endpoint you declare, built on `providers/httpcore`.
2. **`## Install`** with `go get github.com/xavidop/mamori/providers/https`.
3. **`## Scheme`** with the grammar `https://<endpoint>/<path>[#<key>][?<opts>]` and a ref-examples table.
4. **`## Why endpoints are named, not raw URLs`** covering all three reasons, with the concrete parse failure spelled out: `https://api.example.com/cfg?env=prod#/db/pass` yields an empty `Key` and an `Opts` of `env` = `prod#/db/pass`.
5. **`## Endpoint options`** as a table of every `Endpoint` field with its default.
6. **`## Error classification`** with the full status table.
7. **`## Conditional GET`** explaining that each poll revalidates.
8. **`## No native watch`** explaining why `WatchableProvider` is deliberately not implemented, and naming `TestProviderIsNotWatchable` as the test that pins it.
9. **`## Testing status`** as a table with a row per claim and its verification state. Mark the wire behaviour `Verified` (this provider defines its own contract; there is no vendor API to confirm) and the live-endpoint rows `Requires a live endpoint, not run in CI`.
10. **`## Development`** with the `GOWORK=off` commands.

- [ ] **Step 3: Write the docs-site page**

Create `site/src/pages/docs/providers/https.md`, mirroring the README and matching the frontmatter and layout of `site/src/pages/docs/providers/cloudflare-kv.md` exactly.

- [ ] **Step 4: Add the coverage table rows**

In the root `README.md` provider table, insert after the `providers/cloudflare-kv` row:

```markdown
| `providers/https` | `https://` (generic, operator-declared endpoints) | poll (conditional GET) | ✅ |
```

Add the same row to the coverage table in `site/src/pages/docs/providers/index.md`, matching that file's column layout.

Also add the install line to the root README's provider install block:

```bash
go get github.com/xavidop/mamori/providers/https        # https:// (generic REST)
```

- [ ] **Step 5: Update the agent skill**

Add an entry for `https://` to `skills/mamori/references/providers.md`, matching the format of the surrounding entries. It must state that the authority is a registered endpoint name and not a hostname, since that is the thing an agent will otherwise get wrong.

- [ ] **Step 6: Verify the docs site builds**

Run: `cd site && npm run build 2>&1 | tail -20`
Expected: build succeeds with no broken-link warnings for the new page.

- [ ] **Step 7: Verify every README example compiles**

Extract each Go block from `providers/https/README.md` into a scratch file and build it.

Run: `cd providers/https && GOWORK=off go build ./...`
Expected: success

- [ ] **Step 8: Run the full repo test suite**

Run: `make test 2>&1 | tail -30`
Expected: every module passes

- [ ] **Step 9: Run the linter**

Run: `make lint 2>&1 | tail -30`
Expected: no findings

- [ ] **Step 10: Commit**

```bash
git add providers/https/README.md site/src/pages/docs/providers/https.md \
        site/src/pages/docs/providers/index.md README.md \
        skills/mamori/references/providers.md
git commit -m "docs(https): module README, docs site page, coverage tables and agent skill"
```

---

## Final verification

- [ ] **Both modules pass with race detection**

Run: `cd providers/httpcore && GOWORK=off go test -race ./... && cd ../https && GOWORK=off go test -race ./...`
Expected: PASS

- [ ] **The integration build vets**

Run: `cd providers/https && GOWORK=off go vet -tags integration ./...`
Expected: no output

- [ ] **The whole workspace builds and passes**

Run: `make test && make lint`
Expected: PASS, no findings

- [ ] **No third-party dependency crept in**

Run: `cd providers/httpcore && GOWORK=off go mod tidy && git diff --exit-code go.mod`
Expected: no diff, and `go.mod` requires only `github.com/xavidop/mamori`

- [ ] **The conformance kit ran, and the watch cases skipped rather than passed vacuously**

Run: `cd providers/https && GOWORK=off go test -run TestConformance -v ./... 2>&1 | grep -E 'SKIP|FAIL|PASS.*Watch'`
Expected: watch subtests SKIP, no FAIL

## Self-Review Notes

**Spec coverage.** Every PR1 requirement in `docs/superpowers/specs/2026-08-03-provider-and-core-expansion-design.md` maps to a task: `Client` and `Do` to Task 3, `Authenticator` to Task 2, `ClassifyStatus` to Task 1, `Revalidator` to Task 4, `Version` to Task 4, `OAuth2ClientCredentials` to Task 5, `Endpoint` and `New` to Task 7, ref grammar and `SelectKey` to Task 8, `providertest` with `Fail`/`Clear`/`PointerRef` to Task 9, and the documentation contract to Task 10. The spec's "no retry", "caller-supplied detail", "`AllowInsecure`", and "in-process `RoundTripper`, never `httptest.Server`" constraints each have a test that fails if they are violated.

**Out of scope, per the spec.** SSE (PR15), `LongPoll` (PR5), and migrating the existing sixteen providers (PR13 through PR16) appear nowhere in this plan.
