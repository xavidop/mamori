---
layout: ../../../layouts/DocsLayout.astro
title: Viper provider
---

# Viper

Load whatever [Viper](https://github.com/spf13/viper) resolved for a key as config. This is an incremental-adoption provider: a team with an existing Viper setup moves fields into a typed, validated mamori struct one at a time, without reimplementing Viper's precedence in struct tags.

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

## Precedence is inherited, not reimplemented

Viper resolves a key by consulting explicit `Set` calls, then flags, then the environment, then the config file, then key/value stores, then defaults, and returns the winner. A `viper://` ref returns **that winner** - not one particular layer. This is the entire point of the provider: it lets a large existing Viper setup adopt mamori one field at a time.

A key whose only source is `SetDefault` still resolves rather than reporting not-found: Viper's own `IsSet` reports `true` for it, and this provider inherits that deliberately. A Viper default is a real configured value; treating it as missing would silently substitute mamori's `default:` tag for Viper's, changing the value while looking like a lookup.

## Value rendering

| Viper value | Resolved bytes |
| --- | --- |
| string | passed through unchanged, e.g. `info`, not `"info"` |
| bool | `true` / `false` |
| int / int32 / int64 / uint / uint64 | decimal form |
| float32 / float64 | Go's canonical decimal form |
| map / slice / struct | JSON-encoded, e.g. `{"port":5432}` |

The string case matters: `viper://logging.level` yields `info`, not `"info"`. JSON-encoding a string would leave quotes in a `string` field and in every comparison against it. Everything that is not a plain scalar becomes JSON, which is also what a `#json-key` fragment selects against.

## Not found

A key with no value from any source resolves to `mamori.ErrNotFound`.

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

Verified against a real `*viper.Viper` (Viper is pure in-memory once loaded, so the real library is both simpler and a stronger test than a double that could drift from Viper's actual precedence rules): every rendered kind, not-found, the `SetDefault`-counts-as-set behavior, explicit-`Set`-outranks-`default` precedence, `#json-key` selection, and the full `providertest` conformance suite.
