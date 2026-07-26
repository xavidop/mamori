# Workstream E: source chains (precedence)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a field draw from a precedence chain of sources, `source:"env:PORT,aws-ps://svc/port"`, where the first source that yields a value wins, `ErrNotFound` falls through, and a per-field `onfail` policy governs other errors, with every source in the chain watched live.

**Architecture:** `fieldSpec.Ref Ref` becomes `fieldSpec.Refs []Ref` plus a `fieldSpec.OnFail` policy. Single-ref tags produce a one-element chain, so existing behavior is byte-identical. The one-shot path (Load/Doctor) walks the chain per field. The watch path is the invasive change: the engine tracks per-(field, chain-position) source state, watches every position, and recomputes the winning value on any source update. Because a chain expresses precedence (not failover), a non-`NotFound` error stops the walk and applies `onfail`, rather than sliding to a lower-priority source.

**Tech Stack:** Go 1.26, stdlib. No new dependencies.

This implements spec section 10 (`docs/superpowers/specs/2026-07-24-operational-layer-design.md`). It is the most invasive change to core semantics in the whole spec, which is why it lands after the observability work that makes its behavior observable.

> **Note (layout changed since this plan was executed):** `tools/reconcilevet` no longer exists; the analyzer now lives at `cmd/mamori/internal/vetcheck` inside the `cmd/mamori` CLI module (renamed `mamorivet`), reachable via `mamori vet ./...` or `go vet -vettool=$(which mamori) ./...`. Its site doc has moved accordingly to `site/src/pages/docs/cli/vet.md`. See `docs/superpowers/specs/2026-07-24-operational-layer-design.md` section 10.6 for the current description.

## Global Constraints

- **Core dependencies frozen.** stdlib plus the four allowed deps. Nothing added.
- **Do not run `git commit`.** Stage with `git add`, report the suggested message.
- **Work on `main`.** No branches, no worktrees.
- **`GOWORK=off` for every Go command;** `make test` from the repo root.
- **The tree stays green after every task.**
- **No em-dash characters** anywhere.
- **The single-ref invariant.** A field with one source must behave EXACTLY as before this work: same resolution, same watch, same not-found/default/optional handling, same `Change` diffs. Every task re-runs the full existing suite to prove no regression. This is the highest-priority constraint: chains are additive, and the common case must not shift.
- **Precedence, not failover (spec decision D2).** First source yielding a value wins. `ErrNotFound` falls through to the next. Any other error stops the walk and applies `onfail`; it does NOT slide to a lower-priority source. Availability fallback remains `middleware.Failover`.
- Doc comments on every exported symbol, explaining the why.

---

### Task 1: ParseRefs and the fieldSpec plumbing

**Files:**
- Modify: `ref.go` (`ParseRefs`)
- Modify: `decode.go` (`fieldSpec.Refs`, `fieldSpec.OnFail`, walkSpecs parses chains and the `onfail` tag)
- Modify: `ref_test.go`, `decode` tests

**Interfaces:**
- Consumes: `ParseRef` (existing).
- Produces: `ParseRefs(tag string) ([]Ref, error)`, `fieldSpec.Refs []Ref`, `fieldSpec.OnFail onFailPolicy` (unexported type with values keeplast/useDefault/fail). Tasks 2-4 consume these.

**Parsing rules (spec 10.2).** Split on a comma ONLY when the remainder after it begins with a scheme-like token matching `^[a-zA-Z][a-zA-Z0-9+.\-]*:`. This avoids splitting commas inside query options (`?tags=a,b`) and inside opaque `exec:` paths. A literal comma that would otherwise be a separator is percent-encoded as `%2C`. `ParseRef` stays unchanged for single-ref parsing and external callers.

**The `onfail` tag:** `onfail:"keeplast"` (default), `"default"`, `"fail"`. `default` requires the field to have a `default:` tag (reject at walk time otherwise). Default policy reproduces today's behavior exactly.

- [ ] **Step 1: Write the failing tests**

