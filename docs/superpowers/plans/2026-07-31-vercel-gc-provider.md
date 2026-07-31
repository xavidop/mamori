# Vercel Global Config provider implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `providers/vercel-gc`, a mamori provider resolving values from Vercel Global Config over its documented HTTPS read API, using the store digest to make each poll cheap.

**Architecture:** A standard-library-only provider. `Resolve` requests `/<store>/digest` on every call and refetches `/<store>/items` only when the hash changed, caching one snapshot per store id behind a mutex. `ResolveBatch` groups refs by store and costs one `/items` request each. No `Watch`: Vercel offers no streaming or blocking read, and faking one with a ticker is forbidden by the provider contract.

**Tech Stack:** Go 1.26, `net/http`, `encoding/json`, `net/url` from the standard library. `github.com/xavidop/mamori` and `providertest` for conformance. No third-party dependencies.

**Spec:** [2026-07-31-vercel-global-config-provider-design.md](../specs/2026-07-31-vercel-global-config-provider-design.md)

## Global Constraints

- **Module path:** `github.com/xavidop/mamori/providers/vercel-gc`. **Go package name:** `vercelgc` (a Go package identifier cannot contain a hyphen).
- **Zero third-party dependencies.** The only `require` entries are `github.com/xavidop/mamori` plus whatever `go mod tidy` pulls in transitively for tests. Adding any HTTP, JSON, or sync helper library is a plan violation.
- **Every module command runs with the workspace disabled**, matching CI: `GOWORK=off go test ./...` from inside `providers/vercel-gc`.
- **`go.mod` needs `replace github.com/xavidop/mamori => ../..`**, as every other provider module has.
- **No `time` in the resolve path.** No TTL, no clock, no ticker. `Refresh()` and `Doctor` call `Provider.Resolve` directly and must always reach Vercel.
- **`Value.Sensitive` is always `false`.** Global Config holds flags and redirects, not managed secrets.
- **Never log, wrap, or embed a token or a resolved value in an error message.** Error bodies are read through `io.LimitReader(resp.Body, 4096)` for diagnostics only, following `providers/doppler/doppler.go`.
- **Never use an em-dash character in any file this plan creates.** Use a hyphen or restructure the sentence.
- **Commit at the end of every task, on the `xavier/vercel-gc-provider` branch only.** Never `push`, `merge`, or `rebase`, and never touch `main`.
- **Commit messages are Conventional Commits**, because `semantic-release` generates the changelog from them. Use the `feat(vercel-gc):` scope for the provider tasks and `docs(vercel-gc):` for the documentation task. End every commit message with this trailer, on its own line:

```
Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
```

---

### Task 1: Module scaffold, connection string, and path parsing

Pure functions with no network. This task establishes the module and the two parsers everything else depends on.

**Files:**
- Create: `providers/vercel-gc/go.mod`
- Create: `providers/vercel-gc/vercelgc.go`
- Create: `providers/vercel-gc/vercelgc_test.go`
- Modify: `go.work` (add `./providers/vercel-gc` to the `use` block, in alphabetical position between `./providers/vault` and `./providers/viper`)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type connection struct { host, storeID, token string }`
  - `func parseConnectionString(s string) (connection, error)`
  - `func parsePath(path, defaultStore string) (store, key string, err error)`
  - `type Provider struct{...}`, `func New(opts ...Option) *Provider`, `func (p *Provider) Scheme() string`
  - `type Option func(*Provider)` with `WithConnectionString`, `WithStoreID`, `WithToken`, `WithBaseURL`, `WithHTTPClient`
  - `func (p *Provider) connection() (connection, error)`

- [ ] **Step 1: Create the module file**

Create `providers/vercel-gc/go.mod`:

```
module github.com/xavidop/mamori/providers/vercel-gc

go 1.26.0

require github.com/xavidop/mamori v0.1.0

replace github.com/xavidop/mamori => ../..
```

- [ ] **Step 2: Register the module in the workspace**

Add this line to the `use (` block in `go.work`, keeping alphabetical order (after `./providers/vault`, before `./providers/viper`):

```
	./providers/vercel-gc
```

- [ ] **Step 3: Write the failing tests for both parsers**

Create `providers/vercel-gc/vercelgc_test.go`:

```go
package vercelgc

import (
	"strings"
	"testing"
)

func TestParseConnectionString(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    connection
		wantErr string
	}{
		{
			name: "global config host",
			in:   "https://global-config.vercel.com/ecfg_abc123?token=tok_xyz",
			want: connection{host: "https://global-config.vercel.com", storeID: "ecfg_abc123", token: "tok_xyz"},
		},
		{
			name: "legacy edge config host is preserved",
			in:   "https://edge-config.vercel.com/ecfg_old?token=tok_old",
			want: connection{host: "https://edge-config.vercel.com", storeID: "ecfg_old", token: "tok_old"},
		},
		{name: "empty", in: "", wantErr: "empty"},
		{name: "no token", in: "https://global-config.vercel.com/ecfg_abc", wantErr: "token"},
		{name: "no store id", in: "https://global-config.vercel.com?token=t", wantErr: "store"},
		{name: "not a url", in: "://nope", wantErr: "parsing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseConnectionString(tc.in)
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
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParsePath(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		defaultStore string
		wantStore    string
		wantKey      string
		wantErr      string
	}{
		{name: "one segment uses default store", path: "log-level", defaultStore: "ecfg_def", wantStore: "ecfg_def", wantKey: "log-level"},
		{name: "two segments name the store", path: "ecfg_abc/log-level", defaultStore: "ecfg_def", wantStore: "ecfg_abc", wantKey: "log-level"},
		{name: "leading slash tolerated", path: "/log-level", defaultStore: "ecfg_def", wantStore: "ecfg_def", wantKey: "log-level"},
		{name: "one segment with no default store", path: "log-level", defaultStore: "", wantErr: "no store"},
		{name: "empty path", path: "", defaultStore: "ecfg_def", wantErr: "requires a key"},
		{name: "three segments", path: "a/b/c", defaultStore: "ecfg_def", wantErr: "at most"},
		{name: "empty key in two-segment form", path: "ecfg_abc/", defaultStore: "", wantErr: "requires a key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, key, err := parsePath(tc.path, tc.defaultStore)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got err %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if store != tc.wantStore || key != tc.wantKey {
				t.Fatalf("got (%q, %q), want (%q, %q)", store, key, tc.wantStore, tc.wantKey)
			}
		})
	}
}

func TestConnectionPrecedence(t *testing.T) {
	t.Setenv("GLOBAL_CONFIG", "https://global-config.vercel.com/ecfg_global?token=t_global")
	t.Setenv("EDGE_CONFIG", "https://edge-config.vercel.com/ecfg_edge?token=t_edge")

	got, err := New().connection()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.storeID != "ecfg_global" {
		t.Fatalf("GLOBAL_CONFIG must win over EDGE_CONFIG, got store %q", got.storeID)
	}

	got, err = New(WithStoreID("ecfg_explicit"), WithToken("t_explicit")).connection()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.storeID != "ecfg_explicit" || got.token != "t_explicit" {
		t.Fatalf("explicit options must win over the environment, got %+v", got)
	}
	if got.host != defaultHost {
		t.Fatalf("explicit options must default the host to %q, got %q", defaultHost, got.host)
	}
}

