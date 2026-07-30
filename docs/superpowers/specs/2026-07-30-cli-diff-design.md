# mamori diff design

**Status:** approved
**Date:** 2026-07-30

Adds a fourth static command to the CLI:

```
mamori diff <base.json> <head.json> [--json|--markdown]
            [--exit-code=any|privilege] [--policy-format=aws-iam|gcp|external-secret]
```

It compares two `mamori explain --json` outputs and reports what changed about a
service's configuration surface, including the delta in the backend permissions
that surface implies.

## What this is for

A pull request that adds one struct field can change what secrets a service
reads and what IAM permissions it needs to start. Today that is invisible in
review: the reviewer sees a one-line struct tag, and the consequence (this
service now reads `prod/stripe`, and its role needs
`secretsmanager:GetSecretValue` on a new ARN) has to be reconstructed by hand,
correctly, by someone who noticed the tag mattered.

`mamori explain --json` already extracts everything needed to answer that
([cmd/mamori/explain.go](../../../cmd/mamori/explain.go)), and
`collectPolicyRefs` already turns a `[]StructInfo` into the set of backend
paths it touches ([cmd/mamori/policy.go](../../../cmd/mamori/policy.go)).
Nothing consumes either as a *change*. This command is that missing consumer:
it turns the privilege impact of a config change into something a reviewer
reads in the PR, from static analysis, with no credentials and no running
process.

This is a review primitive no Go configuration library ships, and it exists here
only because the static half of the CLI was built to never resolve anything.

## Inputs, and the rejected directions

Both operands are files written by `mamori explain --json`. A single `-` reads
that operand from stdin. In CI:

```bash
git checkout "$BASE" && mamori explain ./... --json > /tmp/base.json
git checkout "$HEAD" && mamori explain ./... --json > /tmp/head.json
mamori diff /tmp/base.json /tmp/head.json --markdown >> "$GITHUB_STEP_SUMMARY"
```

**Rejected: git revisions.** `mamori diff --base origin/main ./...` would be one
command instead of four, but it would have to shell out to `git` for a temporary
checkout and then run `packages.Load` against a historical tree. That needs the
old revision to still build: a resolvable module graph, possibly a populated
module cache, possibly the network. A review tool whose failure mode is "your
base commit no longer builds" is worse than no tool. Taking JSON keeps the
command total, testable from fixtures, and usable in any CI system.

**Rejected: package patterns for the head side.** `mamori diff base.json ./...`
is genuinely convenient, since head is usually just the working tree. It is
excluded anyway because it reintroduces exactly the `packages.Load` dependency
the previous paragraph rules out, for the operand where it is least necessary.
Recorded here as a considered exclusion rather than an oversight.

## The diff model

Structs match on `(Package, TypeName)`. Fields match on
`(Package, TypeName, Path)`. Each produces one of three verdicts: **added**,
**removed**, or **changed**.

"Changed" compares every attribute `Field` carries
([cmd/mamori/extract.go](../../../cmd/mamori/extract.go)): `GoType`, `Source`,
`Refs`, `Default`, `HasDefault`, `Optional`, `Sensitive`, `OnFail`, and
`Validate`. Each differing attribute is reported as old to new, not as a bare
"changed", because the whole point is that the reviewer should not have to open
the diff to learn what actually moved.

Ordering is total and deterministic: structs by package then type name, fields
by path, attributes in the order declared above. This output is pasted into pull
request comments, where unstable ordering would produce phantom churn between
runs and train reviewers to ignore it.

Two cases are deliberately not treated as ordinary attribute changes:

**`Sensitive` flipping false to true** gets its own callout in every output
format. It means the service began reading secret material where it previously
read plain configuration. That is the single most review-worthy fact this tool
can surface, and listing it as one row among eight attribute changes would waste
it.

**A precedence chain gaining or losing a position** is reported at ref
granularity, not as one opaque `Source` string change. `env:PORT` becoming
`env:PORT,aws-ps://svc/port` is not a string edit, it is the service acquiring a
new backend dependency, and it should read like one. `Field.Refs` is already the
chain-split list, so the comparison is over two ordered ref sequences: added
positions, removed positions, and reordering are each distinguishable, and
reordering matters because chain order is precedence.

## Privilege delta

`collectPolicyRefs` takes `[]StructInfo` and nothing else, so it runs unchanged
on both sides. Diffing the resulting per-scheme buckets yields the backend paths
added and removed.

The default rendering is **scheme neutral**: "reads 1 new `aws-sm` path:
`prod/stripe`". This works for all thirty-eight providers and presumes no cloud,
which matters because most users of this command are not on AWS.

