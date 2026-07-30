---
layout: ../../../layouts/DocsLayout.astro
title: Ref interpolation
---

# Ref interpolation

`${VAR}` in a `source` tag is substituted from variables you pass in, so one
struct can serve several environments or regions.

```go
type Config struct {
	DBPassword secret.String `source:"aws-sm://${ENV}/db#password"`
	Bucket     string        `source:"s3://backups-${REGION}/nightly"`
}

cfg, err := mamori.Load[Config](ctx,
	mamori.WithRefVars(map[string]string{"ENV": "prod", "REGION": "eu-west-1"}),
)
```

Only the braced form is substituted. A bare `$VAR` passes through untouched,
so a password or an `exec:` command containing a literal `$` is safe. `$$`
collapses to one `$`.

## Variables come only from `WithRefVars`

**mamori never reads the ambient environment for `${VAR}`.** This is the rule
to remember, because a ref decides *which secret a process reads*. If
expansion used `os.Getenv`, anything that could set an environment variable in
your process could redirect that read to a secret of its own choosing.

When a value genuinely should come from the environment, name it:

```go
mamori.WithRefVars(mamori.EnvVars("ENVIRONMENT", "REGION"))
```

`EnvVars` takes names on purpose, so what can influence a ref stays greppable
at the call site. A name that is not set is omitted rather than mapped to
`""`, so it still fails as undefined instead of expanding to nothing.

Calling `WithRefVars` more than once merges, with later calls winning per key.

## Where variables can appear

Expansion runs over the whole tag before it is parsed, so a variable can
supply a scheme, a path segment, a fragment, or an option value:

```go
Endpoint string `source:"${SCHEME}://prod/db#${KEY}?region=${REGION}"`
```

It is not recursive. A value containing `${B}` is inserted verbatim and never
rescanned, which rules out loops from self-referencing values.

## Errors

Three shapes fail at `Load`, `Watch`, or `Doctor` time rather than expanding
to nothing, all wrapping `ErrInvalid`:

| Situation | Example |
| --- | --- |
| Undefined variable | `${NOPE}` with no `NOPE` key |
| Unterminated | `${NOPE` with no `}` |
| Empty name | `${}` |

Silently expanding to nothing would give you `aws-sm:///db`, which resolves
not-found and quietly takes the field's `default:`. A forgotten `ENV` would
become a wrong value instead of an error.

## Keep delimiters out of variable values

A value containing `#`, `?`, or `,` **re-cuts the ref**, and the result is
usually a valid ref pointing somewhere else rather than an error:

```go
DBUser string `source:"aws-sm://${ENV}/db#/credentials/user"`
```

| `ENV` | What actually resolves |
| --- | --- |
| `prod` | path `prod/db`, pointer `/credentials/user`, as written |
| `prod?x` | path `prod`, **no fragment**, the rest swallowed into the query |
| `prod#y` | path `prod`, fragment is now the literal key `y/db#/credentials/user` |

The last two resolve not-found and fall through to `default:`. A comma can
similarly split one ref into a two-element
[precedence chain](/docs/concepts/source-chains/).

mamori does not escape variable values, because supplying a scheme or fragment
is an advertised use of `${VAR}` and no rule could tell an intended `?` from
an accidental one. The rule to hold instead: **variable values are
operator-supplied identifiers** such as environment names, regions, and
tenants. If one is ever assembled from something less trusted, validate it
there.

## Expanded refs are visible, so values must not be secrets

After expansion the ref shows up as resolved, variable values included, in
[`Status()`](/docs/observability/), the
[admin endpoint](/docs/observability/admin/), and
[`mamori doctor`](/docs/observability/doctor/).

Use `WithRefVars` for anything you would be comfortable seeing on a status
page. A secret belongs in the value a ref *resolves to*, never in the ref's
own text.

## One upgrade note

The scan runs whether or not you call `WithRefVars`, so `$$` collapses and a
stray `${` errors even with no variables supplied. A pre-existing tag
containing either, most plausibly an `exec:` command line, changes meaning.
Gating the scan on opt-in would mean a caller who simply forgot the option
gets `${ENV}` left literal, which resolves not-found and silently takes the
default, which is the failure this feature exists to prevent.

## See also

- [Ref grammar](/docs/concepts/ref-grammar/) - the tag shape variables expand into.
- [Source chains](/docs/concepts/source-chains/) - the comma-split rule a variable can trigger.
- [exec provider](/docs/providers/exec/) - opt-in for the same reason `WithRefVars` is.