func TestConnectionEdgeConfigFallback(t *testing.T) {
	t.Setenv("GLOBAL_CONFIG", "")
	t.Setenv("EDGE_CONFIG", "https://edge-config.vercel.com/ecfg_edge?token=t_edge")

	got, err := New().connection()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.storeID != "ecfg_edge" || got.host != "https://edge-config.vercel.com" {
		t.Fatalf("EDGE_CONFIG fallback failed, got %+v", got)
	}
}

func TestConnectionMissing(t *testing.T) {
	t.Setenv("GLOBAL_CONFIG", "")
	t.Setenv("EDGE_CONFIG", "")

	_, err := New().connection()
	if err == nil {
		t.Fatal("want an error when no connection is configured")
	}
	for _, want := range []string{"GLOBAL_CONFIG", "EDGE_CONFIG", "WithConnectionString"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q, got: %v", want, err)
		}
	}
}
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `cd providers/vercel-gc && GOWORK=off go test ./...`
Expected: FAIL to build, with `undefined: connection`, `undefined: parseConnectionString`, `undefined: parsePath`, `undefined: New`.

- [ ] **Step 5: Write the implementation**

Create `providers/vercel-gc/vercelgc.go`:

```go
// Package vercelgc implements a mamori provider for Vercel Global Config
// (https://vercel.com/docs/global-config), the globally replicated key-value
// store Vercel applications read at runtime for feature flags, redirect maps,
// and experimentation settings.
//
// Vercel publishes no Go SDK, and none is needed: the read path is a
// documented HTTPS API, so this provider uses net/http and the standard
// library only.
//
// # Scheme
//
//	vercel-gc://<key>              store and token from the connection string
//	vercel-gc://<store-id>/<key>   explicit store
//	vercel-gc://<key>#field        select a field of a JSON-valued key
//
// # Authentication
//
// Connecting a Global Config store to a Vercel project sets GLOBAL_CONFIG to a
// connection string of the form
//
//	https://global-config.vercel.com/<store-id>?token=<read-token>
//
// Stores connected before Vercel renamed Edge Config to Global Config instead
// set EDGE_CONFIG, pointing at edge-config.vercel.com. This provider reads
// GLOBAL_CONFIG first and falls back to EDGE_CONFIG, and takes the host from
// the connection string rather than hardcoding it, so both keep working.
//
// # Watching
//
// Vercel exposes no streaming or blocking read for Global Config, so this
// provider deliberately does not implement mamori.WatchableProvider and mamori
// wraps it in the polling adapter. Each Resolve requests the store digest,
// which is replaced whenever the store is updated, and refetches the item body
// only when that hash moved.
package vercelgc

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "vercel-gc"

// defaultHost is used when a store id and token are supplied explicitly rather
// than through a connection string that carries its own host.
const defaultHost = "https://global-config.vercel.com"

// connection is everything needed to address one Global Config store: the API
// origin, the store id, and the read token.
type connection struct {
	host    string
	storeID string
	token   string
}

// Provider resolves vercel-gc:// refs against the Global Config read API. It is
// safe for concurrent use.
type Provider struct {
	connStr string
	storeID string
	token   string
	baseURL string

	httpClient *http.Client

	mu        sync.Mutex
	snapshots map[string]*snapshot
}

// snapshot is the last observed body of one store, tagged with the digest that
// was current when it was fetched.
type snapshot struct {
	digest string
	items  map[string]jsonRaw
}

// Option configures a Provider.
type Option func(*Provider)

// WithConnectionString sets the store, token, and host from a Global Config
// connection string, overriding both GLOBAL_CONFIG and EDGE_CONFIG.
func WithConnectionString(s string) Option { return func(p *Provider) { p.connStr = s } }

// WithStoreID sets the store used by refs that name only a key.
func WithStoreID(id string) Option { return func(p *Provider) { p.storeID = id } }

// WithToken sets the Global Config read token.
func WithToken(t string) Option { return func(p *Provider) { p.token = t } }

// WithBaseURL overrides the API origin, for an httptest.Server or a proxy. It
// is ignored when a connection string supplies its own host.
func WithBaseURL(u string) Option {
	return func(p *Provider) {
		if u != "" {
			p.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithHTTPClient injects a custom *http.Client. A nil client is a no-op.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.httpClient = c
		}
	}
}

// New constructs a Global Config provider. Without options it reads
// GLOBAL_CONFIG (falling back to EDGE_CONFIG) lazily at resolve time, so it is
// safe to register from init even when no credentials exist at process start.
func New(opts ...Option) *Provider {
	p := &Provider{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		snapshots:  map[string]*snapshot{},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func init() { mamori.Register(New()) }

// Scheme returns "vercel-gc".
func (p *Provider) Scheme() string { return scheme }

// connection resolves the effective connection, reading the environment lazily.
// Precedence: WithConnectionString, then explicit WithStoreID/WithToken, then
// GLOBAL_CONFIG, then EDGE_CONFIG.
func (p *Provider) connection() (connection, error) {
	if p.connStr != "" {
		return p.applyBaseURL(parseConnectionString(p.connStr))
	}
	if p.storeID != "" || p.token != "" {
		host := defaultHost
		if p.baseURL != "" {
			host = p.baseURL
		}
		return connection{host: host, storeID: p.storeID, token: p.token}, nil
	}
	if s := os.Getenv("GLOBAL_CONFIG"); s != "" {
		return p.applyBaseURL(parseConnectionString(s))
	}
	if s := os.Getenv("EDGE_CONFIG"); s != "" {
		return p.applyBaseURL(parseConnectionString(s))
	}
	return connection{}, errors.New("mamori/vercel-gc: no connection configured; set GLOBAL_CONFIG or EDGE_CONFIG, or use WithConnectionString / WithStoreID+WithToken")
}

// applyBaseURL lets WithBaseURL redirect a connection-string-derived host,
// which is what points the provider at an httptest.Server in tests.
func (p *Provider) applyBaseURL(c connection, err error) (connection, error) {
	if err != nil {
		return connection{}, err
	}
	if p.baseURL != "" {
		c.host = p.baseURL
	}
	return c, nil
}

// parseConnectionString splits a Global Config connection string of the form
// https://<host>/<store-id>?token=<token> into its parts. The host is taken
// from the string rather than assumed, which is what keeps the legacy
// edge-config.vercel.com origin working.
func parseConnectionString(s string) (connection, error) {
	if strings.TrimSpace(s) == "" {
		return connection{}, errors.New("mamori/vercel-gc: empty connection string")
	}
	u, err := url.Parse(s)
	if err != nil {
		return connection{}, fmt.Errorf("mamori/vercel-gc: parsing connection string: %w", err)
	}
	storeID := strings.Trim(u.Path, "/")
	if storeID == "" || strings.Contains(storeID, "/") {
		return connection{}, errors.New("mamori/vercel-gc: connection string has no store id path segment")
	}
	token := u.Query().Get("token")
	if token == "" {
		return connection{}, errors.New("mamori/vercel-gc: connection string has no token query parameter")
	}
	if u.Scheme == "" || u.Host == "" {
		return connection{}, errors.New("mamori/vercel-gc: connection string is not an absolute URL")
	}
	return connection{host: u.Scheme + "://" + u.Host, storeID: storeID, token: token}, nil
}

// parsePath splits a ref path into a store id and a key. One segment is a key
// resolved against defaultStore; two segments are "<store-id>/<key>". This is
// unambiguous because Global Config keys are documented as alphanumeric
// characters, "_", and "-" only, so a key can never contain a slash. Charset
// validation beyond that is left to the API rather than duplicating a rule
// Vercel may loosen.
func parsePath(path, defaultStore string) (store, key string, err error) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	switch len(segs) {
	case 1:
		key, store = segs[0], defaultStore
	case 2:
		store, key = segs[0], segs[1]
	default:
		return "", "", fmt.Errorf("mamori/vercel-gc: path %q has %d segments; a ref takes at most <store-id>/<key>", path, len(segs))
	}
	if key == "" {
		return "", "", fmt.Errorf("mamori/vercel-gc: path %q requires a key", path)
	}
	if store == "" {
		return "", "", fmt.Errorf("mamori/vercel-gc: path %q names no store and no default store is configured; set GLOBAL_CONFIG or use WithStoreID", path)
	}
	return store, key, nil
}
```

