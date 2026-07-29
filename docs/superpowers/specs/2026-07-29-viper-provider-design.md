# Viper provider design

**Status:** approved
**Date:** 2026-07-29

Adds Viper as a mamori provider module, `providers/viper`, registering:

```
viper://<key>[#json-key]
```

## What this is for

Viper is the most widely used configuration library in Go, and the realistic
obstacle to adopting mamori is not disagreement about the model, it is that a
team already has a large working Viper setup: a config file, environment
bindings, command-line flags, defaults, and the precedence rules between them.
Rewriting that in one step to try mamori is not a reasonable ask.

This provider makes adoption incremental. Viper stays the source of truth for
everything it already resolves, and a mamori struct reads from it by ref, so
new fields can move to mamori one at a time and secret material can come from a
real secret store immediately:

```go
type Config struct {
    Port       int           `source:"viper://server.port"`
    LogLevel   string        `source:"viper://logging.level" default:"info"`
    DBPassword secret.String `source:"aws-sm://prod/db#password"`
}
```

The first two keep working exactly as they did under Viper, with its precedence
intact. The third is something Viper cannot do well, and it now lives in the
same validated struct.

## Why this is not what `env:`, `file://`, and `dotenv://` already do

Core already reads environment variables, files, and dotenv files, so the
obvious objection is that Viper adds nothing.

It adds the one thing those cannot: **layered precedence**. Viper resolves a
key by consulting explicit `Set` calls, then flags, then environment, then the
config file, then key/value stores, then defaults, and returns the winner. It
also parses YAML, TOML, JSON, HCL, INI, and Java properties behind one dotted
key syntax.

A `viper://server.port` ref means "whatever Viper decided `server.port` is,
after applying all of that". Reproducing it with `env:` plus `file://` plus a
chain of mamori defaults would mean reimplementing Viper's precedence, badly,
in every struct tag.

## The rejected direction

The other reading of "Viper shim" is a Viper-compatible API on top of mamori:
`mamori.GetString("server.port")`, `mamori.GetInt(...)`, so a Viper user could
change an import and keep their call sites.

That is rejected. mamori's entire proposition is that configuration is a typed,
validated struct resolved once and kept reconciled, and that a field's type and
constraints are checked before the process trusts it. A `GetString(key)` API
reintroduces exactly what that model exists to remove: untyped access by
stringly-typed key, no validation, no place to hang a `validate:` rule, and a
missing key indistinguishable from an empty one. It would make mamori worse at
its own job in order to look like something else.

Reading *from* Viper costs nothing and takes nothing away. Impersonating Viper
would.

## Ref grammar

```
viper://<key>[#json-key]
```

`<key>` is a Viper key in its usual dotted form (`server.port`,
`logging.level`). Viper's own delimiter is respected, so a Viper instance
configured with a different key delimiter works unchanged; the provider passes
the key through verbatim and never splits it.

`#json-key` selects a field out of a value that renders as a JSON object,
through `mamori.SelectKey`, so both literal keys and RFC 6901 JSON Pointers
work and `providertest.Config.PointerRef` applies. It is most useful when a
Viper key holds a whole nested table.

## Which Viper instance

The provider resolves against `*viper.Viper`.

By default it uses Viper's global instance, the one populated by the
package-level `viper.SetConfigFile`, `viper.AutomaticEnv`, `viper.BindPFlag`,
and friends, which is what the overwhelming majority of Viper codebases use.
That makes the zero-config path work with no wiring at all: an application that
already calls `viper.ReadInConfig()` gets working `viper://` refs immediately.

`WithViper(v *viper.Viper)` injects an explicit instance, for applications that
keep their own rather than using the global, and for tests.

Construction performs no I/O and cannot fail, so `init()` registration is safe
even before Viper has read anything.

## Rendering and versioning

`viper.Get` returns `any`. The provider renders it as text, matching how every
other provider hands core a byte slice for decoding into the field's Go type:

- a string is passed through unchanged
- a bool, integer, or float is formatted as its plain text form
- anything else, including maps and slices, is rendered as JSON, which is also
  what `#json-key` selects against

`Value.Version` is `mamori.VersionHash(data)`. Viper has no revision concept of
any kind, so there is nothing more meaningful available.

`Value.Sensitive` is `false`. Viper holds application configuration. A team
migrating incrementally should move secret material to a real secret store
rather than have this provider pretend Viper's contents are secret, and the
module README says so directly.

## Not-found, and why there is nothing else to classify

A key Viper does not have is reported as an error satisfying
`errors.Is(err, mamori.ErrNotFound)`, so mamori's `default:` and `optional`
handling behave normally.

Beyond that there is nothing to classify, and this is a deliberate, documented
exemption rather than an omission. Viper's read API has **no error return
anywhere**: `Get(key) any` and `IsSet(key) bool`. There is no permission
failure, no rate limit, no unavailability, and no malformed-response case to
map, because the data is already in memory by the time mamori asks. Any failure
to load a config file happened earlier, inside the application's own
`ReadInConfig()` call, and is that application's to handle.

This is exactly the case `CONTRIBUTING.md` describes for
`providertest.Config.NoResolveErrors`: "a backend with no per-key error surface
at all (existence is a bool or a sentinel value, nothing to inject)". The
provider therefore sets `NoResolveErrors: true`, with a comment naming why, and
ships no `## Error classification` section, since it classifies nothing beyond
not-found.

A subtlety worth pinning down: Viper's `IsSet` reports true for a key that
exists only because of `SetDefault`. That is correct behaviour for this
provider to inherit. A Viper default is a real, deliberately configured value,
and a team migrating incrementally will have keys whose only source is a
default. Treating those as missing would silently swap Viper's default for
mamori's, which is a value change disguised as a lookup.

## No Watch

mamori polls this provider. Viper does expose `WatchConfig`, but it watches the
config *file* and mamori's own `file://` provider already watches files
natively; more importantly a poll here is an in-memory map read, so the cost
polling is meant to avoid does not exist. All eight sibling flag providers are
polled for the same reason.

## Testing

The provider is tested against a real `*viper.Viper` rather than a fake. Viper
is a pure in-memory library with no network, no clock, and no I/O once loaded,
so the real thing is both simpler and a stronger test than a hand-written
double that could drift from Viper's actual precedence behaviour.

It passes `providertest.Run` with `Seed`, `Mutate`, and `PointerRef` supplied,
and `NoResolveErrors: true` in place of `Fail`/`Clear` for the reason above.

Dedicated tests beyond the kit:

- Each rendering case: string, bool, int, float, and a nested table as JSON.
- A key present only via `SetDefault` resolves rather than reporting not-found.
- Precedence: a key set both in a config source and by `Set` returns the
  winner Viper picks, which is the whole reason this provider exists.

## Documentation

Module `README.md`, a docs-site page, a row in both coverage tables, a sidebar
entry, and the `skills/` provider reference. No error-classification section,
with a sentence saying why so its absence reads as a decision rather than an
oversight. The README leads with the incremental-migration story, since that is
the only reason to reach for this provider.
