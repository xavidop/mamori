---
layout: ../../../layouts/DocsLayout.astro
title: Secret types
---

# Secret types

Import `github.com/xavidop/mamori/secret`. `secret.String` and `secret.Bytes` redact themselves everywhere a value is normally rendered, so a secret cannot leak into a log line or a JSON dump by accident.

## Redaction

Both types render as `[REDACTED]` through every common formatting path:

```go
s := secret.NewString("hunter2")

fmt.Println(s)              // [REDACTED]
fmt.Sprintf("%v", s)        // [REDACTED]
json.Marshal(s)            // "[REDACTED]"
slog.Info("login", "pw", s) // pw=[REDACTED]
```

`secret.Bytes` behaves the same way for `[]byte` payloads (`secret.NewBytes(...)`), and mamori sets a field's `Value.Sensitive` for you when it decodes into either type.

## Reveal and Zero

Reading the plaintext is deliberately the single, greppable access point, so secret reads are easy to audit:

```go
s.Reveal()      // "hunter2"  <- the only way to read a secret.String
s.Zero()        // best-effort wipe of the backing bytes
```

`secret.String.Reveal()` returns a `string` (`RevealBytes()` returns the raw `[]byte`); `secret.Bytes.Reveal()` returns `[]byte`. `IsZero()` reports whether the backing bytes are empty.

`Zero()` is best-effort: Go's GC may already have copied the value, and mamori documents that honestly rather than promise memory safety it cannot deliver. The `reconcilevet` analyzer flags a secret-bearing ref stored in a plain `string`.

## See also

- [Concepts overview](/docs/concepts/) - refs, providers, and the reconciler.
- [Security](/docs/security/) - redaction guarantees and the `WithHistory` retention tradeoff.
