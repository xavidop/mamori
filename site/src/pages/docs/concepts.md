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
| `onfail:"keeplast\|default\|fail"` | Policy for a chain error (not absence). Default `keeplast`. See [Source chains and precedence](#source-chains-and-precedence). |
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

### Error kinds

Every resolve failure carries a coarse classification you can read with
`mamori.ErrorKind(err)`: `not_found`, `permission_denied`, `unauthenticated`,
`unavailable`, `rate_limited`, `invalid`, or `unknown`.

Only `not_found` changes behavior by default, since it is what triggers a field's
`default:` or `optional` handling (see the table above); the rest are
diagnostic - telemetry (see `x/otel`'s `mamori.error.kind` attribute) and your own error-handling code use them to tell a misconfiguration from an outage. The one opt-in exception is `onfail:"default"` on a source chain, which explicitly treats a non-`not_found` error like absence too; see [Source chains and precedence](#source-chains-and-precedence).

Match them with `errors.Is`:

```go
if errors.Is(err, mamori.ErrPermissionDenied) {
    // the credential is fine, the authorization is not
}
```

## Source chains and precedence

A `source` tag can hold more than one ref, separated by commas:

```go
type Config struct {
	Port string `source:"env:PORT,aws-ps://svc/port"`
}
```

This is a **precedence chain**, not a list of alternates tried on failure:

1. The first ref that resolves to a value wins; the walk stops there.
2. A ref that resolves not-found falls through to the next ref in the chain.
3. A ref that fails with any other error (permission denied, unavailable, rate limited, an unregistered scheme, ...) stops the walk immediately. Lower-priority refs are deliberately **not** tried: sliding to a lower-priority source because a higher-priority one is transiently broken would make config resolution depend on backend health instead of the order you declared. That error becomes the field's terminal error and is handled by the `onfail` policy below.
4. If every ref resolves not-found, the field falls back to `default:` / `optional:"true"` exactly as a single-ref field does today.

A single-ref tag (the common case) is just a one-element chain, so an existing, untagged field behaves exactly as before.

This is precedence, not availability fallback. If what you want is "try the primary, and on a transport error use a replica of the *same* backend," that is `middleware.Failover` (see [Middleware](../middleware)) - a decorator around one `Provider`. A source chain instead lets one field name sources across *different* schemes in priority order (an environment override ahead of a Parameter Store default, say), and it is a property of the tag, not the provider.

### Chain grammar and the comma-ambiguity rule

A comma is a chain separator only when the text right after it looks like a new scheme, i.e. matches `^[a-zA-Z][a-zA-Z0-9+.-]*:`. A comma anywhere else - inside a query string (`?tags=a,b`) or an opaque `exec:` path (`exec:echo a,b`) - is left alone and stays part of that ref's value:

```go
// Two refs: "env:PORT" and "aws-ps://svc/port"
Port string `source:"env:PORT,aws-ps://svc/port"`

// One ref: the comma in the query string is not a chain separator
Tags string `source:"consul://kv/tags?filter=a,b"`

// One ref: the comma in the exec: path is not a chain separator
Report string `source:"exec:echo a,b"`
```

To force a literal comma into a value at a spot where it would otherwise be read as a separator, percent-encode it as `%2C`. mamori does not percent-decode it back: the ref's parsed path keeps the literal `%2C`.

```go
// Without escaping, "echo a,env:b" would split into two refs after the
// comma (env: looks like a new scheme). %2C keeps it one ref, and the
// resolved path is literally "echo a%2Cenv:b" (not "echo a,env:b").
V string `source:"exec:echo a%2Cenv:b"`
```

### onfail: what happens when a chain position errors

`onfail` controls what a field does when step 3 above stops the walk on a genuine error, as opposed to absence:

| Policy | Behavior on a non-not-found error |
| --- | --- |
| `onfail:"keeplast"` (default) | Keep the last successfully applied value and report the error via `OnError`/`Status`. On the very first `Load` there is no prior value to keep, so it **fails** rather than silently falling back to `default:`. |
| `onfail:"default"` | Apply the field's `default:` value, exactly as if every ref in the chain had resolved not-found. This is the explicit opt-in for "treat this error like absence." It requires a `default:` tag and is rejected when the spec is parsed if there isn't one. |
| `onfail:"fail"` | Reject the candidate outright: the whole update on `Watch` (not just this field), or the whole `Load`. |

This is uniform for a single-ref field and a multi-ref chain - an untagged single-ref field is `onfail:"keeplast"`, reproducing today's behavior exactly.

The rule to hold onto: **`default:` applies only to genuine absence**, never to an error. A permission-denied, an outage, or a misconfigured provider never silently falls back to `default:` unless the field explicitly opts in with `onfail:"default"`. This is deliberate: masking a real error behind a quiet default is exactly the footgun mamori avoids elsewhere in this package (for example, classifying a missing `exec:` binary as `unknown` rather than `not_found` so it cannot be mistaken for absence), and the same principle holds here.

### Chains are watched live

Every position in a chain is watched, not just the current winner, so precedence is live. Exporting a higher-precedence source at runtime takes over immediately - `Watch` recomputes the winner and delivers one `Change` - and removing it again falls back to whatever a lower-priority position already holds, again as one `Change`. A change to a position that is not currently winning is still observed (it is reflected in `Status()`) but does not move `Get()` or fire a `Change`, since a higher-priority ref is still winning. See [Loading & watching](../usage#source-chains) for a worked example of the takeover and fallback.

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

## Snapshot versions and pinning

Every `Watcher[T]` carries a monotonically increasing **snapshot version**: `1` at the initial load inside `Watch`, incremented by one each time a validated, non-empty update is applied. The version keeps climbing in the background even while a pin has frozen what `Get()` returns, since sources are still watched and reconciled the whole time - a pin only holds back delivery to `Get()` and `OnChange`, not the reconciliation itself.

```go
type Snapshot[T any] struct {
	Version uint64
	At      time.Time
	Config  T
	Fields  []FieldChange // what changed relative to the previous snapshot
}
```

`WithHistory(n)` (default `0`) retains the `n` most recent `Snapshot[T]` values in addition to the current one; `w.History()` returns them newest first. `w.Pin(version)` freezes `Get()` at a retained snapshot (`ErrNoSuchSnapshot` if that version has fallen out of the retained window), `w.PinCurrent()` freezes at whatever snapshot is being served right now, and `w.Unpin()` resumes, delivering exactly one coalesced `Change` for everything that changed while pinned. See [Loading & watching](../usage#snapshot-history-and-pinning) for the full pin/investigate/unpin walkthrough and [Security](../security#withhistory-retains-past-secrets-in-memory) for the secret-retention tradeoff that comes with turning `WithHistory` on.
