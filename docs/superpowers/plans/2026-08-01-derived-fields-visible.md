# Making derived fields visible implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a derived field appear in `ev.Changed()` and in `Status()`'s per-field report, so it behaves like every other field from a user's point of view.

**Architecture:** `WithDerive` gains variadic path arguments declaring which fields the hook writes. mamori cannot infer that from opaque Go, so the caller states it. Declared paths then get a `Report` entry marked `Derived`, and join the change diff by value comparison rather than by ref version.

**Tech Stack:** Go 1.26, standard library only. No new dependencies.

## Why this supersedes the shipped behavior

`WithDerive` shipped documenting a limitation: a derived field could never appear in `ev.Changed()` or `Status()`, because both are keyed on `source`-tagged specs and per-ref versions, and a derived field has neither.

That was accepted as a design tradeoff and written up honestly. Reading the resulting page from a user's perspective changed the call: a field that mamori maintains but will not report is a field the user has to remember is special, which is exactly the bookkeeping the feature exists to remove. The reaction trigger having to name inputs by hand is the same problem one level up.

So: the caller declares what the hook writes, and mamori treats those paths as first-class in both surfaces.

## Global Constraints

- **Standard library only.** No new entry in the core `go.mod`.
- **Backward compatible.** `WithDerive(fn)` with no declared paths must keep compiling and keep working exactly as today, reporting nothing. The variadic form makes this free; do not break it.
- **`Report` must stay safe to serve over HTTP.** It is the admin endpoint's body. A derived field's entry carries no value, ever.
- **A derived field never makes `Healthy` false.** It has no ref, so it cannot be stale or carry a resolve error. Confirm the health rule skips them rather than accidentally counting an empty `LastKind` as healthy by luck.
- **No `time`-based behavior added to the resolve path.**
- **Never use an em-dash character in any file.**
- Must pass `GOWORK=off go test -race ./...` and `GOWORK=off golangci-lint run --timeout=5m` from the repo root, plus `make all` and `make site-linkcheck`.
- Commit at the end of every task on `xavier/derived-fields`. Never push, merge, or rebase, never touch `main`. Conventional Commits ending with:

```
Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
```

Committing is authorized on this branch by the human directly.

---

### Task 1: Declare what a derive writes

**Files:**
- Modify: `reconcile.go` (`WithDerive`, the `options` struct), `reconciler.go` (`typedDerives`)
- Modify: `derive_test.go`

**Interfaces produced:**
- `func WithDerive[T any](fn func(*T) error, writes ...string) Option`
- `type deriveEntry struct { fn any; writes []string }` on the options struct as `derives []deriveEntry`
- `type typedDerive[T any] struct { fn func(*T) error; writes []string }`
- `func typedDerives[T any](o *options) ([]typedDerive[T], error)`

- [ ] **Step 1: Write the failing tests**

Add to `derive_test.go`:

- `WithDerive(fn)` with no paths still registers and runs, and reports no writes. This is the backward-compatibility pin.
- `WithDerive(fn, "DSN")` records `writes` as `["DSN"]`.
- `WithDerive(fn, "DSN", "RedisURL")` records both, in order.
- Two `WithDerive` calls keep their own writes, and registration order is preserved.
- A path that is empty or only whitespace is rejected at `Load`/`Watch` time with an error satisfying `ErrInvalid`, naming the offending hook position. A silently ignored bad path would reintroduce exactly the invisible-field problem this change exists to fix.
- The existing type-mismatch behavior is unchanged: a mismatched `T` still fails loudly with `ErrInvalid` and names both types.

- [ ] **Step 2: Run to verify they fail, then implement**

`deriveEntry` holds `fn any` and `writes []string`. Keeping `writes` on the non-generic side is deliberate: it is just strings, and it lets the reconciler read the declared paths without knowing `T`.

`typedDerives` asserts each `fn` exactly as it does today, and carries `writes` through unchanged.

