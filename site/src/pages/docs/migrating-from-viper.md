---
layout: ../../layouts/DocsLayout.astro
title: Migrating from Viper
---

# Migrating from Viper

If your service already runs on [Viper](https://github.com/spf13/viper), with `SetConfigFile`, `AutomaticEnv`, `BindPFlag`, and years of accumulated precedence, mamori does not ask you to rip that out before you can use it. The [`viper://`](/docs/providers/viper/) provider resolves a ref by reading whatever your existing Viper instance already decided, so a mamori config struct can point at `viper://` refs today and behave exactly as it did before mamori was in the loop. There is no flag day, and no need to reimplement Viper's own precedence chain in mamori struct tags.

This page is the path, not the pitch: for the case for making the move at all, see [Why mamori](/docs/comparison/). Here the point is narrower and more concrete: adoption is incremental, field by field, at whatever pace fits, and some fields may reasonably never move.

## 1. Keep Viper as it is

Nothing changes yet. Viper stays the single source of truth for every key it already resolves, consulting its own precedence in order: explicit `Set` calls, then flags, then the environment, then the config file, then key/value stores, then defaults. Whatever wins under that chain today keeps winning tomorrow.

```go
v := viper.New()
v.SetConfigFile("./config.yaml")
v.AutomaticEnv()
_ = v.BindPFlag("server.port", cmd.Flags().Lookup("port"))
_ = v.ReadInConfig()
```

mamori has not been introduced yet. This is exactly what the application already does.

## 2. Declare a config struct pointing at viper:// refs

Import the provider and point a mamori struct at the same keys Viper already resolves:

```go
import (
	"context"

	"github.com/xavidop/mamori"
	_ "github.com/xavidop/mamori/providers/viper"
)

type Config struct {
	Port       int    `source:"viper://server.port"`
	LogLevel   string `source:"viper://logging.level" default:"info"`
	DBPassword string `source:"viper://db.password"`
}

cfg, err := mamori.Load[Config](context.Background())
```

Behavior is unchanged, because Viper is still doing all the resolving: a `viper://` ref returns the winner of Viper's precedence chain, not one particular layer, and a key whose only source is `SetDefault` still resolves rather than reporting not-found. With no options, the provider reads Viper's **global** instance, the one `SetConfigFile`, `AutomaticEnv`, and `BindPFlag` already populate, so this works with no extra wiring. An application that keeps its own `*viper.Viper` instance injects it explicitly with `viperprov.WithViper`.

At this point mamori has only added typing and validation on top of Viper's existing decisions. Nothing about which value wins has changed.

## 3. Move secrets first

This is the step with the most value, because secrets are what Viper handles worst: a `viper://` value is never marked sensitive, since Viper holds application configuration, not secrets. Move the field to a real secret manager and change its type to `secret.String`:

```go
import (
	"context"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/secret"
	_ "github.com/xavidop/mamori/providers/viper"
	_ "github.com/xavidop/mamori/providers/vault"
)

type Config struct {
	Port       int           `source:"viper://server.port"`
	LogLevel   string        `source:"viper://logging.level" default:"info"`
	DBPassword secret.String `source:"vault://secret/app#password"`
}

cfg, err := mamori.Load[Config](context.Background())
```

`DBPassword` no longer touches Viper at all; `aws-sm://` works the same way if that is your secret manager instead. That one field change buys three things immediately, none of which a `viper://` ref could ever provide: redaction everywhere the value would normally be rendered, in a log line, `fmt`, or a JSON dump (see [Secret types](/docs/concepts/secret-types/)); live rotation, so a credential changed in the backend reaches the running process with no restart; and the `PreApply` gate, so a rotated credential is proven to actually work before it becomes the config your application serves (see [Rotation safety](/docs/usage/rotation/)). The remaining fields keep resolving through Viper exactly as before.

## 4. Migrate the rest opportunistically

Move the rest field by field, as it becomes worth it. There is no requirement to finish, and no schedule to hit. A [source chain](/docs/concepts/source-chains/) lets a field try a new source first and fall back to the old `viper://` ref while you gain confidence, rather than cutting over all at once:

```go
type Config struct {
	Port       int           `source:"env:PORT,viper://server.port"`
	LogLevel   string        `source:"viper://logging.level" default:"info"`
	DBPassword secret.String `source:"vault://secret/app#password"`
}
```

`Port` now prefers a plain `env:` ref and only falls through to `viper://server.port` if `PORT` is unset, so the cutover is reversible right up until you delete the fallback. `LogLevel` is left untouched. Say this plainly: a field like a log level, read once and rarely changed, may never need to move at all, and leaving it on `viper://` is not a compromise, it is the honest outcome for a field with no rotation, no secrecy, and no validation need beyond what Viper already gives it.

## What does not carry over

Two things about Viper do not have a mamori equivalent, and a migrating team should know both before starting rather than discover them midway.

**Viper's runtime `Set` / `BindPFlag` mutation API has no mamori equivalent.** A mamori config struct is resolved from declared sources, not assembled imperatively by calling setters at runtime. If your application (or its tests) leans on `v.Set(...)` to override a value in-process, that pattern stays available on the underlying Viper instance for any field still behind a `viper://` ref, since mamori never touches your Viper instance itself. But once a field moves off `viper://` to a real source, its value comes from that source, not from a runtime `Set` call; changing it means changing the underlying source (the environment variable, the secret manager entry, the config file) and letting mamori's poller or watcher pick it up.

**`viper.WatchConfig()` becomes unsafe on an instance mamori is also polling.** Viper itself is not safe for concurrent read and write: its internal maps carry no mutex, so mamori's polling goroutine (which reads through `IsSet` and `Get`) races with a concurrent `Set`, `SetDefault`, or a reload triggered by your own `viper.WatchConfig()`. If your application currently relies on `viper.WatchConfig()` for file-based hot reload, that call must stop once you point a `viper://` ref at the same instance; the fix is to let mamori's own poller detect changes instead, or, if file-level change detection without polling is what you actually want, to use mamori's built-in [`file://`](/docs/providers/file/) provider, which watches a file natively via fsnotify with no such race.

Neither gap is fatal to the migration. Both just mean the fields that depend on runtime mutation or `WatchConfig()` are exactly the fields to migrate off `viper://` earliest, or to leave alone with eyes open.

## See also

- [Viper provider](/docs/providers/viper/) - the full `viper://` scheme reference.
- [Why mamori](/docs/comparison/) - the case for making the move.
- [Secret types](/docs/concepts/secret-types/) - redaction and `Reveal()`.
- [Rotation safety](/docs/usage/rotation/) - `PreApply` and safe credential rotation.
- [Source chains](/docs/concepts/source-chains/) - precedence across multiple refs on one field.
