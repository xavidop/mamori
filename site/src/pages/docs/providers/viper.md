---
layout: ../../../layouts/DocsLayout.astro
title: Viper provider
---

# Viper

Load whatever [Viper](https://github.com/spf13/viper) resolved for a key as config. This is an incremental-adoption provider: a team with an existing Viper setup moves fields into a typed, validated mamori struct one at a time, without reimplementing Viper's precedence in struct tags. See [Migrating an existing Viper setup](#migrating-an-existing-viper-setup) below for the walkthrough, including which fields to move first and what does not carry over.

| | |
| --- | --- |
| Scheme | `viper://` |
| Module | `github.com/xavidop/mamori/providers/viper` |
| Sensitive | no |
| Watch | poll |
| Auth | none - reads whatever your application's own Viper instance already resolved |

## Install

```bash
go get github.com/xavidop/mamori/providers/viper
```

```go
import _ "github.com/xavidop/mamori/providers/viper"
```

## Using the ref

A `viper://` ref points at one Viper key, in its usual dotted form.

```text
viper://<key>[#json-key]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<key>` | yes | A Viper key, passed through verbatim (never split), so an instance with a non-default key delimiter works unchanged. |
| `#json-key` | no | Select one field out of a table-valued key (via `mamori.SelectKey`). |

```go
type Config struct {
	Port     int    `source:"viper://server.port"`
	LogLevel string `source:"viper://logging.level" default:"info"`
}
```

## Which Viper instance

With no options, `viper.New()` resolves against Viper's global instance - the one your application's own `SetConfigFile`, `AutomaticEnv`, and `BindPFlag` calls already populate. An application that already calls `viper.ReadInConfig` gets working `viper://` refs with no wiring. Inject an explicit instance with `WithViper`, for an application that keeps its own `*viper.Viper` or in tests:

```go
import viperprov "github.com/xavidop/mamori/providers/viper"

mamori.WithProvider(viperprov.New(viperprov.WithViper(myViper)))
```

## Concurrency: do not mutate a Viper instance mamori is polling

**Viper v1.21.0 itself is not safe for concurrent read and write.** Its internal `config`/`override`/`defaults` maps carry no mutex of their own, so mamori's background polling goroutine calling `Resolve` (which reads through `IsSet` and `Get`) races with any concurrent `Set`, `SetDefault`, or a reload triggered by your application's own `viper.WatchConfig()` - confirmed under `go test -race`.

This provider adds no locking of its own: the writes happen in your application code, which this package never sees, so nothing here could serialize them correctly. **Do not call `viper.WatchConfig()` (or otherwise mutate the instance from another goroutine) on a `*viper.Viper` that mamori is polling.** Let mamori's own poller detect changes instead - it only ever reads. If file-level change detection is what you actually want, mamori's built-in [`file://`](/docs/providers/file/) provider already watches a file natively via fsnotify, with no such race.

## Precedence is inherited, not reimplemented

Viper resolves a key by consulting explicit `Set` calls, then flags, then the environment, then the config file, then key/value stores, then defaults, and returns the winner. A `viper://` ref returns **that winner** - not one particular layer. This is the entire point of the provider: it lets a large existing Viper setup adopt mamori one field at a time.

A key whose only source is `SetDefault` still resolves rather than reporting not-found: Viper's own `IsSet` reports `true` for it, and this provider inherits that deliberately. A Viper default is a real configured value; treating it as missing would silently substitute mamori's `default:` tag for Viper's, changing the value while looking like a lookup.

## Migrating an existing Viper setup

There is no flag day. Leave your Viper wiring exactly as it is, point a mamori struct at the same keys, and move fields off `viper://` one at a time.

```go
type Config struct {
	Port       int    `source:"viper://server.port"`
	LogLevel   string `source:"viper://logging.level" default:"info"`
	DBPassword string `source:"viper://db.password"`
}
```

Nothing about which value wins has changed: each ref returns the winner of Viper's own precedence chain. All you have added so far is typing and validation.

**Move secrets first.** That is what Viper handles worst, since a `viper://` value is never marked sensitive. Repointing one field at a real secret manager and changing its type buys three things a `viper://` ref cannot give you: redaction in logs, `fmt`, and JSON ([Secret types](/docs/concepts/secret-types/)); live rotation with no restart; and the `PreApply` gate, which proves a rotated credential works before it becomes the config you serve ([Rotation safety](/docs/usage/rotation/)).

```go
	DBPassword secret.String `source:"vault://secret/app#password"`
```

**Then migrate opportunistically.** A [source chain](/docs/concepts/source-chains/) makes each cutover reversible: the field prefers the new source and falls back to the old `viper://` ref until you delete the fallback.

```go
	Port int `source:"env:PORT,viper://server.port"`
```

There is no requirement to finish. A field like a log level, read once and rarely changed, may never need to move, and leaving it on `viper://` is the honest outcome rather than a compromise.

### What does not carry over

**Viper's runtime `Set` / `BindPFlag` mutation has no mamori equivalent.** A mamori struct is resolved from declared sources, not assembled by calling setters. That pattern still works on your Viper instance for any field still behind a `viper://` ref, but once a field moves to a real source, changing its value means changing that source and letting mamori pick it up.

**`viper.WatchConfig()` becomes unsafe** on an instance mamori is polling, for the reason described in [Concurrency](#concurrency-do-not-mutate-a-viper-instance-mamori-is-polling) above.

Neither is fatal. Both just identify which fields to migrate off `viper://` earliest, or to consciously leave on it.

## Value rendering

| Viper value | Resolved bytes |
| --- | --- |
| string | passed through unchanged, e.g. `info`, not `"info"` |
| bool | `true` / `false` |
| int / int32 / int64 / uint / uint64 (narrower widths too, via the JSON fallback) | decimal form |
| float32 / float64 | plain decimal (`'f'`, not `'g'` - see below) |
| time.Duration | `Duration.String()`, e.g. `30s` |
| time.Time | RFC 3339, e.g. `2026-07-29T00:00:00Z` |
| map / slice / struct | JSON-encoded, e.g. `{"port":5432}` |

The string case matters: `viper://logging.level` yields `info`, not `"info"`. JSON-encoding a string would leave quotes in a `string` field and in every comparison against it. Everything that is not a plain scalar becomes JSON, which is also what a `#json-key` fragment selects against.

Three cases exist because the JSON fallback silently corrupts them, each confirmed against a real config file:

- **Floats use `'f'`, not `'g'`.** Viper's JSON decoding stores every number as float64. `'g'` switches to exponent notation once the exponent reaches 6, so an ordinary value like `10485760` (10MiB) would render as `"1.048576e+07"`, which mamori's own int decode (`strconv.ParseInt`) rejects.
- **`time.Duration` renders as `Duration.String()`, not a nanosecond count.** `v.SetDefault("timeout", 30*time.Second)` is canonical Viper wiring; falling through to JSON would render `30000000000`, which `time.ParseDuration` rejects for missing a unit.
- **`time.Time` renders as RFC 3339, not a quoted string.** `gopkg.in/yaml.v3` decodes a bare YAML timestamp into a `time.Time`, so an ordinary YAML config takes this path, not the string case above; falling through to JSON would wrap it in quotes.

## Not found

A key with no value from any source (no `Set`, no environment variable, nothing in the config file, no key/value store entry, no `SetDefault`, and no pflag that was actually changed) resolves to `mamori.ErrNotFound`.

**A `SetDefault` value and an unset bound pflag are not the same, even though `Get` returns a default value in both cases.** Viper's own `IsSet` - which this provider inherits rather than reimplements - only counts the former: a `SetDefault` default is "set", but an unset pflag counts only once it has actually changed (`pflag.Flag.HasChanged()`). Resolving a key backed only by an unset pflag reports not-found, and mamori's own `default:` struct tag applies from there, if one is set.

## Sensitive

Values are never marked `Sensitive`: Viper holds application configuration, not secrets. Move secret material to a real secret store (see the other providers in this catalog) rather than relying on this provider to treat Viper's contents as secret.

## Error classification

Viper's read API has no error return anywhere - `Get(key) any` and `IsSet(key) bool` - so not-found is the only failure this provider can report. There is no permission, rate-limit, unavailability, or malformed-response case to inject: the data is already in memory by the time mamori asks. A config-load failure happens earlier, inside your application's own `ReadInConfig` call. This provider is conformance-exempt from the `ErrorClassification` case via `providertest.Config.NoResolveErrors`.

## Watch

A read here is an in-memory map lookup with no cost for mamori's poller to avoid, so this provider is **not watchable**: mamori polls it (`WithPollInterval` + jitter).

## Configuration

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

Verified against a real `*viper.Viper`, including real YAML parsed with `ReadConfig` for the cases that only actually arise that way (a `time.Time` from a YAML timestamp, a config-file value outranking a default, an env binding outranking a config file): every rendered kind (including a float large enough to require `'f'` over `'g'`, and both the typed and string forms of `time.Duration`), not-found (including the unset-bound-pflag case above), the `SetDefault`-counts-as-set behavior, precedence across every adjacent pair in Viper's chain, `#json-key` selection, and the full `providertest` conformance suite.
