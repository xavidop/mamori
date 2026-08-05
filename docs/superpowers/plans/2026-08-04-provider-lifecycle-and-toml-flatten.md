# Provider lifecycle and TOML flatten Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** give every provider that holds a backend connection a reachable `Close`, and let `flatten` decode TOML alongside JSON, YAML and env.

**Architecture:** Two independent changes. TOML adds one `case` to `decodeFlatten` plus a core dependency. The lifecycle work adds `Close() error` to 22 provider modules against a stated contract, enforced by a new `providertest` conformance case. Core's public API does not change: `Watcher.Close()` still closes only what mamori created, and the caller owns every provider instance it constructs.

**Tech Stack:** Go 1.26, multi-module monorepo driven by `go.work`, `github.com/pelletier/go-toml/v2`, `providertest` conformance kit, `goleak`.

**Spec:** `docs/superpowers/specs/2026-08-04-provider-lifecycle-and-toml-flatten-design.md`

## Global Constraints

- **No em-dash.** The U+2014 character must not appear in prose, docs, code comments, or commit messages. Use a spaced hyphen ( - ), a colon, parentheses, or restructure.
- **Commits are authorized for this execution.** The repo's standing rule is that the user handles all git, but the user explicitly relaxed it for this branch on 2026-08-04: implementers commit per task so review packages can diff task boundaries. Run `git commit` with the exact message each Commit step gives. Never run `git push`: the controller handles pushing. Stage only the files your task touched, and never stage anything under `docs/superpowers/`.
- **Docs ship with every feature.** A feature is not done until `site/`, the module README, the root README, and `skills/` are updated. Tasks 8 and 9 carry this for their features.
- **Core dependency discipline.** Core (`github.com/xavidop/mamori`) must not gain a cloud SDK dependency. `pelletier/go-toml/v2` is the single approved addition in this plan.
- **Multi-module.** Each provider is its own module. After touching a provider's `go.mod`, run `make tidy` and `make work-sync` from the repo root.
- **Go version floor:** core `go.mod` declares `go 1.26.0`; `go.work` declares `go 1.26.5`.
- **Error sentinel:** post-close `Resolve` must return an error satisfying `errors.Is(err, mamori.ErrUnavailable)` (`errors.go:161`).

## The Close contract

Every `Close` added by this plan satisfies all four:

1. **Idempotent.** Two calls, no error, no panic.
2. **Safe with no prior use.** `New()` then `Close()` with no `Resolve` in between must not dial, block or panic.
3. **Concurrency-safe.** Callable while `Resolve` is in flight and while another goroutine calls `Close`.
4. **Terminal.** After `Close`, `Resolve` returns `mamori.ErrUnavailable` rather than rebuilding a client or panicking on a nil one.

A provider must never close a client the caller injected (`WithPool`, `WithDB`, `WithHTTPClient`). Track ownership with a boolean set only on the lazily-built path. `providers/mysql/mysql.go:380` is the existing reference implementation of this rule.

`Close` does not stop a `Watch`. Context cancellation remains the only watch shutdown path, already covered by `testWatchCloses` (`providertest/providertest.go:217`).

---

# Part 1: TOML flatten

### Task 1: TOML flatten decoding in core

**Files:**
- Modify: `go.mod` (add `github.com/pelletier/go-toml/v2`)
- Modify: `decode.go:23`, `decode.go:143`, `decode.go:262`, `decode.go:265-297`
- Modify: `reconcile.go:128`
- Test: `decode_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `flatten:"toml"` as an accepted struct tag value. No new exported Go symbol.

- [ ] **Step 1: Write the failing tests**

Append to `decode_test.go`:

```go
func TestLoadFlattenTOML(t *testing.T) {
	type Redis struct {
		Host     string        `mapstructure:"host"`
		Port     int           `mapstructure:"port"`
		Password secret.String `mapstructure:"password"`
		Timeout  time.Duration `mapstructure:"timeout"`
	}
	type Config struct {
		Redis Redis `source:"env:REDIS_TOML" flatten:"toml"`
	}

	t.Setenv("REDIS_TOML", "host = \"cache.internal\"\nport = 6379\npassword = \"hunter2\"\ntimeout = \"5s\"\n")

	cfg, err := Load[Config](context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Redis.Host != "cache.internal" {
		t.Errorf("Host = %q, want cache.internal", cfg.Redis.Host)
	}
	if cfg.Redis.Port != 6379 {
		t.Errorf("Port = %d, want 6379", cfg.Redis.Port)
	}
	if cfg.Redis.Password.Reveal() != "hunter2" {
		t.Errorf("Password did not decode through flattenHook")
	}
	if cfg.Redis.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", cfg.Redis.Timeout)
	}
}