Also add this placeholder type at the bottom of the file so the `snapshot` struct compiles; Task 2 replaces it with the real alias:

```go
// jsonRaw is a stored Global Config value, kept as raw JSON so no numeric
// precision is lost on the way through.
type jsonRaw = json.RawMessage
```

and add `"encoding/json"` to the import block.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd providers/vercel-gc && GOWORK=off go mod tidy && GOWORK=off go test ./...`
Expected: PASS, all of `TestParseConnectionString`, `TestParsePath`, `TestConnectionPrecedence`, `TestConnectionEdgeConfigFallback`, `TestConnectionMissing`.

- [ ] **Step 7: Checkpoint and commit**

Run `cd providers/vercel-gc && GOWORK=off go vet ./...` and confirm it is clean.

```bash
git add providers/vercel-gc/go.mod providers/vercel-gc/go.sum providers/vercel-gc/vercelgc.go providers/vercel-gc/vercelgc_test.go go.work
git commit -m "feat(vercel-gc): module scaffold, connection string and path parsing

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Value mapping

Turns a stored JSON value into `mamori.Value`, including `#key` selection.

**Files:**
- Create: `providers/vercel-gc/value.go`
- Create: `providers/vercel-gc/value_test.go`
- Modify: `providers/vercel-gc/vercelgc.go` (remove the `jsonRaw` placeholder added in Task 1 Step 5, since `value.go` now owns it)

**Interfaces:**
- Consumes: `type jsonRaw = json.RawMessage` from Task 1 (moved here).
- Produces: `func valueFor(raw jsonRaw, ref mamori.Ref, storeID, digest string) (mamori.Value, error)` and `func rawToBytes(raw jsonRaw) ([]byte, error)`.

- [ ] **Step 1: Write the failing test**

Create `providers/vercel-gc/value_test.go`:

```go
package vercelgc

import (
	"errors"
	"testing"

	"github.com/xavidop/mamori"
)

func TestRawToBytes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "string is unquoted", raw: `"info"`, want: "info"},
		{name: "string with escapes is decoded", raw: `"a\"b\nc"`, want: "a\"b\nc"},
		{name: "true", raw: `true`, want: "true"},
		{name: "false", raw: `false`, want: "false"},
		{name: "integer", raw: `5432`, want: "5432"},
		{name: "float", raw: `0.25`, want: "0.25"},
		{name: "null is a value, not an absence", raw: `null`, want: "null"},
		{name: "object is compacted json", raw: "{\n  \"timeout\": \"5s\"\n}", want: `{"timeout":"5s"}`},
		{name: "array is compacted json", raw: `[1, 2, 3]`, want: `[1,2,3]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rawToBytes(jsonRaw(tc.raw))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValueForSelectsKey(t *testing.T) {
	ref := mamori.Ref{Scheme: scheme, Path: "api-config", Key: "timeout", Raw: "vercel-gc://api-config#timeout"}

	v, err := valueFor(jsonRaw(`{"timeout":"5s","retries":3}`), ref, "ecfg_abc", "dig1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "5s" {
		t.Fatalf("got %q, want %q", v.Bytes, "5s")
	}
}

func TestValueForSelectsJSONPointer(t *testing.T) {
	ref := mamori.Ref{Scheme: scheme, Path: "api-config", Key: "/nested/deep", Raw: "vercel-gc://api-config#/nested/deep"}

	v, err := valueFor(jsonRaw(`{"nested":{"deep":"found"}}`), ref, "ecfg_abc", "dig1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "found" {
		t.Fatalf("got %q, want %q", v.Bytes, "found")
	}
}

func TestValueForSelectingFieldOfStringIsInvalid(t *testing.T) {
	ref := mamori.Ref{Scheme: scheme, Path: "log-level", Key: "timeout", Raw: "vercel-gc://log-level#timeout"}

	_, err := valueFor(jsonRaw(`"info"`), ref, "ecfg_abc", "dig1")
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrInvalid", err)
	}
}

func TestValueForMetadataAndFlags(t *testing.T) {
	ref := mamori.Ref{Scheme: scheme, Path: "log-level", Raw: "vercel-gc://log-level"}

	v, err := valueFor(jsonRaw(`"info"`), ref, "ecfg_abc", "dig1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Sensitive {
		t.Error("Global Config holds flags, not managed secrets; Sensitive must be false")
	}
	if v.Version != mamori.VersionHash([]byte("info")) {
		t.Errorf("Version must hash the resolved bytes, got %q", v.Version)
	}
	if v.Metadata["store"] != "ecfg_abc" || v.Metadata["digest"] != "dig1" {
		t.Errorf("got metadata %v, want store=ecfg_abc digest=dig1", v.Metadata)
	}
}

func TestValueForVersionTracksSelectedBytes(t *testing.T) {
	ref := mamori.Ref{Scheme: scheme, Path: "api-config", Key: "timeout", Raw: "vercel-gc://api-config#timeout"}

	// Same selected field, different sibling. The version must not move: the
	// digest changes for any store edit, which is exactly why it is not the
	// version.
	a, err := valueFor(jsonRaw(`{"timeout":"5s","retries":3}`), ref, "ecfg_abc", "dig1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := valueFor(jsonRaw(`{"timeout":"5s","retries":9}`), ref, "ecfg_abc", "dig2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Version != b.Version {
		t.Fatalf("version moved on an unrelated edit: %q then %q", a.Version, b.Version)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd providers/vercel-gc && GOWORK=off go test -run 'TestRawToBytes|TestValueFor' ./...`
Expected: FAIL to build, with `undefined: rawToBytes` and `undefined: valueFor`.

- [ ] **Step 3: Write the implementation**

Delete the `jsonRaw` placeholder block from `vercelgc.go` (and its `encoding/json` import if now unused), then create `providers/vercel-gc/value.go`:

```go
package vercelgc

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/xavidop/mamori"
)

// jsonRaw is a stored Global Config value, kept as raw JSON so no numeric
// precision is lost on the way through.
type jsonRaw = json.RawMessage

// rawToBytes converts a stored JSON value to the bytes mamori decodes from.
//
// Only a JSON string is unwrapped, to its raw text without quotes or escapes.
// Every other type passes through as its own compacted JSON encoding, which
// makes a number exactly what the store holds rather than what a float64
// round trip would produce.
//
// A key stored as null exists, so it yields the four bytes "null". Only an
// absent key is not-found.
func rawToBytes(raw jsonRaw) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("mamori/vercel-gc: empty value: %w", mamori.ErrInvalid)
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, fmt.Errorf("mamori/vercel-gc: decoding string value: %w: %w", mamori.ErrInvalid, err)
		}
		return []byte(s), nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return nil, fmt.Errorf("mamori/vercel-gc: compacting value: %w: %w", mamori.ErrInvalid, err)
	}
	return buf.Bytes(), nil
}

