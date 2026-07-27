---
layout: ../../../layouts/DocsLayout.astro
title: Loading and watching
---

# Loading and watching

mamori loads your typed config from refs (environment variables, secret managers, files, and more). Two entry points, both generic over your config type `T`: `Load` reads once, `Watch` stays reconciled and hands you diff-aware callbacks.

## Quick start

Define a config struct, tag each field with a `source:`, and `Load` it:

```go
import (
	"context"
	"log"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/secret"
)

type Config struct {
	Port       string        `source:"env:PORT" default:"8080"`
	DBPassword secret.String `source:"aws-sm://prod/db-password"`
	// #/credentials/user is an RFC 6901 JSON Pointer fragment, selecting a
	// value nested inside the secret's JSON payload rather than a top-level key.
	DBUser     string        `source:"aws-sm://prod/db-password#/credentials/user"`
}

func main() {
	ctx := context.Background()

	cfg, err := mamori.Load[Config](ctx)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("listening on :%s", cfg.Port)
	pool.Connect(cfg.DBPassword.Reveal())
}
```

One call resolves every ref, applies defaults, validates, and returns a fully typed `Config`.

## Load config once

`Load` resolves every ref once, applies defaults, validates, and returns the typed config. It fails fast: on any resolve or validation error it returns the zero value and a non-nil error, so you never get partial config.

```go
cfg, err := mamori.Load[Config](ctx, opts...)
```

Batch-capable providers (for example AWS Secrets Manager) are resolved in a single API call automatically.

## Next

- [Watch for changes](/docs/usage/watching/) - `Watch`, `Get`, `OnChange`, and `OnError`.
- [Source chains](/docs/concepts/source-chains/) - comma-separated precedence and the `onfail` policy.
- [Snapshots and pinning](/docs/usage/snapshots/) - `Status`, `WithHistory`, and `Pin` / `Unpin`.

## See also

- [Concepts](/docs/concepts/) for refs, the tag grammar, and error kinds.
- [Ref grammar](/docs/concepts/ref-grammar/) for the full `#key` fragment grammar, including nested JSON Pointer selection.
- [Validation](/docs/validation/) for the defaults and validation rules applied on every load.
- [Observability](/docs/observability/) for `Status`, `Health`, `Doctor`, and the read-only HTTP surface.
