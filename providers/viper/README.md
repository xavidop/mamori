# mamori Viper provider

`github.com/xavidop/mamori/providers/viper`

A [mamori](https://github.com/xavidop/mamori) provider for
**[Viper](https://github.com/spf13/viper)**, the configuration library. It
registers the `viper` scheme and resolves whatever Viper resolved for a key -
inheriting Viper's own precedence rather than reimplementing it in struct
tags.

![conformance](https://img.shields.io/badge/providertest-passing-brightgreen)

Passes the mamori provider conformance kit (`providertest.Run`) against a real
`*viper.Viper` instance. See [What is verified](#what-is-verified).

## Install

```bash
go get github.com/xavidop/mamori/providers/viper
```

Import for its side effect (the package `init()` registers the provider):

```go
import _ "github.com/xavidop/mamori/providers/viper"
```

The package identifier is `viper`.

## Why use this: incremental migration

This provider exists for teams with a large, already-working Viper setup who
want mamori's typed structs, validation, watching, and observability without
a rewrite. Move fields into a mamori struct one at a time:

```go
type Config struct {
    Port     int    `source:"viper://server.port"`
    LogLevel string `source:"viper://logging.level" default:"info"`
}
```

Every field you have not migrated yet keeps working exactly as it does today,
read straight off your existing `*viper.Viper`. There is no flag day, and no
need to reimplement Viper's own precedence rules (explicit `Set`, then flags,
then environment, then config file, then key/value store, then defaults) in
mamori struct tags: a `viper://` ref simply returns whatever Viper already
decided.

Secret material should move to a real secret manager as part of this
migration, not stay behind a `viper://` ref - see [Sensitivity](#sensitivity).

## Scheme

```
viper://<key>[#json-key]
```

| Part | Required | Meaning |
| --- | --- | --- |
| `<key>` | yes | A Viper key, in its usual dotted form (e.g. `server.port`). Passed to Viper verbatim and never split, so an instance configured with a non-default key delimiter works unchanged. |
| `#json-key` | no | Select one field out of a table-valued key with `mamori.SelectKey` (an RFC 6901 JSON Pointer such as `#/creds/port`, or a literal top-level key). |

## Which Viper instance

With no options, `New()` resolves against Viper's **global instance** - the
one your application's own package-level `viper.SetConfigFile`,
`viper.AutomaticEnv`, and `viper.BindPFlag` calls already populate. An
application that already calls `viper.ReadInConfig` gets working `viper://`
refs with no wiring at all.

Inject an explicit instance with `WithViper`, for an application that keeps
its own `*viper.Viper` (or in tests):

```go
import (
    spf "github.com/spf13/viper"
    viperprov "github.com/xavidop/mamori/providers/viper"
)

v := spf.New()
v.SetConfigFile("./config.yaml")
_ = v.ReadInConfig()

mamori.WithProvider(viperprov.New(viperprov.WithViper(v)))
```

`New` performs no I/O and never fails: it does not touch the config file or
any other source. Registering the default instance from `init()` (via a
blank import) is always safe, even before Viper has read anything.

## Precedence: the ref returns the winner, not a layer

Viper resolves a key by consulting explicit `Set` calls, then flags, then the
environment, then the config file, then key/value stores, then defaults, and
returns the winner. A `viper://` ref returns **that winner**. Given:

```go
v.SetDefault("server.port", 8080)
v.Set("server.port", 9090)
```

`viper://server.port` resolves to `9090`, the value Viper actually picked -
not the default, and not any other single layer. Reimplementing this
precedence in mamori struct tags would be redundant at best and wrong at
worst; the whole point of this provider is to inherit it instead.

### A `SetDefault`-only key resolves, not not-found

```go
v.SetDefault("only.default", "from-default")
```

`viper://only.default` resolves to `"from-default"` rather than reporting
`mamori.ErrNotFound`. This is deliberate: Viper's own `IsSet` reports `true`
for a key whose only source is `SetDefault`, and this provider inherits that
on purpose. A Viper default is a real configured value; treating it as
missing would silently substitute mamori's own `default:` tag for Viper's
value, changing the value under the guise of a lookup. A team migrating
incrementally will have keys that come only from a Viper default, and they
must keep resolving exactly as they do today.

## Value rendering

| Viper value | Resolved bytes |
| --- | --- |
| `string` | passed through **unchanged** |
| `bool` | `true` / `false` |
| `int`, `int32`, `int64`, `uint`, `uint64` | decimal form |
| `float32`, `float64` | Go's canonical decimal form (`strconv.FormatFloat(..., 'g', -1, ...)`) |
| `[]byte` | passed through unchanged |
| map, slice, struct, or anything else JSON-encodable | JSON, e.g. `{"port":5432}` |

The string case is deliberate and load-bearing: `viper://logging.level`
yields `info`, **not** `"info"`. JSON-encoding a string would leave quotes in
the resolved bytes, which would survive into a `string` field and into every
comparison made against it afterward. Everything that is not a plain scalar
becomes JSON, which is also what a `#json-key` fragment selects against.

## Not found

A key with no value from any Viper source (no `Set`, no bound flag, no
environment variable, nothing in the config file, no key/value store entry,
and no `SetDefault`) resolves to an error satisfying
`errors.Is(err, mamori.ErrNotFound)`.

## Sensitivity

Resolved values are **never** marked sensitive (`Value.Sensitive == false`):
Viper holds application configuration, not secrets. If a `viper://` key
currently carries secret material, move it to a real secret manager (see the
other providers in this catalog) as part of your migration, rather than
relying on this provider to treat Viper's contents as secret.

`Value.Version` is `mamori.VersionHash` of the resolved bytes: Viper has no
native revision concept to reuse, but a content hash still gives correct,
cheap change detection.

## Watch (polling, no native push)

This provider is **not watchable**. A read here is an in-memory map lookup -
there is no cost for a native watch to avoid, and nothing to subscribe to
either, since Viper itself has no push-style change notification. mamori
polls it on the configured interval (`WithPollInterval` + jitter) instead.

## Error classification

There is none, and that is by design rather than an oversight. Viper's read
API has **no error return anywhere**: `Get(key) any` and `IsSet(key) bool`.
There is no permission, rate-limit, unavailability, or malformed-response case
to inject, because the data is already sitting in memory by the time mamori
asks for it - any failure to load a config file, reach a key/value store, or
parse a value happened earlier, inside your application's own
`viper.ReadInConfig` (or equivalent) call, long before a `viper://` ref is
ever resolved. Not-found is therefore the only failure this provider can
report.

This provider is explicitly exempted from the `providertest` conformance
kit's `ErrorClassification` case via `providertest.Config.NoResolveErrors`,
the documented exemption in [`CONTRIBUTING.md`](../../CONTRIBUTING.md) for a
backend with no per-key error surface at all - not a shortcut.

## What is verified

Unit tests and the [`providertest`](../../providertest) conformance kit run
against a real `*viper.Viper` instance. Viper is pure in-memory once loaded,
with no I/O of its own to fake, so the real library is both simpler to test
against and a stronger test than a hand-rolled double that could drift from
Viper's actual precedence rules. Verified: scheme and registration, value
rendering for every kind (string, bool, int, float, table), not-found, the
`SetDefault`-counts-as-set behavior, explicit-`Set`-outranks-`default`
precedence, `#json-key` selection, context cancellation, concurrency,
goroutine hygiene, and the full `providertest.Run` conformance suite.

## Development

```bash
cd providers/viper
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
