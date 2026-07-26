---
layout: ../../layouts/DocsLayout.astro
title: Quick start
---

# Quick start

From zero to typed, validated, hot-reloading config in a few minutes. This walks through installing mamori, loading a config struct once, then upgrading to a live watch.

## 1. Install

The core module ships the `env:` and `file://` providers with zero cloud-SDK dependencies:

```bash
go get github.com/xavidop/mamori
```

Requires Go 1.26 or newer. Each backend (AWS, Vault, GCP, ...) is a separate module you add only if you use it:

```bash
go get github.com/xavidop/mamori/providers/aws   # aws-sm://  aws-ps://
```

## 2. Describe your config

Tag each field with a `source:` ref. Use `secret.String` for secret values so they redact everywhere except an explicit `Reveal()`, and add `validate:` rules to reject bad config at load time:

```go
package main

import (
	"context"
	"log"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/secret"
)

type Config struct {
	LogLevel string        `source:"env:LOG_LEVEL" default:"info" validate:"oneof=debug info warn error"`
	Workers  int           `source:"env:WORKERS" default:"4" validate:"gte=1,lte=256"`
	DBURL    secret.String `source:"env:DATABASE_URL" validate:"required"`
}
```

## 3. Load it once

`Load` resolves every field, validates the struct, and returns it. If anything fails to resolve or validate, it returns an error and no partial struct:

```go
func main() {
	cfg, err := mamori.Load[Config](context.Background())
	if err != nil {
		log.Fatal(err) // e.g. DATABASE_URL missing, or WORKERS out of range
	}
	log.Printf("level=%s workers=%d db=%s", cfg.LogLevel, cfg.Workers, cfg.DBURL)
	// db prints as [REDACTED]; cfg.DBURL.Reveal() returns the real value.
}
```

Run it:

```bash
LOG_LEVEL=debug WORKERS=8 DATABASE_URL=postgres://... go run .
```

## 4. Add a backend provider

Providers register with the `database/sql` blank-import pattern. Import one, then point a field at its scheme:

```go
import _ "github.com/xavidop/mamori/providers/aws" // registers aws-sm:// and aws-ps://

type Config struct {
	DBPassword secret.String `source:"aws-sm://prod/db#password"`
}
```

See the [providers overview](/docs/providers/) for every scheme and how to authenticate it.

## 5. Watch for changes

Swap `Load` for `Watch` to keep the struct reconciled while your program runs. `Get` always returns the latest fully-valid snapshot, and `OnChange` fires when a value changes:

```go
w, err := mamori.Watch[Config](context.Background(),
	mamori.OnChange(func(ev mamori.Change[Config]) {
		if ev.Changed("Workers") {
			pool.Resize(ev.New.Workers)
		}
	}),
)
if err != nil {
	log.Fatal(err)
}
defer w.Close()

cfg := w.Get() // current snapshot, safe to call anytime
```

Rotate a secret in the backend and your process picks it up, with no restart and no half-applied config ever observed.

## A runnable example

A complete program that loads from `env:` and `file://`, watches, and reacts to a live file rotation lives in [`examples/basic`](https://github.com/xavidop/mamori/tree/main/examples/basic):

```bash
LOG_LEVEL=debug WORKERS=8 go run ./examples/basic
```

## Where to go next

- [Loading and watching](/docs/usage/) - `Load` vs `Watch`, change events, and error handling.
- [Concepts](/docs/concepts/) - refs, the reconciler, source chains, and error kinds.
- [Validation](/docs/validation/) - the full `validate:` rule set.
- [Providers overview](/docs/providers/) - every backend and its watch strategy.
- [Observability](/docs/observability/) - `Status`, `Health`, and the pre-deploy `Doctor` check.
