---
layout: ../../../layouts/DocsLayout.astro
title: diff
---

# mamori diff

`mamori diff` compares two [`mamori explain --json`](/docs/cli/explain) outputs
and reports what changed about a service's configuration surface: fields added,
removed, and modified, precedence chains gained and lost, and the backend paths
the service newly reads or stops reading.

It is the only static command that does not read Go source. Both operands are
JSON files, so it needs no build, no module graph, and no network.

## Why

A pull request that adds one struct field can change which secrets a service
reads and which permissions its role needs to start. In review that is usually
invisible: the reviewer sees a one-line struct tag, and has to reconstruct the
consequence by hand. `mamori diff` states it outright.

## Usage

```
mamori diff <base.json> <head.json> [--json|--markdown]
            [--exit-code[=any|privilege]] [--policy-format=<f>]
```

| Flag | Meaning |
| --- | --- |
| `<base.json>` | The earlier explain output. `-` reads from stdin. |
| `<head.json>` | The later explain output. `-` reads from stdin. At most one operand may be `-`. |
| `--json` | Emit the whole diff as JSON. Mutually exclusive with `--markdown`. |
| `--markdown` | Emit markdown for a pull request comment or `$GITHUB_STEP_SUMMARY`. |
| `--exit-code` | Signal findings through the exit code. A bare `--exit-code` means `any`. |
| `--policy-format` | Render the concrete grant per changed path: `aws-iam`, `gcp`, or `external-secret`. |

## In CI

```bash
git checkout "$BASE" && mamori explain ./... --json > /tmp/base.json
git checkout "$HEAD" && mamori explain ./... --json > /tmp/head.json
mamori diff /tmp/base.json /tmp/head.json --markdown >> "$GITHUB_STEP_SUMMARY"
```

To block a merge that grows the permission surface:

```bash
mamori diff /tmp/base.json /tmp/head.json --exit-code=privilege
```

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success. Also the result when findings exist but `--exit-code` was not given: showing a diff is not a failure. |
| 1 | Usage error, unreadable operand, or JSON that is not an explain output. |
| 2 | Findings present, and `--exit-code` asked for them to be signalled. |

`--exit-code=any` signals on any change. `--exit-code=privilege` signals only
when the permission surface **grew**: a surface that only shrinks is not a
finding, because losing access is not a risk worth gating a merge on.

## What counts as a change

Structs match on package and type name, fields on their dotted path. A matched
field is compared attribute by attribute (`GoType`, `Default`, `Optional`,
`Sensitive`, `OnFail`, `Validate`), and each difference is reported old to new.

Two cases are called out specially rather than listed as ordinary attribute
changes:

- **A field becoming sensitive.** When `Sensitive` goes from false to true, the
  service began reading secret material where it previously read plain
  configuration. That gets its own marker in every output format.
- **A precedence chain gaining or losing a position.** `env:PORT` becoming
  `env:PORT,aws-ps://svc/port` is reported as a ref-level addition, not as one
  opaque string edit, because it means the service acquired a new backend
  dependency. Reordering is reported too, since chain order is precedence.

## Privilege delta

The privilege section lists the backend paths gained and lost, bucketed by
scheme. By default it is scheme neutral, which works for every provider:

```
privilege delta
  + aws-ps  svc/port
  + aws-sm  prod/stripe
  - aws-sm  prod/legacy
```

`--policy-format` additionally renders each path as the concrete grant that
[`mamori policy`](/docs/cli/policy) would emit for it, for the same fixture
pair as above:

```
privilege delta
  + aws-ps  svc/port  ssm:GetParameter on arn:aws:ssm:*:*:parameter/svc/port
  + aws-sm  prod/stripe  secretsmanager:GetSecretValue on arn:aws:secretsmanager:*:*:secret:prod/stripe
  - aws-sm  prod/legacy  secretsmanager:GetSecretValue on arn:aws:secretsmanager:*:*:secret:prod/legacy
```

A scheme the chosen format has no vocabulary for (`vault://`, `k8s-secret://`,
and most others) still gets its neutral line. Nothing is dropped from the
report just because no IAM equivalent exists for it.

## Determinism

Output ordering is total: structs by package then type name, fields by path,
attributes in a fixed order, scheme buckets and paths sorted. The same input
pair always renders byte for byte identically, so a diff pasted into a pull
request comment does not churn between runs.