// valueFor converts a stored value into a mamori.Value, applying #key selection
// when requested and hashing the resolved bytes for the version.
//
// Selection happens after unwrapping, matching valueFor in
// providers/launchdarkly, so selecting a field of a string-valued key fails
// with ErrInvalid rather than silently returning the whole string.
//
// Version is a content hash rather than the store digest: the digest is
// replaced whenever any key in the store changes, so using it would fire a
// spurious change for every unrelated field on every unrelated edit. The
// digest is reported in Metadata instead.
func valueFor(raw jsonRaw, ref mamori.Ref, storeID, digest string) (mamori.Value, error) {
	b, err := rawToBytes(raw)
	if err != nil {
		return mamori.Value{}, err
	}
	if ref.Key != "" {
		sel, err := mamori.SelectKey(b, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		b = sel
	}
	return mamori.Value{
		Bytes:     b,
		Version:   mamori.VersionHash(b),
		Sensitive: false,
		Metadata: map[string]string{
			"store":  storeID,
			"digest": digest,
		},
	}, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd providers/vercel-gc && GOWORK=off go test ./...`
Expected: PASS, including every Task 1 test still passing.

- [ ] **Step 5: Checkpoint and commit**

Run `cd providers/vercel-gc && GOWORK=off go vet ./...`.

```bash
git add providers/vercel-gc/value.go providers/vercel-gc/value_test.go providers/vercel-gc/vercelgc.go
git commit -m "feat(vercel-gc): map stored JSON values to mamori values

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Digest-gated Resolve and error classification

The core of the provider: the HTTP calls, the per-store snapshot, and the status mapping.

**Files:**
- Create: `providers/vercel-gc/resolve.go`
- Create: `providers/vercel-gc/fake_test.go`
- Create: `providers/vercel-gc/resolve_test.go`

**Interfaces:**
- Consumes: `connection`, `parsePath`, `Provider`, `snapshot`, `jsonRaw` (Task 1), `valueFor` (Task 2).
- Produces:
  - `func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error)`
  - `func (p *Provider) fetchDigest(ctx context.Context, c connection, store string) (string, error)`
  - `func (p *Provider) fetchItems(ctx context.Context, c connection, store string) (map[string]jsonRaw, error)`
  - `func classifyStatus(code int, statusErr error) error`
  - test helper `newFake() *fakeGC` with methods `set`, `del`, `failStatus`, `clearFail`, `counts`, `handle`, and `provider(opts ...Option) *Provider`

- [ ] **Step 1: Write the fake backend**

Create `providers/vercel-gc/fake_test.go`. Note the in-process `RoundTripper`: `providertest`'s `NoGoroutineLeak` case runs `goleak.VerifyNone` with no ignore options, which a live `httptest.Server` accept goroutine can never satisfy, so Task 5 drives this same handler without a real listener.

```go
package vercelgc

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

const (
	testStore = "ecfg_test"
	testToken = "tok_test"
)

// fakeGC is an in-memory emulation of the Global Config read API. It serves
// GET /<store>/digest and GET /<store>/items, and counts both so tests can
// assert exactly how many item bodies were fetched.
type fakeGC struct {
	mu        sync.Mutex
	stores    map[string]map[string]jsonRaw // store id -> key -> raw JSON
	rev       map[string]int                // store id -> bumped on every edit, rendered as the digest
	failCode  map[string]int                // store id -> status to return until cleared
	digestReq int
	itemsReq  int
	lastAuth  string
}

func newFake() *fakeGC {
	return &fakeGC{
		stores:   map[string]map[string]jsonRaw{},
		rev:      map[string]int{},
		failCode: map[string]int{},
	}
}

// set writes a raw JSON value and bumps the store digest, exactly as a real
// Global Config edit does.
func (f *fakeGC) set(store, key, rawJSON string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stores[store] == nil {
		f.stores[store] = map[string]jsonRaw{}
	}
	f.stores[store][key] = jsonRaw(rawJSON)
	f.rev[store]++
}

// del removes a key and bumps the store digest.
func (f *fakeGC) del(store, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.stores[store], key)
	f.rev[store]++
}

// failStatus makes every request for store return code until clearFail.
func (f *fakeGC) failStatus(store string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCode[store] = code
}

func (f *fakeGC) clearFail(store string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failCode, store)
}

// counts returns how many digest and items requests have been served.
func (f *fakeGC) counts() (digest, items int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.digestReq, f.itemsReq
}

func (f *fakeGC) handle(w http.ResponseWriter, r *http.Request) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) != 2 {
		http.NotFound(w, r)
		return
	}
	store, endpoint := segs[0], segs[1]

	f.mu.Lock()
	f.lastAuth = r.Header.Get("Authorization")
	if code, ok := f.failCode[store]; ok {
		f.mu.Unlock()
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"error":{"code":"injected","message":"injected failure"}}`)
		return
	}
	items, known := f.stores[store]
	rev := f.rev[store]
	switch endpoint {
	case "digest":
		f.digestReq++
	case "items":
		f.itemsReq++
	}
	f.mu.Unlock()

	if !known {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"edge_config_not_found"}}`)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	switch endpoint {
	case "digest":
		// The read API documents a digest endpoint returning JSON but does not
		// pin the shape, so the fake serves the bare-string form and
		// TestDigestShapes covers the object form as well.
		_, _ = io.WriteString(w, strconv.Quote("rev"+strconv.Itoa(rev)))
	case "items":
		out := map[string]json.RawMessage{}
		f.mu.Lock()
		for k, v := range items {
			out[k] = json.RawMessage(v)
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	default:
		http.NotFound(w, r)
	}
}

// server starts a real httptest.Server. Unit tests use this; the conformance
// suite must not, because goleak cannot tolerate its accept goroutine.
func (f *fakeGC) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(f.handle))
}

// roundTripper drives the same handler in-process, with no listener and no
// background goroutine.
type roundTripper struct{ f *fakeGC }

func (rt roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	// Honor cancellation explicitly. http.Client delegates context handling to
	// the transport, so a RoundTripper that ignores it would serve a request
	// whose context is already dead and hide a provider that forgot to thread
	// ctx through.
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	rec := httptest.NewRecorder()
	rt.f.handle(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// provider builds a Provider talking to this fake in-process, with no network
// listener. Extra options are applied last so a test can override the store.
func (f *fakeGC) provider(opts ...Option) *Provider {
	base := []Option{
		WithConnectionString(fmt.Sprintf("https://global-config.vercel.com/%s?token=%s", testStore, testToken)),
		WithHTTPClient(&http.Client{Transport: roundTripper{f: f}}),
	}
	return New(append(base, opts...)...)
}
```

- [ ] **Step 2: Write the failing resolve tests**

Create `providers/vercel-gc/resolve_test.go`:

