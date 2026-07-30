---
layout: ../../../layouts/DocsLayout.astro
title: Ref grammar
---

# Ref grammar

Every `source` tag has the same shape:

```text
scheme://path[#key][?opt=v&...]   aws-sm, vault, file, and most others
scheme:path[#key][?opt=v&...]     env and exec, which take everything after the colon
```

```go
type Config struct {
	DBPassword secret.String `source:"aws-sm://prod/db#password"`
	LogLevel   string        `source:"env:LOG_LEVEL"`
}
```

## The one surprise: `#key` comes before `?opts`

This is the reverse of a normal URL, which puts the fragment last.

```go
// correct
Leased secret.String `source:"vault://kv/data/api#key?renew=true"`

// wrong: the fragment swallows the option
Leased secret.String `source:"vault://kv/data/api?renew=true#key"`
```

If a ref behaves as though an option were missing, check this first.

## Selecting part of a payload

Many backends return a JSON document rather than a bare value. The `#key`
fragment picks one piece out of it, and its **first character decides how it
is read**:

| Fragment | Reads as |
| --- | --- |
| `#password` | a literal top-level key |
| `#ca.crt` | a literal top-level key, dots and all |
| `#/credentials/password` | a JSON Pointer, nested through an object |
| `#/replicas/5/host` | a JSON Pointer, through an array element |

Anything not starting with `/` is a literal key. That is what keeps
Kubernetes' `ca.crt`, `tls.crt`, and `tls.key` addressing the keys they name
rather than being read as paths.

An empty fragment selects nothing, so the whole payload passes through.

```go
type Config struct {
	// one top-level key
	Password secret.String `source:"aws-sm://prod/db#password"`
	// nested, two levels down
	APIKey   secret.String `source:"aws-sm://prod/svc#/credentials/api_key"`
	// the whole payload, decoded into a struct
	Creds    Creds         `source:"aws-sm://prod/db" flatten:"json"`
}
```

## Options

`?opt=v` pairs configure the ref. Some are provider-specific (`?version=`,
`?renew=`); these are handled by mamori itself:

| Option | Does |
| --- | --- |
| `?decode=` | [Decodes the value](/docs/usage/decoding/) before it reaches your field |
| `?debounce=` | Overrides the coalescing window for this field |
| `?optional=` | Lets this ref be absent without failing the load |

## Next

- [JSON Pointer selection](/docs/concepts/json-pointer/) - escapes, array indices, and which failures your `default:` absorbs.
- [Value decoding](/docs/usage/decoding/) - `?decode=base64`, `gzip`, and stacking them.
- [Ref interpolation](/docs/usage/ref-interpolation/) - `${VAR}` in the tag itself.
- [Source chains](/docs/concepts/source-chains/) - several refs on one field, with precedence.
