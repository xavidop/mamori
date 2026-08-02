# Derived fields as first-class fields

**Goal:** a `WithDerive`-declared field behaves like any other field: it carries a
`Version`, it is probed by `Doctor`, and it can make a report unhealthy.

**Status:** design approved, pending spec review.

**Lands on:** `xavier/derived-fields` (PR #112), folded in rather than stacked, so
`WithDerive` ships first-class in one release instead of shipping a documented
limitation and removing it in the next.

## Why

`WithDerive` shipped with three separate carve-outs that together make a derived
field visibly second-class:

| # | Today | Site |
| --- | --- | --- |
| 1 | `Version` is always blank | `report.go:111` |
| 2 | absent from a `Doctor` report entirely | `doctor.go:39-84` |
| 3 | can never make a report unhealthy | `report.go:145-147` |

Each was defensible alone. Together they mean an operator cannot answer "is my
DSN the one I expect, and would it build at all" from any mamori surface, which
is the question the feature exists to serve.

## 1. `Version` is a content hash of the computed value

A new unexported helper produces canonical bytes for a field value and hashes
them with the existing exported `mamori.VersionHash` (FNV-64a, hex):

```go
// derivedVersion returns a VersionHash of v's canonical bytes, or "" if v
// cannot be read.
func derivedVersion(v reflect.Value) string
```

- `secret.String` -> `RevealBytes()`
- `secret.Bytes` -> `Reveal()`
- anything else -> `fmt.Appendf(nil, "%v", v.Interface())`

Dispatch uses the `secretStringType` / `secretBytesType` sentinels already in
`decode.go:51-52`.

### The one thing this must get right

`secret.String.String()`, `GoString()`, and `MarshalJSON()` all return the
constant `[REDACTED]` (`secret/secret.go:38-44`). An implementation that reaches
for `%v` or `json.Marshal` without the type switch therefore hashes the same
constant for every secret derived field: every such field reports an identical
version, and that version never changes when the underlying password rotates.

That is precisely the failure `WithDerive` exists to prevent, reintroduced one
layer up. It gets a dedicated test that rotates a password and asserts the
version moves, and that test must be verified by mutation: remove the reveal
branch, watch the test fail, restore it.

### Why hashing a secret is consistent here

Content-hashing a secret value into `Version` is established house behavior, not
a new exposure. `builtin_exec.go:51` returns `Version: VersionHash(b)` with
`Sensitive: true`, and every major secret manager falls back to the same helper
when the backend supplies no native revision: `providers/aws/sm.go:155`,
`providers/vault/vault.go:251`, `providers/gcp/gcp.go:194`,
`providers/azure/azure.go:175`, `providers/scaleway-sm/resolve.go:238`,
`providers/onepassword/onepassword.go:139`. Roughly 35 call sites in total.

`TestReportJSONNeverCarriesDerivedValue` (`derive_test.go:634`) is the guard that
a report never carries the derived *value*. A hash is not the value, so that test
must keep passing untouched. If it fails, the implementation is wrong.

## 2. `mamori.Doctor` probes derived fields

### Two different things share the name Doctor

Read this before touching anything, because the two are unrelated in code:

| | `mamori.Doctor[T]()` | `mamori doctor` (CLI) |
| --- | --- | --- |
| File | `doctor.go` | `cmd/mamori/doctor.go` |
| What it is | library preflight, no process running | client that GETs a live process's admin endpoint |
| Derived rows today | **absent** | **already present**, from the live watcher's `Status` |
| What changes here | starts emitting them | those rows gain a `Version` |

This section is about the **library** function. The CLI needs no probe logic: it
renders whatever `Report` the endpoint returns, so it inherits the version for
free. `TestDoctorCompareIgnoresDeclaredDerivedField` is a CLI test and is
unaffected by the library change.

### The change

`mamori.Doctor` today resolves each ref in isolation and never builds a `T`: no decode,
no validation, no `default:` / `optional:` / `onfail:` policy (`doctor.go:27-91`,
and its own doc comment says so). Running a hook requires all three. The hooks
are already reachable, since `Doctor` takes the same `opts` and populates
`o.derives`, so this is semantics rather than plumbing.

A second phase runs after the existing per-spec loop, only when
`len(o.derives) > 0`:

**Every source field healthy.** Decode the values the loop already probed into a
`T` via `buildInto` (`reconcile.go:470`), reusing the resolved `Value`s so no
extra round trips occur. Run the hooks in registration order. Emit one
`FieldStatus` per declared write path with `Derived: true`, `Sensitive` computed
as it is today, and a real `Version`.

A hook that returns an error gives its declared write paths `LastKind:
KindInvalid` and the `DeriveError` text, and the report goes unhealthy.

**Any source field unhealthy.** Emit the derived rows with a blank `Version` and
a `LastError` stating they were not evaluated because a source field is
unreachable. `LastKind` stays empty: the unreachable source field already flipped
the report, and a derived row should not double-count it.

Publishing a hash computed from zero-valued inputs was rejected: it looks like a
real version, will not match what production computes, and is worse than no
version at all.

### Two consequences that must be stated, not discovered

**Non-derived reporting semantics do not change.** `Doctor` still reports each
ref's raw probe outcome without applying policy. Policy is used only to
*construct* the `T` the hooks need. Construction is added; reporting is
untouched. A reviewer will flag this, so the split is explicit.

**`Doctor` begins executing user code.** Hooks are documented as pure
transformations and nothing enforces it. A preflight that runs them is a real
behavior change for anyone whose hook has a side effect. This is documented
rather than prevented, as a stated decision.

**Blocked is all-or-nothing.** mamori cannot inspect a closure to learn which
fields it reads, so "blocked" applies to every derived field whenever any source
field is unhealthy, not to the specific ones whose inputs failed. This is a
known coarseness, documented on the page rather than buried.

## 3. `fieldUnhealthy` stops special-casing derived rows

```go
func fieldUnhealthy(fs FieldStatus) bool {
	if fs.Derived {
		return false   // <- removed
	}
	...
}
```

A derived row with `KindInvalid` from a failed hook must count toward
`Report.Healthy`. `Stale` remains irrelevant for a derived field, which has no
ref and so can never independently go stale.

`TestFieldUnhealthyDerivedGuardShortCircuits` (`derive_test.go:1291`) tests the
removed guard directly and is rewritten, not deleted: it becomes the assertion
that a failed hook *does* flip health while a healthy derived row does not.

## 4. Go comment sweep

The user asked for this explicitly, and it carries one trap.

**"A derived field has no version" is true in one layer and false in the other
after this change.** A find-and-replace over that phrase corrupts the internal
comments.

Stays true, do not touch. These describe the internal **per-ref version map**,
where a derived field still has no entry:

- `reconciler.go:1504` - "A derived FieldChange carries no ref and no Version"
- `reconciler.go:1792-1793` - "before/after carry only per-ref Versions, and a
  derived field has none"

Becomes false, must be rewritten. These describe the **reported**
`FieldStatus.Version` or `Doctor`'s contents:

- `status.go:29` - "Scheme, Ref, Version, LastOK, Age, Stale" all blank
- `status.go:35-37` - "A Derived entry appears in Watcher.Status, never in a
  Doctor report"
- `status.go:50-54` - the Derived-append rule described as Status-only
- `doc.go:77` - "no ref, scheme, or version, since a derived field has none"
- `report.go:57` - the `fieldUnhealthy` note
- `cmd/mamori/doctor.go:131-140` - the `DERIVED` column doc, which says a derived
  row "always renders SCHEME, REF, and VERSION blank". `VERSION` stops being blank.

Also stays true, do not touch: `cmd/mamori/doctor.go:182-187`, the `--compare`
exclusion rationale. It is about path comparison, not versions. A derived field
still has no `source:` tag, so it still cannot appear on the source side, and
excluding it from the live side is still what prevents false drift.
`TestDoctorCompareIgnoresDeclaredDerivedField` (`cmd/mamori/doctor_test.go:204`)
keeps passing unchanged.

## 5. Tests that assert the old behavior

These encode carve-outs being removed and are rewritten rather than deleted, so
the new behavior inherits a guard rather than losing one:

| Test | Change |
| --- | --- |
| `TestStatusReportsDeclaredDerivedField:587` | blank `Version` -> a real hash |
| `TestHealthyUnaffectedByDerivedField:667` | a failing hook now affects health |
| `TestFieldUnhealthyDerivedGuardShortCircuits:1291` | guard removed, becomes its inverse |
| `TestDoctorTableRendersDerivedColumn:263` | extends to a version and a blocked row |
| `TestReportJSONNeverCarriesDerivedValue:634` | **must keep passing unchanged** |

New coverage: version changes on rotation (mutation-verified), `Doctor` emits
derived rows, `Doctor` marks a failing hook unhealthy, `Doctor` blocks derived
rows when a source field is unreachable, and no extra resolve round trips occur.

## 6. Docs

Per the standing rule that docs ship with the feature:

- `site/src/pages/docs/observability/doctor.md` - "Derived fields are not probed"
  is rewritten, not edited
- `site/src/pages/docs/cli/doctor-status.md` - both example tables gain a derived
  row, one healthy with a version and one blocked; neither shows a derived row today
- `site/src/pages/docs/usage/derived-fields.md` - the CLI-visibility section and
  "What mamori still cannot see" both lose the version and probe limitations
- `site/src/pages/docs/observability.md` - the `FieldStatus` shape

Limitations that genuinely survive and stay documented: an undeclared write is
still invisible, a `source` tag plus a derive assignment still cannot be flagged
as a conflict, and the static commands (`explain`, `schema`, `policy`, `vet`, and
`diff` transitively) still cannot see a derived field, because a derive puts
nothing on the struct field for a source-tree walk to find.

## Out of scope

Validation of derived values inside `Doctor`. `Doctor` does not validate today
and this change does not add it.
