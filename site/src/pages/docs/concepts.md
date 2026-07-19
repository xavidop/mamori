---
layout: ../../layouts/DocsLayout.astro
title: Concepts
---

# Concepts

The three types you interact with most: the **ref** parsed from a tag, the **Value** a provider returns, and the **secret** wrappers that keep sensitive data out of your logs.

## Refs and the tag grammar

A ref is parsed from the `source` tag. The grammar is:

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

`ParseRef` produces a `Ref{Scheme, Path, Key, Opts, Raw}`. `#key` selects one field from a structured (JSON) payload; `?opts` are provider-specific plus a few core-recognized options.

### Supplementary tags

| Tag | Meaning |
| --- | --- |
| `default:"..."` | Value used when the ref resolves to not-found (not on error). |
| `validate:"..."` | Field validation (go-playground/validator syntax), evaluated on **every** update. See the [Validation](../validation) page for the available rules. |
| `flatten:"json\|yaml\|env"` | Decode a single provider payload into a nested struct. |
| `optional:"true"` | Not-found is tolerated with no default (field keeps its zero value). |
| `?debounce=<dur>` | Per-field coalescing window override, e.g. `?debounce=0` for certs. |

Nested structs compose. A struct field with a `source` and `flatten` decodes one payload into the sub-struct; a struct field with no `source` is a container mamori recurses into:

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

Providers return a `Value`, not raw bytes. This is the keystone for rotation and hygiene:

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

## Secret types

Import `github.com/xavidop/mamori/secret`. `secret.String` and `secret.Bytes` redact themselves everywhere a value is normally rendered:

```go
s := secret.NewString("hunter2")

fmt.Println(s)              // [REDACTED]
fmt.Sprintf("%v", s)        // [REDACTED]
json.Marshal(s)            // "[REDACTED]"
slog.Info("login", "pw", s) // pw=[REDACTED]

s.Reveal()                  // "hunter2"  <- the only way to read it
s.Zero()                    // best-effort wipe of the backing bytes
```

`Reveal()` is deliberately the single, greppable access point, so secret reads are easy to audit. `Zero()` is best-effort: Go's GC may already have copied the value, and we document that honestly rather than promise memory safety we cannot deliver. The `reconcilevet` analyzer flags a secret-bearing ref stored in a plain `string`.