Validate the paths in `typedDerives`, not in `WithDerive`: `Option` returns nothing, so `WithDerive` has no way to report an error, and failing at `Load`/`Watch` matches how every other malformed-input case in this package behaves.

**Do not validate that a declared path exists on `T`.** That is tempting and wrong here: the path is a dotted field path into a possibly nested struct, the same shape `spec.Path` uses, and a resolver for it does not exist outside the decode machinery. A declared path that matches no field simply never reports, which degrades to today's behavior. Say so in the doc comment.

- [ ] **Step 3: Verify and commit**

```bash
git add reconcile.go reconciler.go derive_test.go
git commit -m "feat(core): let WithDerive declare which fields it writes

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Report and diff participation

**Files:**
- Modify: `status.go` (`FieldStatus`), `report.go` (`buildReport`), `reconciler.go` (the candidate diff and `diffApplied`), `reconcile.go` (the initial-load `Change`)
- Modify: `derive_test.go`, `derive_watch_test.go`

**Interfaces produced:**
- `FieldStatus.Derived bool`
- derived paths appearing in `Change.Fields` and therefore `ev.Changed()`

- [ ] **Step 1: Add `Derived` to `FieldStatus`**

```go
	// Derived is true for a field a WithDerive hook declares it writes. A
	// derived field has no ref, so Scheme, Ref, Version, LastOK, Age, Stale,
	// LastError and LastKind are all zero for it, and it never affects Healthy:
	// there is no resolve that could fail. It is reported so an operator can
	// see the field exists and is maintained, which is the whole reason a
	// caller declares it.
	Derived bool
```

Additive on a public struct, which is safe. Confirm nothing in the repo constructs `FieldStatus` with an unkeyed literal, which adding a field would break; `grep -rn 'FieldStatus{' --include='*.go' .` answers it.

- [ ] **Step 2: Write the failing tests**

- `Status().Fields` contains an entry for a declared derived path, with `Derived: true`, an empty `Ref` and `Scheme`, and no staleness.
- That entry does **not** carry the field's value, checked by asserting the report's JSON encoding contains none of the derived value's bytes. `Report` is the admin endpoint's body, so this is the security-relevant assertion.
- `Healthy` stays true when the only unusual field is a derived one.
- After a rotation that changes an input, `ev.Changed("DSN")` is **true**, where `DSN` is declared. This is the headline behavior and the reason for the whole change.
- `ev.Changed("DSN")` is **false** on an update where the derived value did not change, so it reports actual change rather than merely firing on every update.
- An **undeclared** derived field still does not appear in either surface, and behaves exactly as before.

- [ ] **Step 3: Implement the report entry**

`buildReport` iterates `e.specs`. After that loop, append one `FieldStatus` per declared write path, with `Derived: true` and everything else zero. Preserve the documented "struct declaration order" property as far as reasonable: derived entries have no spec index, so append them after the sourced ones and say so in `Report`'s doc comment rather than silently changing what the ordering promise means.

- [ ] **Step 4: Implement diff participation**

The existing diff compares per-ref `Version` strings, which a derived field has none of. So compare the **value** at the declared path, before and after.

You need to read a field by dotted path. `setField` (`decode.go`) writes by path; there is no public getter. Write a small unexported `fieldByPath(root reflect.Value, path string) (reflect.Value, bool)` beside it, mirroring how `setField` walks the path, and reuse it at every site rather than duplicating the walk.

Compare with `reflect.DeepEqual` on the two values. For a `secret.String` this compares the underlying bytes, which is correct and does not reveal them.

Apply at all three `FieldChange` construction sites so the surfaces agree: the reconciler's candidate build, `diffApplied`, and the initial-load `Change` in `reconcile.go`. **They must agree.** Two of the three disagreeing is the exact defect class that produced two Criticals earlier in this project, and a test should assert `Resolve`-path and reconciler-path behavior match for the same derive.

- [ ] **Step 5: Verify and commit**

Run the suite, `-race`, `go vet`, `golangci-lint`.

```bash
git add status.go report.go reconciler.go reconcile.go decode.go derive_test.go derive_watch_test.go
git commit -m "feat(core): report derived fields in Status and the change diff

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Rewrite the docs from the user's perspective

