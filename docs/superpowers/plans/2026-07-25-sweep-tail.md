# Error Mapping Sweep, Part 4 (final): the flag/SaaS tail, and flip Fail to required

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring the remaining 14 providers into the error-classification model, then flip `providertest.Config.Fail` from optional to required, completing conformance enforcement for all 35 providers.

**Architecture:** The 14 remaining modules split three ways by their error surface. (1) Ones with a real error vocabulary AND an injectable seam get a `classify*` plus `Fail`/`Clear`. (2) Ones with a per-key error seam but no vocabulary get `Fail`/`Clear` only, so the conformance case verifies their error path preserves the `errors.Is` chain (catching `%v` flattening) without a classifier. (3) Three modules (`unleash`, `configcat`, `split`) have error-free bool/string interfaces, so their real `Resolve` cannot produce a classifiable error at all; a new explicit `Config.NoResolveErrors` flag exempts them, greppably. The final task makes `Fail`-or-`NoResolveErrors` required.

**Tech Stack:** Go 1.26. Per-module SDKs; no new core dependencies.

Parts 1-3 covered the 21 providers where permission/auth errors actually bite (core built-ins, cloud, object stores, datastores). This part is the completionist tail plus the enforcement flip.

## The `NoResolveErrors` design decision (spec amendment)

The original spec (§6.2) said `providertest.Config.Fail` becomes flatly required. Research during this plan found that `unleash`, `configcat`, and `split` have client interfaces that return only `bool`/`string` (existence-via-membership, control-string sentinels); their real SDKs surface no per-key error, so their `Resolve` can only ever return `ErrNotFound` or a client-construction error, never a classifiable per-resolve error. There is nothing to inject or flatten.

Rather than force a fictional error-returning method with no real-SDK counterpart, this plan amends the enforcement: `providertest.Run` requires EITHER `Fail`+`Clear` OR an explicit `Config.NoResolveErrors: true`. An unset provider still fails `Run` (forcing a deliberate choice), and the exemption is a one-line, greppable, commented declaration, so "deliberately exempt" is distinguishable from "not updated yet" (the exact ambiguity the spec wanted to avoid). This is the honest enforcement.

## Global Constraints

