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
	DBPassword secret.String `source:"aws-sm://${ENV}/db#password"`
	// #/credentials/user is an RFC 6901 JSON Pointer fragment, selecting a
	// value nested inside the secret's JSON payload rather than a top-level key.
	// It is a secret.String, not a plain string, because anything drawn from a
	// secret-bearing scheme should stay redacted - `mamori vet` enforces this.
	DBUser     secret.String `source:"aws-sm://${ENV}/db#/credentials/user"`
	// ?decode=base64 declares the stored value is base64; core decodes it
	// back to raw bytes before TLSKey is populated.
	TLSKey     secret.Bytes  `source:"aws-sm://prod/tls#key?decode=base64"`
}

func main() {
	ctx := context.Background()

	cfg, err := mamori.Load[Config](ctx,
		// ${ENV} above is expanded from this map, never from the ambient
		// environment - see Ref interpolation below.
		mamori.WithRefVars(map[string]string{"ENV": "prod"}),
	)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("listening on :%s", cfg.Port)
	pool.Connect(cfg.DBPassword.Reveal())
}
```

One call resolves every ref, applies defaults, validates, and returns a fully typed `Config`.

`${ENV}` in `DBPassword` and `DBUser` is [ref interpolation](/docs/concepts/ref-interpolation/): it is expanded from the map passed to `mamori.WithRefVars`, never from the process environment, before the tag is parsed. An undefined variable, such as forgetting to pass `ENV`, is a hard error rather than a ref silently missing a path segment.

`TLSKey`'s `?decode=base64` is a ref query option, not a struct tag: it tells core the resolved value is encoded, so it is decoded (left to right, outermost wrapper first for a comma-separated list like `?decode=base64,gzip`) before the field is populated. A field's `default:` value is exempt - it is used as-is, undecoded, since it is a literal you wrote rather than an encoded payload from a backend. See [Ref grammar](/docs/concepts/decoding/) for the full coding table and failure semantics.

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
- [Rotation safety](/docs/usage/rotation/) - `PreApply`, a gate that proves a rotated credential works before it becomes current.

## See also

- [Concepts](/docs/concepts/) for refs, the tag grammar, and error kinds.
- [Ref grammar](/docs/concepts/ref-grammar/) for the full `#key` fragment grammar, including nested JSON Pointer selection, `?decode=` value transforms, and `${VAR}` interpolation.
- [Validation](/docs/validation/) for the defaults and validation rules applied on every load.
- [Observability](/docs/observability/) for `Status`, `Health`, `Doctor`, and the read-only HTTP surface.