func TestLoadFlattenTOMLNestedTable(t *testing.T) {
	type Inner struct {
		User string `mapstructure:"user"`
	}
	type Outer struct {
		Name  string `mapstructure:"name"`
		Creds Inner  `mapstructure:"creds"`
	}
	type Config struct {
		App Outer `source:"env:APP_TOML" flatten:"toml"`
	}

	t.Setenv("APP_TOML", "name = \"svc\"\n\n[creds]\nuser = \"admin\"\n")

	cfg, err := Load[Config](context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.App.Creds.User != "admin" {
		t.Errorf("nested table did not decode: Creds.User = %q", cfg.App.Creds.User)
	}
}

func TestLoadFlattenTOMLMalformedIsLoud(t *testing.T) {
	type Inner struct {
		Host string `mapstructure:"host"`
	}
	type Config struct {
		App Inner `source:"env:BAD_TOML" flatten:"toml"`
	}

	t.Setenv("BAD_TOML", "this is not = = valid toml")

	_, err := Load[Config](context.Background())
	if err == nil {
		t.Fatal("malformed TOML decoded silently; want an error")
	}
	if !strings.Contains(err.Error(), "toml flatten") {
		t.Errorf("error %q does not name the toml flatten stage", err)
	}
}
```

Ensure `decode_test.go` imports `strings`, `time`, and `github.com/xavidop/mamori/secret`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./ -run 'TestLoadFlattenTOML' -v`

Expected: all three FAIL. The first two fail with `unknown flatten "toml"`; the third fails because `err` is non-nil but the message names `unknown flatten` rather than `toml flatten`.

- [ ] **Step 3: Add the dependency**

```bash
go get github.com/pelletier/go-toml/v2@latest
```

- [ ] **Step 4: Add the decode case**

In `decode.go`, add the import `toml "github.com/pelletier/go-toml/v2"`, then add this case to the `switch spec.Flatten` in `decodeFlatten`, between the `yaml` and `env` arms:

```go
	case "toml":
		if err := toml.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("mamori: field %s: toml flatten: %w", spec.Path, err)
		}
```

- [ ] **Step 5: Update the four comment and error sites**

`decode.go:23`, the `Flatten` field comment:

```go
	Flatten    string       // "", "json", "yaml", "toml", or "env"
```

`decode.go:143`, the missing-tag error:

```go
			return nil, fmt.Errorf("mamori: field %s is a struct with a source but no flatten tag; add flatten:\"json|yaml|toml|env\"", path)
```

`decode.go:262`, `decodeFlatten`'s doc comment first line:

```go
// struct field, per the flatten tag. It supports json, yaml, toml, and env
// (KEY=VALUE lines). Secret and duration fields inside the flattened struct are
// handled via mapstructure decode hooks.
```

`reconcile.go:128`, `WithDecodeHook`'s doc comment:

```go
// flatten:"json|yaml|toml|env" payload into a nested struct. Hooks run after the
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./ -run 'TestLoadFlatten' -v`

Expected: PASS, including the pre-existing `TestLoadFlattenJSON`.

- [ ] **Step 7: Run the full core suite and the race detector**

Run: `go test ./... && go test -race ./ -run 'TestLoadFlatten'`

Expected: PASS. If any test asserts the exact text of the `add flatten:` error from Step 5, update it to the new string.

- [ ] **Step 8: Tidy and verify the dependency landed only in core**

Run: `make tidy && make work-sync && git diff --stat go.mod go.sum`

Expected: the root `go.mod` gains exactly one **direct** require, `github.com/pelletier/go-toml/v2`, and no other direct dependency anywhere.

Every other module in `go.work` will also change, gaining that same module as a single `// indirect` line. That is correct and unavoidable: each module `replace`-imports core, so once core requires `go-toml/v2` it enters every dependent module's build graph and `go mod tidy` records it. A module missing the entry fails to build with `missing go.sum entry`. Commit that propagation separately from the feature (see Task 9 Step 3, which states the same invariant).

- [ ] **Step 9: Commit**

```bash
git add go.mod go.sum go.work.sum decode.go reconcile.go decode_test.go
git commit -m "feat(core): decode TOML payloads with flatten:\"toml\""
```

---

### Task 2: TOML documentation

**Files:**
- Modify: `site/src/pages/docs/concepts/index.md:53`
- Modify: `site/src/pages/docs/concepts/decoding.md`
- Modify: `site/src/pages/docs/providers/file.md`
- Modify: `README.md:235`
- Check: `skills/mamori/`

**Interfaces:**
- Consumes: `flatten:"toml"` from Task 1.
- Produces: nothing code-facing.

- [ ] **Step 1: Update the tag reference table**

In `site/src/pages/docs/concepts/index.md:53`, change the flatten row:

```markdown
| `flatten:"json\|yaml\|toml\|env"` | Decode a single provider payload into a nested struct. |
```

- [ ] **Step 2: Add a TOML example to the file provider page**

In `site/src/pages/docs/providers/file.md`, after the existing `flatten:"yaml"` example around line 37 to 44, add:

```markdown
- `file:///etc/app/config.toml` with `flatten:"toml"` decodes the file into a nested struct.

```go
type Config struct {
	Config AppConfig `source:"file:///etc/app/config.toml" flatten:"toml"`
}
```
```

- [ ] **Step 3: Mention TOML in the decoding concept page**

In `site/src/pages/docs/concepts/decoding.md`, wherever the flatten formats are enumerated, add `toml` to the list. Keep the sentence structure already there; do not restructure the page.

- [ ] **Step 4: Correct the root README dependency sentence**

`README.md:235` currently reads that the core depends only on `go-playground/validator`, `go-viper/mapstructure`, and `fsnotify`, which already omits `yaml.v3`. Replace that clause with:

```markdown
The core (`github.com/xavidop/mamori`) depends only on `go-playground/validator`, `go-viper/mapstructure`, `fsnotify`, `yaml.v3`, and `go-toml/v2`.
```

- [ ] **Step 5: Check the agent skill**

Run: `grep -rn "flatten" skills/mamori/`

If any file enumerates the flatten formats, add `toml`. If nothing matches, no change is needed.

- [ ] **Step 6: Build the docs site and check links**

Run: `make site-build && make site-linkcheck`

Expected: build succeeds, no broken internal links.

- [ ] **Step 7: Commit**

```bash
git add site/ README.md skills/
git commit -m "docs: document flatten:\"toml\""
```

---

# Part 2: Provider resource lifecycle

### Task 3: providertest closer conformance case

This task comes first in Part 2 so every subsequent provider task has a harness to prove itself against. Expect it to fail against some of the nine providers that already have a `Close`; Task 7 fixes those.

**Files:**
- Modify: `providertest/providertest.go` (add cases to `Run`, add two test functions)
- Test: `providertest/providertest_test.go`

**Interfaces:**
- Consumes: `mamori.ErrUnavailable` (`errors.go:161`), `Config.New`, `Config.Key`, `Config.parseRef` (all existing in `providertest`).
- Produces: two subtests registered in `providertest.Run`, named `CloserContract` and `CloseDuringResolve`, plus the exported-for-testing pure checker `checkCloserContract(p mamori.Provider, ref mamori.Ref) error`. Provider modules do not call these directly; they run automatically for any provider whose `New()` returns something satisfying `io.Closer`.

The assertions live in `checkCloserContract`, a pure function returning an error, rather than directly in the `testing.T` helper. That split exists so the meta-test in Step 1 can assert the checker rejects a bad provider: calling a `t.Fatalf`-based helper with a hand-constructed `&testing.T{}` would hit `runtime.Goexit` rather than recording a failure, so the meta-test could not observe the result.

- [ ] **Step 1: Write the failing test**

Add to `providertest/providertest_test.go` a fake provider that deliberately violates the contract, proving the checker detects it:

```go
// closeLeakProvider implements io.Closer but keeps resolving after Close, which
// the CloserContract case must reject.
type closeLeakProvider struct{ closed bool }

func (p *closeLeakProvider) Scheme() string { return "closeleak" }
func (p *closeLeakProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	return mamori.Value{Bytes: []byte("still alive"), Version: "1"}, nil
}
func (p *closeLeakProvider) Close() error { p.closed = true; return nil }

// closeCleanProvider satisfies the contract.
type closeCleanProvider struct {
	mu     sync.Mutex
	closed bool
}

func (p *closeCleanProvider) Scheme() string { return "closeclean" }
func (p *closeCleanProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return mamori.Value{}, fmt.Errorf("%w: closeclean: provider is closed", mamori.ErrUnavailable)
	}
	return mamori.Value{Bytes: []byte("alive"), Version: "1"}, nil
}
func (p *closeCleanProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

func TestCheckCloserContractRejectsResolveAfterClose(t *testing.T) {
	ref, err := mamori.ParseRef("closeleak://some-key")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if err := checkCloserContract(&closeLeakProvider{}, ref); err == nil {
		t.Fatal("checkCloserContract accepted a provider that resolves after Close")
	}
}

func TestCheckCloserContractAcceptsACompliantProvider(t *testing.T) {
	ref, err := mamori.ParseRef("closeclean://some-key")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if err := checkCloserContract(&closeCleanProvider{}, ref); err != nil {
		t.Fatalf("checkCloserContract rejected a compliant provider: %v", err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./providertest/ -run TestCheckCloserContract -v`

Expected: both FAIL with `undefined: checkCloserContract`.

- [ ] **Step 3: Implement the checker and the conformance cases**

Add to `providertest/providertest.go` (import `io`, `fmt` and `sync`):

```go
// checkCloserContract runs the Close contract assertions against p and reports
// the first violation. It is a pure function so it can itself be tested; the
// testing.T wrapper below is what the conformance kit registers.
//
// The caller must have already established that p implements io.Closer.
func checkCloserContract(p mamori.Provider, ref mamori.Ref) error {
	closer, ok := p.(io.Closer)
	if !ok {
		return errors.New("provider does not implement io.Closer")
	}

	// Close on a provider that has never resolved must not dial or panic.
	// Providers that build their client lazily (postgres, mysql) reach this
	// path whenever a configured source turns out to be unused.
	if err := closer.Close(); err != nil {
		return fmt.Errorf("Close on a provider that never resolved: %w", err)
	}

	// Idempotent.
	if err := closer.Close(); err != nil {
		return fmt.Errorf("second Close: %w", err)
	}

	// Terminal: a closed provider reports unavailable rather than silently
	// rebuilding the client it was just told to release.
	_, err := p.Resolve(context.Background(), ref)
	if err == nil {
		return errors.New("Resolve after Close returned a value; want mamori.ErrUnavailable")
	}
	if !errors.Is(err, mamori.ErrUnavailable) {
		return fmt.Errorf("Resolve after Close = %v; want errors.Is(err, mamori.ErrUnavailable)", err)
	}
	return nil
}

// testCloser verifies the Close contract for any provider that implements
// io.Closer. Providers that hold no releasable resource do not implement it and
// skip this case, which is why the kit type-asserts rather than requiring it.
func testCloser(t *testing.T, c Config) {
	p := c.New()
	if _, ok := p.(io.Closer); !ok {
		t.Skip("provider does not implement io.Closer")
	}
	if err := checkCloserContract(p, c.parseRef(t, c.key("afterclose"))); err != nil {
		t.Fatal(err)
	}
}

// testCloseDuringResolve closes a provider while resolves are in flight. Either
// outcome is acceptable for any individual resolve (a value, or an error); what
// must not happen is a panic on a released client or a data race. Run under
// -race for this to carry its full weight.
func testCloseDuringResolve(t *testing.T, c Config) {
	p := c.New()
	closer, ok := p.(io.Closer)
	if !ok {
		t.Skip("provider does not implement io.Closer")
	}

	ctx := context.Background()
	key := c.key("closerace")
	if err := c.Seed(ctx, key, "value"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	ref := c.parseRef(t, key)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.Resolve(ctx, ref) // value or error, both fine
		}()
	}
	if err := closer.Close(); err != nil {
		t.Errorf("Close during in-flight resolves: %v", err)
	}
	wg.Wait()
}
```

- [ ] **Step 4: Register both cases in `Run`**

In `providertest/providertest.go`, add these two lines after the `ConcurrentResolve` line (`providertest.go:214`) and before `VersionMonotonic`:

```go
	t.Run("CloserContract", func(t *testing.T) { testCloser(t, c) })
	t.Run("CloseDuringResolve", func(t *testing.T) { testCloseDuringResolve(t, c) })
```

They must sit inside the existing `ignoreGoroutines` envelope so `NoGoroutineLeak` still sees anything they leak.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./providertest/ -race -v`

Expected: PASS, including both `TestCheckCloserContractRejectsResolveAfterClose` and `TestCheckCloserContractAcceptsACompliantProvider`.

- [ ] **Step 6: Establish the baseline across every provider**

Run: `go test ./providers/... -run 'TestConformance|TestProvider' 2>&1 | tee /tmp/closer-baseline.txt; grep -E "FAIL|CloserContract" /tmp/closer-baseline.txt`

Expected: providers with no `Close` skip. Some of the nine that already have one may FAIL on the terminal-resolve requirement. Record which ones; Task 7 fixes exactly that list. Do not fix them here.

- [ ] **Step 7: Commit**

```bash
git add providertest/
git commit -m "test(providertest): add the Close contract conformance case"
```

---

### Task 4: postgres Close

The sharpest case in the codebase: `pgxpool.Pool` built lazily, plus a `WithPool` injection path whose pool the caller owns.

**Files:**
- Modify: `providers/postgres/postgres.go:135-145` (struct), `:156-164` (`WithPool`), `:237-256` (`backendFor`), plus a new `Close`
- Test: `providers/postgres/postgres_test.go`

**Interfaces:**
- Consumes: the `CloserContract` and `CloseDuringResolve` cases from Task 3.
- Produces: `func (p *Provider) Close() error` on `providers/postgres`.

- [ ] **Step 1: Write the failing tests**

Add to `providers/postgres/postgres_test.go`:

```go
func TestCloseWithoutResolveNeverDials(t *testing.T) {
	// A DSN pointing at a black hole: if Close dials, this test hangs or errors.
	p := New(WithDSN("postgres://user:pw@203.0.113.1:5432/db"))
	if err := p.Close(); err != nil {
		t.Fatalf("Close on an unused provider: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestResolveAfterCloseIsUnavailable(t *testing.T) {
	p := New(WithDSN("postgres://user:pw@203.0.113.1:5432/db"))
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ref, err := mamori.ParseRef("postgres://settings/some-key")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	_, err = p.Resolve(context.Background(), ref)
	if !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("Resolve after Close = %v; want ErrUnavailable", err)
	}
}

func TestCloseDoesNotCloseAnInjectedPool(t *testing.T) {
	// WithPool hands in a caller-owned backend; Close must leave it usable.
	fake := newFakeBackend() // existing test helper in this package
	p := New()
	p.be = fake
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fake.closed {
		t.Error("Close released a caller-injected backend; the caller owns it")
	}
}
```

If the package's existing fake backend has no `closed` field, add one and set it in whatever `Close`-like method it has. If it has no such method, add `closed bool` and leave it false, so the assertion still proves the provider did not reach for it.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./providers/postgres/ -run 'TestClose|TestResolveAfterClose' -v`

Expected: FAIL with `p.Close undefined`.

- [ ] **Step 3: Add the ownership and closed fields**

In `providers/postgres/postgres.go`, extend the `Provider` struct (currently ending at line 145):

```go
	mu      sync.Mutex
	be      backend         // resolved backend (injected or lazily built)
	ownPool *pgxpool.Pool   // non-nil only when backendFor built it, so Close
	                        // never releases a pool injected via WithPool
	closed  bool
```

- [ ] **Step 4: Record ownership on the lazy path and refuse a closed provider**

Replace `backendFor` (`postgres.go:237-256`) with:

```go
func (p *Provider) backendFor(ctx context.Context) (backend, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, fmt.Errorf("%w: postgres: provider is closed", mamori.ErrUnavailable)
	}
	if p.be != nil {
		return p.be, nil
	}
	dsn := p.dsn
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return nil, errors.New("postgres: no DSN configured; set DATABASE_URL or use postgres.WithDSN")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	p.ownPool = pool
	p.be = &poolBackend{pool: pool}
	return p.be, nil
}
```

- [ ] **Step 5: Implement Close**

Add after `backendFor`:

```go
// Close releases the connection pool this provider built lazily. It is a no-op
// when the backend was injected with WithPool (the caller owns that pool) and
// when nothing was ever built, so New followed by Close never dials. Close is
// idempotent, and afterwards Resolve and Watch report mamori.ErrUnavailable
// rather than rebuilding the pool they were just told to release.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.be = nil
	if p.ownPool != nil {
		p.ownPool.Close() // pgxpool.Pool.Close returns nothing
		p.ownPool = nil
	}
	return nil
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./providers/postgres/ -race -v`

Expected: PASS, including the existing conformance suite, whose `CloserContract` case now runs instead of skipping.

- [ ] **Step 7: Commit**

```bash
git add providers/postgres/
git commit -m "feat(postgres): release the lazily built pool with Close"
```

---

### Task 5: remaining tier-1 providers

Six modules that hold a real closable resource. Each follows the Task 4 shape: a `closed` guard checked wherever the client is obtained, an ownership flag so an injected client is never closed, and an idempotent `Close`.

Two of these already have a working `Close` on an unexported adapter that no caller can reach: `providers/redis/redis.go:92` (`universalAdapter.Close`) and `providers/launchdarkly/launchdarkly.go:119` (`realClient.Close`). Those need exposing on `*Provider`, not writing from scratch.

**Files:**
- Modify: `providers/mongodb/mongodb.go`, `providers/etcd/etcd.go`, `providers/sqlite/sqlite.go`, `providers/redis/redis.go`, `providers/launchdarkly/launchdarkly.go`, `providers/k8s/k8s.go`
- Test: the matching `_test.go` in each module

**Interfaces:**
- Consumes: the Task 3 conformance cases.
- Produces: `func (p *Provider) Close() error` on each of the six modules.

Per-module specifics:

| Module | Release call | Injection option whose client must NOT be closed |
|---|---|---|
| `mongodb` | `client.Disconnect(ctx)` | `WithClient` |
| `etcd` | `client.Close()` | `WithClient` |
| `sqlite` | `db.Close()` | `WithDB` |
| `redis` | the existing `universalAdapter.Close()` | `WithClient` |
| `launchdarkly` | the existing `realClient.Close()` | `WithClient` |
| `k8s` | `CloseIdleConnections()` on the rest client transport | `WithClientset` |

Check each module's actual option names with `grep -n "^func With" providers/<name>/*.go` before writing; the table records intent, and the option name in the code wins.

For each of the six modules, in order:

- [ ] **Step 1: Write the failing tests**

For module `<name>`, add to `providers/<name>/<name>_test.go`, substituting the package's real constructor options:

```go
func TestCloseWithoutUseIsSafe(t *testing.T) {
	p := New()
	if err := p.Close(); err != nil {
		t.Fatalf("Close on an unused provider: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestResolveAfterCloseIsUnavailable(t *testing.T) {
	p := New()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ref, err := mamori.ParseRef("<scheme>://some/key")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if _, err := p.Resolve(context.Background(), ref); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("Resolve after Close = %v; want ErrUnavailable", err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./providers/<name>/ -run 'TestClose|TestResolveAfterClose' -v`

Expected: FAIL with `p.Close undefined`.

- [ ] **Step 3: Add the closed guard and ownership flag**

Add `closed bool` to the `Provider` struct, plus an `own<Client>` flag set only where the provider constructs the client itself. Guard the accessor that returns the client:

```go
	if p.closed {
		return nil, fmt.Errorf("%w: <name>: provider is closed", mamori.ErrUnavailable)
	}
```

If the module has no lazy accessor and stores the client directly on the struct, add the same guard at the top of `Resolve`.

- [ ] **Step 4: Implement Close**

```go
// Close releases the backing client this provider created. It is a no-op when
// the client was injected by the caller (who owns it) and when none was ever
// created. Close is idempotent, and afterwards Resolve reports
// mamori.ErrUnavailable.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if !p.ownClient || p.client == nil {
		return nil
	}
	err := p.client.Close() // or Disconnect(ctx) for mongodb
	p.client = nil
	return err
}
```

For `mongodb`, `Disconnect` needs a context; use `context.Background()` bounded by a short timeout:

```go
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := p.client.Disconnect(ctx)
```

- [ ] **Step 5: Run to verify they pass**

Run: `go test ./providers/<name>/ -race -v`

Expected: PASS, with `CloserContract` and `CloseDuringResolve` now running rather than skipping.

- [ ] **Step 6: Commit each module separately**

```bash
git add providers/<name>/
git commit -m "feat(<name>): release the backing client with Close"
```

---

### Task 6: tier-2 HTTP providers

Fifteen modules that own only an `*http.Client`. `CloseIdleConnections` is close to a no-op, and the point is uniformity: a caller should be able to write `defer p.Close()` against any provider that owns a connection without type-asserting to find out whether this one needs it. It also matters for callers who inject a client with a custom `Transport`.

**Files:**
- Modify, one `Close` each: `providers/doppler/doppler.go`, `providers/supabase/supabase.go`, `providers/heroku/heroku.go`, `providers/infisical/infisical.go`, `providers/posthog/posthog.go`, `providers/scaleway-sm/scalewaysm.go`, `providers/vercel-gc/vercelgc.go`, `providers/bitwarden/bitwarden.go`, `providers/cloudflare-kv/cloudflarekv.go`, `providers/nacos/nacos.go`, `providers/hcp-vault-secrets/hcpvaultsecrets.go`, `providers/onepassword/onepassword.go`, `providers/https/https.go`, `providers/mamori/mamori.go`, `providers/firebase-rtdb/firebasertdb.go`
- Test: the matching `_test.go` in each module

**Interfaces:**
- Consumes: the Task 3 conformance cases.
- Produces: `func (p *Provider) Close() error` on each of the fifteen modules.

The `*http.Client` field name varies. Verified names:

| Field name | Modules |
|---|---|
| `httpClient` | doppler, supabase, heroku, infisical, posthog, scaleway-sm, vercel-gc, bitwarden, cloudflare-kv, nacos, hcp-vault-secrets |
| `hc` | onepassword |
| `Client` (exported) | https |
| `HTTPClient` and `client` | mamori |
| none directly; holds `httpcore.SSEConfig` and a `backend` | firebase-rtdb |

- [ ] **Step 1: Write the failing test for the module**

```go
func TestCloseIsIdempotentAndTerminal(t *testing.T) {
	p := New()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	ref, err := mamori.ParseRef("<scheme>://some/key")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if _, err := p.Resolve(context.Background(), ref); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("Resolve after Close = %v; want ErrUnavailable", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./providers/<name>/ -run TestCloseIsIdempotentAndTerminal -v`

Expected: FAIL with `p.Close undefined`.

- [ ] **Step 3: Add the closed guard**

Add `closed bool` guarded by the struct's existing mutex (add a `sync.Mutex` if the struct has none), and guard `Resolve`:

```go
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return mamori.Value{}, fmt.Errorf("%w: <name>: provider is closed", mamori.ErrUnavailable)
	}
```

- [ ] **Step 4: Implement Close**

Using the field name from the table above:

```go
// Close marks the provider closed and returns its idle HTTP connections to the
// pool. It is idempotent, and afterwards Resolve reports mamori.ErrUnavailable.
// A caller-injected client (WithHTTPClient) keeps its own transport: only idle
// connections are released, never the client itself.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.httpClient != nil {
		p.httpClient.CloseIdleConnections()
	}
	return nil
}
```

`firebase-rtdb` is the one module in this tier with a different shape: it holds no direct `*http.Client`, only an `httpcore.SSEConfig` and a `newBackend` factory (`providers/firebase-rtdb/firebasertdb.go:124-128`). Before writing its `Close`, inspect what a live backend holds:

Run: `grep -n "type backend\|func.*backend.*Close\|sse\." providers/firebase-rtdb/firebasertdb.go providers/httpcore/sse.go`

`providers/httpcore/sse.go` already has a `Close` on its stream type (verified present). Give the provider a `closed` flag plus a reference to any live stream it created, and have `Close` set the flag and call that stream's `Close`. If the provider creates a stream only inside `Watch` and hands ownership to the watch goroutine, then `Close` sets the flag only, and the watch continues to end on context cancellation as before: do not add a second teardown path.

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./providers/<name>/ -race -v`

Expected: PASS.

- [ ] **Step 6: Commit each module separately**

```bash
git add providers/<name>/
git commit -m "feat(<name>): add Close releasing idle HTTP connections"
```

---

### Task 7: audit the nine providers that already have Close

`gcp`, `gcs`, `mysql`, `firestore`, `configcat`, `firebase-rc`, `growthbook`, `split`, `unleash` expose `Close() error` already, written before a contract existed. Task 3 Step 6 recorded which of them fail `CloserContract`.

**Files:**
- Modify: only the modules that failed the Task 3 baseline
- Test: the matching `_test.go` in each

**Interfaces:**
- Consumes: the `/tmp/closer-baseline.txt` failure list from Task 3 Step 6.
- Produces: all nine modules passing `CloserContract` and `CloseDuringResolve`.

- [ ] **Step 1: Re-run the baseline to get the current list**

Run: `go test ./providers/{gcp,gcs,mysql,firestore,configcat,firebase-rc,growthbook,split,unleash}/ -race -run 'Conformance' -v 2>&1 | grep -E "FAIL|CloserContract|CloseDuringResolve"`

Expected: a list of which modules fail which case. The likely failure is the terminal requirement: `gcp`'s `Close` sets `p.client = nil` (`providers/gcp/gcp.go:144`) and its lazy accessor will simply build a fresh client on the next `Resolve`.

- [ ] **Step 2: For each failing module, write the failing test**

```go
func TestResolveAfterCloseIsUnavailable(t *testing.T) {
	p := New()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ref, err := mamori.ParseRef("<scheme>://some/key")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if _, err := p.Resolve(context.Background(), ref); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("Resolve after Close = %v; want ErrUnavailable", err)
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./providers/<name>/ -run TestResolveAfterCloseIsUnavailable -v`

Expected: FAIL, because the provider rebuilds its client.

- [ ] **Step 4: Add the closed guard**

Add `closed bool` to the struct and guard the lazy client accessor, keeping the existing `Close` body and adding `p.closed = true` to it:

```go
	if p.closed {
		return nil, fmt.Errorf("%w: <name>: provider is closed", mamori.ErrUnavailable)
	}
```

- [ ] **Step 5: Verify mysql's ownership rule still holds**

Run: `go test ./providers/mysql/ -race -v`

`providers/mysql/mysql.go:380` already refuses to close a `WithDB`-injected pool. Confirm the added `closed` flag did not change that: a caller-injected `*sql.DB` must still be left open, while `Resolve` still reports unavailable.

- [ ] **Step 6: Run the whole provider tree**

Run: `go test ./providers/... -race`

Expected: PASS with no `CloserContract` failures anywhere.

- [ ] **Step 7: Commit**

```bash
git add providers/
git commit -m "fix(providers): make existing Close implementations terminal"
```

---

### Task 8: provider lifecycle documentation

**Files:**
- Create: an ownership section in `site/src/pages/docs/writing-a-provider/index.md`
- Modify: `site/src/pages/docs/writing-a-provider/capabilities.md`, `site/src/pages/docs/writing-a-provider/conformance.md`
- Modify: `site/src/pages/docs/providers/<name>.md` for each of the 22 changed modules
- Modify: `providers/<name>/README.md` for each of the 22 changed modules
- Modify: `README.md`, `doc.go`
- Check: `skills/mamori/`

**Interfaces:**
- Consumes: the `Close` methods from Tasks 4 through 7.
- Produces: nothing code-facing.

- [ ] **Step 1: Write the ownership section**

Add to `site/src/pages/docs/writing-a-provider/index.md`, after the SPI introduction:

```markdown
## Who closes a provider

mamori never closes a provider. `Watcher.Close()` releases only what mamori
itself created: the watch goroutines, the callback queue, and the admin server.
A provider instance belongs to whoever constructed it.

That rule exists because `Register` stores a provider in a process-global map,
normally from a package `init`, and every `Watcher` in the process shares that
one instance. A `Watcher.Close()` that released it would hand the next `Watch`
call a dead client.

So a provider that holds a connection is yours to close:

```go
p := postgres.New(postgres.WithDSN(dsn))
defer p.Close()      // you own p

w, err := mamori.Watch[Config](ctx, mamori.WithProvider(p))
defer w.Close()      // closes only what mamori created
```

Providers that hold no releasable handle (`env:`, `file://`, `aws-sm://`, and
others) have no `Close` at all, so there is nothing to forget.

### The contract

A provider `Close` is:

- **Idempotent.** Two calls, no error.
- **Safe with no prior use.** Constructing a provider and closing it without
  resolving must not dial. Lazily built pools make this a real path, not a
  hypothetical.
- **Concurrency-safe.** Callable while a `Resolve` is in flight.
- **Terminal.** After `Close`, `Resolve` returns `mamori.ErrUnavailable` rather
  than rebuilding the client it was just told to release.
- **Never the owner of an injected client.** A pool passed in through
  `WithPool`, `WithDB` or `WithHTTPClient` belongs to the caller. Track what you
  built yourself and release only that.

`Close` does not stop a `Watch`. Cancelling the context does, and that stays the
only shutdown path.
```

- [ ] **Step 2: Add the conformance checklist line**

In `site/src/pages/docs/writing-a-provider/conformance.md`, add to the checklist near line 81:

```markdown
- [ ] If the provider holds a releasable handle, `Close` is idempotent, safe without prior use, never closes a caller-injected client, and leaves `Resolve` reporting `ErrUnavailable`.
```

Also add `Close` contract to the list of what `providertest.Run` exercises in that file's opening paragraph.

- [ ] **Step 3: Document Close in capabilities.md**

In `site/src/pages/docs/writing-a-provider/capabilities.md`, add `io.Closer` alongside the existing optional `WatchableProvider` and `BatchProvider` entries, noting it is stdlib `io.Closer` rather than a mamori interface.

- [ ] **Step 4: Update the 22 provider pages and READMEs**

For each module changed in Tasks 4 through 6, add a line to its `site/src/pages/docs/providers/<name>.md` and its `providers/<name>/README.md` options or lifecycle section:

```markdown
`Close()` releases the connection this provider opened. Call it when you are done with the provider; mamori does not close it for you.
```

For tier-2 HTTP modules use:

```markdown
`Close()` returns this provider's idle HTTP connections to the pool. A client injected with `WithHTTPClient` is left open, since the caller owns it.
```

- [ ] **Step 5: Add a defer to the stateful WithProvider examples**

Search for examples that construct a now-closable provider:

Run: `grep -rn "WithProvider(postgres.New\|WithProvider(mongodb.New\|WithProvider(etcd.New\|WithProvider(redis.New\|WithProvider(sqlite.New" site/ README.md doc.go examples/ skills/`

Add `defer p.Close()` to each hit, splitting the inline construction into a named variable where needed.

- [ ] **Step 6: Check the agent skill**

Run: `grep -rn "WithProvider\|Close()" skills/mamori/`

If the skill teaches provider construction, add the ownership rule and a `defer p.Close()`.

- [ ] **Step 7: Build the docs site and check links**

Run: `make site-build && make site-linkcheck`

Expected: build succeeds, no broken internal links.

- [ ] **Step 8: Commit**

```bash
git add site/ providers/*/README.md README.md doc.go skills/
git commit -m "docs: document provider Close and caller ownership"
```

---

### Task 9: full verification

**Files:** none modified unless a failure surfaces.

**Interfaces:**
- Consumes: everything from Tasks 1 through 8.

- [ ] **Step 1: Build, test, and lint every module**

Run: `make all`

Expected: PASS across every module in `go.work`.

- [ ] **Step 2: Run the race detector across the tree**

Run: `make race`

Expected: PASS with no data races, particularly in `CloseDuringResolve`.

- [ ] **Step 3: Confirm the dependency surface**

Run: `git diff --stat main -- '**/go.mod' | tail -3` and `git diff main -- '**/go.mod' | grep '^+' | grep -v '^+++' | sort -u`

Expected: core `go.mod` gains exactly one **direct** require, `github.com/pelletier/go-toml/v2`. Every other module in `go.work` gains the same module as a single `// indirect` line, and nothing else.

That propagation is correct and unavoidable, not a mistake to undo. Every provider module `replace`-imports core, so once core imports `go-toml/v2` it enters each dependent module's build graph and `go mod tidy` records it. A provider whose `go.sum` lacked the entry would fail to build with `missing go.sum entry`. What this step is really checking is that **no module gained a direct require it should not have**, and that no dependency other than `go-toml/v2` appeared anywhere.

- [ ] **Step 4: Confirm no em-dash entered the tree**

Run: `git diff main | grep -n "$(printf '\342\200\224')" || echo "clean"`

Expected: `clean`. The UTF-8 byte escape for U+2014 is used deliberately so this plan does not itself contain the character it searches for, which would make the check match its own text.

- [ ] **Step 5: Run the vet analyzer**

Run: `make vet-analyzer`

Expected: PASS.

- [ ] **Step 6: Confirm the conformance case actually ran**

Run: `go test ./providers/... -run Conformance -v 2>&1 | grep -c "CloserContract"`

Expected: a count matching the number of provider modules with a conformance suite. Then confirm the 22 changed modules report PASS rather than SKIP:

Run: `go test ./providers/... -run Conformance -v 2>&1 | grep -A1 "CloserContract" | grep -c SKIP`

Expected: equal to the number of tier-3 stateless modules, not more.

---

## Notes for the implementer

- **Tier 3 modules get nothing.** `env`, `file`, `exec`, `dotenv`, `sops`, `viper`, `aws`, `azure`, `azblob`, `s3`, `cosmos`, `dynamodb`, `vault`, `consul`, `flagsmith`, `flipt`, `goff`, `openfeature`. If you find yourself adding a no-op `Close` to one of these, stop: it asserts a lifetime that does not exist.

- **The sqlite exception, ruled 2026-08-05.** This plan originally listed `sqlite` as holding a persistent `*sql.DB` behind a `WithDB` option. Both were wrong: `sqlite` opens and closes a fresh `*sql.DB` inside every `Resolve` and holds nothing between calls. It nonetheless **keeps** a `Close` that releases nothing and only makes the provider terminal.

  The rule is therefore: **a provider gets `Close` when it holds a releasable resource, or when its siblings do.** `sqlite` sits beside `postgres` and `mysql`, which both have one, so a caller sweeping shutdown across the database providers would be surprised to find `sqlite` alone still serving values. That sibling argument does not extend to the genuinely stateless providers above, where nobody reaches for `Close` at all, so tier 3 is unchanged. `sqlite`'s doc comment and its README line must both say plainly that its `Close` releases nothing.
- **`aws-appconfig` and `azure-appconfig` are schemes**, not modules. They live in `providers/aws` and `providers/azure`.
- **The globally registered instance is never closed.** Every provider's `init()` calls `mamori.Register(New())`. That instance is a process-global singleton and closing it is not this plan's business.
- **Task ordering matters only within Part 2.** Task 3 must precede Tasks 4 through 7. Part 1 is fully independent and can land first, last, or in parallel.
