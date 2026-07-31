# Derived fields (`WithDerive`) implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `WithDerive`, a typed hook that computes fields from already-resolved fields, running on every `Load` and every reconciled update so a value assembled from other fields stays correct across rotation.

**Architecture:** A generic `Option` storing `[]any` in the non-generic `options` struct, asserted back to `[]func(*T) error` through a shared `typedDerives[T]` helper modelled exactly on the existing `typedPreApply[T]`. The hooks run at one point in the pipeline, reached by two call sites: after fields are decoded and **before** validation, on both the `Load` path and the reconciler's candidate path.

**Tech Stack:** Go 1.26, standard library only. No new dependencies.

**Spec:** [2026-07-31-derived-fields-design.md](../specs/2026-07-31-derived-fields-design.md)

## The pipeline position, which is the whole design

```
resolve refs -> decode into struct -> DERIVE -> validate -> PreApply -> swap in
```

Both placements are load-bearing and a test pins each:

- **Before validation**, so `validate:"required,url"` on a derived field checks the assembled value rather than the zero value it held a moment earlier.
- **Before `PreApply`**, so a rotation-safety hook proves the *rebuilt* DSN. Running derive after `PreApply` would prove the previous one, which is worse than proving nothing because it would look like it worked.

## The two call sites

Read both before starting. They are the only places this feature touches the pipeline.

**Load path**, `reconcile.go` in `loadValue`, currently:

```go
	if err := buildInto(reflect.ValueOf(&cfg).Elem(), res, o.decodeHooks); err != nil {
		return cfg, nil, err
	}
	if err := o.validator.Validate(cfg); err != nil {
		return cfg, nil, &ValidationError{Err: err}
	}
```

**Reconciler path**, `reconciler.go` in the candidate build, currently:

```go
	for _, spec := range e.specs {
		// ... setField into dst ...
	}
	if err := e.o.validator.Validate(cand); err != nil {
		ve := &ValidationError{Err: err}
		e.o.meter.RecordApplyRejected(RejectValidation)
		e.o.log().Error("candidate rejected by validation; continuing to serve the previous config",
			errAttrs(err)...)
		e.emitErr(ve)
		return cand, nil, ve
	}
```

Derive goes immediately before the `Validate` call in each.

## Global Constraints

- **Standard library only.** No new module dependencies. The core's `go.mod` requires exactly `fsnotify`, `go-playground/validator`, and `go-viper/mapstructure`; this adds none.
- **`WithDerive` must fail loudly on a type mismatch**, following `typedPreApply` and explicitly **not** following `onChange`. A mismatched `OnChange` is silently discarded today (`onChange, _ = o.onChange.(func(Change[T]))`), which is filed as a bug and must not be copied here. See Task 2.
- **No `context.Context` in the hook signature.** The asymmetry with `PreApply(func(ctx, Change[T]) error)` is deliberate: `PreApply` does I/O, a derive is a pure transformation. Do not add a context "for symmetry".
- **A derive error rejects the whole candidate**, exactly as a validation failure does. Never partially apply.
- **Never use an em-dash character in any file.** Use a hyphen or restructure.
- **Every command runs with the workspace disabled**, matching CI: `GOWORK=off go test ./...` from the repo root for the core module.
- **Must pass `GOWORK=off golangci-lint run --timeout=5m`** with zero issues from the repo root.
- **Commit at the end of every task on the `xavier/derived-fields` branch only.** Never `push`, `merge`, or `rebase`, and never touch `main`. Conventional Commits, ending with this trailer on its own line:

```
Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
```

Committing is explicitly authorized for this branch. Implementers on this project have repeatedly declined citing a general instruction against it; that instruction is overridden here.

---

### Task 1: The option, the error type, and the typed accessor

Pure plumbing with no pipeline wiring yet, so it is testable on its own.

**Files:**
- Modify: `reconcile.go` (the `options` struct, and the new `WithDerive`)
- Modify: `errors.go` (add `DeriveError`)
- Modify: `reconciler.go` (add `typedDerives[T]` beside `typedPreApply[T]`)
- Create: `derive_test.go`

**Interfaces produced:**
- `func WithDerive[T any](fn func(*T) error) Option`
- `type DeriveError struct { Err error }` with `Error() string` and `Unwrap() error`
- `func typedDerives[T any](o *options) ([]func(*T) error, error)`
- `o.derives []any` on the `options` struct

- [ ] **Step 1: Write the failing tests**

Create `derive_test.go`:

```go
package mamori

import (
	"errors"
	"strings"
	"testing"
)

type deriveCfg struct {
	A string
	B string
}

func TestWithDeriveRegistersInOrder(t *testing.T) {
	o := &options{}
	WithDerive(func(c *deriveCfg) error { c.A = "first"; return nil })(o)
	WithDerive(func(c *deriveCfg) error { c.A += "-second"; return nil })(o)

	fns, err := typedDerives[deriveCfg](o)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fns) != 2 {
		t.Fatalf("got %d derives, want 2", len(fns))
	}

	var cfg deriveCfg
	for _, fn := range fns {
		if err := fn(&cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if cfg.A != "first-second" {
		t.Fatalf("got %q, want %q: derives must run in registration order", cfg.A, "first-second")
	}
}

func TestTypedDerivesNoneIsNil(t *testing.T) {
	fns, err := typedDerives[deriveCfg](&options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fns != nil {
		t.Fatalf("got %v, want nil when no derive is registered", fns)
	}
}

// A mismatched type parameter must be a loud error, not a silent no-op. This is
// the property that distinguishes WithDerive from OnChange, which discards the
// failed assertion and then never fires. See the DeriveError doc comment.
func TestTypedDerivesTypeMismatchIsLoud(t *testing.T) {
	type otherCfg struct{ Z string }

	o := &options{}
	WithDerive(func(c *otherCfg) error { return nil })(o)

	_, err := typedDerives[deriveCfg](o)
	if err == nil {
		t.Fatal("want an error when the derive's type parameter does not match")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error must satisfy ErrInvalid, got %v", err)
	}
	for _, want := range []string{"func(*mamori.otherCfg) error", "func(*mamori.deriveCfg) error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q so the mistake is findable, got: %v", want, err)
		}
	}
}

func TestDeriveErrorUnwraps(t *testing.T) {
	base := errors.New("boom")
	de := &DeriveError{Err: base}

	if !errors.Is(de, base) {
		t.Error("DeriveError must unwrap to its cause")
	}
	if !strings.Contains(de.Error(), "boom") {
		t.Errorf("DeriveError message must carry the cause, got %q", de.Error())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOWORK=off go test -run 'TestWithDerive|TestTypedDerives|TestDeriveError' ./...`
Expected: build failure, `undefined: WithDerive`, `undefined: typedDerives`, `undefined: DeriveError`.

- [ ] **Step 3: Add the `derives` field to the options struct**

In `reconcile.go`, in the `options` struct, immediately after the `preApply` / `preApplyTimeout` block, add:

```go
	// derives holds the WithDerive hooks, each a func(*T) error typed per T and
	// stored as any for the same reason preApply above is. They run after fields
	// are decoded and BEFORE validation, so a derived field is validated like
	// any other, and before the PreApply gate, so a rotation-safety hook proves
	// the derived value rather than the one it replaced.
	//
	// A slice rather than a single hook: unrelated derivations stay in separate
	// functions instead of accreting into one closure, and a field derived from
	// another derived field works with no new concept, because a later hook sees
	// an earlier one's output.
	derives []any
```

- [ ] **Step 4: Add `WithDerive`**

In `reconcile.go`, near `PreApply`'s neighbours:

```go
// WithDerive installs a hook that computes fields from already-resolved fields.
// It runs on every Load and every reconciled update, after values are decoded
// into the struct and before validation, so a value assembled from other fields
// is rebuilt whenever any of its inputs changes rather than going stale.
//
// The canonical case is a DSN assembled from a host, a user, and a rotating
// password. Built once in application code after Get, such a value is silently
// wrong the moment the password rotates; built here, it is rebuilt on every
// applied update and proven by any PreApply gate before Get serves it.
//
//	mamori.WithDerive(func(c *Config) error {
//	    c.DSN = secret.NewString((&url.URL{
//	        Scheme: "postgres",
//	        User:   url.UserPassword(c.User, c.Password.Reveal()),
//	        Host:   c.Host,
//	        Path:   "/" + c.DB,
//	    }).String())
//	    return nil
//	})
//
// Escaping and secret hygiene are the caller's, deliberately: net/url already
// escapes a password containing '@' or '/' correctly, and assigning into a
// secret.String is what keeps the assembled value redacted in fmt, JSON, and
// slog. A tag-based derivation would have had to reinvent both.
//
// Unlike PreApply, the hook takes no context.Context. PreApply does I/O to
// prove a credential; a derive is a pure transformation of a struct that has
// already been resolved, and the missing parameter is how the API says so.
//
// Multiple calls run in registration order. Returning an error rejects the
// whole candidate configuration exactly as a validation failure does: Get keeps
// serving the last valid config and the error reaches OnError as a *DeriveError.
func WithDerive[T any](fn func(*T) error) Option {
	return func(o *options) {
		if fn == nil {
			return
		}
		o.derives = append(o.derives, fn)
	}
}
```