```go
package vercelgc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
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

func TestResolveReturnsValue(t *testing.T) {
	f := newFake()
	f.set(testStore, "log-level", `"debug"`)
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "vercel-gc://log-level"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("got %q, want %q", v.Bytes, "debug")
	}
}

func TestResolveSendsBearerToken(t *testing.T) {
	f := newFake()
	f.set(testStore, "k", `"v"`)
	p := f.provider()

	if _, err := p.Resolve(context.Background(), ref(t, "vercel-gc://k")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.mu.Lock()
	auth := f.lastAuth
	f.mu.Unlock()
	if auth != "Bearer "+testToken {
		t.Fatalf("got Authorization %q, want %q", auth, "Bearer "+testToken)
	}
}

// The whole point of the digest: an unchanged store must never refetch bodies.
func TestResolveDigestGatesItemFetches(t *testing.T) {
	f := newFake()
	f.set(testStore, "a", `"1"`)
	f.set(testStore, "b", `"2"`)
	p := f.provider()
	ctx := context.Background()

	for range 5 {
		if _, err := p.Resolve(ctx, ref(t, "vercel-gc://a")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := p.Resolve(ctx, ref(t, "vercel-gc://b")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	digests, items := f.counts()
	if digests != 10 {
		t.Errorf("got %d digest requests, want 10 (one per Resolve)", digests)
	}
	if items != 1 {
		t.Errorf("got %d items requests, want 1 (the store never changed)", items)
	}

	f.set(testStore, "a", `"changed"`)
	v, err := p.Resolve(ctx, ref(t, "vercel-gc://a"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "changed" {
		t.Fatalf("got %q, want %q", v.Bytes, "changed")
	}
	if _, items = f.counts(); items != 2 {
		t.Errorf("got %d items requests after one edit, want 2", items)
	}
}

func TestResolveNotFound(t *testing.T) {
	f := newFake()
	f.set(testStore, "present", `"yes"`)
	p := f.provider()

	_, err := p.Resolve(context.Background(), ref(t, "vercel-gc://absent"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrNotFound", err)
	}
}

func TestResolveNullIsAValueNotNotFound(t *testing.T) {
	f := newFake()
	f.set(testStore, "maybe", `null`)
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "vercel-gc://maybe"))
	if err != nil {
		t.Fatalf("a key stored as null exists; got error %v", err)
	}
	if string(v.Bytes) != "null" {
		t.Fatalf("got %q, want %q", v.Bytes, "null")
	}
}

func TestResolveExplicitStore(t *testing.T) {
	f := newFake()
	f.set(testStore, "k", `"default-store"`)
	f.set("ecfg_other", "k", `"other-store"`)
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "vercel-gc://ecfg_other/k"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "other-store" {
		t.Fatalf("got %q, want %q", v.Bytes, "other-store")
	}
}

// Two stores must hold independent snapshots: editing one may not invalidate
// or shadow the other.
func TestResolveTwoStoresAreIndependent(t *testing.T) {
	f := newFake()
	f.set(testStore, "k", `"a1"`)
	f.set("ecfg_other", "k", `"b1"`)
	p := f.provider()
	ctx := context.Background()

	if _, err := p.Resolve(ctx, ref(t, "vercel-gc://k")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := p.Resolve(ctx, ref(t, "vercel-gc://ecfg_other/k")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, itemsBefore := f.counts()

	f.set("ecfg_other", "k", `"b2"`)

	v, err := p.Resolve(ctx, ref(t, "vercel-gc://k"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "a1" {
		t.Fatalf("default store changed under an edit to another store: got %q", v.Bytes)
	}
	if _, items := f.counts(); items != itemsBefore {
		t.Errorf("editing another store refetched the default store: %d then %d", itemsBefore, items)
	}

	v, err = p.Resolve(ctx, ref(t, "vercel-gc://ecfg_other/k"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "b2" {
		t.Fatalf("got %q, want %q", v.Bytes, "b2")
	}
}

// Concurrent resolves across two stores exercise the snapshot map and the
// benign duplicate-fetch path under -race. Values must stay coherent: a
// resolve never returns another store's body, and never a partially installed
// snapshot.
func TestResolveConcurrentAcrossStores(t *testing.T) {
	f := newFake()
	f.set(testStore, "k", `"a"`)
	f.set("ecfg_other", "k", `"b"`)
	p := f.provider()
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tag, want := "vercel-gc://k", "a"
			if i%2 == 1 {
				tag, want = "vercel-gc://ecfg_other/k", "b"
			}
			r, err := mamori.ParseRef(tag)
			if err != nil {
				errs <- err
				return
			}
			v, err := p.Resolve(ctx, r)
			if err != nil {
				errs <- err
				return
			}
			if string(v.Bytes) != want {
				errs <- fmt.Errorf("%s: got %q, want %q", tag, v.Bytes, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// The digest endpoint returns JSON whose exact shape Vercel does not pin, so
// both a bare string and an object with a digest field must parse.
func TestDigestShapes(t *testing.T) {
	for _, body := range []string{`"abc123"`, `{"digest":"abc123"}`} {
		t.Run(body, func(t *testing.T) {
			got, err := parseDigest([]byte(body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != "abc123" {
				t.Fatalf("got %q, want %q", got, "abc123")
			}
		})
	}
}

func TestResolveErrorClassification(t *testing.T) {
	tests := []struct {
		code int
		want error
	}{
		{http.StatusUnauthorized, mamori.ErrUnauthenticated},
		{http.StatusForbidden, mamori.ErrPermissionDenied},
		{http.StatusTooManyRequests, mamori.ErrRateLimited},
		{http.StatusBadRequest, mamori.ErrInvalid},
		{http.StatusInternalServerError, mamori.ErrUnavailable},
		{http.StatusBadGateway, mamori.ErrUnavailable},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			f := newFake()
			f.set(testStore, "k", `"v"`)
			p := f.provider()
			f.failStatus(testStore, tc.code)

			_, err := p.Resolve(context.Background(), ref(t, "vercel-gc://k"))
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: got %v, want an error satisfying %v", tc.code, err, tc.want)
			}
		})
	}
}

func TestResolveUnknownStoreIsNotFound(t *testing.T) {
	f := newFake()
	p := f.provider()

	_, err := p.Resolve(context.Background(), ref(t, "vercel-gc://ecfg_missing/k"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrNotFound", err)
	}
}

func TestResolveHonorsContextCancellation(t *testing.T) {
	f := newFake()
	f.set(testStore, "k", `"v"`)
	p := f.provider()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.Resolve(ctx, ref(t, "vercel-gc://k")); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd providers/vercel-gc && GOWORK=off go test ./...`
Expected: FAIL to build, with `p.Resolve undefined` and `undefined: parseDigest`.

- [ ] **Step 4: Write the implementation**

Create `providers/vercel-gc/resolve.go`:

