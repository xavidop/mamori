# Static commands see derived fields

**Goal:** `mamori schema` stops omitting rules mamori enforces, and `mamori explain` and `mamori diff` report `WithDerive`-declared fields.

**Status:** design approved, pending spec review.

## Two gaps, not one

`Extract` (`cmd/mamori/extract.go`) walks a source tree and keeps only fields
carrying a `source:` tag: `walkFields`'s `switch` has a `hasSource` case and a
`default` that recurses into source-less container structs and otherwise skips.
Everything downstream inherits that scope.

**Gap 1, a correctness bug.** `Validate` calls `v.Struct(cfg)` on the whole
struct (`validator.go:23`), with no source-tag filtering, so every field with a
`validate:` tag is enforced on load and on every reconciled update. A field
written

```go
DSN secret.String `validate:"required,url"`
```

is therefore validated at runtime and rejected when invalid, while appearing
nowhere in the schema you would feed to a validator. `mamori schema` describes
less than mamori enforces. Derived fields are one instance; any validated field
without a `source:` tag has the same problem.

**Gap 2, a missing feature.** A `WithDerive`-declared field is invisible to
`explain` and `diff`, so a config surface listing does not mention a field
mamori maintains.

## Field gains a Kind

`Extract` has four consumers: `explain.go:55`, `schema.go:63`, `policy.go:104`,
and `doctor.go:197` (`--compare`). Widening its output without a discriminator
breaks two of them, so every consumer must opt in to the kinds it wants.

`vet` is NOT a consumer. It runs the `vetcheck` go/analysis Analyzer, which
walks its own syntax trees, so the `Kind` work below does not reach it. Piece 3
gives it its own rule.

```go
type FieldKind string

const (
	KindSource   FieldKind = "source"   // carries a source: tag
	KindDerived  FieldKind = "derived"  // named by a WithDerive call
	KindValidate FieldKind = "validate" // no source:, has validate:
)
```

| Kind | explain / diff | schema | policy | doctor --compare |
| --- | --- | --- | --- | --- |
| `source` | yes | yes | yes | yes |
| `derived` | yes | yes | no | **no** |
| `validate` | no | yes | no | **no** |

`explain` answers "what does mamori read", so a validate-only field the
application populates itself is not its business. `schema` answers "what shape
must this config be", so everything mamori validates belongs. `policy` emits
permissions from refs, and neither new kind has one.

`doctor --compare` is the dangerous one. It diffs statically extracted paths
against a live report's paths and currently builds its source set from every
field `Extract` returns (`doctor.go:203-207`). Left unfiltered it would report
false drift twice over: a validate-only field is never in a live report at all,
and a derived field is deliberately excluded from the live side already
(`doctor.go:209`), so both would surface as "only in source" on a healthy
config. It filters to `KindSource`, which keeps its behavior byte-for-byte
identical to today's.

**Struct qualification does not change.** A struct still needs at least one
`source:` tag to count as a mamori config struct. Only the field enumeration
widens, so an unrelated struct carrying a `validate:` tag does not start
appearing.

## Piece 1: the schema gap

One case added to `walkFields`, between `hasSource` and `default`: a field with
no `source:` tag but a non-empty `validate:` tag becomes a `Field` with
`Kind: KindValidate`, empty `Source`, and nil `Refs`. Its `GoType`, `Validate`,
`Optional`, `Default`, and `Sensitive` are populated exactly as a sourced field's
are.

`schema.go` already builds a property from `GoType` and `Validate` and needs no
change beyond not filtering the new kind out. `policy.go` and `runCompare`
filter to `KindSource`.

This piece needs no AST work and is independently shippable.

## Piece 2: derive discovery

Feasibility was confirmed by prototype against this repo before writing this
spec: 60 of 61 `WithDerive` call sites resolved completely, recovering both the
config type and the declared paths, across package boundaries
(`mamori.deriveCfg`, `mamori_test.rotCfg`).

A new file, `cmd/mamori/derives.go`, walks the syntax trees `Extract` already
loads (`packages.NeedSyntax | packages.NeedTypesInfo` are set at
`extract.go:76-77`) and returns, per config type, the declared write paths.

For each `*ast.CallExpr`:

1. Resolve the callee through `TypesInfo.Uses` rather than matching the name
   text, so a local function called `WithDerive` is not mistaken for mamori's.
   Require `obj.Pkg().Path() == "github.com/xavidop/mamori"`. Handle
   `*ast.Ident`, `*ast.SelectorExpr`, and `*ast.IndexExpr` (an explicit type
   argument) as callee shapes.
2. Recover `T` from `TypesInfo.Instances[id].TypeArgs.At(0)`.
3. Read each argument after the hook via `TypesInfo.Types[arg].Value`, which
   yields a constant for a literal and nil otherwise.

Paths are matched to a `StructInfo` by the recovered type's name and package. A
path naming no field on `T` is kept out of the output, matching the runtime
behavior that a path matching nothing simply never reports as written.

