---
layout: ../../../layouts/DocsLayout.astro
title: Ref grammar
---

# Ref grammar

A `source` tag is parsed into a `Ref` by `ParseRef`. This page is the
consolidated reference for that grammar: the scheme forms, the two fragment
forms (literal key and JSON Pointer), the RFC 6901 escaping and array-index
rules, the error table that decides whether `default:` applies, and the
gotcha you hit when a value is itself a string containing JSON.

## The full grammar

```text
scheme://path[#key][?opt=v&...]   (hierarchical: aws-sm, vault, file, ...)
scheme:path[#key][?opt=v&...]     (opaque: env, exec)
```

Opaque schemes such as `env:` and `exec:` take everything after the colon as
the path, with no `//` authority section. Both forms place the optional
`#key` fragment **before** the optional `?opts` query - the reverse of a
standard URL, which puts the fragment last. `ParseRef` therefore parses by
hand rather than via `net/url`, splitting the query off first and the
fragment second:

```go
type Config struct {
	// literal top-level key
	DBPassword secret.String `source:"aws-sm://prod/db#password"`
	// nested key via a JSON Pointer fragment
	APIKey     secret.String `source:"aws-sm://prod/svc#/credentials/api_key"`
	// #key before ?opts, not after
	Leased     secret.String `source:"vault://kv/data/api#key?renew=true"`
	// opaque scheme
	LogLevel   string        `source:"env:LOG_LEVEL"`
}
```

`ParseRef` produces `Ref{Scheme, Path, Key, Opts, Raw}`. `Key` holds the
fragment verbatim, including any leading `/`; it is `SelectKey`, not
`ParseRef`, that decides what the fragment means.

## Fragment selection: two forms

The fragment's first character is the whole discriminator:

- **A fragment beginning with `/` is an RFC 6901 JSON Pointer**, addressing a
  value at any depth through objects and array elements alike.
- **Any other fragment is a literal top-level key**, exactly as it has always
  been.

```text
aws-sm://prod/db#password                     literal key "password"
k8s-secret://prod/tls#ca.crt                  literal key "ca.crt"
aws-sm://prod/db#/credentials/password        nested, through an object
aws-sm://prod/db#/replicas/5/creds/password   nested, through an array element
aws-sm://prod/db#/a~1b                        a pointer whose one token
                                               unescapes to "a/b", so it
                                               also addresses a top-level key -
                                               just one whose name itself
                                               contains a "/"
```

This is what keeps `#ca.crt`, `#tls.crt`, and `#tls.key` addressing the keys
they name rather than being parsed as paths: those are the canonical
Kubernetes TLS secret keys, and dotted keys are the norm in ConfigMaps and
Java properties files too. A leading `/` is a discriminator no such key can
collide with, since none of them start with a slash.

An empty fragment is unaffected either way: `SelectKey` returns the payload
unchanged when `key` is empty.

## JSON Pointer syntax

### Escapes

A reference token may contain two RFC 6901 escapes:

| Escape | Meaning |
| --- | --- |
| `~0` | literal `~` |
| `~1` | literal `/` |

They are applied in one left-to-right pass, not as two independent
find-and-replace calls: replacing every `~0` before every `~1` would turn the
literal token `~01` into `~1` and then into `/`, which is wrong. Escapes let a
pointer address a key whose name contains a literal `/` or `~`, exactly as if
you had looked it up directly by that name.

### Array indices

Indices are **zero-based**: `/replicas/5` is the sixth element.

```json
{
  "replicas": [
    { "host": "r0.db" },
    { "host": "r1.db" }
  ]
}
```

```go
type Config struct {
	Host string `source:"aws-sm://prod/db#/replicas/1/host"`
}
```

An index token is digits only, per RFC 6901's `"0" / (%x31-39 *DIGIT)`:

- A **leading zero is rejected**: `/replicas/05` is an error, not a silent
  alias for `/replicas/5`.
- A **sign is rejected**: `/replicas/+1` and `/replicas/-0` are both errors,
  never `1` and `0`.
- The **`-` token is rejected**: RFC 6901 defines it as one-past-the-end for
  JSON Patch's add operation, and it can never address an existing value, so
  mamori treats it as invalid rather than not-found.

## Return semantics

Object keys and array indices interleave freely at any depth. A JSON string
yields its unquoted contents; any other JSON value (object, array, number,
boolean, null) yields its JSON encoding, byte-for-byte as it appeared in the
payload. This holds for both fragment forms and is what makes the same
fragment usable for both a `secret.String` field and a `flatten:"json"`
struct field with no second rule.

## Errors

| Situation | Sentinel | `default:` / `optional` applies |
| --- | --- | --- |
| Object key absent | `ErrNotFound` | yes |
| Array index out of range | `ErrNotFound` | yes |
| Pointer descends into a string, number, boolean, or null | `ErrInvalid` | no |
| Non-numeric, signed, or leading-zero array token; the `-` token | `ErrInvalid` | no |
| Malformed escape (`~` not followed by `0` or `1`) | `ErrInvalid` | no |
| Payload is not valid JSON | `ErrInvalid` | no |

Only the first two rows are genuine absence, so only they trigger a field's
`default:` or `optional:"true"` handling, exactly like a missing top-level key
today. Every other row is a structural mismatch - a malformed request against
this particular payload, not an absence - and must fail loudly rather than be
silently masked by a default. See [Error kinds](/docs/concepts/error-kinds/)
for how `ErrNotFound` and `ErrInvalid` map onto `mamori.ErrorKind`.

## When a value is itself a JSON string

The row worth knowing before you hit it: if a value is stored as a **string
containing JSON**, rather than a nested object, a pointer cannot descend into
it - that is the "pointer descends into a string" row above, reported as
`ErrInvalid`. This happens whenever a backend double-encodes a payload (a
secret manager entry whose value is itself a serialized JSON document, for
instance), and it looks at first glance like it should be walkable.

The remedy is two steps: select the string itself with a pointer (or a
literal key, if it is top-level), and give the field a struct type with
`flatten:"json"` so mamori's own decode step does the second unwrap:

```go
type Creds struct {
	User     string        `mapstructure:"user"`
	Password secret.String `mapstructure:"password"`
}

type Config struct {
	// #/metadata selects the JSON *string*; flatten:"json" then decodes it.
	Creds Creds `source:"aws-sm://prod/db#/metadata" flatten:"json"`
}
```

## Value decoding (`?decode=`)

Not yet released - this section will be filled in when it ships.

## Ref interpolation (`${VAR}`)

Not yet released - this section will be filled in when it ships.

## See also

- [Concepts overview](/docs/concepts/) - refs, providers, and the reconciler.
- [Source chains and precedence](/docs/concepts/source-chains/) - multiple
  refs per field, `onfail`, and the comma-split rule.
- [Error kinds](/docs/concepts/error-kinds/) - the typed `Kind` values and
  their sentinels.
- [Resolve and errors](/docs/writing-a-provider/resolve/) - how a provider
  earns pointer support by calling `mamori.SelectKey`.
- [Kubernetes provider](/docs/providers/kubernetes/) - a provider whose
  fragment is always a literal key, never a pointer.