```go
package vercelgc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/xavidop/mamori"
)

// Resolve fetches the value for ref.
//
// Every call requests the store digest, which Vercel replaces on any edit, and
// refetches the item body only when that hash moved. There is deliberately no
// TTL and no clock: mamori.Refresh and mamori.Doctor both call Resolve
// directly, and a time-based cache would let either return a held value
// without contacting Vercel at all.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	conn, err := p.connection()
	if err != nil {
		return mamori.Value{}, err
	}
	store, key, err := parsePath(ref.Path, conn.storeID)
	if err != nil {
		return mamori.Value{}, err
	}

	digest, err := p.fetchDigest(ctx, conn, store)
	if err != nil {
		return mamori.Value{}, err
	}
	snap, err := p.snapshotFor(ctx, conn, store, digest)
	if err != nil {
		return mamori.Value{}, err
	}

	raw, ok := snap.items[key]
	if !ok {
		return mamori.Value{}, fmt.Errorf("mamori/vercel-gc: key %q not in store %s: %w", key, store, mamori.ErrNotFound)
	}
	return valueFor(raw, ref, store, snap.digest)
}

// snapshotFor returns the store body matching digest, fetching it only when the
// held snapshot is missing or stale.
//
// The fetch happens outside the lock, so two goroutines observing the same
// change may both fetch. That is accepted rather than prevented: the fetch is
// an idempotent GET, it happens only on an actual edit, and avoiding it would
// mean either serializing every digest request or taking on a single-flight
// dependency in a module whose whole appeal is having none. Each caller uses
// the snapshot it built, so nobody ever reads a body another goroutine was
// mid-install of; a losing writer's older snapshot simply causes one extra
// fetch on the next Resolve.
func (p *Provider) snapshotFor(ctx context.Context, c connection, store, digest string) (*snapshot, error) {
	p.mu.Lock()
	held := p.snapshots[store]
	p.mu.Unlock()
	if held != nil && held.digest == digest {
		return held, nil
	}

	items, err := p.fetchItems(ctx, c, store)
	if err != nil {
		return nil, err
	}
	fresh := &snapshot{digest: digest, items: items}

	p.mu.Lock()
	p.snapshots[store] = fresh
	p.mu.Unlock()
	return fresh, nil
}

// fetchDigest returns the current digest of store.
func (p *Provider) fetchDigest(ctx context.Context, c connection, store string) (string, error) {
	body, err := p.get(ctx, c, store, "digest")
	if err != nil {
		return "", err
	}
	return parseDigest(body)
}

// parseDigest reads the digest out of the endpoint's JSON response. Vercel
// documents that the endpoint returns JSON but does not pin the shape, so both
// a bare string and an object carrying a "digest" field are accepted rather
// than betting on one.
func parseDigest(body []byte) (string, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "", fmt.Errorf("mamori/vercel-gc: empty digest response: %w", mamori.ErrInvalid)
	}
	var s string
	if err := json.Unmarshal([]byte(trimmed), &s); err == nil {
		return s, nil
	}
	var obj struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal([]byte(trimmed), &obj); err == nil && obj.Digest != "" {
		return obj.Digest, nil
	}
	return "", fmt.Errorf("mamori/vercel-gc: unrecognized digest response shape: %w", mamori.ErrInvalid)
}

// fetchItems returns every key in store in one request.
func (p *Provider) fetchItems(ctx context.Context, c connection, store string) (map[string]jsonRaw, error) {
	body, err := p.get(ctx, c, store, "items")
	if err != nil {
		return nil, err
	}
	var items map[string]jsonRaw
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("mamori/vercel-gc: decoding items of store %s: %w: %w", store, mamori.ErrInvalid, err)
	}
	if items == nil {
		items = map[string]jsonRaw{}
	}
	return items, nil
}

// get performs one authenticated GET against a store endpoint and returns the
// body. The token travels in the Authorization header rather than the
// documented token query parameter, so it can never reach a log line, a trace
// span, or an error message through a URL.
func (p *Provider) get(ctx context.Context, c connection, store, endpoint string) ([]byte, error) {
	url := c.host + "/" + store + "/" + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Read a bounded amount of the error body for diagnostics. Never log it.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		statusErr := fmt.Errorf("mamori/vercel-gc: unexpected status %d from %s of store %s: %s",
			resp.StatusCode, endpoint, store, strings.TrimSpace(string(msg)))
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("mamori/vercel-gc: store %s not found: %w", store, mamori.ErrNotFound)
		}
		return nil, classifyStatus(resp.StatusCode, statusErr)
	}
	return io.ReadAll(resp.Body)
}

// classifyStatus maps a Global Config read API status onto a mamori
// classification sentinel, wrapping statusErr so both the sentinel and the
// diagnostic context survive in the errors.Is chain. 404 is handled by its own
// branch in get and never reaches this function.
//
// The mapping follows ordinary HTTP semantics. One caveat is worth stating
// rather than hiding: Vercel's documented error body for a request missing an
// authentication token carries "code": "forbidden", so a 403 from this API can
// mean an absent credential rather than an insufficient one. Vercel has not
// published the full error-code vocabulary, so the mapping keys on status
// rather than guessing at a body it cannot rely on. Codes not listed report
// unknown rather than being guessed at.
func classifyStatus(code int, statusErr error) error {
	if statusErr == nil {
		return nil
	}
	var sentinel error
	switch {
	case code == http.StatusUnauthorized:
		sentinel = mamori.ErrUnauthenticated
	case code == http.StatusForbidden:
		sentinel = mamori.ErrPermissionDenied
	case code == http.StatusTooManyRequests:
		sentinel = mamori.ErrRateLimited
	case code == http.StatusBadRequest:
		sentinel = mamori.ErrInvalid
	case code >= 500:
		sentinel = mamori.ErrUnavailable
	default:
		return statusErr
	}
	return fmt.Errorf("%w: %w", sentinel, statusErr)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd providers/vercel-gc && GOWORK=off go test ./...`
Expected: PASS, every test in all three files.

- [ ] **Step 6: Run with the race detector**

Run: `cd providers/vercel-gc && GOWORK=off go test -race -run TestResolveConcurrentAcrossStores -count=5 ./...` then `GOWORK=off go test -race ./...`
Expected: PASS with no race reports. `-count=5` is there because a snapshot-map race is timing-dependent and a single run can miss it.

- [ ] **Step 7: Checkpoint and commit**

Run `cd providers/vercel-gc && GOWORK=off go vet ./...`.

```bash
git add providers/vercel-gc/resolve.go providers/vercel-gc/fake_test.go providers/vercel-gc/resolve_test.go
git commit -m "feat(vercel-gc): digest-gated Resolve with error classification

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: ResolveBatch

Collapses a `Load` to one request per store.

**Files:**
- Modify: `providers/vercel-gc/resolve.go` (append `ResolveBatch`)
- Create: `providers/vercel-gc/batch_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1 to 3.
- Produces: `func (p *Provider) ResolveBatch(ctx context.Context, refs []mamori.Ref) (map[string]mamori.Value, error)`, satisfying `mamori.BatchProvider`.

- [ ] **Step 1: Write the failing test**

Create `providers/vercel-gc/batch_test.go`:

```go
package vercelgc

import (
	"context"
	"testing"

	"github.com/xavidop/mamori"
)

func TestResolveBatchIsOneRequestPerStore(t *testing.T) {
	f := newFake()
	f.set(testStore, "a", `"1"`)
	f.set(testStore, "b", `"2"`)
	f.set(testStore, "c", `"3"`)
	f.set("ecfg_other", "d", `"4"`)
	p := f.provider()

	refs := []mamori.Ref{
		ref(t, "vercel-gc://a"),
		ref(t, "vercel-gc://b"),
		ref(t, "vercel-gc://c"),
		ref(t, "vercel-gc://ecfg_other/d"),
	}

	got, err := p.ResolveBatch(context.Background(), refs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d values, want 4", len(got))
	}
	for tag, want := range map[string]string{
		"vercel-gc://a":             "1",
		"vercel-gc://b":             "2",
		"vercel-gc://c":             "3",
		"vercel-gc://ecfg_other/d":  "4",
	} {
		if string(got[tag].Bytes) != want {
			t.Errorf("%s: got %q, want %q", tag, got[tag].Bytes, want)
		}
	}

	if _, items := f.counts(); items != 2 {
		t.Errorf("got %d items requests, want 2 (one per store)", items)
	}
}

// Per the BatchProvider contract, a missing key is omitted so mamori applies
// the field default rather than failing the whole batch.
func TestResolveBatchOmitsNotFound(t *testing.T) {
	f := newFake()
	f.set(testStore, "present", `"yes"`)
	p := f.provider()

	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{
		ref(t, "vercel-gc://present"),
		ref(t, "vercel-gc://absent"),
	})
	if err != nil {
		t.Fatalf("a missing key must not fail the batch, got %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d values, want 1", len(got))
	}
	if _, ok := got["vercel-gc://absent"]; ok {
		t.Error("absent key must be omitted from the result map")
	}
}

// The batch installs its snapshot, so a Load followed by watching costs no
// redundant fetch.
func TestResolveBatchInstallsSnapshot(t *testing.T) {
	f := newFake()
	f.set(testStore, "a", `"1"`)
	p := f.provider()
	ctx := context.Background()

	if _, err := p.ResolveBatch(ctx, []mamori.Ref{ref(t, "vercel-gc://a")}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, itemsAfterBatch := f.counts()

	if _, err := p.Resolve(ctx, ref(t, "vercel-gc://a")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, items := f.counts(); items != itemsAfterBatch {
		t.Errorf("Resolve after ResolveBatch refetched items: %d then %d", itemsAfterBatch, items)
	}
}

func TestResolveBatchEmpty(t *testing.T) {
	f := newFake()
	p := f.provider()

	got, err := p.ResolveBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d values, want 0", len(got))
	}
	if digests, items := f.counts(); digests != 0 || items != 0 {
		t.Errorf("an empty batch must make no requests, got %d digest and %d items", digests, items)
	}
}

func TestProviderImplementsBatchProvider(t *testing.T) {
	var _ mamori.BatchProvider = (*Provider)(nil)
}

// Vercel offers no streaming or blocking read, so faking a Watch with a ticker
// is forbidden by the provider contract.
func TestProviderIsNotWatchable(t *testing.T) {
	if _, ok := any(New()).(mamori.WatchableProvider); ok {
		t.Fatal("vercel-gc must not implement WatchableProvider: Vercel has no native change notification")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd providers/vercel-gc && GOWORK=off go test -run TestResolveBatch ./...`
Expected: FAIL to build, with `p.ResolveBatch undefined`.

- [ ] **Step 3: Write the implementation**

Append to `providers/vercel-gc/resolve.go`:

```go
// ResolveBatch resolves every ref, grouping by store so each store costs one
// digest request and one items request instead of one pair per ref. mamori
// calls it automatically on the Load path.
//
// A ref whose key is absent is omitted from the result map rather than failing
// the batch, per the BatchProvider contract, so mamori applies that field's
// default. A ref that cannot be parsed, or a store that cannot be read, does
// fail the batch: those are configuration and connectivity faults rather than
// an absent value.
func (p *Provider) ResolveBatch(ctx context.Context, refs []mamori.Ref) (map[string]mamori.Value, error) {
	if len(refs) == 0 {
		return map[string]mamori.Value{}, nil
	}
	conn, err := p.connection()
	if err != nil {
		return nil, err
	}

	// Group by store, preserving each ref so its Raw can key the result.
	type target struct {
		ref mamori.Ref
		key string
	}
	byStore := map[string][]target{}
	for _, r := range refs {
		store, key, err := parsePath(r.Path, conn.storeID)
		if err != nil {
			return nil, err
		}
		byStore[store] = append(byStore[store], target{ref: r, key: key})
	}

	out := make(map[string]mamori.Value, len(refs))
	for store, targets := range byStore {
		digest, err := p.fetchDigest(ctx, conn, store)
		if err != nil {
			return nil, err
		}
		snap, err := p.snapshotFor(ctx, conn, store, digest)
		if err != nil {
			return nil, err
		}
		for _, t := range targets {
			raw, ok := snap.items[t.key]
			if !ok {
				continue // omit not-found refs; mamori applies the default
			}
			v, err := valueFor(raw, t.ref, store, snap.digest)
			if err != nil {
				return nil, err
			}
			out[t.ref.Raw] = v
		}
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd providers/vercel-gc && GOWORK=off go test ./...`
Expected: PASS.

- [ ] **Step 5: Checkpoint and commit**

Run `cd providers/vercel-gc && GOWORK=off go vet ./... && GOWORK=off go test -race ./...`.

```bash
git add providers/vercel-gc/resolve.go providers/vercel-gc/batch_test.go
git commit -m "feat(vercel-gc): ResolveBatch, one request per store

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Conformance kit and live integration test

**Files:**
- Create: `providers/vercel-gc/conformance_test.go`
- Create: `providers/vercel-gc/vercelgc_integration_test.go`

**Interfaces:**
- Consumes: `newFake`, `roundTripper`, `fakeGC.provider` (Task 3); everything the provider exports.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Write the conformance test**

Create `providers/vercel-gc/conformance_test.go`. The suite must drive the fake through the in-process `roundTripper`, never `httptest.Server`: `providertest`'s `NoGoroutineLeak` case runs `goleak.VerifyNone` with no ignore options, and a live server's accept goroutine can never satisfy it.

```go
package vercelgc

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

func TestConformance(t *testing.T) {
	f := newFake()
	// Seed the store so it exists before the first resolve; an unknown store
	// is a 404, which is a different case from an absent key.
	f.set(testStore, "__conformance_bootstrap", `"ok"`)

	// quote renders a plain string as the JSON the API would store.
	quote := func(val string) string {
		b, err := json.Marshal(val)
		if err != nil {
			t.Fatalf("marshaling %q: %v", val, err)
		}
		return string(b)
	}

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider {
			return f.provider()
		},
		Ref: func(key string) string { return "vercel-gc://" + key },
		Seed: func(_ context.Context, key, val string) error {
			f.set(testStore, key, quote(val))
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set(testStore, key, quote(val))
			return nil
		},
		Fail: func(_ context.Context, _ string, _ error) error {
			// The read API surfaces failures per request, not per key, so the
			// whole store is failed and cleared. 503 maps to ErrUnavailable.
			f.failStatus(testStore, http.StatusServiceUnavailable)
			return nil
		},
		Clear: func(_ context.Context, _ string) error {
			f.clearFail(testStore)
			return nil
		},
		PointerRef: func(key, frag string) string {
			return "vercel-gc://" + key + frag
		},
	})
}
```

- [ ] **Step 2: Run the conformance suite**

Run: `cd providers/vercel-gc && GOWORK=off go test -run TestConformance -v ./...`
Expected: PASS. Watch cases skip, because the provider does not implement `WatchableProvider`.

If `JSONPointerSelection` fails, check that `providertest` seeds a JSON object for that case and that `PointerRef` puts the fragment where `mamori.SelectKey` expects it. If `NoGoroutineLeak` fails, an `httptest.Server` has leaked into the path: the conformance provider must use `roundTripper`.

- [ ] **Step 3: Run everything with the race detector**

Run: `cd providers/vercel-gc && GOWORK=off go test -race ./...`
Expected: PASS.

- [ ] **Step 4: Write the live integration test**

Create `providers/vercel-gc/vercelgc_integration_test.go`:

```go
//go:build integration