- [ ] **Step 5: Add `DeriveError`**

In `errors.go`, immediately after `ValidationError`:

```go
// DeriveError is delivered to OnError when a WithDerive hook returns an error.
// The update is rejected atomically, exactly as a validation failure is, and
// Get continues to return the last valid config.
//
// Rejecting rather than continuing is deliberate: a configuration whose derived
// fields were not built is not one anyone should serve, and half-applying it
// would produce a snapshot where some fields reflect a rotated credential and a
// value derived from them still reflects the old one.
type DeriveError struct {
	Err error
}

func (e *DeriveError) Error() string {
	return fmt.Sprintf("mamori: derive failed: %v", e.Err)
}

func (e *DeriveError) Unwrap() error { return e.Err }
```

- [ ] **Step 6: Add `typedDerives`**

In `reconciler.go`, immediately after `typedPreApply`:

```go
// typedDerives asserts the WithDerive hooks back to their concrete type. Like
// typedPreApply it is shared by every caller that can observe a mismatch, so
// none of them can drift into tolerating it.
//
// A mismatch is a loud error, deliberately unlike onChange's silent discard
// below. A derive whose type parameter does not match Watch's would otherwise
// present as "my derived field is empty and nothing told me why", with the
// hook installed, never invoked, and no signal anywhere. The error names both
// types so the mistake is findable by grep.
func typedDerives[T any](o *options) ([]func(*T) error, error) {
	if len(o.derives) == 0 {
		return nil, nil
	}
	fns := make([]func(*T) error, 0, len(o.derives))
	for _, d := range o.derives {
		fn, ok := d.(func(*T) error)
		if !ok {
			var want func(*T) error
			return nil, fmt.Errorf("mamori: WithDerive hook has type %T, want %T: %w", d, want, ErrInvalid)
		}
		fns = append(fns, fn)
	}
	return fns, nil
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `GOWORK=off go test ./... && GOWORK=off go vet ./... && GOWORK=off golangci-lint run --timeout=5m`
Expected: all pass, lint clean.

- [ ] **Step 8: Commit**

```bash
git add reconcile.go errors.go reconciler.go derive_test.go
git commit -m "feat(core): WithDerive option, DeriveError, and the typed accessor

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Wire the Load path

**Files:**
- Modify: `reconcile.go` (`loadValue`)
- Modify: `derive_test.go`

**Interfaces:**
- Consumes: `WithDerive`, `typedDerives[T]`, `DeriveError` from Task 1.
- Produces: derives running on `Load` and on `Watch`'s initial resolve, which shares this function.

- [ ] **Step 1: Write the failing tests**

Append to `derive_test.go`. Use the in-repo test provider from `mamoritest` if `loadValue` needs one; otherwise `env:` refs with `t.Setenv` are sufficient and simpler.