Add to `ref_test.go`. Cover the comma-ambiguity corpus explicitly:
- `ParseRefs("env:PORT,aws-ps://svc/port")` -> two refs, schemes `env` and `aws-ps`.
- `ParseRefs("env:PORT")` -> one ref (single-ref still works).
- `ParseRefs("vault://kv?tags=a,b")` -> ONE ref (the comma is inside a query, not a separator), and the ref's `tags` opt is `a,b`.
- `ParseRefs("exec:echo a,b")` -> ONE ref (opaque exec path, comma is not a scheme boundary since `b` is not `scheme:`). This is the known ambiguous case; assert the whole `echo a,b` is one ref's path.
- `ParseRefs("exec:echo foo%2Cbar")` -> ONE ref whose decoded path contains the literal comma (document that `%2C` is how you force a literal comma).
- `ParseRefs("env:A,env:B,aws-sm://c")` -> three refs.
- `ParseRefs("")` -> error (empty).

Add a decode-level test: a struct field `source:"env:A,env:B" default:"x"` produces a `fieldSpec` with two `Refs` and `HasDefault`; a field with `onfail:"default"` but no `default:` tag fails walkSpecs with a clear error.

- [ ] **Step 2: Run, confirm failure**

```bash
GOWORK=off go test ./... -run 'TestParseRefs|TestWalkSpecsChain' -v
```

- [ ] **Step 3: Implement `ParseRefs`**

In `ref.go`:

```go
// schemeStart matches a scheme-like token at the start of a string, e.g.
// "aws-sm:" or "env:". It is how ParseRefs decides a comma is a chain
// separator rather than a comma inside a path or query.
var schemeStart = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*:`)