package vercelgc_test

import (
	"context"
	"os"
	"testing"

	"github.com/xavidop/mamori"
	vercelgc "github.com/xavidop/mamori/providers/vercel-gc"
)

// TestIntegrationResolve exercises a real Vercel Global Config store. It is
// guarded by a build tag and skips unless GLOBAL_CONFIG (or EDGE_CONFIG) and
// VERCEL_GC_TEST_KEY name an existing store and key.
//
// It is also the check on the one part of the read API whose response shape
// Vercel documents only as "JSON": the digest endpoint. parseDigest accepts
// both a bare string and an object with a digest field, and this test is what
// proves which one production actually returns.
//
//	export GLOBAL_CONFIG='https://global-config.vercel.com/ecfg_xxx?token=yyy'
//	export VERCEL_GC_TEST_KEY=my-existing-key
//	GOWORK=off go test -tags integration -run Integration ./...
func TestIntegrationResolve(t *testing.T) {
	if os.Getenv("GLOBAL_CONFIG") == "" && os.Getenv("EDGE_CONFIG") == "" {
		t.Skip("set GLOBAL_CONFIG or EDGE_CONFIG to run the integration test")
	}
	key := os.Getenv("VERCEL_GC_TEST_KEY")
	if key == "" {
		t.Skip("set VERCEL_GC_TEST_KEY to an existing Global Config key")
	}

	p := vercelgc.New()
	ref, err := mamori.ParseRef("vercel-gc://" + key)
	if err != nil {
		t.Fatalf("parsing ref: %v", err)
	}

	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("resolving %q: %v", key, err)
	}
	if len(v.Bytes) == 0 {
		t.Error("resolved an empty value")
	}
	if v.Metadata["digest"] == "" {
		t.Error("no digest in metadata: the digest endpoint returned a shape parseDigest did not recognize")
	}
	t.Logf("resolved %q (%d bytes), store digest %s", key, len(v.Bytes), v.Metadata["digest"])
}
```

- [ ] **Step 5: Verify the integration test compiles and skips**

Run: `cd providers/vercel-gc && GOWORK=off go test -tags integration -run Integration ./...`
Expected: PASS with a skip message, assuming no `GLOBAL_CONFIG` is set in the shell.

- [ ] **Step 6: Checkpoint and commit**

Run from the repo root: `make test` and `make vet`.

```bash
git add providers/vercel-gc/conformance_test.go providers/vercel-gc/vercelgc_integration_test.go
git commit -m "test(vercel-gc): providertest conformance and live integration test

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Documentation

Docs ship with the feature, not after it. Every surface that lists providers gets the new one.

**Files:**
- Create: `providers/vercel-gc/README.md`
- Create: `site/src/pages/docs/providers/vercel-gc.md`
- Modify: `site/src/layouts/DocsLayout.astro` (nav entry in the "KV & config" group)
- Modify: `site/src/pages/docs/providers/index.md` (provider matrix row)
- Modify: `README.md` (provider table row and install list)
- Modify: `skills/mamori/references/providers.md` (scheme table row)

**Interfaces:**
- Consumes: the shipped provider API from Tasks 1 to 5.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Write the module README**

Create `providers/vercel-gc/README.md` following the structure of `providers/launchdarkly/README.md`: a one-paragraph intro, the conformance badge, the import line, `## Scheme` with the grammar and a ref-examples table, a Go struct example, `### How stored values map to config values` with the value table from the spec, `## Error classification` with the status table and the 403 caveat stated plainly, `## Authentication & configuration` covering `GLOBAL_CONFIG` and the `EDGE_CONFIG` fallback, an options table, `## No native watch` explaining the digest-gated poll and why `WatchableProvider` is deliberately not implemented, `## Testing status`, and `## Development` with the `GOWORK=off` commands.

The value table:

| Stored value | Bytes |
| --- | --- |
| string | the raw text, unquoted |
| boolean | `true` or `false` |
| number | its JSON text, e.g. `5432` or `0.25` |
| object or array | its compacted JSON encoding |
| null | `null`, because a key stored as null exists |

State three things explicitly, because each is a decision a reader would otherwise assume went the other way:
- `Value.Sensitive` is always `false`; wrap a field in `secret.String` to redact anyway.
- `Value.Version` is a content hash, not the store digest, because the digest moves on any edit to any key.
- Each `Resolve` makes a digest request; there is no cache TTL, so `Refresh` and `Doctor` always reach Vercel.

- [ ] **Step 2: Write the site page**

Create `site/src/pages/docs/providers/vercel-gc.md` with the standard front matter:

```markdown
---
layout: ../../../layouts/DocsLayout.astro
title: Vercel Global Config
---
```

Cover the same ground as the module README. Compare against a sibling page such as `site/src/pages/docs/providers/doppler.md` for heading depth and tone.

- [ ] **Step 3: Add the navigation entry**

In `site/src/layouts/DocsLayout.astro`, add to the `KV & config` group, after the `providers/etcd` line:

```js
      { slug: "providers/vercel-gc", title: "Vercel Global Config", indent: true },
```

- [ ] **Step 4: Add the provider matrix row**

In `site/src/pages/docs/providers/index.md`, add after the `etcd://` row:

```markdown
| `vercel-gc://` | Vercel Global Config | no | poll (digest) | ✅ |
```

- [ ] **Step 5: Update the root README**

In `README.md`, add to the provider table after the `providers/etcd` row:

```markdown
| `providers/vercel-gc` | `vercel-gc://` | poll (digest) | ✅ |
```

and add to the install code block under `## Install`:

```
go get github.com/xavidop/mamori/providers/vercel-gc  # vercel-gc://
```

- [ ] **Step 6: Update the agent skill**

In `skills/mamori/references/providers.md`, add a row to the scheme table matching the existing format:

```markdown
| `vercel-gc` | `vercel-gc://` | `vercel-gc://my-flag`, `vercel-gc://ecfg_abc/my-flag` |
```

- [ ] **Step 7: Verify the site builds with no broken links**

Run from the repo root: `make site-linkcheck`
Expected: the build succeeds and the link checker reports no broken internal links.

- [ ] **Step 8: Checkpoint and commit**

Run from the repo root: `make build && make test && make vet`.

```bash
git add providers/vercel-gc/README.md site/src/pages/docs/providers/vercel-gc.md site/src/layouts/DocsLayout.astro site/src/pages/docs/providers/index.md README.md skills/mamori/references/providers.md
git commit -m "docs(vercel-gc): module README, site page, provider matrix and skill reference

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Final verification

- [ ] From the repo root, `make all` passes (tidy, build, test, lint across every module).
- [ ] `cd providers/vercel-gc && GOWORK=off go test -race ./...` passes.
- [ ] `providers/vercel-gc/go.mod` has exactly one non-indirect require, `github.com/xavidop/mamori`.
- [ ] `grep -rn '—' providers/vercel-gc site/src/pages/docs/providers/vercel-gc.md` returns nothing.
- [ ] The provider appears in all four places: root README table, site provider matrix, site nav, agent skill reference.