### Undiscoverable writes are reported, never silently dropped

Three shapes cannot be resolved statically: a path passed as a variable, a
`paths...` expansion from a slice, and options assembled by a helper returning
`[]Option`. The prototype's single unresolved site is the third-party-shaped
case, `WithDerive(fn, paths...)`.

When a `WithDerive` call for a struct has any non-literal path argument, that
`StructInfo` is marked `DerivesIncomplete: true`, and `explain` prints a note
that the struct declares writes it could not read. Under-reporting silently
would make an incomplete listing look complete, which is worse than not listing
derived fields at all.

### Test files are excluded

`Extract` does not set `packages.Config.Tests`, so test files are not loaded and
a `WithDerive` call appearing only in a test does not contribute a derived field.
This is deliberate and stays that way: `explain` describes the shipping config
surface.

## diff compatibility

`mamori diff` compares two `explain --json` outputs. Adding `kind` to that JSON
means an old base file diffed against a new head file would show every field as
modified. `diff` therefore treats a missing `kind` as `source`, so a base file
produced by an older binary keeps comparing cleanly.

## Piece 3: vet flags a derive that launders a secret

`vet` today flags a field whose `source:` ref names a secret-bearing scheme but
whose Go type is a plain `string` or `[]byte`. A derived field has no scheme, so
the existing rule cannot reach it. The naive replacement, "flag any derived
write into a plain string", is unusable: `FullName = First + " " + Last` is the
common harmless case and would fire on every one.

**The sound signal is `Reveal`.** `Reveal()` and `RevealBytes()` exist only on
`secret.String` and `secret.Bytes`, and calling one is the caller explicitly
extracting secret material. So the rule is:

> A `WithDerive` hook whose body calls `Reveal` or `RevealBytes` on a
> `secret.String` or `secret.Bytes`, and which declares a write path naming a
> field of plain `string` or `[]byte` type, is flagged.

That is exactly the laundering case, and nothing else:

| Hook | Flagged | Why |
| --- | --- | --- |
| `DSN string`, body calls `c.Pass.Reveal()` | yes | secret material into an unredacted type |
| `DSN secret.String`, body calls `c.Pass.Reveal()` | no | target redacts |
| `FullName string`, `c.First + " " + c.Last` | no | no `Reveal`, no secret |

The `Reveal` call is resolved through the type checker, not by method name, so a
`Reveal()` on some unrelated type is not mistaken for the secret package's.
Match on the receiver's package **path** (`github.com/xavidop/mamori/secret`),
not on a pointer to the real package: `vetcheck`'s `analysistest` fixtures
already vendor a stub secret package at that same import path
(`internal/vetcheck/testdata/src/github.com/xavidop/mamori/secret/`), and a
check that only recognizes the real package would pass in production and never
fire in its own tests.

**The discovery is duplicated here, deliberately.** `vetcheck` must keep working
under `go vet -vettool=$(which mamori)`, where `unitchecker` runs it one package
at a time and calling `Extract` (which does its own `packages.Load`) is not
possible. Sharing the call-site matcher as a small helper in
`internal/sourcetag` is fine; sharing `Extract` is not.

**Scope limit, accepted.** A hook that calls a helper function which reveals,
rather than revealing inline, is not detected. That is under-detection, not a
false positive, and it matches how the existing rule only sees the tag in front
of it.

## Testing

**Piece 1.** A fixture struct with a source-tagged field and a validate-only
field asserts the validate-only field reaches `schema` and stays out of
`policy`. Mutation-verify by removing the `KindValidate` case and watching the
schema test fail.

**Regression guard for `--compare`, required.** A test running
`doctor --compare` against a config carrying both a derived field and a
validate-only field must report no drift. Mutation-verify by removing the
`KindSource` filter from `runCompare` and watching it report both as "only in
source". This is the one place the change can silently break a working command,
and `TestDoctorCompareIgnoresDeclaredDerivedField` covers only the live side.

**Piece 2.** Fixtures for: literal paths on one struct; multiple paths in one
call; two `WithDerive` calls on the same struct; a call in a different package
from the struct; a path naming no field on `T` (must not appear); a non-literal
path (must set `DerivesIncomplete`, must not silently drop); and a local
function named `WithDerive` (must not be picked up). Mutation-verify the
package-path check and the non-literal detection.

**Compatibility.** A `diff` test feeding a base JSON with no `kind` key against
a head JSON with one asserts no spurious modifications.

**Piece 3.** `analysistest` fixtures, the shape `vetcheck` already uses, for:
a hook revealing into a plain `string` (flagged); the same hook revealing into a
`secret.String` (not flagged); a hook concatenating two plain fields with no
`Reveal` (not flagged); a `Reveal()` method on an unrelated type (not flagged);
and a hook declaring two paths where only one is plain (exactly one
diagnostic). Mutation-verify the type resolution of `Reveal` by pointing it at
method name alone and watching the unrelated-type fixture start failing.