// ParseRefs parses a source tag that may hold a comma-separated precedence
// chain of refs, e.g. "env:PORT,aws-ps://svc/port". A comma is a separator only
// when the text after it begins with a scheme-like token; a comma inside a query
// option or an opaque exec: path is preserved. To force a literal comma that
// would otherwise be read as a separator, percent-encode it as %2C.
//
// A single-ref tag yields a one-element slice, so callers that do not use chains
// see no behavior change.
func ParseRefs(tag string) ([]Ref, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil, fmt.Errorf("mamori: empty source ref")
	}
	parts := splitChain(tag)
	refs := make([]Ref, 0, len(parts))
	for _, p := range parts {
		ref, err := ParseRef(p)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// splitChain splits tag on commas that begin a new scheme-prefixed ref.
func splitChain(tag string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(tag); i++ {
		if tag[i] != ',' {
			continue
		}
		if schemeStart.MatchString(tag[i+1:]) {
			parts = append(parts, tag[start:i])
			start = i + 1
		}
	}
	parts = append(parts, tag[start:])
	return parts
}
```

Add `regexp` to `ref.go`'s imports. Confirm `ParseRef` already percent-decodes the path so `%2C` becomes a literal comma in `Ref.Path` (read `ParseRef`; if it does not decode, the `%2C` case's assertion is on the raw `%2C` staying in the path, which is also acceptable as long as it is documented and not split, report which).

- [ ] **Step 4: Plumb Refs and OnFail into fieldSpec**

In `decode.go`, change `fieldSpec.Ref Ref` to `fieldSpec.Refs []Ref`, add `OnFail onFailPolicy`. Define:

```go
type onFailPolicy int

const (
	onFailKeepLast onFailPolicy = iota // default: retain last value, deliver error, continue
	onFailUseDefault                   // use the default: tag value
	onFailFail                         // reject the whole candidate snapshot
)
```

In `walkSpecs`, replace `ParseRef(source)` with `ParseRefs(source)`, parse the `onfail` tag (default keeplast), and reject `onfail:"default"` without a `default:` tag. Store `Refs` and `OnFail`.

**This breaks every `spec.Ref` reference in the package.** Task 1's job is ONLY to introduce `Refs`/`OnFail` and make the package COMPILE again by updating the trivial single-ref call sites to `spec.Refs[0]` where they currently use `spec.Ref`, WITHOUT yet implementing chain semantics. So: resolve.go, reconciler.go, report.go, doctor.go all switch `spec.Ref` -> `spec.Refs[0]` as a mechanical, behavior-preserving change (a one-element chain resolves/watches exactly as the single ref did). Chain resolution (Task 2) and chain watching (Task 3) then build on this. State clearly in your report that after Task 1, only the FIRST ref of any chain is used, so a multi-ref tag parses but does not yet chain; that is expected and the next tasks complete it.

- [ ] **Step 5: Run, confirm the full existing suite still passes**

```bash
GOWORK=off go test ./... -v
GOWORK=off go test -race ./...
make test
```

Every existing test must pass unchanged: single-ref tags now go through `Refs[0]` but behave identically. This is the single-ref-invariant checkpoint.

- [ ] **Step 6: Stage**

```bash
git add ref.go decode.go ref_test.go
```

```
feat(core): parse source chains into fieldSpec.Refs, plus onfail

A source tag may now hold a comma-separated precedence chain; ParseRefs splits
on scheme-prefixed commas so commas inside queries and exec paths are
preserved. fieldSpec carries Refs and an onfail policy. This commit only
parses and plumbs: resolution and watching still use the first ref, so
single-ref behavior is byte-identical. Chain semantics follow.
```

---

### Task 2: Chain resolution (Load and Doctor)

**Files:**
- Modify: `resolve.go` (`resolveAll`, `resolveOne`, batch grouping, `applyDefault`)
- Modify: `doctor.go` (`probeField` walks the chain)
- Test: `resolve` tests, `doctor_test.go`

**Interfaces:**
- Consumes: `fieldSpec.Refs`, `OnFail` (Task 1); `ErrorKind`, the sentinels.
- Produces: chain-aware one-shot resolution. Task 3 (watch) reuses the winner logic.

**The walk (spec 10.3), factor it into one shared function** both `resolveAll` and `probeField` (and Task 3) call:

```go
// resolveChain resolves a field's precedence chain: the first ref yielding a
// value wins; ErrNotFound falls through to the next; any other error stops the
// walk (precedence, not failover). It returns the winning value and the index
// of the winning ref, or an error describing the terminal condition.
```

Semantics:
1. ref yields a value -> that ref wins; return it.
2. ref returns `ErrNotFound` -> continue to the next ref.
3. ref returns any other kind -> stop; the caller applies `OnFail`.
4. all refs return `ErrNotFound` -> apply `default:` if present, else `optional`, else fail (exactly today's single-ref all-not-found behavior).

`onfail` handling on a non-not-found terminal error (case 3), for the one-shot Load path:
- `keeplast`: keep the last applied value if one exists; on initial Load there is no last value, so FAIL (return the error). It does NOT fall back to `default:` on an error, because `default:` applies only to genuine absence (`ErrNotFound`), never to an error. To use the default on an error, opt in with `onfail:"default"`. (Corrected from an earlier spec draft that wrongly had keeplast fall to default on error, which would reintroduce the exec-missing-binary footgun.)
- `default`: use the default.
- `fail`: return the error (rejects the Load).

**Batch grouping (spec 10.6):** `resolveAll` groups by scheme for `BatchProvider`. With chains, group by `(scheme, chain position)`: all refs at position 0 of scheme X batch together, etc. Keep it correct rather than clever; a straightforward approach is to resolve chains ref-by-ref but still batch same-scheme refs that sit at the same walk stage. If batching across chains is complex, a correct fallback is to batch only single-ref fields (the overwhelming common case) by scheme as today, and resolve multi-ref chains ref-by-ref; if you take that fallback, `log`-document it in the code comment and report it. Correctness first.

- [ ] **Step 1: Write the failing tests**

Use `mamoritest` with multiple providers/schemes. Cover:
- First ref wins: `source:"test://a,test://b"` with both set -> value of `a`.
- Fall-through: `a` absent (Del), `b` set -> value of `b`.
- Non-not-found stops: `a` fails with `ErrPermissionDenied`, `b` set -> the chain does NOT return `b`; it applies onfail (default keeplast -> on Load with no default, error).
- All absent -> default applies; all absent + no default + optional -> zero value; all absent + required -> error.
- `onfail:"fail"` with `a` erroring -> Load fails.
- `onfail:"default"` with `a` erroring -> Load uses the default.
- Single-ref unchanged: existing resolve tests still pass.
- Doctor: a chain where `a` errors and `b` would resolve reports the field per the precedence/onfail rules (Doctor is a reachability check; report the winning ref's outcome).

- [ ] **Steps 2-5: Implement, run, race, full suite, stage**

Implement `resolveChain`, route `resolveOne`/`resolveAll` and `probeField` through it, handle `OnFail`. Run the full suite (single-ref invariant), `-race`, `make test`. Stage `resolve.go doctor.go` + tests with:

```
feat(core): resolve source chains by precedence

The first ref in a chain to yield a value wins; ErrNotFound falls through; any
other error stops the walk and applies the field's onfail policy (keeplast,
default, or fail) rather than sliding to a lower-priority source. Single-ref
fields resolve exactly as before.
```

---

### Task 3: Chain watching (the invasive engine rework)

**Files:**
- Modify: `reconciler.go` (`start`, `loop`, `srcUpdate`, per-source state, winner recomputation, `handleErr`, `schemeForPath`, `debounceFor`)
- Modify: `poll.go` if the poll-watch signature needs the ref (it takes a ref already)
- Test: `watch_test.go` / new chain-watch tests

**Interfaces:**
- Consumes: Task 1's `Refs`/`OnFail`, Task 2's `resolveChain`/winner logic.
- Produces: live chain watching. This is where every chain position is watched and the winner recomputed on any update.

**The rework.** Today `start` creates one watch source per spec and `srcUpdate{idx}` carries the spec index; `observed[path]` holds the single value. For chains:
- `start` creates one watch source per `(specIdx, refPos)` pair. `srcUpdate` carries BOTH indices.
- The engine tracks per-source state: `sources[specIdx][refPos] = srcState{value, err, seen}`.
- On any source update, update that source's state, then recompute the winner for `specIdx` by walking its chain over the per-source states (the same precedence rules as Task 2, but over already-observed states rather than live resolves). If the winning value changed, set `observed[path]` and mark the field pending, exactly as today.
- `onfail` on a non-not-found winner-terminal error: `keeplast` keeps `observed[path]` as-is and delivers the error to `OnError` (today's behavior); `default` sets the default value; `fail` marks the field such that `flush` rejects the whole candidate.
- `handleErr` and `lastErr`/`Status` now key on the field but must reflect the winning/terminal source's error. `schemeForPath` returns the WINNING ref's scheme (so metric labels stay meaningful).
- `debounceFor` reads the debounce opt from whichever ref carries it; if multiple do, use the smallest (matching the existing coalescing-window rule).

**Preserve exactly:** for a one-element chain, this must reduce to today's behavior. The winner of a one-element chain is that element; a not-found on it applies default/optional as today; an error applies keeplast as today. Re-run the full watch suite.

- [ ] **Step 1: Write the failing tests**

Cover live precedence with `mamoritest` (multiple providers):
- Live takeover: chain `env:PORT,test://port`; initially `env:PORT` unset so `test://port` wins; then set `env:PORT` at runtime -> the env value takes over (higher precedence), one `Change`; then unset it -> falls back to `test://port`, one `Change`. (Use `mamoritest` for the `test://` source and `t.Setenv` for env, or two `mamoritest` providers if driving env at runtime is awkward; report the mechanism.)
- A non-not-found error on the winning source applies `onfail` (keeplast: value retained, error to `OnError`; fail: candidate rejected; default: default applied).
- Every position is watched: a change to a LOWER-priority source while a HIGHER one is winning does NOT change `Get()` (the higher one still wins), but is observed (no error).
- Single-ref watch unchanged: full existing watch suite passes.
- `-race` with concurrent updates across chain positions; `goleak` on Close (more sources means more forwarder goroutines; all must exit).

