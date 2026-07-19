---
layout: ../../layouts/DocsLayout.astro
title: Introduction
---

# Introduction

`mamori` (守り, "protection") loads configuration and secrets from heterogeneous sources into a typed, validated Go struct, and keeps that struct reconciled while your program runs.

Use the sidebar to navigate. New here? Read this page, then **Concepts**, then **Loading & watching**.

## How it works

The model has three moving parts:

1. **Refs.** Each struct field carries a `source` tag: a small URL-ish reference to a value in some provider (`aws-sm://prod/db#password`, `env:LOG_LEVEL`, `file:///etc/tls/tls.crt`).
2. **Providers.** A provider resolves a scheme (`aws-sm`, `vault`, `env`, ...) into a `Value`. Providers register with the `database/sql` pattern; the core module has zero cloud-SDK dependencies.
3. **The reconciler.** `Watch` resolves everything once (fail-fast), then watches each source: natively where the backend can push, by polling with jitter otherwise. On a change it re-validates the whole struct and, only if valid, atomically swaps it in and fires your callback.

The result: rotate a database password in Secrets Manager, and your connection pool rotates too, without a restart and without a half-applied config ever being observed.

## Install

The core module and the built-in `env:` / `file://` providers:

```bash
go get github.com/xavidop/mamori
```

Each cloud provider is a separate module, so a cloud SDK only enters your build if you use it:

```bash
go get github.com/xavidop/mamori/providers/aws     # aws-sm://  aws-ps://
go get github.com/xavidop/mamori/providers/vault   # vault://
go get github.com/xavidop/mamori/providers/k8s     # k8s-secret://  k8s-cm://
go get github.com/xavidop/mamori/providers/gcp     # gcp-sm://
go get github.com/xavidop/mamori/providers/azure   # azure-kv://
go get github.com/xavidop/mamori/providers/consul  # consul://
go get github.com/xavidop/mamori/providers/doppler # doppler://
go get github.com/xavidop/mamori/providers/onepassword # op://
go get github.com/xavidop/mamori/providers/sops    # sops://
```

Requires Go 1.26 or newer.

## Quickstart

Tag a struct, then load it. A blank import registers a provider (the `database/sql` pattern):

```go
package main

import (
	"context"
	"log"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/secret"
	_ "github.com/xavidop/mamori/providers/aws" // registers aws-sm:// and aws-ps://
)

type Config struct {
	DBPassword secret.String `source:"aws-sm://prod/db#password"`
	LogLevel   string        `source:"env:LOG_LEVEL" default:"info" validate:"oneof=debug info warn error"`
	Workers    int           `source:"env:WORKERS" default:"4" validate:"gte=1,lte=256"`
	TLSCert    []byte        `source:"file:///etc/tls/tls.crt"`
}

func main() {
	cfg, err := mamori.Load[Config](context.Background())
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("workers=%d level=%s password=%s", cfg.Workers, cfg.LogLevel, cfg.DBPassword)
	// password prints as [REDACTED]; cfg.DBPassword.Reveal() returns the value.
}
```

To react to changes instead of loading once, see **Loading & watching**.
