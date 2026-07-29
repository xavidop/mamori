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

## Concurrency: do not mutate a Viper instance mamori is polling

**Viper v1.21.0 itself is not safe for concurrent read and write.** Its
internal `config`/`override`/`defaults` maps carry no mutex of their own, so
mamori's background polling goroutine calling `Resolve` (which reads through
`IsSet` and `Get`) races with any concurrent `Set`, `SetDefault`, or a reload
triggered by your application's own `viper.WatchConfig()`. This was confirmed
under `go test -race`, with the write in Viper's `Set` and the read in
`Resolve`'s `IsSet`.

This provider adds no locking of its own to paper over that, deliberately:
the writes happen in your application code, which this package never sees,
so nothing here could serialize them correctly.

**Do not call `viper.WatchConfig()` (or otherwise mutate the instance from
another goroutine) on a `*viper.Viper` that mamori is polling through this
provider.** Let mamori's own poller detect changes instead - it already does
this safely, since polling only ever reads. If file-level change detection is
what you actually want, mamori's built-in `file://` provider already watches
a file natively via `fsnotify`, with no such race.

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
| `int`, `int32`, `int64`, `uint`, `uint64` (and the narrower widths, via the JSON fallback) | decimal form |
| `float32`, `float64` | plain decimal (`strconv.FormatFloat(..., 'f', -1, ...)`) |
| `time.Duration` | `Duration.String()`, e.g. `30s` |
| `time.Time` | RFC 3339, e.g. `2026-07-29T00:00:00Z` |
| `[]byte` | copied through unchanged |
| map, slice, struct, or anything else JSON-encodable | JSON, e.g. `{"port":5432}` |

The string case is deliberate and load-bearing: `viper://logging.level`
yields `info`, **not** `"info"`. JSON-encoding a string would leave quotes in
the resolved bytes, which would survive into a `string` field and into every
comparison made against it afterward. Everything that is not a plain scalar
becomes JSON, which is also what a `#json-key` fragment selects against.

Three cases exist specifically because the JSON fallback silently corrupts
them, each confirmed against a real config file:

- **Floats use `'f'`, not `'g'`.** Viper's JSON decoding stores every number
  as `float64`, including whole numbers like a byte size or a millisecond
  timeout. Go's `'g'` verb switches to exponent notation once the exponent
  reaches 6, so an entirely ordinary value like `10485760` (10MiB, a typical
  max-upload byte count) would render as `"1.048576e+07"`, which mamori's own
  int decode path (`strconv.ParseInt`) rejects outright. `'f'` always
  produces plain decimal digits, matching what a YAML/TOML source (which
  preserves `int`) already gives for the same value.
- **`time.Duration` renders as its own `String()` form, not a nanosecond
  count.** `v.SetDefault("timeout", 30*time.Second)` is canonical Viper
  wiring. `time.Duration`'s underlying type is `int64`, but a Go type switch
  matches the named type exactly, so falling through to `json.Marshal` (or
  even to the `int64` case) would render `30000000000` - a bare nanosecond
  count that `time.ParseDuration` rejects for missing a unit. Rendered as
  `30s` instead, it matches what a YAML file's `timeout: 30s` already gives
  as a plain string, so both paths decode.
- **`time.Time` renders as RFC 3339, not a quoted JSON string.**
  `gopkg.in/yaml.v3` decodes a bare YAML timestamp (e.g.
  `expires: 2026-07-29T00:00:00Z`) into a `time.Time` when unmarshaling into
  `any`, so an ordinary YAML config takes this path, not the string case
  above. Falling through to `json.Marshal` here would wrap it in quotes - the
  exact defect the string case exists to prevent, just arriving through a
  different Go type.

## Not found

A key with no value from any Viper source (no `Set`, no environment
variable, nothing in the config file, no key/value store entry, no
`SetDefault`, and no pflag that was actually changed) resolves to an error
satisfying `errors.Is(err, mamori.ErrNotFound)`.

### The `SetDefault` vs. unset-pflag asymmetry

This provider checks Viper's own `IsSet`, and `IsSet` treats two cases that
look similar very differently - a real asymmetry worth naming rather than
leaving a reader to discover it the hard way:

- **A `SetDefault` value counts as set.** `viper://only.default` resolves to
  the default (see above), because Viper's `IsSet` says so.
- **An unset, bound pflag does *not* count as set**, even though `Get`
  *also* returns a default in this case - the flag's zero/default value, not
  `nil`. `IsSet` only consults a bound flag once it has actually changed
  (`pflag.Flag.HasChanged()`); an unchanged flag makes `IsSet` report `false`
  regardless of what `Get` would return. Resolving such a key reports
  `mamori.ErrNotFound`, and mamori's own `default:` struct tag then applies,
  if one is set.

Both behaviors follow directly from inheriting Viper's own `IsSet` rather
than reimplementing it, which is this provider's whole design. The two cases
simply land on opposite sides of it, because Viper itself treats a
`SetDefault` default and an unbound pflag's default differently at the
`IsSet` layer, even though both are "a default" in the everyday sense.

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
against a real `*viper.Viper` instance, including real YAML parsed with
`ReadConfig` where the value under test only actually arises that way (a
`time.Time` from a YAML timestamp, precedence across a config file and an
env binding). Viper is pure in-memory once loaded, with no I/O of its own to
fake, so the real library is both simpler to test against and a stronger
test than a hand-rolled double that could drift from Viper's actual
precedence rules.

Verified: scheme and registration, value rendering for every kind (string,
bool, int, a float large enough to require `'f'` over `'g'`, `time.Duration`
in both its typed and string forms, `time.Time` decoded from real YAML,
table), not-found (including the unset-bound-pflag case, see the asymmetry
above), the `SetDefault`-counts-as-set behavior, precedence across every
adjacent pair in Viper's chain (`Set` over `SetDefault`, a config file over
a default, an env binding over a config file), `#json-key` selection,
context cancellation, concurrency (of this provider's own state; see
[Concurrency](#concurrency-do-not-mutate-a-viper-instance-mamori-is-polling)
for the limits of Viper's own instance), goroutine hygiene, and the full
`providertest.Run` conformance suite.

## Development

```bash
cd providers/viper
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
