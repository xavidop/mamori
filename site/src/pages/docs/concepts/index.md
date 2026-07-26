---
layout: ../../../layouts/DocsLayout.astro
title: Concepts
---

# Concepts

You annotate config struct fields with `source:` tags. mamori parses each tag into a **ref** (or a precedence chain of refs), a **provider** resolves each ref to a `Value`, and a **reconciler** applies validated results as monotonically versioned snapshots you can pin.

## The mental model

- **Ref** - a parsed pointer to a value in a provider, produced from a `source` tag by `ParseRef`. See the grammar below.
- **Provider** - resolves a ref to a `Value` (bytes plus metadata). One provider per scheme (`aws-sm`, `vault`, `env`, `file`, ...).
- **Reconciler** - the goroutine behind every `Watcher[T]`. It watches every ref, decodes and validates the result, and publishes a new snapshot on each accepted update. `Get`, `Status`, `Pin`, and `OnChange` all read what it publishes.

## The source ref grammar

A ref is produced from a `source` tag by `ParseRef`. The grammar is:

```text
<scheme>://<path>[#<key>][?<opt>=<v>&...]
```

Opaque schemes such as `env:` and `exec:` take everything after the colon as the path (no `//`):

```go
type Config struct {
	// whole secret string
	APIKey     secret.String `source:"aws-sm://prod/api-key"`
	// one key of a JSON secret
	DBPassword secret.String `source:"aws-sm://prod/db#password"`
	// provider option
	Leased     secret.String `source:"vault://kv/data/api#key?renew=true"`
	// opaque scheme
	LogLevel   string        `source:"env:LOG_LEVEL"`
	// absolute file path
	Cert       []byte        `source:"file:///etc/tls/tls.crt"`
}
```

`ParseRef` produces a `Ref{Scheme, Path, Key, Opts, Raw}`. `#key` selects one field from a structured (JSON) payload; `?opts` are provider-specific plus a small set of core-recognized options (`debounce`, `optional`, `version`).

### Supplementary tags

Other struct tags refine how a field resolves:

| Tag | Meaning |
| --- | --- |
| `default:"..."` | Value used when the ref resolves to not-found (never on error). |
| `validate:"..."` | Field validation (go-playground/validator syntax), evaluated on **every** update. See [Validation](/docs/validation/). |
| `flatten:"json\|yaml\|env"` | Decode a single provider payload into a nested struct. |
| `optional:"true"` | Not-found is tolerated with no default (field keeps its zero value). |
| `onfail:"keeplast\|default\|fail"` | Policy for a chain error, not absence. Default `keeplast`. See [Source chains and precedence](/docs/concepts/source-chains/#the-onfail-policy). |
| `?debounce=<dur>` | Per-field coalescing window override, e.g. `?debounce=0` for certs. |

A struct field with a `source` and `flatten` decodes one payload into the sub-struct; a struct field with no `source` is a container mamori recurses into:

```go
type Config struct {
	Redis RedisConfig `source:"aws-sm://prod/redis" flatten:"json"`
}

type RedisConfig struct {
	Addr     string        `mapstructure:"addr"`
	Password secret.String `mapstructure:"password"`
	DB       int           `mapstructure:"db"`
}
```

## The Value type

Providers return a `Value`, not raw bytes. This is the keystone for change detection and lease-aware refresh:

```go
type Value struct {
	Bytes     []byte
	Version   string            // provider revision: SM VersionId, Vault version, file mtime hash
	Sensitive bool              // drives redaction downstream
	NotAfter  time.Time         // zero if unknown; e.g. a Vault lease expiry schedules a refresh
	Metadata  map[string]string
}
```

`Version` gives cheap change detection (no byte comparison when the provider supplies a revision). `NotAfter` lets lease-based providers request a refresh *before* expiry rather than waiting for the next poll tick.

## Next

- [Source chains and precedence](/docs/concepts/source-chains/) - multiple refs per field, `onfail`, and the comma-split rule.
- [Snapshots and pinning](/docs/usage/snapshots/) - snapshot versions, `WithHistory`, and pinning.
- [Error kinds](/docs/concepts/error-kinds/) - the typed `Kind` values and their sentinels.
- [Secret types](/docs/concepts/secret-types/) - `secret.String` / `secret.Bytes`, redaction, `Reveal`.

## See also

- [Loading and watching](/docs/usage/) - the how-to for chains, snapshots, and pinning.
- [Middleware](/docs/middleware/) - per-provider decorators such as `middleware.Failover`.