- **Do not run `git commit`.** Stage with `git add`, report the suggested message. (There is a large amount of pre-existing staged work from workstreams A-E on this tree; touch only your task's files, and NEVER run `git stash`, `git clean`, `git checkout -- <file>`, or `git reset` on this tree, a prior subagent nearly clobbered it with `git stash`.)
- **Work on `main`.** No branches, no worktrees.
- **`GOWORK=off` for every Go command,** from inside the module directory; `make test` from the repo root.
- **The tree stays green after every task.** Until the final task, `Fail` is still optional, so a module that has not yet wired it just skips the case.
- **No em-dash characters** anywhere.
- **Wrapping is `fmt.Errorf("%w: %w", sentinel, err)`** where a classifier maps an SDK error; where there is no classifier, the existing error path must wrap the injected error with `%w` (never `%v`) so the conformance case passes. Confirm each module's `Resolve` error path uses `%w`.
- **`ErrNotFound` behavior must not change.** Preserve each module's existing not-found detection exactly.
- **Never guess a code.** Verify every SDK code/status against the SDK source or docs; earlier parts shipped several fabricated codes caught only in review.
- **A passing `ErrorClassification` case does not prove a mapping;** the table test over real SDK errors does. For no-classifier modules, the conformance case proves only chain-preservation, which is its whole job there.

## Module groups

**Group 1, classify + wire Fail (real vocabulary + seam):**
- `launchdarkly` (map-fake, native watch): `ldreason` eval-error kinds. Thin; classify what is real (FlagNotFound stays not-found; others mostly client-init, not per-eval), wire Fail. If the only real per-eval kind is not-found, treat as Group 2 and report.
- `flipt` (map-fake, gRPC): `codes.NotFound`/`InvalidArgument` + `flipterrors.ErrUnauthenticated`/`ErrUnauthorized`/`ErrCanceled`. Map the real ones (permission_denied, unauthenticated, invalid); leave the rest unknown.
- `goff` (map-fake): OpenFeature `flag.ErrorCode` enum (`PROVIDER_NOT_READY`->unavailable, `PARSE_ERROR`/`TYPE_MISMATCH`/`INVALID_CONTEXT`->invalid, `GENERAL`->unknown, `FLAG_NOT_FOUND` stays not-found).
- `doppler` (httptest/RoundTripper): HTTP status (403->permission_denied, 401->unauthenticated, 429->rate_limited, 5xx->unavailable, 400->invalid). Inject at the RoundTripper.
- `onepassword` (httptest/RoundTripper): HTTP status, same map. Coarse seam (whole-item fetch), so a single pending-error var, not a per-key map; the conformance case issues one Resolve at a time, so that suffices. Report the seam.
- `firebase-rc` (map+httptest): HTTP status. Coarse seam (whole-template fetch); single pending-error var on the fake. Report.
- `sops` (real files + injected DecryptFunc): `os.IsNotExist` stays not-found; `os.IsPermission`->permission_denied. Inject via a `fails map[string]error` consulted inside the conformance `DecryptFunc` closure (the cleanest seam of all).
- `sqlite` (real temp file, NO seam today): has a real vocabulary (`*sqlite.Error.Code()`: `SQLITE_PERM`/`SQLITE_READONLY`->permission_denied, `SQLITE_BUSY`/`SQLITE_LOCKED`->unavailable, `SQLITE_CANTOPEN`->unavailable, `SQLITE_AUTH`->unauthenticated). Requires ADDING an injectable opener/queryer seam to the provider (it hardcodes `sql.Open`). This is the one provider-code-structure change; do it carefully and behavior-preservingly, or if the seam proves too invasive for this pass, classify from the real `*sqlite.Error` in `scanErr` and mark the module `NoResolveErrors` for the conformance case with a comment that its real vocabulary is classified but not conformance-injectable without a driver double. Report which you chose.

**Group 2, wire Fail only (per-key seam, no vocabulary):**
- `firebase-rtdb` (map-fake, per-path Get), `growthbook` (map-fake, per-key), `flagsmith` (map-fake, per-key). Add `fails map[string]error` consulted in the per-key serving method; confirm `Resolve` wraps with `%w`; no classifier. The conformance case then verifies chain preservation.

**Group 3, NoResolveErrors exemption (error-free interface):**
- `unleash`, `configcat`, `split`. Their client interfaces return only bool/string. Set `Config.NoResolveErrors: true` in their conformance `providertest.Run` call, with a comment naming why (the real SDK surfaces no per-key error). No classifier, no Fail.

## Per-task shape

Groups 1 and 2 follow the established sweep shape (see `2026-07-24-error-sweep-datastores.md`): table test over real SDK errors (Group 1 only), `classify*` (Group 1 only), route the error path through it / confirm `%w`, add `fails`+`fail`+`clear` to the fake (or the coarse pending-error var), wire `Fail`/`Clear` into `providertest.Run`, add the mandatory `Resolve`-level test injecting a REAL SDK error (Group 1) or a real error through the seam (Group 2), module README + docs-site error section, do NOT touch the shared coverage tables (a later task owns them). Group 3 is just the `NoResolveErrors` line + a README note.

Run each module's module directory tests with `-race`, then `make test` from the repo root. Prove each classifier non-vacuous (Group 1) by the temporary-revert method. The disjoint modules may be executed in parallel; the shared coverage tables and the `Fail`-required flip are the final serialized task.

---

### Tasks

Because the modules are disjoint (each is its own module plus its own docs-site page), they are grouped for parallel execution and reviewed individually:

- [ ] **Task 1: `launchdarkly`** (Group 1/2 per its real vocabulary; report which)
- [ ] **Task 2: `flipt`** (Group 1, gRPC + flipterrors)
- [ ] **Task 3: `goff`** (Group 1, OpenFeature error codes)
- [ ] **Task 4: `doppler`** (Group 1, HTTP status, RoundTripper seam)
- [ ] **Task 5: `onepassword`** (Group 1, HTTP status, coarse pending-error seam)
- [ ] **Task 6: `firebase-rc`** (Group 1, HTTP status, coarse pending-error seam)
- [ ] **Task 7: `sops`** (Group 1, os.IsPermission, DecryptFunc seam)
- [ ] **Task 8: `sqlite`** (Group 1, sqlite.Error.Code; the seam decision)
- [ ] **Task 9: `firebase-rtdb`** (Group 2, wire Fail only)
- [ ] **Task 10: `growthbook`** (Group 2, wire Fail only)
- [ ] **Task 11: `flagsmith`** (Group 2, wire Fail only)
- [ ] **Task 12: `unleash` + `configcat` + `split`** (Group 3, NoResolveErrors exemption; one task, three modules)
- [ ] **Task 13: providertest `NoResolveErrors` field + flip `Fail` to required** (require Fail+Clear OR NoResolveErrors; Run fails otherwise). Update `writing-a-provider.md` and CONTRIBUTING.md; the CHANGELOG note is generated from the commit, so write a clear commit body. This is the enforcement flip; it must come AFTER all 34 other providers (the 21 done + the 13 here) supply Fail/Clear or NoResolveErrors, or it turns their conformance tests red.
- [ ] **Task 14: coverage tables + cross-check + docs** (root README + `site/.../providers/index.md`: mark the newly-classifying modules; state final coverage; cross-check each new table against code). 35/35 providers now either classify or are explicitly `NoResolveErrors`.

Each Group 1/2 task's detailed steps mirror the datastore-sweep plan; each names its module's file, fake type, `providertest.Run` line, and mapping (from the research above). Group 3 and the flip are detailed inline in Tasks 12-13.

---

## Self-Review

**Spec coverage.** Completes spec §6.2 (the per-provider sweep) for the final 14 modules and executes the "flip Fail to required" enforcement, amended by the `NoResolveErrors` exemption (documented above) to handle the three providers with no error surface honestly rather than with fictional seams.

**Placeholders.** The per-module steps reference the established datastore-sweep shape rather than re-inlining it 11 times (which would invite copy-drift); each task carries its module's specific seam, fake, and mapping from the research. The two genuinely novel decisions, `sqlite`'s seam and the `NoResolveErrors` flip, are detailed and flagged for a reported judgment call.

**Type consistency.** Every Group 1 task produces `classify<Module>` with the established shape. `Config.NoResolveErrors bool` is added once (Task 13) and set by Group 3 (Task 12) and any Group 1 module that takes the sqlite fallback. `Fail`/`Clear` are uniform across all wired fakes.

**Risk noted.** The highest-risk items: `sqlite` needs a provider-code seam (or the documented fallback), the only structural provider change here; and the `Fail`-required flip (Task 13) must land only after every other provider supplies `Fail`/`Clear` or `NoResolveErrors`, or it reddens their conformance tests, so it is strictly last before the docs task. The `NoResolveErrors` exemption is the one spec amendment; it is deliberately explicit and greppable so it cannot become a silent skip.
