---
layout: ../../../layouts/DocsLayout.astro
title: Watching one ref
---

# Watching one ref

Sometimes you do not want a config struct. You want to watch **one value** and react when it changes: a TLS certificate, a rotating API token, a feature flag. `WatchRef` does exactly that and nothing else.

```go
func WatchRef(ctx context.Context, p Provider, ref Ref, opts ...Option) <-chan Update
```

You hand it a provider and a ref, and you get a channel of updates.

## A complete example

Watch one Vault secret and rotate a database pool whenever it changes:

```go
package main

import (
	"context"
	"log"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/vault"
)

func main() {
	ctx := context.Background()

	// 1. Build the provider yourself. See "Getting a provider" below:
	//    a blank import is NOT enough here.
	p := vault.New()

	// 2. Parse the ref string you would otherwise put in a source: tag.
	ref, err := mamori.ParseRef("vault://secret/data/app#db_password")
	if err != nil {
		log.Fatal(err)
	}

	// 3. Watch it. The first update arrives with the current value.
	for u := range mamori.WatchRef(ctx, p, ref) {
		if u.Err != nil {
			log.Printf("vault: %v", u.Err) // transient; the channel stays open
			continue
		}
		pool.Rotate(string(u.Value.Bytes))
		log.Printf("rotated to version %s", u.Value.Version)
	}
	// The loop exits only when ctx is cancelled and the channel closes.
}
```

The ref string is the same grammar you would write in a `source:` tag, so all of these are valid:

```go
mamori.ParseRef("vault://secret/data/app#db_password")
mamori.ParseRef("k8s-secret://default/tls-cert#tls.crt")
mamori.ParseRef("aws-sm://prod/stripe#key")
mamori.ParseRef("file:///etc/tls/tls.crt")
mamori.ParseRef("env:LOG_LEVEL")
```

See [Ref grammar](/docs/concepts/ref-grammar/) for the full syntax, including `?decode=` pipelines and JSON Pointer fragments.

## Getting a provider

This is the part that trips people up, so it is worth stating plainly.

Everywhere else in mamori you register a provider with a blank import and the library finds it by scheme:

```go
import _ "github.com/xavidop/mamori/providers/vault" // works for Load and Watch[T]
```

**That is not enough for `WatchRef`.** `WatchRef` takes a `Provider` value as an argument, and mamori's scheme lookup is internal, so there is no exported way to ask "give me the provider for `vault://`". You construct the one you need directly:

```go
import "github.com/xavidop/mamori/providers/vault"

p := vault.New() // or vault.New(vault.WithClient(myClient)), etc.
```

Each provider package exports its own constructor and options; see that provider's page for what it takes. If you want scheme-based dispatch across many providers without wiring them yourself, that is what [`Load` and `Watch[T]`](/docs/usage/watching/) are for.

## What you give up versus Watch[T]

`WatchRef` is deliberately the low-level primitive. Compared to [`Watch[T]`](/docs/usage/watching/) you lose:

- **Validation.** No struct means no `validate:` tags. Nothing checks what arrives.
- **The atomic swap and last-good snapshot.** You get raw updates, including error updates, and decide yourself what each one means. There is no `Get()` still serving the last good value while you work that out.
- **The `PreApply` gate.** Nothing stands between an update and your code acting on it, so a rotated credential that does not work reaches you anyway.
- **`Change` diffing.** An `Update` is one value or one error. There is no `Changed("Field")`.
- **`Status()` and `Health()`.** There is no watcher object, so nothing reports readiness or staleness.
- **History and pinning.** No snapshot store, so nothing to pin or roll back to.

If you want any of those, use `Watch[T]` with a one-field struct. That is usually the better answer:

```go
type Config struct {
    DBPassword secret.String `source:"vault://secret/data/app#db_password"`
}
```

Reach for `WatchRef` when you are building infrastructure rather than an application: mamori's own [config server](/docs/server/) uses it to watch each binding, because it never has a struct to fill in the first place.

## Which options apply

`WatchRef` accepts the same `Option` values as `Load` and `Watch`, so you do not need a second vocabulary. Only four actually do anything here, because only the polling loop reads them:

| Option | Effect on `WatchRef` |
| --- | --- |
| [`WithPollInterval`](/docs/usage/options/) | How often a polled ref is re-read (default 30s) |
| [`WithJitter`](/docs/usage/options/) | Randomizes that interval (default 0.2) |
| [`WithBackoff`](/docs/usage/options/) | Backs off after repeated resolve failures (off by default) |
| [`WithClock`](/docs/usage/options/) | Replaces the clock, for tests |

Everything else, including `WithValidator` and `PreApply`, is accepted without error and does nothing, because there is no validation step or gate here for it to attach to.

## The channel contract

Three rules, and they matter because there is no `Get()` to fall back on:

1. **The first update carries the current value**, so you do not need a separate initial read.
2. **An error is an update, not a termination.** A transient failure arrives as an `Update` with a non-nil `Err`, and the channel stays open. Log it and keep looping; the next successful resolve arrives on the same channel.
3. **Closure means done.** The channel closes only when `ctx` is cancelled. That is the signal to stop, not an error value.

```go
for u := range mamori.WatchRef(ctx, p, ref) {
	if u.Err != nil {
		continue // transient, keep going
	}
	use(u.Value.Bytes)
}
// Only reached after ctx is cancelled.
```

## Native watch versus polling

You do not choose this, and you do not need to: `WatchRef` picks the best available method for whichever provider you passed, exactly as `Watch[T]` does for every field.

Providers with a real change-notification API (Kubernetes, Consul, etcd, Redis, Postgres, MongoDB, Firestore, LaunchDarkly) are watched natively, so a change reaches you as soon as the backend publishes it. Everything else (AWS, GCP, Azure, Vault, and the rest) is polled on the interval above. If a native watch cannot start, `WatchRef` falls back to polling rather than failing.

The **Watch** column in the [provider table](/docs/providers/) tells you which you get.