```go
type dsnCfg struct {
	User string `source:"env:DERIVE_USER"`
	Pass string `source:"env:DERIVE_PASS"`
	DSN  string
}

func TestDeriveRunsOnLoad(t *testing.T) {
	t.Setenv("DERIVE_USER", "alice")
	t.Setenv("DERIVE_PASS", "s3cret")

	cfg, err := Load[dsnCfg](context.Background(),
		WithDerive(func(c *dsnCfg) error {
			c.DSN = c.User + ":" + c.Pass
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DSN != "alice:s3cret" {
		t.Fatalf("got %q, want %q", cfg.DSN, "alice:s3cret")
	}
}

// The load-bearing ordering test: a derived field carrying a validate tag must
// be validated on its DERIVED value, not on the zero value it held a moment
// earlier. If derive ran after validation this would fail.
type validatedDeriveCfg struct {
	User string `source:"env:DERIVE_USER"`
	DSN  string `validate:"required"`
}

func TestDeriveRunsBeforeValidation(t *testing.T) {
	t.Setenv("DERIVE_USER", "alice")

	if _, err := Load[validatedDeriveCfg](context.Background(),
		WithDerive(func(c *validatedDeriveCfg) error { c.DSN = "postgres://" + c.User; return nil }),
	); err != nil {
		t.Fatalf("a derive that fills a required field must satisfy validation, got %v", err)
	}

	_, err := Load[validatedDeriveCfg](context.Background(),
		WithDerive(func(c *validatedDeriveCfg) error { return nil }),
	)
	if err == nil {
		t.Fatal("a derive that leaves a required field empty must fail validation")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("got %T, want *ValidationError", err)
	}
}

func TestDeriveErrorFailsLoad(t *testing.T) {
	t.Setenv("DERIVE_USER", "alice")
	t.Setenv("DERIVE_PASS", "s3cret")
	boom := errors.New("boom")

	_, err := Load[dsnCfg](context.Background(),
		WithDerive(func(c *dsnCfg) error { return boom }),
	)
	if err == nil {
		t.Fatal("a derive returning an error must fail the Load")
	}
	var de *DeriveError
	if !errors.As(err, &de) {
		t.Fatalf("got %T, want *DeriveError", err)
	}
	if !errors.Is(err, boom) {
		t.Error("the cause must survive in the chain")
	}
}

func TestDeriveTypeMismatchFailsLoad(t *testing.T) {
	t.Setenv("DERIVE_USER", "alice")
	t.Setenv("DERIVE_PASS", "s3cret")
	type otherCfg struct{ Z string }

	_, err := Load[dsnCfg](context.Background(),
		WithDerive(func(c *otherCfg) error { return nil }),
	)
	if err == nil {
		t.Fatal("a mismatched derive type must fail Load, not be silently skipped")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("got %v, want an error satisfying ErrInvalid", err)
	}
}
```

Add `"context"` to the test file's imports.

- [ ] **Step 2: Run to verify they fail**

Run: `GOWORK=off go test -run TestDerive ./...`
Expected: `TestDeriveRunsOnLoad` fails with `got "" want "alice:s3cret"`, because nothing calls the hooks yet.

- [ ] **Step 3: Wire it into `loadValue`**

In `reconcile.go`, between `buildInto` and `o.validator.Validate`:

```go
	if err := buildInto(reflect.ValueOf(&cfg).Elem(), res, o.decodeHooks); err != nil {
		return cfg, nil, err
	}
	// Derives run here, after decode and before validation, so a derived field
	// is validated on its derived value rather than the zero value it held a
	// moment ago. See WithDerive for why this position and not after.
	derives, err := typedDerives[T](o)
	if err != nil {
		return cfg, nil, err
	}
	for _, d := range derives {
		if err := d(&cfg); err != nil {
			return cfg, nil, &DeriveError{Err: err}
		}
	}
	if err := o.validator.Validate(cfg); err != nil {
		return cfg, nil, &ValidationError{Err: err}
	}
```

Note `err` is already declared in this scope, so use `=` rather than `:=` if the compiler objects; adjust to match the surrounding style.

- [ ] **Step 4: Run to verify they pass**

Run: `GOWORK=off go test ./... && GOWORK=off go vet ./...`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add reconcile.go derive_test.go
git commit -m "feat(core): run derives on the Load path, before validation

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Wire the reconciler path