`--policy-format=aws-iam|gcp|external-secret` additionally renders the added and
removed grants in that artifact's vocabulary, reusing the existing `awsSecretARN`,
`awsParameterARN`, and `gcpResourceName` helpers so the ARNs and resource names
match what `mamori policy` would emit for the same refs. It is opt in because
defaulting to a specific cloud would presume one.

Only the schemes those helpers know how to address (`aws-sm`, `aws-ps`,
`gcp-sm`) can be rendered concretely. Every other scheme still appears in the
scheme-neutral section, so a change to a `vault://` or `k8s-secret://` ref is
never silently dropped from the privilege view just because no IAM vocabulary
exists for it.

## Output and exit codes

Three formats: an aligned text table by default (matching `explain`'s table
style), `--json` (matching `explain`'s flag), and `--markdown` for direct
consumption by `gh pr comment` and `$GITHUB_STEP_SUMMARY`.

Exit codes follow `git diff` rather than `diff(1)`: findings do not fail the
command unless asked. The common case is posting a diff as a PR comment, and a
tool that exits non-zero for "there was a change" breaks a naive CI step for
doing exactly what it was asked to do.

| Code | Meaning |
| --- | --- |
| 0 | Success. Also the result when findings exist but no `--exit-code` was given. |
| 1 | Usage error, unreadable file, or unparseable JSON. Matches `explain`. |
| 2 | Findings present and `--exit-code` asked for them to be signalled. |

`--exit-code=any` signals on any structural or privilege change.
`--exit-code=privilege` signals only when the privilege surface **grows**, which
is the security gate worth blocking a merge on. A privilege surface that only
shrinks is not a finding under `privilege`, since losing access is not a risk to
gate.

A bare `--exit-code` with no value means `any`, matching `git diff --exit-code`,
whose convention this section already borrows. `--json` and `--markdown` are
mutually exclusive; supplying both is a usage error (exit 1) rather than a
silent precedence rule. Flags are hand parsed in the `--flag=value` style
`explain` and `policy` already use, not via `flag.FlagSet`, which the live
commands use but the static ones do not.

## The `explain --json` stability promise

`explain --json` currently writes a bare `[]StructInfo` array with no version
envelope. That was adequate while it was a human convenience. This command
promotes it to a compatibility surface, because `base.json` is typically a
stored CI artifact produced weeks earlier, possibly by an older `mamori` binary
than the one running the diff.

The envelope is **not** being added. Wrapping the array would break every
existing consumer of `explain --json` to solve a problem that has not occurred,
and the schema is a flat record of struct tags whose shape is driven by the tag
vocabulary itself, which only grows.

Instead:

- `diff` decodes both operands tolerantly: unknown object fields are ignored, so
  a newer binary's output remains readable by an older `diff`, and an older
  file remains readable by a newer one.
- A missing attribute decodes to its zero value and is compared as such. This is
  correct for every attribute in `Field`, whose zero values (`""`, `false`) each
  already mean "tag absent".
- `cli/explain.md` gains an explicit stability promise: fields may be added to
  this JSON, never removed and never retyped. That is a real commitment and is
  documented as one.
- `diff` rejects an operand that is not a JSON array at the top level with exit
  code 1 and a message naming the file, since the likeliest cause is a file
  produced by a different command.

## Testing

Golden-file table tests in `cmd/mamori`, matching the style of `explain_test.go`
and `policy_test.go`. Each case is a pair of fixture JSON files plus the
expected output for every format and the expected exit code. No `packages.Load`,
no network, no git, so the suite stays fast and hermetic.

Coverage includes: struct added and removed; field added and removed; each
attribute changing individually; `Sensitive` false to true (and true to false,
which is not specially called out); chain position added, removed, and reordered;
privilege growth, shrink, and both at once; every `--exit-code` mode against
each; both `--policy-format` renderings; an empty diff in all three formats;
stdin via `-`; and the three exit-code-1 failure modes (missing file, malformed
JSON, top-level non-array).

Determinism is tested directly: the same input pair rendered twice must produce
byte-identical output.

## Documentation

Per this project's standard that documentation ships with the feature:

- `site/src/pages/docs/cli/diff.md`, a new page covering the CI recipe, the
  three formats, the exit-code table, and the privilege delta.
- `site/src/layouts/DocsLayout.astro`, adding the `cli/diff` nav entry under
  Tooling, after `cli/policy`.
- `site/src/pages/docs/cli/explain.md`, adding the `--json` stability promise.
- `site/src/pages/docs/cli/index.md`, listing `diff` among the static commands.
- `cmd/mamori/help.go`, the command list and `diffUsage` text.
- Root `README.md`, in the CLI section, since the static/live split described
  there now has a fourth static command.
- `skills/mamori/SKILL.md`, so an agent knows the command exists and when to
  reach for it.