- [ ] **Steps 2-6: Implement, run, race, goleak, full suite, stage**

This is the highest-risk task in the plan. After implementing, run `GOWORK=off go test -race ./...` and `make test`, and explicitly re-run the workstream-B/C/D suites (Status, Doctor, Pin, mamoritest) since they all observe engine state that chains now feed. Report that the single-ref invariant holds (existing watch tests unchanged) and that the new forwarder goroutines all exit on Close (goleak clean).

```
feat(core): watch every source in a precedence chain

The engine now watches each position of a field's chain and recomputes the
winning value on any source update, so exporting a higher-priority source at
runtime takes over and unsetting it falls back. A non-not-found error applies
the field's onfail policy rather than sliding down the chain. Single-source
fields watch exactly as before; goroutine hygiene holds.
```

---

### Task 4: mamorivet chain awareness

**Files:**
- Modify: `cmd/mamori/internal/vetcheck/analyzer.go` (parse chains, not just single refs)
- Test: `cmd/mamori/internal/vetcheck/analyzer_test.go` + testdata

**Interfaces:**
- Consumes: the chain tag syntax (Task 1).
- Produces: a `mamorivet` that flags a sensitive ref anywhere in a multi-ref tag.

**Why this is a correctness fix, not an enhancement.** `mamorivet` flags a sensitive ref assigned to a plain `string` field. It currently parses a single ref per `source` tag. With chains, a tag like `source:"env:X,aws-sm://secret"` would have its sensitive `aws-sm://` ref SILENTLY IGNORED by the analyzer, so the vet check under-reports. A vet analyzer that quietly stops catching the thing it exists to catch is worse than one that errors. This task makes it parse every ref in the chain.