**Files:**
- Modify: `reconciler.go` (the candidate build, and the engine's cached hook slice)
- Modify: `observ.go` (add a reject reason)
- Modify: `derive_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1 and 2.
- Produces: `RejectDerive RejectReason = "derive"`; derives running on every reconciled update.

- [ ] **Step 1: Add the reject reason**

In `observ.go`, beside `RejectValidation` and `RejectPreApply`:

```go
	// RejectDerive means a WithDerive hook returned an error.
	RejectDerive RejectReason = "derive"
```

- [ ] **Step 2: Write the failing tests**

Append to `derive_test.go`. `mamoritest` supplies the scriptable provider (`NewProvider(scheme)`, `Set(key, val)`) and the deterministic wait helpers (`WaitForSnapshot`, `CaptureErrors`, `WaitForError`). Follow the patterns already in `watch_test.go`.

The motivating test, written out because it is the single most important one in the feature:

```go
type rotCfg struct {
	User string        `source:"fake://user"`
	Pass secret.String `source:"fake://pass"`
	DSN  secret.String
}

func TestDeriveRebuildsOnRotation(t *testing.T) {
	p := mamoritest.NewProvider("fake")
	p.Set("user", "alice")
	p.Set("pass", "old")

	buildDSN := func(c *rotCfg) error {
		c.DSN = secret.NewString("postgres://" + c.User + ":" + c.Pass.Reveal() + "@db")
		return nil
	}

	var changed bool
	w, err := Watch[rotCfg](context.Background(),
		WithProvider(p),
		WithDerive(buildDSN),
		OnChange(func(ev Change[rotCfg]) { changed = ev.Changed("Pass") }),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer w.Close()

	if got := w.Get().DSN.Reveal(); got != "postgres://alice:old@db" {
		t.Fatalf("initial: got %q", got)
	}

	p.Set("pass", "new")
	mamoritest.WaitForSnapshot(t, w, 2)

	if got := w.Get().DSN.Reveal(); got != "postgres://alice:new@db" {
		t.Fatalf("after rotation: got %q, want the rebuilt DSN", got)
	}
	// Changed("Pass"), NOT Changed("DSN"). A derived field can never appear in
	// the diff: both diff sites walk e.specs, which only carries source-tagged
	// fields, and compare per-ref versions. A derived field has neither. The
	// DSN itself is rebuilt correctly, which the assertion above proves; only
	// the trigger has to name an input.
	if !changed {
		t.Error(`ev.Changed("Pass") must be true after a rotation that changed the password`)
	}
}
```

Note the assertion is `Changed("Pass")`, not `Changed("DSN")`. A derived field can never enter the diff, and the comment in the test says why so nobody "fixes" it later.

Then add, in the same style:

- **The derive runs before `PreApply`.** Register a `PreApply` capturing `ev.New.DSN.Reveal()` into a variable, rotate, and assert the captured value is the *rebuilt* DSN. If derive ran after `PreApply` this fails, which is the point.
- **A derive error rejects the update.** After a successful first apply, make the derive return an error on the next; assert `w.Get()` still returns the previous config, that `CaptureErrors` saw a `*DeriveError`, and that the cause survives `errors.Is`.
- **A mismatched type fails `Watch`**, satisfying `ErrInvalid`, rather than being silently skipped.
- **Secret hygiene.** A derived `secret.String` must redact: assert `fmt.Sprintf("%v", w.Get().DSN)` and `fmt.Sprintf("%+v", w.Get())` contain neither `"new"` nor the assembled DSN, and that `json.Marshal` of the config does not either. This is the test that would catch a future example or doc snippet steering people toward a plain `string` target.

- [ ] **Step 3: Run to verify they fail**

Run: `GOWORK=off go test -run TestDerive ./...`
Expected: the rotation test fails because the reconciler path does not call the hooks yet.

- [ ] **Step 4: Cache the typed hooks on the engine**

`typedDerives[T]` asserts on every call, and the reconciler builds a candidate on every flush, so assert once at engine construction and store the result. The engine already does exactly this for `preApply`:

```go
	// reconciler.go, in the engine[T] struct, around line 335
	preApply func(context.Context, Change[T]) error
```

Add a sibling immediately after it:

```go
	// derives are the WithDerive hooks, asserted once here rather than on every
	// flush. A mismatched type parameter fails at Watch time, where it is
	// findable, rather than silently never running (see typedDerives).
	derives []func(*T) error
```

Populate it from `typedDerives[T](o)` wherever `preApply` is populated during engine construction, propagating the error the same way, so a mismatch fails `Watch` exactly as it fails `Load` in Task 2.

- [ ] **Step 5: Wire it into the candidate build**

In `reconciler.go`, immediately before `e.o.validator.Validate(cand)`:

```go
	// Same position as the Load path: after decode, before validation, before
	// the PreApply gate. See WithDerive.
	for _, d := range e.derives {
		if err := d(&cand); err != nil {
			de := &DeriveError{Err: err}
			e.o.meter.RecordApplyRejected(RejectDerive)
			e.o.log().Error("candidate rejected by a derive hook; continuing to serve the previous config",
				errAttrs(err)...)
			e.emitErr(de)
			return cand, nil, de
		}
	}
	if err := e.o.validator.Validate(cand); err != nil {
```

Match the surrounding error-handling shape exactly: the meter call, the log line, `emitErr`, and the return triple are all copied from the validation branch directly below, because a derive rejection and a validation rejection are the same event from the reconciler's point of view.

- [ ] **Step 6: Run to verify they pass**

Run: `GOWORK=off go test ./... && GOWORK=off go test -race ./... && GOWORK=off go vet ./... && GOWORK=off golangci-lint run --timeout=5m`
Expected: all pass, no races, lint clean.

- [ ] **Step 7: Commit**

```bash
git add reconciler.go observ.go derive_test.go
git commit -m "feat(core): run derives on the reconciler path, with a RejectDerive reason

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Documentation

**Files:**
- Create: `site/src/pages/docs/usage/derived-fields.md`
- Modify: `site/src/layouts/DocsLayout.astro`, `site/src/pages/docs/usage/options.md`, `site/src/pages/docs/usage/rotation.md`, `doc.go`, `README.md`

- [ ] **Step 1: Write the usage page**

`site/src/pages/docs/usage/derived-fields.md`, front matter matching a sibling exactly:

```markdown
---
layout: ../../../layouts/DocsLayout.astro
title: Derived fields
---
```

**Lead with the problem, not the API.** Open with the DSN built once after `Get()` and silently wrong after the first rotation, then show `WithDerive` as the fix. A reader who does not already have this problem does not need this feature.

The page must state, plainly rather than in a footnote, the three things mamori cannot know because the derivation is opaque Go:

- Derived fields are **invisible to `mamori explain`, `schema`, and `diff`**, which read struct tags, and there is no tag.
- Derived fields **do not appear in `Status()`'s per-field report**. There is no ref, so there is no staleness or error kind to report. An operator debugging a wrong DSN will look there first and find nothing.
- A field with both a `source` tag and a derive assignment **cannot be flagged as a conflict**; the derive wins, because mamori cannot see the assignment.
- **A derived field never appears in `ev.Changed()`.** This one needs its own worked example, because it is the most likely to bite. Both diff sites walk `e.specs`, which only carries `source`-tagged fields, and compare per-ref versions; a derived field has neither. So a caller triggers on an **input** and reads the derived field:

```go
mamori.OnChange(func(ev mamori.Change[Config]) {
    if ev.Changed("Pass") || ev.Changed("Host") || ev.Changed("User") {
        pool.Rotate(w.Get().DSN.Reveal())   // always correct, always rebuilt
    }
})
```

  Say plainly that the derived value itself is never stale, only the trigger has to name inputs, and that forgetting an input there means the reaction never fires for that field. That is the same bookkeeping `WithDerive` set out to delete, so it is a real cost and must not be buried.

Also cover: registration order for multiple hooks, derived-from-derived working as a consequence of that order, a derive error rejecting the whole update, and why there is no `context.Context`.

- [ ] **Step 2: Navigation and options reference**

In `site/src/layouts/DocsLayout.astro`, add to the Usage group after the entry for snapshots or rotation, matching the surrounding shape:

```js
      { slug: "usage/derived-fields", title: "Derived fields", indent: true },
```

In `site/src/pages/docs/usage/options.md`, add a row to the "Reacting to change" table:

```markdown
| `WithDerive` | Computes fields from already-resolved fields, after decoding and before validation; see [Derived fields](/docs/usage/derived-fields/). | none (no derivation runs) |
```

- [ ] **Step 3: Connect it to rotation**

In `site/src/pages/docs/usage/rotation.md`, add a short section explaining that `WithDerive` and `PreApply` together are what make a rotated credential both **rebuilt** and **proven**: the derive reassembles the DSN from the new password, and because it runs first, `PreApply` proves the rebuilt DSN rather than the one it replaced.

- [ ] **Step 4: Package doc and README**

Add `WithDerive` to `doc.go` wherever the other options are introduced, and add a line to the README's feature list near the rotation-safety bullet.

- [ ] **Step 5: Verify and commit**

Run `make site-linkcheck` from the repo root (the site build needs Node 22; use `nvm use 22` if the default is older), then `make build && make test && make vet`. Revert incidental churn to `go.work.sum` or `site/package-lock.json`.

```bash
git add site/src/pages/docs/usage/derived-fields.md site/src/layouts/DocsLayout.astro site/src/pages/docs/usage/options.md site/src/pages/docs/usage/rotation.md doc.go README.md
git commit -m "docs(core): derived fields usage page, options row, and rotation link

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Final verification

- [ ] `make all` passes from the repo root.
- [ ] `GOWORK=off go test -race ./...` passes for the core module.
- [ ] No new entry in the core `go.mod`.
- [ ] No em-dash in any changed file.
- [ ] `WithDerive` fails loudly on a type mismatch on **both** the `Load` and `Watch` paths, each pinned by its own test.
- [ ] The before-validation and before-`PreApply` orderings are each pinned by a test that would fail if the call were moved.
