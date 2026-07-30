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
| `?decode=` | [Decodes the value](/docs/concepts/decoding/) before it reaches your field |
| `?debounce=` | Overrides the coalescing window for this field |
| `?optional=` | Lets this ref be absent without failing the load |

## Parsing a ref in code

Most of the time you never do this: you write a ref in a `source:` tag and mamori parses it for you. But the parser is exported, so a ref string can be built or read at runtime, which is what [`WatchRef`](/docs/usage/watch-ref/) needs since it takes a `Ref` rather than a tag.

```go
ref, err := mamori.ParseRef("aws-sm://prod/db#/credentials/password?decode=base64")
if err != nil {
	return err // malformed: empty, or no scheme
}

ref.Scheme        // "aws-sm"
ref.Path          // "prod/db"
ref.Key           // "/credentials/password"
ref.Opt("decode") // "base64"
ref.String()      // the original tag, for diagnostics
```

For a [source chain](/docs/concepts/source-chains/), use `ParseRefs`, which splits on commas that are followed by a scheme token and returns one `Ref` per position:

```go
refs, err := mamori.ParseRefs("env:PORT,aws-ps://svc/port")
// refs[0].Scheme == "env", refs[1].Scheme == "aws-ps"
```

A single-ref tag returns a one-element slice, so `ParseRefs` is safe to use even when you do not expect a chain.

Both return an error only for a tag that is empty or has no scheme. A tag that parses but points nowhere is not an error here: it resolves not-found later, which is what lets `default:` and chain fallback do their job.

## Next

- [JSON Pointer selection](/docs/concepts/json-pointer/) - escapes, array indices, and which failures your `default:` absorbs.
- [Value decoding](/docs/concepts/decoding/) - `?decode=base64`, `gzip`, and stacking them.
- [Ref interpolation](/docs/concepts/ref-interpolation/) - `${VAR}` in the tag itself.
- [Source chains](/docs/concepts/source-chains/) - several refs on one field, with precedence.