- [ ] **Step 1: Write the failing test**

Add a testdata case with a chained sensitive ref on a plain `string` field (`source:"env:X,aws-sm://prod/db#password"`) and assert the analyzer flags it. Read how `cmd/mamori/internal/vetcheck` shares tag-parsing (the spec notes tag parsing should be shared with the CLI; if the analyzer has its own parser, make it use the same scheme-comma split as `ParseRefs`, or replicate the split faithfully and note the duplication for the later shared-extraction work in the CLI plan).

- [ ] **Steps 2-4: Implement, run, stage**

```bash
cd cmd/mamori && GOWORK=off go test ./internal/vetcheck/...
make vet-analyzer
```

```
fix(mamorivet): flag sensitive refs anywhere in a source chain

The analyzer parsed only the first ref of a source tag, so a sensitive ref in
a later chain position was silently ignored and a plain string field bound to
it went unflagged. It now checks every ref in the chain.
```

---

### Task 5: Documentation

**Files:**
- Modify: `site/src/pages/docs/concepts.md` (chain grammar, precedence, onfail, the %2C escape)
- Modify: `site/src/pages/docs/usage.md` (chain + onfail examples)
- Modify: `site/src/pages/docs/cli/vet.md` (chain-aware)
- Modify: `README.md`

- [ ] Document the chain grammar and precedence rules, the `onfail` policies (with `keeplast` as the default reproducing single-ref behavior), the comma-ambiguity rule and the `%2C` escape, and that every chain position is watched so precedence is live. State that availability fallback remains `middleware.Failover` (chains are precedence, not failover). Verify examples. `make site-build`. Stage.

---

## Self-Review

**Spec coverage.** Implements spec section 10 in full: 10.1 syntax and 10.2 parsing (Task 1), 10.3 resolution and 10.4 failure policy (Task 2), 10.5 watch behavior and 10.6 ripple (Task 3), the `mamorivet` correctness fix called out in 10.6 (Task 4), documentation (Task 5).

**Placeholders.** None. Task 1 gives full `ParseRefs`/`splitChain` code; Tasks 2-3 specify the shared `resolveChain`/winner logic and the exact engine rework with the single-ref invariant as the guardrail. Two steps flag a judgment call and require a report: Task 2's batch-grouping (full `(scheme, position)` grouping vs the documented single-ref-only fallback) and Task 3's runtime-env test mechanism.

**Type consistency.** `fieldSpec.Refs []Ref` and `OnFail` are introduced once (Task 1) and consumed by resolve, doctor, and the engine. The precedence walk is one shared `resolveChain`/winner function used by the one-shot path (Task 2) and adapted for the observed-state path (Task 3), so the rules cannot diverge. `schemeForPath` and the report/status field scheme all resolve to the WINNING ref's scheme.

**Risk noted.** Task 3 is the highest-risk change in the entire project: it rewrites the reconciler's per-source tracking, which every existing user depends on. The mitigation is the single-ref invariant, enforced by re-running the complete existing suite (watch, status, doctor, pin, mamoritest) after Tasks 1, 2, and 3, plus `-race` and `goleak`. Task 1 is deliberately behavior-preserving (parse and plumb only, `Refs[0]`), so the risky semantics land in reviewable, separately-verified steps rather than one big change. The comma-ambiguity in `exec:` is documented and tested rather than solved (the `%2C` escape), matching the spec.
