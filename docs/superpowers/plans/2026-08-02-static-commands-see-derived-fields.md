# Static Commands See Derived Fields Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `mamori schema` stops omitting rules mamori enforces, `explain` and `diff` report `WithDerive`-declared fields, and `vet` flags a hook that reveals a secret into a plain string.

**Architecture:** `Field` gains a `Kind` so `Extract`'s four consumers can each opt in to the kinds they want. `walkFields` gains a validate-only case. A new `cmd/mamori/derives.go` finds `mamori.WithDerive` call sites in the syntax trees `Extract` already loads, resolving the callee through the type checker. `vetcheck` gets its own independent rule, because it must keep working under `go vet -vettool` where `Extract` is unavailable.

**Tech Stack:** Go 1.26, `golang.org/x/tools/go/packages` and `go/analysis` (both already dependencies of `cmd/mamori`). No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-02-static-commands-see-derived-fields-design.md`

**Branch:** work on `xavier/derived-fields` unless told otherwise.

## Global Constraints

- Never use an em dash (Unicode U+2014) in any file, including code comments, docs, and commit messages. Use a plain hyphen or restructure.
- Commit your task's work when its tests pass. Conventional Commits, ending with the trailer `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`. Never push, merge, rebase, or touch `main`.
- `make all` must be green across all 43 modules before a task is done.
- A passing test is not evidence. Each task names guards to mutation-verify: remove the guard, watch the named test fail, restore it, and report the actual failure output.
- `explain --json` changing shape is intended. There is deliberately NO back-compatibility fallback for a missing `kind`.
- Write for the reader in any user-facing text. No internal identifiers in `site/` or `skills/`.

---

## File Structure

| File | Responsibility | Task |
| --- | --- | --- |
| `cmd/mamori/extract.go` | `FieldKind`, `Field.Kind`, validate-only case | 1 |
| `cmd/mamori/policy.go:206` | filter to `KindSource` | 1 |
| `cmd/mamori/doctor.go:203` | filter to `KindSource` | 1 |
| `cmd/mamori/explain.go:141` | filter out `KindValidate` | 1 |
| `cmd/mamori/derives.go` (new) | find `WithDerive` call sites | 2 |
| `cmd/mamori/internal/vetcheck/derives.go` (new) | the reveal-into-plain rule | 3 |
| `cmd/mamori/internal/vetcheck/testdata/src/github.com/xavidop/mamori/mamori.go` (new) | stub `WithDerive` for fixtures | 3 |
| `site/`, `skills/` | docs | 4 |

---

### Task 1: Field.Kind and the schema gap

**Files:**
- Modify: `cmd/mamori/extract.go` (the `Field` struct at 29-45, `walkFields` at 184-230)
- Modify: `cmd/mamori/policy.go:206`, `cmd/mamori/doctor.go:203`, `cmd/mamori/explain.go:141`
- Modify: `cmd/mamori/testdata/example/config.go` (the shared fixture struct)
- Test: `cmd/mamori/schema_test.go`, `cmd/mamori/doctor_test.go`

**How testing works here, since there is no `extract_test.go`.** `Extract` has no
direct test file. It is exercised through the command tests, which all run
against ONE shared fixture module at `cmd/mamori/testdata/example/`, whose
primary struct is `Config` (`testdata/example/config.go`). Each command test has
a helper (`runExplain`, `runSchema`, `runPolicy` in their respective `_test.go`
files) that chdirs into that module and captures stdout, then `compareGolden`
diffs against `testdata/*.golden`. Golden files regenerate with
`go test -update` (the flag is declared at `explain_test.go:14`).

So a new fixture field is added to `Config` once and is visible to every
command's test at the same time, which is exactly what makes the
policy-goldens-must-not-move check in Step 8 meaningful.

**Interfaces:**
- Produces: `type FieldKind string` with constants `KindSource`, `KindDerived`, `KindValidate`, and `Field.Kind FieldKind`. Task 2 sets `KindDerived`.

**Why this exists:** `Validate` calls `v.Struct(cfg)` on the whole struct (`validator.go:23`) with no source-tag filtering, so every `validate:` tag is enforced at runtime. `mamori schema` emits only source-tagged fields, so it describes less than mamori enforces.

**THE TRAP, read this before writing code.** `walkFields`'s `default` branch recurses into a source-less struct field to reach its nested source-tagged fields. A nested struct commonly carries its own tag:

```go
type Config struct {
    Redis RedisConfig `validate:"required"`   // recursed into today
}
```

If the validate-only case is added as a `switch` case BEFORE that recursion, `Redis` is emitted as a leaf validate field and its nested `source:` fields vanish from every command. Recurse first, then consider a validate-only leaf. A test pins this.

- [ ] **Step 1: Extend the shared fixture**

Append to `Config` in `cmd/mamori/testdata/example/config.go`:

```go
	// Computed carries no source tag but is validated, which mamori enforces
	// because the validator runs against the whole struct. It is the fixture
	// for KindValidate: schema must emit it, explain and policy must not.
	Computed string `validate:"required"`
```

And, as a sibling type in the same file, the regression fixture for the
ordering trap:

```go
// TaggedNest is a nested struct carrying its own validate tag. walkFields must
// still recurse into it to reach Addr; emitting TaggedNest as a validate-only
// leaf would make every nested source field disappear.
type TaggedNest struct {
	Addr string `source:"env:NEST_ADDR"`
}
```

with this field on `Config`:

```go
	Nested TaggedNest `validate:"required"`
```

- [ ] **Step 2: Write the failing tests**

Add to `cmd/mamori/schema_test.go`:

```go
// TestSchemaEmitsValidateOnlyField pins that a field mamori validates at
// runtime appears in the schema even with no source tag. Before this, schema
// described less than mamori enforced.
func TestSchemaEmitsValidateOnlyField(t *testing.T) {
	stdout, _, code := runSchema(t, "./...")
	if code != 0 {
		t.Fatalf("schema exited %d", code)
	}
	if !strings.Contains(stdout, `"Computed"`) {
		t.Fatalf("schema omitted the validate-only field Computed:\n%s", stdout)
	}
}

// TestSchemaStillRecursesIntoTaggedNestedStruct is the regression guard for
// the ordering trap: the nested source field must survive.
func TestSchemaStillRecursesIntoTaggedNestedStruct(t *testing.T) {
	stdout, _, code := runSchema(t, "./...")
	if code != 0 {
		t.Fatalf("schema exited %d", code)
	}
	if !strings.Contains(stdout, `"Addr"`) {
		t.Fatalf("nested source field Addr vanished; walkFields stopped recursing:\n%s", stdout)
	}
}
```

Add to `cmd/mamori/explain_test.go`:

```go
// TestExplainOmitsValidateOnlyField pins that explain lists what mamori reads.
// A field the application populates itself has no ref and does not belong.
func TestExplainOmitsValidateOnlyField(t *testing.T) {
	stdout, _, code := runExplain(t, "./...")
	if code != 0 {
		t.Fatalf("explain exited %d", code)
	}
	if strings.Contains(stdout, "Computed") {
		t.Fatalf("explain listed a validate-only field:\n%s", stdout)
	}
}
```

Add to `cmd/mamori/doctor_test.go`, following
`TestDoctorCompareIgnoresDeclaredDerivedField` (doctor_test.go:204) for wiring:

```go
// TestDoctorCompareIgnoresValidateOnlyField pins that widening Extract does not
// make --compare report drift. A validate-only field never appears in a live
// report, so an unfiltered source set calls it "only in source" on a healthy
// config.
func TestDoctorCompareIgnoresValidateOnlyField(t *testing.T) {
	// Build a live Report containing only the fixture's source-tagged paths,
	// run runCompare against ./..., and assert the output reports no drift and
	// never mentions "Computed".
}
```

- [ ] **Step 3: Run the tests and verify they fail**

Run: `cd cmd/mamori && go test -run "TestSchemaEmitsValidateOnly|TestSchemaStillRecurses|TestExplainOmitsValidateOnly|TestDoctorCompareIgnoresValidateOnly" ./...`
Expected: FAIL. The schema and doctor tests fail on behavior; nothing references `KindValidate` yet, so add the constant in the next step.

- [ ] **Step 4: Add FieldKind and Field.Kind**

In `cmd/mamori/extract.go`, above the `Field` struct:

```go
// FieldKind says why a field appears in a StructInfo, so each command can take
// the kinds it can act on. Extract has four consumers and they disagree: policy
// emits permissions from refs, doctor --compare diffs paths against a live
// report, schema describes everything mamori validates, and explain lists what
// mamori reads.
type FieldKind string

const (
	// KindSource carries a source: tag. Every consumer wants these.
	KindSource FieldKind = "source"
	// KindDerived is named by a WithDerive call's declared write paths. It has
	// no ref, so it grants no permissions and cannot appear in a live report.
	KindDerived FieldKind = "derived"
	// KindValidate has no source: tag but does carry validate: rules, which
	// mamori enforces on every load and update because the validator runs
	// against the whole struct. Only schema wants these.
	KindValidate FieldKind = "validate"
)
```

Add to `Field`, as the field after `Path`:

```go
	Kind FieldKind // why this field is here; see FieldKind
```

In the existing `case hasSource:` branch, add `Kind: KindSource,` to the
`Field` literal.

- [ ] **Step 5: Add the validate-only case, recursion first**

Replace `walkFields`'s `default` branch with:

```go
		default:
			// Recurse BEFORE considering a validate-only leaf. A nested struct
			// commonly carries its own `validate:"required"`, and treating that
			// as a leaf here would drop every source-tagged field inside it.
			if nested, ok := v.Type().Underlying().(*types.Struct); ok && !sensitiveType {
				fields = append(fields, walkFields(pkg, nested, path, firstSensitive)...)
				continue
			}
			if validate != "" {
				// mamori validates the whole struct, so this rule is enforced at
				// runtime even with no source tag. schema would otherwise
				// describe less than mamori enforces.
				fields = append(fields, Field{
					Path:       path,
					Kind:       KindValidate,
					GoType:     types.TypeString(v.Type(), shortQualifier(pkg)),
					Default:    def,
					HasDefault: hasDefault,
					Optional:   optional,
					Sensitive:  sensitiveType,
					Validate:   validate,
				})
			}
			// Neither sourced, nor a container, nor validated: skip, exactly as
			// before (decode.go leaves it to its zero value).
```

- [ ] **Step 6: Filter the three consumers that must not widen**

`cmd/mamori/policy.go`, in `collectPolicyRefs`'s inner loop, as the first statement:

```go
			if f.Kind != KindSource {
				continue // no ref, so no permission to grant
			}
```

`cmd/mamori/doctor.go`, in `runCompare`'s source-set loop:

```go
		for _, f := range s.Fields {
			if f.Kind != KindSource {
				// A derived or validate-only field is never in a live report,
				// so counting it here would report drift on a healthy config.
				continue
			}
			source[f.Path] = true
		}
```

`cmd/mamori/explain.go`, in its field loop:

```go
		for _, f := range s.Fields {
			if f.Kind == KindValidate {
				continue // explain lists what mamori reads; this it does not
			}
```

- [ ] **Step 7: Run the tests and verify they pass**

Run: `cd cmd/mamori && go test ./...`
Expected: PASS, except golden-file mismatches handled next.

- [ ] **Step 8: Regenerate goldens, and check the policy ones did NOT move**

Run `cd cmd/mamori && go test ./... -update` (the flag is declared at
`explain_test.go:14`). That rewrites `testdata/explain.json.golden`,
`testdata/explain.table.golden`, and `testdata/schema.json.golden` from actual
output. Update the `testdata/diff/` inputs the same way if a diff test reads
them from explain output; otherwise edit them to match the new shape.

`testdata/policy.aws-iam.golden`, `testdata/policy.gcp.golden`, and `testdata/policy.external-secret.golden` **must be byte-identical**. Run `git diff --stat cmd/mamori/testdata/` and confirm no `policy.*` file appears. If one does, the `KindSource` filter in Step 6 is wrong. Do not regenerate it to make the test pass.

- [ ] **Step 9: Mutation-verify two guards**

1. Move the `if validate != ""` block ABOVE the recursion in Step 5.
   `TestSchemaStillRecursesIntoTaggedNestedStruct` must FAIL. Restore.
2. Remove the `f.Kind != KindSource` filter from `runCompare`.
   `TestDoctorCompareIgnoresValidateOnlyField` must FAIL. Restore.

Report the actual failure output for each.

- [ ] **Step 10: Run the full suite and commit**

Run: `make all` from the repo root. Expected: exit 0.
Commit with a `feat(cli):` message.

---

### Task 2: Discover WithDerive call sites

**Files:**
- Create: `cmd/mamori/derives.go`
- Modify: `cmd/mamori/extract.go` (call the new discovery from `Extract`; `StructInfo` gains `DerivesIncomplete bool`)
- Modify: `cmd/mamori/explain.go` (note incomplete discovery)
- Test: `cmd/mamori/derives_test.go`

**Interfaces:**
- Consumes: `FieldKind`, `KindDerived`, `Field.Kind` from Task 1.
- Produces: `func findDerives(pkgs []*packages.Package) (map[string][]string, map[string]bool)` returning declared write paths keyed by `"pkgpath.TypeName"`, and a set of type keys with at least one unreadable path.

**Feasibility is established.** A prototype recovered 60 of 61 `WithDerive` call sites in this repo, including the config type and declared paths, across package boundaries. You are re-implementing a known-working approach, not exploring.

**The mechanism, verbatim from the prototype:**

```go
// Resolve the callee through types, never by name text, so a local function
// called WithDerive is not mistaken for mamori's.
var id *ast.Ident
switch fn := call.Fun.(type) {
case *ast.Ident:
	id = fn
case *ast.SelectorExpr:
	id = fn.Sel
case *ast.IndexExpr: // an explicit type argument: WithDerive[Config](...)
	switch inner := fn.X.(type) {
	case *ast.Ident:
		id = inner
	case *ast.SelectorExpr:
		id = inner.Sel
	}
}
if id == nil {
	return true
}
obj, _ := pkg.TypesInfo.Uses[id].(*types.Func)
if obj == nil || obj.Name() != "WithDerive" || obj.Pkg() == nil ||
	obj.Pkg().Path() != "github.com/xavidop/mamori" {
	return true
}

// T comes from the instantiated type arguments.
inst, ok := pkg.TypesInfo.Instances[id]
if !ok || inst.TypeArgs.Len() == 0 {
	return true
}

// Declared paths: every argument after the hook. A literal yields a constant.
for _, a := range call.Args[1:] {
	if tv, ok := pkg.TypesInfo.Types[a]; ok && tv.Value != nil {
		// tv.Value.String() is quoted; use strconv.Unquote.
		continue
	}
	// non-literal: mark this type's discovery incomplete
}
```

- [ ] **Step 1: Write the failing tests**

Create `cmd/mamori/derives_test.go`. There is no `extract_test.go`; the command
tests share one fixture module at `cmd/mamori/testdata/example/` and reach it
through `runExplain` / `runSchema` helpers that chdir into it. Follow that
pattern: add the `WithDerive` calls to the fixture module itself (a new file
`testdata/example/derives.go`, which must import the real `mamori` package the
module already replaces), and drive assertions through `findDerives` directly
plus one end-to-end check through `runExplain`. Cover:

```go
// TestFindDerivesLiteralPaths      - WithDerive(fn, "DSN") is discovered
// TestFindDerivesMultiplePaths     - WithDerive(fn, "A", "B") yields both
// TestFindDerivesTwoCallsSameType  - two calls accumulate
// TestFindDerivesCrossPackage      - the call lives in a different package from T
// TestFindDerivesUnknownPathDropped- a path naming no field on T does not appear
// TestFindDerivesNonLiteralMarks   - WithDerive(fn, paths...) sets DerivesIncomplete
// TestFindDerivesIgnoresLocalFunc  - a local func named WithDerive is not picked up
```

Each asserts on `findDerives`'s two return values.

- [ ] **Step 2: Run and verify they fail**

Run: `cd cmd/mamori && go test -run TestFindDerives ./...`
Expected: FAIL, `undefined: findDerives`.

- [ ] **Step 3: Implement findDerives**

Create `cmd/mamori/derives.go` using the mechanism above, keyed by
`inst.TypeArgs.At(0)`'s named type as `"pkgpath.TypeName"`.

- [ ] **Step 4: Wire into Extract**

After `Extract` builds its `StructInfo` list, call `findDerives`, and for each
struct append one `Field` per declared path that names a real field on that
struct, with `Kind: KindDerived`, its `GoType` from the resolved field, empty
`Source`, and nil `Refs`. Set `StructInfo.DerivesIncomplete` from the second
return value.

A declared path naming no field on `T` is dropped, matching the runtime rule
that a path matching nothing simply never reports as written.

- [ ] **Step 5: Test files stay excluded**

Do NOT set `packages.Config.Tests`. `explain` describes the shipping config
surface, so a `WithDerive` call appearing only in a test contributes nothing.
Add a test asserting a derive declared in a `_test.go` fixture does not appear.

- [ ] **Step 6: explain reports incomplete discovery**

When `DerivesIncomplete` is true, `explain` prints, after that struct's table:

```
note: this struct declares WithDerive write paths that could not be read
      statically (a variable or a slice expansion); the derived fields listed
      above may be incomplete
```

Silent under-reporting would make an incomplete listing look complete.

- [ ] **Step 7: Run and verify**

Run: `cd cmd/mamori && go test ./...` then `make all`.
Regenerate `explain.*.golden` and `schema.json.golden` if the fixtures now carry derives. `policy.*.golden` must still not move.

- [ ] **Step 8: Mutation-verify two guards**

1. Delete the `obj.Pkg().Path() != "github.com/xavidop/mamori"` check.
   `TestFindDerivesIgnoresLocalFunc` must FAIL. Restore.
2. Make the non-literal branch `continue` without marking incomplete.
   `TestFindDerivesNonLiteralMarks` must FAIL. Restore.

- [ ] **Step 9: Commit** with a `feat(cli):` message.

---

### Task 3: vet flags a derive that launders a secret

**Files:**
- Create: `cmd/mamori/internal/vetcheck/derives.go`
- Create: `cmd/mamori/internal/vetcheck/testdata/src/github.com/xavidop/mamori/mamori.go`
- Modify: `cmd/mamori/internal/vetcheck/analyzer.go` (`run` gains a second node filter)
- Modify: `cmd/mamori/internal/vetcheck/testdata/src/a/a.go` (fixtures)

**Interfaces:**
- Consumes: nothing from Tasks 1 or 2. This is independent, deliberately.

**Why the duplication is correct:** `vetcheck` must keep working under
`go vet -vettool=$(which mamori)`, where `unitchecker` runs it one package at a
time. `Extract` does its own `packages.Load`, which is not possible there.

**The rule:**

> A `WithDerive` hook whose body calls `Reveal` or `RevealBytes` on a
> `secret.String` or `secret.Bytes`, and which declares a write path naming a
> field of plain `string` or `[]byte` type, is flagged.

`FullName = First + " " + Last` never calls `Reveal`, so it never fires. A hook
writing into a `secret.String` never fires. This is the laundering case only.

- [ ] **Step 1: Add a stub mamori package to the fixtures**

`vetcheck`'s `analysistest` tree has a stub `secret` package but no stub for
`mamori` itself, so a fixture calling `mamori.WithDerive` will not compile.
Create `testdata/src/github.com/xavidop/mamori/mamori.go`:

```go
package mamori

// Option is the stub of mamori's real Option, enough for analysistest.
type Option func()

// WithDerive mirrors the real signature so the analyzer's type resolution
// exercises the same shape it sees in production.
func WithDerive[T any](fn func(*T) error, writes ...string) Option {
	return func() {}
}
```

- [ ] **Step 2: Write the failing fixtures**

Append to `testdata/src/a/a.go`:

```go
type DeriveCfg struct {
	Pass     secret.String `source:"aws-sm://prod/db#password"`
	First    string        `source:"env:FIRST"`
	Last     string        `source:"env:LAST"`
	PlainDSN string
	SafeDSN  secret.String
	FullName string
}

// BAD: reveals a secret into a plain string.
var _ = mamori.WithDerive(func(c *DeriveCfg) error {
	c.PlainDSN = "postgres://" + c.Pass.Reveal() + "@h/db" // want `derive hook writes revealed secret material into "PlainDSN", a plain string; use secret.String or secret.Bytes`
	return nil
}, "PlainDSN")

// OK: reveals into a redacting type.
var _ = mamori.WithDerive(func(c *DeriveCfg) error {
	c.SafeDSN = secret.NewString("postgres://" + c.Pass.Reveal() + "@h/db")
	return nil
}, "SafeDSN")

// OK: no Reveal anywhere, so no secret material moved.
var _ = mamori.WithDerive(func(c *DeriveCfg) error {
	c.FullName = c.First + " " + c.Last
	return nil
}, "FullName")

// OK: Reveal on an unrelated type is not the secret package's.
type fakeSecret struct{}

func (fakeSecret) Reveal() string { return "" }

var _ = mamori.WithDerive(func(c *DeriveCfg) error {
	c.FullName = fakeSecret{}.Reveal()
	return nil
}, "FullName")
```

The stub already provides `NewString` and `NewBytes` (verified at
`testdata/src/github.com/xavidop/mamori/secret/secret.go:12,23`), so no change
is needed there.

- [ ] **Step 3: Run and verify they fail**

Run: `cd cmd/mamori/internal/vetcheck && go test ./...`
Expected: FAIL, the `want` comment on `PlainDSN` has no matching diagnostic.

- [ ] **Step 4: Implement the rule**

Create `derives.go` in `vetcheck`. Add `(*ast.CallExpr)(nil)` to `run`'s node
filter, and for each call:

1. Resolve the callee to `mamori.WithDerive` by package path, exactly as Task 2
   does. Reuse that matcher shape; duplicating ~20 lines is correct here.
2. Walk the hook literal's body for a `*ast.CallExpr` whose callee is a
   `*ast.SelectorExpr` named `Reveal` or `RevealBytes`, resolving the receiver's
   type through `pass.TypesInfo` and requiring its package path to be
   `github.com/xavidop/mamori/secret`. **Match on the path, not on a pointer to
   the real package**: the fixtures vendor a stub at that same import path, so a
   check recognizing only the real package would pass in production and never
   fire in its own tests.
3. If a reveal was found, for each literal write path resolve that field on `T`
   and report when its type is plain `string` or `[]byte`.

Message format, matching the existing rule's voice:

```
derive hook writes revealed secret material into %q, a plain %s; use secret.String or secret.Bytes
```

- [ ] **Step 5: Run and verify they pass**

Run: `cd cmd/mamori/internal/vetcheck && go test ./...`
Expected: PASS, exactly one diagnostic.

- [ ] **Step 6: Mutation-verify the type resolution**

Change the `Reveal` check to match on method name alone, dropping the package
path requirement. The `fakeSecret` fixture must start failing with an
unexpected diagnostic. Restore.

- [ ] **Step 7: Update the analyzer Doc string**

`Analyzer.Doc` describes only the tag rule. Extend it to name both rules, keeping
the rendered-from-the-set-itself property so the help text cannot drift.

- [ ] **Step 8: Run the full suite and commit** with a `feat(vet):` message.

---

### Task 4: Documentation

**Files:**
- Modify: `site/src/pages/docs/cli/explain.md`, `cli/schema.md`, `cli/vet.md`, `cli/diff.md` (whichever exist; check `ls site/src/pages/docs/cli/`)
- Modify: `site/src/pages/docs/usage/derived-fields.md` (the static-commands paragraph is now wrong)
- Modify: `skills/mamori/SKILL.md`

- [ ] **Step 1: Correct the now-false claim on the derived-fields page**

It currently says static commands never see a derived field. `explain`, `schema`, and `diff` now do; `policy` still does not, because a derive grants no permissions. Rewrite that paragraph, and note that a path built at runtime cannot be seen.

- [ ] **Step 2: Same claim in SKILL.md**

The bullet reading "Static commands never see a derived field" is now false. Correct it, and add the new `vet` rule to the `vet` line.

- [ ] **Step 3: Document the schema change**

`schema` now emits any field carrying `validate:` rules, source-tagged or not, because mamori validates the whole struct. Say so on the schema page: it is the reason the output may contain fields `explain` does not list.

- [ ] **Step 4: Build the site**

Run: `source ~/.nvm/nvm.sh && nvm use 22.23.2 && cd site && npm run build`
Expected: success. The default `node` is v20.17.0 and too old; do not skip the build.

- [ ] **Step 5: Verify no stale claim survives**

```bash
grep -rn "never see a derived\|cannot see a derived\|no static output" site/src/pages/docs/ skills/
```
Expected: no output.

- [ ] **Step 6: Commit** with a `docs:` message.

---

## Self-Review

**Spec coverage.** Spec "Gap 1" and "Field gains a Kind" are Task 1. "Piece 2: derive discovery", including the undiscoverable-writes rule and test-file exclusion, is Task 2. "Piece 3: vet" is Task 3. "The explain --json shape changes" is Task 1 Step 7 and Task 2 Step 7 (golden regeneration, with the policy goldens as a correctness check). Docs are Task 4.

**Type consistency.** `FieldKind`, `KindSource`, `KindDerived`, `KindValidate`, and `Field.Kind` are defined in Task 1 and consumed with those exact names in Tasks 2 and 3. `findDerives` returns `(map[string][]string, map[string]bool)` in Task 2's interface block and is used that way in Task 2 Step 4. `StructInfo.DerivesIncomplete` is introduced in Task 2 and read in Task 2 Step 6.

**Known gap, deliberate.** Task 1 Step 2's doctor test and Task 2 Step 1's
tests describe their assertions rather than spelling out every line, because
both depend on the shared fixture module and the `runX` helpers already in
`cmd/mamori/*_test.go`, and inventing a second mechanism would be worse than
pointing at the first. Every assertion each test must make is stated. Task 1's
schema and explain tests and Task 3's `analysistest` fixtures are written out in
full, the latter because those fixtures ARE the test.

**Corrected during self-review.** An earlier draft told implementers to add
tests to `cmd/mamori/extract_test.go`, which does not exist, and hedged on how
to regenerate goldens. `Extract` is tested only through the command tests, and
goldens regenerate with `go test -update` (`explain_test.go:14`).