**Files:**
- Rewrite: `site/src/pages/docs/usage/derived-fields.md`
- Modify: `reconcile.go` (`WithDerive`'s godoc), `site/src/pages/docs/usage/options.md`, `site/src/pages/docs/usage/rotation.md`, `skills/mamori/SKILL.md`, `docs/superpowers/specs/2026-07-31-derived-fields-design.md`

- [ ] **Step 1: Rewrite the usage page**

The current page was written to explain a limitation that no longer exists, and it explains it in maintainer's terms. **Strip every internal.** Specifically, none of these belong on a user-facing page: `e.specs`, `fieldSpecs`, `buildCandidate`, `diffApplied`, "the three places mamori computes a diff", "populated only from `source`-tagged fields, each compared by its own resolved version", or the reconciler goroutine's mechanics.

Keep, in roughly this order:

1. **The problem.** A DSN built once after `Get()` is silently wrong the moment the password rotates, and `Status()` reports every field healthy because reconciliation reached every field it knows about and stopped where you assembled them.
2. **The fix**, with a working example using `net/url` so escaping is correct.
3. **Declaring what it writes**, and what that buys: the field shows up in `ev.Changed()` and in `Status()`, so you react to it and observe it like any other field. This is now the ordinary path, so show it as the example rather than as an advanced option.
4. **Reacting to it**, with `ev.Changed("DSN")` as the natural trigger.
5. **Secret hygiene.** The assembled DSN embeds the password, so the target should be a `secret.String`. Show it.
6. **Errors.** A derive returning an error rejects the whole update and `Get()` keeps the last good config.
7. **Order.** Multiple hooks run in registration order, so one may build on another's output.
8. **The reentrancy rule**, reduced to the rule plus one wrong example. State that calling `Refresh`, `Pin`, `PinCurrent`, or `Unpin` from inside a derive returns `ErrReentrantCall`, and do not explain the goroutine architecture that makes it so.

What still needs stating honestly, briefly: a hook that writes a field it did not declare will not report that field, and mamori cannot detect it. One sentence, not a section.

- [ ] **Step 2: Update the other surfaces**

`WithDerive`'s godoc gains the `writes` parameter and one line on what declaring buys. `options.md`'s row mentions it. `rotation.md`'s example declares its path. `SKILL.md`'s section shows the declared form, since that file teaches an agent to write these.

Correct the design spec's "Change detection does NOT come free" section: it is now wrong. Replace it with what shipped and why the earlier decision was revisited, so the history stays readable rather than looking like the spec was always right.

- [ ] **Step 3: Verify and commit**

`make site-linkcheck`, then `make all`. Revert incidental churn to `go.work.sum` or `site/package-lock.json`.

```bash
git add site/src/pages/docs/usage/derived-fields.md reconcile.go site/src/pages/docs/usage/options.md site/src/pages/docs/usage/rotation.md skills/mamori/SKILL.md docs/superpowers/specs/2026-07-31-derived-fields-design.md
git commit -m "docs(core): rewrite derived fields for the reader, not the maintainer

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Final verification

- [ ] `make all` passes.
- [ ] `WithDerive(fn)` with no declared paths still compiles and behaves as before, pinned by a test.
- [ ] A declared derived field appears in `ev.Changed()` and in `Status()`, each pinned by a test.
- [ ] The report never carries a derived field's value, pinned by a JSON-encoding assertion.
- [ ] All three `FieldChange` sites agree for the same derive.
- [ ] No internal identifier (`e.specs`, `buildCandidate`, `diffApplied`, `fieldSpecs`) appears in any user-facing page.
