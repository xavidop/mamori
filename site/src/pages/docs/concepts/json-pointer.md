---
layout: ../../../layouts/DocsLayout.astro
title: JSON Pointer selection
---

# JSON Pointer selection

A `#key` fragment starting with `/` is an [RFC 6901](https://datatracker.ietf.org/doc/html/rfc6901)
JSON Pointer, so it can reach any depth through objects and arrays alike.

```json
{ "credentials": { "password": "s3cr3t" },
  "replicas": [ { "host": "r0.db" }, { "host": "r1.db" } ] }
```

```go
type Config struct {
	Password secret.String `source:"aws-sm://prod/db#/credentials/password"`
	Replica  string        `source:"aws-sm://prod/db#/replicas/1/host"`
}
```

## Array indices are zero-based

`/replicas/1` is the second element. Index tokens are digits only:

| Token | Result |
| --- | --- |
| `/replicas/5` | the sixth element |
| `/replicas/05` | error, not an alias for `5` |
| `/replicas/+1`, `/replicas/-0` | error, signs are rejected |
| `/replicas/-` | error; RFC 6901 defines `-` as one-past-the-end, which can never address an existing value |

## Keys containing `/` or `~`

Two escapes let a pointer name a key that contains a delimiter:

| Escape | Means |
| --- | --- |
| `~0` | a literal `~` |
| `~1` | a literal `/` |

The case that catches people is a top-level key whose name **begins** with
`/`, since a leading `/` is also what makes a fragment a pointer at all:

| Fragment | Selects |
| --- | --- |
| `#/etc/passwd` | two tokens: key `etc`, then key `passwd` |
| `#/~1etc~1passwd` | the single key literally named `/etc/passwd` |

Only the second reaches that key. The first is not an error, just a
well-formed pointer that finds no `etc` object, so it reports `ErrNotFound`
and your `default:` absorbs it silently. **If a key beginning with `/` seems
to have gone missing, escape the leading slash as `~1`.**

## What you get back

A JSON string yields its unquoted contents. An object, array, number, or
boolean yields its JSON encoding, byte for byte. The same fragment therefore
works for both a `secret.String` field and a `flatten:"json"` struct field.

**A JSON `null` selects successfully and yields zero bytes**, not the four
bytes `null`. It is indistinguishable from `""`, and because it is a success
rather than a not-found it does **not** trigger `default:` or `optional`
either. If null has to be told apart from empty, select the enclosing object
and let `flatten:"json"` decode it into a pointer field: `null` leaves the
pointer `nil`, `""` gives a non-nil pointer to an empty string.

This applies only to a `null` the fragment selects directly. One nested inside
a selected object or array is carried through verbatim, so `#/replicas` over
`{"replicas":[null,1]}` still yields `[null,1]`.

## Which failures your `default:` absorbs

| Situation | Sentinel | `default:` / `optional` applies |
| --- | --- | --- |
| Object key absent | `ErrNotFound` | yes |
| Array index out of range | `ErrNotFound` | yes |
| Pointer descends into a string, number, boolean, or null | `ErrInvalid` | no |
| Bad index token, or the `-` token | `ErrInvalid` | no |
| Malformed escape (`~` not followed by `0` or `1`) | `ErrInvalid` | no |
| Payload is not valid JSON | `ErrInvalid` | no |

Only the first two are genuine absence. The rest are a mismatch between your
pointer and this payload, which fails loudly rather than being masked by a
default. See [Error kinds](/docs/concepts/error-kinds/).

## When the value is itself a JSON string

If a backend stores a payload double-encoded, so a value is a **string
containing JSON** rather than a nested object, a pointer cannot descend into
it. That is the "descends into a string" row above, and it reports
`ErrInvalid`.

Select the string, then let `flatten:"json"` do the second unwrap:

```go
type Creds struct {
	User     string        `mapstructure:"user"`
	Password secret.String `mapstructure:"password"`
}

type Config struct {
	// #/metadata selects the JSON string; flatten:"json" decodes it.
	Creds Creds `source:"aws-sm://prod/db#/metadata" flatten:"json"`
}
```

## See also

- [Ref grammar](/docs/concepts/ref-grammar/) - where the fragment sits in a tag.
- [Value decoding](/docs/concepts/decoding/) - runs after selection, so `#key` cannot look inside what decoding produces.
- [Error kinds](/docs/concepts/error-kinds/) - how these sentinels surface.
