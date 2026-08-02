# Derived Fields as First-Class Fields Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `WithDerive`-declared field carries a `Version`, is probed by `mamori.Doctor`, and can make a report unhealthy, so it behaves like any other field.

**Architecture:** A derived field's `Version` becomes a content hash of the computed value, produced by the existing exported `mamori.VersionHash` (FNV-64a). `mamori.Doctor` gains a second phase that reuses the values its probe loop already resolved, decodes them into a `T` via `buildInto`, runs the registered hooks, and emits a `FieldStatus` per declared write path. The `fieldUnhealthy` carve-out that made derived rows unconditionally healthy is removed so a failed hook counts.

**Tech Stack:** Go 1.26, standard library only. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-02-derived-fields-first-class-design.md`

**Branch:** `xavier/derived-fields` (PR #112). This work is folded into that PR, not stacked on it.

## Global Constraints

- Never use an em dash (Unicode U+2014) in any file, including code comments, docs, and commit messages. Use a plain hyphen or restructure the sentence.
- Commit your task's work on the current branch (`xavier/derived-fields`) when the task's tests pass. Conventional Commits format, and end the message with the trailer `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`. Never push, never merge, never rebase, and never touch `main`: the user owns everything past the commit.
- Docs ship with the feature: `site/`, module READMEs, the root README, and `skills/` are part of the deliverable, not a follow-up.
- `make all` must be green across all 43 modules before a task is considered done.
- A test that passes is not evidence. A guard is only verified when you remove it, watch the test fail, and restore it. Every task below names which guards to mutation-verify.
- `TestReportJSONNeverCarriesDerivedValue` (`derive_test.go:634`) must keep passing **unchanged** in every task. A version hash is not the value. If it fails, the implementation is wrong.
- Two unrelated things are named Doctor: `mamori.Doctor[T]()` in `doctor.go` (library preflight, no process running) and `mamori doctor` in `cmd/mamori/doctor.go` (a client that GETs a live process's admin endpoint). Task 2 changes only the library one.

---

## File Structure

| File | Responsibility | Task |
| --- | --- | --- |
| `derivedversion.go` (new) | canonical bytes for a derived value, hashed via `VersionHash` | 1 |
| `derivedversion_test.go` (new) | unit coverage for the above, including the reveal trap | 1 |
| `report.go:111` | populate `Version` on the derived `FieldStatus` append | 1 |
| `report.go:144-153` | `fieldUnhealthy` stops short-circuiting on `Derived` | 2 |
| `doctor.go` | derive probe phase | 2 |
| `derive_test.go` | rewrite the three tests asserting removed carve-outs | 1, 2 |
| `doctor_derive_test.go` (new) | Doctor probe coverage | 2 |
| `status.go`, `doc.go`, `report.go`, `cmd/mamori/doctor.go` | comment sweep | 3 |
| `site/src/pages/docs/**` | four pages | 4 |

---

### Task 1: `derivedVersion` and a version on every derived `FieldStatus`

**Files:**
- Create: `derivedversion.go`
- Create: `derivedversion_test.go`
- Modify: `report.go:110-111`
- Modify: `derive_test.go:587` (`TestStatusReportsDeclaredDerivedField`)

**Interfaces:**
- Produces: `func derivedVersion(v reflect.Value) string` - unexported, package `mamori`. Returns `""` when `v` is invalid or unreadable, otherwise a `VersionHash` of the value's canonical bytes. Task 2 calls this for Doctor's rows.

**Context you need:** `secret.String` and `secret.Bytes` deliberately redact themselves. `String()`, `GoString()`, and `MarshalJSON()` all return the constant `secret.Redacted` (`secret/secret.go:38-44`, `115-121`). Hashing through `%v` or `json.Marshal` therefore produces the **same hash for every secret derived field**, and that hash **never changes when the password rotates**. That is the exact bug `WithDerive` exists to prevent. The reveal branch is the whole point of this task.

The package already has `reflect.Type` sentinels for both types at `decode.go:51-52`: `secretStringType` and `secretBytesType`.

- [ ] **Step 1: Write the failing tests**

Create `derivedversion_test.go`:

```go
package mamori

import (
	"reflect"
	"testing"

	"github.com/xavidop/mamori/secret"
)

// TestDerivedVersionRevealsSecretString is the guard against hashing the
// redacted form. secret.String.String() returns "[REDACTED]", so an
// implementation reaching for %v gives every secret field an identical,
// rotation-proof version. Two different secrets must hash differently.
func TestDerivedVersionRevealsSecretString(t *testing.T) {
	a := derivedVersion(reflect.ValueOf(secret.NewString("postgres://u:old@h/db")))
	b := derivedVersion(reflect.ValueOf(secret.NewString("postgres://u:new@h/db")))
	if a == "" || b == "" {
		t.Fatalf("expected non-empty versions, got %q and %q", a, b)
	}
	if a == b {
		t.Fatalf("two different secrets hashed identically to %q: the redacted form was hashed, not the revealed value", a)
	}
}

// TestDerivedVersionRevealsSecretBytes is TestDerivedVersionRevealsSecretString
// for the secret.Bytes half, which redacts through the same methods.
func TestDerivedVersionRevealsSecretBytes(t *testing.T) {
	a := derivedVersion(reflect.ValueOf(secret.NewBytes([]byte("old"))))
	b := derivedVersion(reflect.ValueOf(secret.NewBytes([]byte("new"))))
	if a == b {
		t.Fatalf("two different secrets hashed identically to %q", a)
	}
}

// TestDerivedVersionMatchesVersionHash pins that a derived version is the same
// helper ~35 providers use, not a second hashing scheme, so an operator
// comparing a derived row against a sourced one is comparing like with like.
func TestDerivedVersionMatchesVersionHash(t *testing.T) {
	got := derivedVersion(reflect.ValueOf("plain"))
	want := VersionHash([]byte("plain"))
	if got != want {
		t.Fatalf("derivedVersion = %q, want VersionHash = %q", got, want)
	}
}

// TestDerivedVersionStableAcrossCalls pins determinism: the same value must
// hash identically every time, or Status would report spurious churn.
func TestDerivedVersionStableAcrossCalls(t *testing.T) {
	v := reflect.ValueOf(map[string]int{"b": 2, "a": 1})
	if derivedVersion(v) != derivedVersion(v) {
		t.Fatal("derivedVersion is not deterministic for the same value")
	}
}

// TestDerivedVersionUnreadableIsEmpty covers the unexported-field path, which
// report.go already guards with CanInterface: derivedVersion must not panic.
func TestDerivedVersionUnreadableIsEmpty(t *testing.T) {
	if got := derivedVersion(reflect.Value{}); got != "" {
		t.Fatalf("invalid reflect.Value gave %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run the tests and verify they fail**

Run: `go test -run TestDerivedVersion ./...`
Expected: FAIL, `undefined: derivedVersion`.

- [ ] **Step 3: Implement `derivedVersion`**

Create `derivedversion.go`:

```go
package mamori

import (
	"fmt"
	"reflect"

	"github.com/xavidop/mamori/secret"
)

// derivedVersion returns a VersionHash of v's canonical bytes, giving a derived
// field the same kind of content-derived version a provider without a native
// revision already reports (builtin_exec.go, providers/aws/sm.go,
// providers/vault, and ~30 others all call VersionHash on the value's bytes).
// It returns "" for a value that cannot be read, matching the CanInterface
// guard report.go already applies before appending a Derived entry.
//
// secret.String and secret.Bytes are revealed rather than formatted. Both
// redact themselves through String, GoString, and MarshalJSON (secret.Redacted),
// so formatting one would hash the constant "[REDACTED]": every secret derived
// field would report an identical version, and that version would never change
// when the underlying credential rotated - the precise failure WithDerive
// exists to prevent, reintroduced one layer up. The hash is not the value and
// is never a way back to it; a Report still carries no derived value at all
// (see TestReportJSONNeverCarriesDerivedValue).
func derivedVersion(v reflect.Value) string {
	if !v.IsValid() || !v.CanInterface() {
		return ""
	}
	switch v.Type() {
	case secretStringType:
		return VersionHash(v.Interface().(secret.String).RevealBytes())
	case secretBytesType:
		return VersionHash(v.Interface().(secret.Bytes).Reveal())
	}
	return VersionHash(fmt.Appendf(nil, "%v", v.Interface()))
}
```

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test -run TestDerivedVersion ./...`
Expected: PASS.

- [ ] **Step 5: Mutation-verify the reveal branch**

Delete the `case secretStringType:` line and its return, run `go test -run TestDerivedVersionRevealsSecretString ./...`, and confirm it FAILS. Restore it. A test that passes both with and without the branch is not testing the branch.

- [ ] **Step 6: Populate `Version` on the derived append**

In `report.go`, change line 110-111 from:

```go
			sensitive := v.Type() == secretStringType || v.Type() == secretBytesType
			fields = append(fields, FieldStatus{Path: p, Derived: true, Sensitive: sensitive})
```

to:

```go
			sensitive := v.Type() == secretStringType || v.Type() == secretBytesType
			fields = append(fields, FieldStatus{
				Path:      p,
				Derived:   true,
				Sensitive: sensitive,
				Version:   derivedVersion(v),
			})
```

- [ ] **Step 7: Update the Status test that asserts a blank version**

`derive_test.go:587`, `TestStatusReportsDeclaredDerivedField`, currently asserts the derived entry carries no version. Change that assertion to require a non-empty `Version`, and add a sibling test that a rotation moves it:

```go
// TestDerivedFieldVersionChangesOnRotation is the end-to-end form of the
// reveal guard: a rotated password must move the derived DSN's reported
// version, or an operator comparing versions across replicas would see a
// stale credential as identical to a fresh one.
func TestDerivedFieldVersionChangesOnRotation(t *testing.T) {
	// Build a watcher whose Pass field comes from a fake provider and whose
	// DSN is derived from it, mirroring TestDerivedFieldChangedTrueAfterInputRotation
	// (derive_test.go:698) for wiring; assert Status().Fields' DSN entry has a
	// different Version before and after the rotation.
}
```

Fill that body using the existing rotation harness in `TestDerivedFieldChangedTrueAfterInputRotation` (`derive_test.go:698`), which already rotates a value and waits for the change to land. Do not invent a second harness.

- [ ] **Step 8: Run the full suite**

Run: `make all`
Expected: exit 0. `TestReportJSONNeverCarriesDerivedValue` must still pass untouched.

- [ ] **Step 9: Report**

Do not commit. Report the files changed and the mutation-verification result for Step 5.

---

### Task 2: `mamori.Doctor` probes derived fields

**Files:**
- Modify: `doctor.go:27-91`
- Modify: `report.go:139-153` (`fieldUnhealthy`)
- Create: `doctor_derive_test.go`
- Modify: `derive_test.go:667` (`TestHealthyUnaffectedByDerivedField`), `derive_test.go:1291` (`TestFieldUnhealthyDerivedGuardShortCircuits`)

**Interfaces:**
- Consumes: `derivedVersion(reflect.Value) string` from Task 1.
- Consumes: `buildInto(dst reflect.Value, res []resolved, hooks []mapstructure.DecodeHookFunc) error` (`reconcile.go:470`) and `typedDerives[T](o *options) ([]typedDerive[T], error)` (`reconciler.go:282`).
- Consumes: `resolved{spec fieldSpec; value Value; found bool; set bool}` (`resolve.go:10-15`).

**Context you need:** `mamori.Doctor` today walks `fieldSpecs`, calls `probeField` per spec, and builds a `Report` without ever constructing a `T`. It deliberately does not apply `default:` / `optional:` / `onfail:` policy to what it **reports**. This task does not change that. Policy is used only to **construct** the `T` the hooks need, and non-derived rows keep reporting their raw probe outcome exactly as before.

`resolveOne` represents an applied default as `Value{Bytes: []byte(spec.Default), Sensitive: spec.Sensitive, Version: "default"}` with `set: true` (`resolve.go:286-290`). The derive phase must mirror that, or a field that is absent-but-defaulted (which Doctor already reports healthy) would derive from a zero value and publish a hash that does not match production.

- [ ] **Step 1: Write the failing tests**

Create `doctor_derive_test.go`:

```go
package mamori

import (
	"context"
	"strings"
	"testing"
)

// TestDoctorEmitsDerivedRow pins the headline behavior: a declared write path
// gets a FieldStatus from Doctor, carrying a real version, where before this
// change Doctor never emitted one at all.
func TestDoctorEmitsDerivedRow(t *testing.T) {
	// Wire a fake provider for a Host field, plus WithDerive writing "DSN".
	// Assert rep.Fields contains a Derived entry for "DSN" with non-empty Version.
}

// TestDoctorDerivedRowBlockedWhenSourceUnhealthy pins the chosen semantics: a
// hash computed from a zero value is worse than no hash, because it looks real
// and will not match what production computes.
func TestDoctorDerivedRowBlockedWhenSourceUnhealthy(t *testing.T) {
	// Wire a provider that returns ErrNotFound for a required field.
	// Assert the DSN row has an empty Version and a LastError mentioning that
	// it was not evaluated, and that LastKind is empty so it does not
	// double-count the already-unhealthy source field.
}

// TestDoctorDerivedRowUsesDefaultNotZero pins that an absent-but-defaulted
// field, which Doctor already reports healthy, feeds its default into the hook.
// Deriving from the zero value here would publish a version that silently
// disagrees with production.
func TestDoctorDerivedRowUsesDefaultNotZero(t *testing.T) {
	// Field with `default:"h"` and a provider returning ErrNotFound.
	// Assert the derived version equals the version derived from "h".
}

// TestDoctorFailingHookIsUnhealthy pins that a derived field can now fail a
// preflight, which is the whole point of probing it.
func TestDoctorFailingHookIsUnhealthy(t *testing.T) {
	// WithDerive hook returning an error, all sources healthy.
	// Assert rep.Healthy is false and the DSN row carries KindInvalid.
}

// TestDoctorDeriveAddsNoRoundTrips pins that the derive phase reuses values the
// probe loop already resolved. A counting provider must see exactly the same
// call count with and without WithDerive.
func TestDoctorDeriveAddsNoRoundTrips(t *testing.T) {
	// Count Resolve calls with and without a WithDerive option; assert equal.
}
```

Fill each body using the provider fakes already in `doctor_test.go` and `derive_test.go`. Do not introduce a new fake-provider style; match what those files do.

- [ ] **Step 2: Run and verify they fail**

Run: `go test -run TestDoctorDerive -run TestDoctorEmitsDerivedRow -run TestDoctorFailingHook ./...`
Expected: FAIL.

- [ ] **Step 3: Accumulate `resolved` in Doctor's probe loop**

In `doctor.go`, declare `res := make([]resolved, 0, len(specs))` beside `fields`, and inside the loop, after the existing `switch` that sets `fs`, append the policy-applied value:

```go
		switch {
		case rerr == nil:
			res = append(res, resolved{spec: spec, value: val, found: true, set: true})
		case errors.Is(rerr, ErrNotFound) && spec.HasDefault:
			// Mirror resolveOne (resolve.go:286-290) exactly: an absent field
			// covered by a default is reported healthy above, so the hooks must
			// see the default rather than the zero value, or the version this
			// publishes would not match what Load computes.
			res = append(res, resolved{
				spec:  spec,
				value: Value{Bytes: []byte(spec.Default), Sensitive: spec.Sensitive, Version: "default"},
				found: false,
				set:   true,
			})
		}
```

Leave every other case out of `res`: an optional-and-absent field is `set: false` in `resolveOne` too, and a genuinely failed field blocks the phase entirely.

- [ ] **Step 4: Add the derive phase**

After the probe loop and before the `return Report{...}`, add:

```go
	derivedFields, derivedHealthy := doctorDerivedFields[T](o, specs, res, healthy)
	fields = append(fields, derivedFields...)
	healthy = healthy && derivedHealthy
```

and implement it in `doctor.go`:

```go
// doctorDerivedFields runs the registered WithDerive hooks against the values
// the probe loop already resolved and returns one FieldStatus per declared
// write path. It is Doctor's counterpart to the Derived append in buildReport
// (report.go), and uses the same hasSpecPath / fieldByPath / CanInterface gates
// so the two can never disagree about which paths produce a row.
//
// The hooks run only when every sourced field probed healthy. A hook fed a zero
// value because its input was unreachable would produce a version that looks
// real, does not match what production computes, and is worse than no version:
// those rows report blocked instead. mamori cannot inspect a closure to learn
// which fields it reads, so blocked is all-or-nothing across derived fields
// rather than per-input.
//
// Running the hooks means Doctor executes caller code during a preflight.
// WithDerive documents a hook as a pure transformation and nothing enforces it,
// so this is a deliberate, documented trade: probing a derived field is not
// possible without evaluating the function that produces it.
func doctorDerivedFields[T any](o *options, specs []fieldSpec, res []resolved, sourcesHealthy bool) ([]FieldStatus, bool) {
	derives, err := typedDerives[T](o)
	if err != nil || len(derives) == 0 {
		// A mismatched hook type is Load's loud error, not a preflight's: it
		// cannot produce a row here, and Doctor's contract is that the returned
		// error covers only an unwalkable T.
		return nil, true
	}
	var cfg T
	var deriveErr error
	if sourcesHealthy {
		if err := buildInto(reflect.ValueOf(&cfg).Elem(), res, o.decodeHooks); err != nil {
			deriveErr = err
		} else {
			for _, d := range derives {
				if err := d.fn(&cfg); err != nil {
					deriveErr = &DeriveError{Err: err}
					break
				}
			}
		}
	}
	cv := reflect.ValueOf(cfg)
	seen := make(map[string]struct{})
	healthy := true
	var out []FieldStatus
	for _, d := range derives {
		for _, p := range d.writes {
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			if hasSpecPath(specs, p) {
				continue
			}
			v, ok := fieldByPath(cv, p)
			if !ok || !v.CanInterface() {
				continue
			}
			fs := FieldStatus{
				Path:      p,
				Derived:   true,
				Sensitive: v.Type() == secretStringType || v.Type() == secretBytesType,
			}
			switch {
			case !sourcesHealthy:
				fs.LastError = "not evaluated: a source field is unreachable"
			case deriveErr != nil:
				fs.LastError = deriveErr.Error()
				fs.LastKind = KindInvalid
			default:
				fs.Version = derivedVersion(v)
			}
			if fieldUnhealthy(fs) {
				healthy = false
			}
			out = append(out, fs)
		}
	}
	return out, healthy
}
```

`specs` is passed in from the caller rather than rebuilt from `res`, because `res`
deliberately excludes optional-and-absent fields while `hasSpecPath` must still
see every spec to suppress a duplicate row for a path that also carries a
`source` tag.

- [ ] **Step 5: Remove the `fieldUnhealthy` carve-out**

In `report.go`, delete these three lines from `fieldUnhealthy`:

```go
	if fs.Derived {
		return false
	}
```

and replace the paragraph of its doc comment beginning "A Derived field is never unhealthy" with:

```go
// A Derived field is judged by the same rules. It has no ref and no staleness
// clock, so LastKind and Stale are zero for it in a Watcher.Status report and
// it cannot go unhealthy there: a hook that fails rejects the whole candidate
// in buildCandidate, so a published config never contains a failed derive. A
// Doctor probe is the case this matters for - it evaluates the hooks directly
// (see doctorDerivedFields, doctor.go) and marks a failing one KindInvalid,
// which must count.
```

- [ ] **Step 6: Rewrite the two tests that assert the removed carve-out**

`TestFieldUnhealthyDerivedGuardShortCircuits` (`derive_test.go:1291`) tested the deleted guard directly. Rewrite it as its inverse, keeping the name meaningful:

```go
// TestFieldUnhealthyDerivedCountsInvalidHook replaces the short-circuit test
// this guard used to need. A derived row carrying KindInvalid from a failed
// Doctor hook must count as unhealthy; a healthy derived row must not.
func TestFieldUnhealthyDerivedCountsInvalidHook(t *testing.T) {
	if !fieldUnhealthy(FieldStatus{Path: "DSN", Derived: true, LastKind: KindInvalid}) {
		t.Fatal("a derived row with KindInvalid must be unhealthy")
	}
	if fieldUnhealthy(FieldStatus{Path: "DSN", Derived: true, Version: "abc"}) {
		t.Fatal("a healthy derived row must not be unhealthy")
	}
}
```

`TestHealthyUnaffectedByDerivedField` (`derive_test.go:667`) asserts a derived field never affects health. Narrow it to the case that stays true - a **Watcher.Status** derived row never affects health - and rename it `TestWatcherHealthyUnaffectedByDerivedField`, adding a comment that the Doctor case is now covered by `TestDoctorFailingHookIsUnhealthy`.

- [ ] **Step 7: Run and verify**

Run: `go test -race ./...` then `make all`
Expected: exit 0.

- [ ] **Step 8: Mutation-verify two guards**

1. Change `case !sourcesHealthy:` to `case false:` so blocked rows compute a version anyway. `TestDoctorDerivedRowBlockedWhenSourceUnhealthy` must FAIL. Restore.
2. Delete the `errors.Is(rerr, ErrNotFound) && spec.HasDefault` branch from Step 3. `TestDoctorDerivedRowUsesDefaultNotZero` must FAIL. Restore.

- [ ] **Step 9: Report**

Do not commit. Report both mutation results.

---

### Task 3: Go comment sweep

**Files:**
- Modify: `status.go:28-41`, `status.go:44-56`
- Modify: `doc.go:77`
- Modify: `report.go:51-57`
- Modify: `cmd/mamori/doctor.go:131-140`

**Context you need, and the trap:** the sentence "a derived field has no version" is **true in one layer and false in the other** after Tasks 1 and 2. A find-and-replace over that phrase corrupts working comments.

**Leave these alone. They describe the internal per-ref version map, where a derived field still has no entry:**
- `reconciler.go:1504` - "A derived FieldChange carries no ref and no Version"
- `reconciler.go:1792-1793` - "before/after carry only per-ref Versions, and a derived field has none"
- `cmd/mamori/doctor.go:182-187` - the `--compare` exclusion rationale, which is about path comparison, not versions. The exclusion itself is unchanged and `TestDoctorCompareIgnoresDeclaredDerivedField` keeps passing.

**Rewrite these. They describe the reported `FieldStatus.Version` or Doctor's contents:**

- [ ] **Step 1: `status.go:28-41`, the `Derived` field doc**

Replace the two paragraphs with text stating: `Derived` is true for a field a `WithDerive` hook declares it writes; it has no ref, so `Scheme`, `Ref`, `LastOK`, `Age`, and `Stale` are zero for it, but `Version` is a content hash of the computed value (see `derivedVersion`, `derivedversion.go`); it appears in both `Watcher.Status` and a `Doctor` report; in a `Doctor` report it may carry `LastKind` `KindInvalid` when the hook itself failed, and it affects `Healthy` in that case.

- [ ] **Step 2: `status.go:44-56`, the `Report` doc**

The "Derived-append rule is a Watcher.Status rule, not a Doctor one" paragraph is now false in its entirety. Replace it with: both `Watcher.Status` and `Doctor` append derived entries after every sourced field, in `WithDerive` registration order, via the same `hasSpecPath` / `fieldByPath` gates, so the two never disagree about which paths produce a row.

- [ ] **Step 3: `doc.go:77`**

"Path-only entry - no ref, scheme, or version, since a derived field has none" becomes an entry with a content-hash version but no ref or scheme.

- [ ] **Step 4: `report.go:51-57`**

The sentence "They never affect healthy: fieldUnhealthy always returns false for a Derived entry, so there is nothing to check here" is false as of Task 2. Replace with: a `Watcher.Status` derived entry still cannot be unhealthy, because a failing hook rejects the candidate in `buildCandidate` so a published config never contains one, but `fieldUnhealthy` no longer short-circuits and a `Doctor` row can be unhealthy.

- [ ] **Step 5: `cmd/mamori/doctor.go:131-140`**

"A Derived row ... always renders SCHEME, REF, and VERSION blank" is false: `VERSION` is now populated. Rewrite to say `SCHEME` and `REF` are blank while `VERSION` carries a content hash of the computed value, and that a blank `VERSION` on a derived row means the value was not evaluated (a `Doctor` probe whose source field was unreachable).

- [ ] **Step 6: Verify no stale claim survives**

Run:

```bash
grep -rn "never in a Doctor\|no ref, scheme, or version\|always returns false for a Derived\|VERSION blank" --include="*.go" .
```

Expected: no output. If a line matches, it was missed above.

- [ ] **Step 7: Run the suite**

Run: `make all`
Expected: exit 0. Comment-only changes must not alter behavior.

- [ ] **Step 8: Report**

Do not commit. List every comment changed and confirm the three "leave alone" sites are untouched with `git diff --stat`.

---

### Task 4: Documentation

**Files:**
- Modify: `site/src/pages/docs/observability/doctor.md` (the "Derived fields are not probed" section)
- Modify: `site/src/pages/docs/cli/doctor-status.md` (both example tables)
- Modify: `site/src/pages/docs/usage/derived-fields.md` (the CLI-visibility section and "What mamori still cannot see")
- Modify: `site/src/pages/docs/observability.md` (the `FieldStatus` shape)

**Context you need:** the user explicitly asked for the `#what-mamori-still-cannot-see` section to be updated, and the goal is stated as "derived fields like any other field". Write from the reader's perspective. No internal identifiers (`e.specs`, `buildCandidate`, `hasSpecPath`) belong on these pages.

- [ ] **Step 1: `observability/doctor.md`**

The section titled "Derived fields are not probed" is now false end to end. Delete it and write "Derived fields are probed" in its place, covering: `Doctor` runs the registered hooks and reports a row per declared write path with a version; a hook that fails makes the report unhealthy; when a source field is unreachable the derived rows report that they were not evaluated rather than publishing a version computed from a zero value; and that blocked is all-or-nothing because mamori cannot see which fields a hook reads. State plainly that `Doctor` executes the hook, so a hook with side effects will run during a preflight.

- [ ] **Step 2: `cli/doctor-status.md`**

Neither example table currently shows a derived row. Add one to each, matching the real column order from `writeReportTable` (`cmd/mamori/doctor.go:143`):

```
PATH            SCHEME  REF                     VERSION   STALE  LAST_KIND  LAST_ERROR  SENSITIVE  DERIVED
Redis.Addr      env     env://REDIS_ADDR        3         false  -          -           false      false
Redis.Password  aws-sm  aws-sm://prod/redis-pw  3         false  -          -           true       false
DSN                                             a3f9c1e2  false  -          -           true       true
```

Update the paragraph below it, which currently says `VERSION` is blank for a derived row.

- [ ] **Step 3: `usage/derived-fields.md`**

Two sections change. In "Seeing a derived field from the CLI", the sentence saying `VERSION` is blank is now wrong, and the static-vs-live split stays correct. In "What mamori still cannot see", remove the version and probe limitations.

The limitations that genuinely survive and must stay: an undeclared write is still invisible; a field carrying both a `source` tag and a derive assignment still cannot be flagged as a conflict; and the static commands (`explain`, `schema`, `policy`, `vet`, and `diff` transitively) still cannot see a derived field, because a derive puts nothing on the struct field for a source-tree walk to find.

- [ ] **Step 4: `observability.md`**

Update the `FieldStatus` shape description so `Version` is documented as populated for a derived entry.

- [ ] **Step 5: Build the site**

Run: `cd site && npm run build`
Expected: success. Node must be >= 22.12; if the local Node is older, say so in the report rather than skipping silently.

- [ ] **Step 6: Verify no stale doc claim survives**

Run:

```bash
grep -rn "not probed\|no ref, so Doctor\|VERSION.*blank\|never in a Doctor" site/src/pages/docs/
```

Expected: no output.

- [ ] **Step 7: Report**

Commit per the Global Constraints. List every page changed.

---

## Self-Review

**Spec coverage.** Spec section 1 (`derivedVersion`) is Task 1. Section 2 (`Doctor` probes) is Task 2. Section 3 (`fieldUnhealthy`) is Task 2 Step 5, folded there because a derived row can only ever be unhealthy in a `Doctor` report, so the change is untestable without the probe phase. Section 4 (comment sweep) is Task 3. Section 5 (tests asserting old behavior) is distributed: `TestStatusReportsDeclaredDerivedField` in Task 1 Step 7, `TestHealthyUnaffectedByDerivedField` and `TestFieldUnhealthyDerivedGuardShortCircuits` in Task 2 Step 6, `TestDoctorTableRendersDerivedColumn` in Task 4 Step 2's table update. Section 6 (docs) is Task 4.

**Type consistency.** `derivedVersion(reflect.Value) string` is defined in Task 1 and consumed in Task 2 with the same signature. `doctorDerivedFields[T](o *options, specs []fieldSpec, res []resolved, sourcesHealthy bool) ([]FieldStatus, bool)` is defined and called in Task 2 only, and the call site at Step 4 matches it argument for argument. `resolved`'s four fields match `resolve.go:10-15` verbatim.

**Known gap, deliberate.** Task 2 Step 1's test bodies are described rather than written out, because they depend on the provider-fake style already in `doctor_test.go` and `derive_test.go`, and inventing a second style would be worse than pointing at the first. Every assertion each test must make is stated. This is the one place the plan trades literal code for a pointer to existing code, and it is called out here rather than left to be discovered.
