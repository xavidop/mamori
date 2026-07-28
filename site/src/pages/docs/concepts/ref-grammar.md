---
layout: ../../../layouts/DocsLayout.astro
title: Ref grammar
---

# Ref grammar

A `source` tag is parsed into a `Ref` by `ParseRef`. This page is the
consolidated reference for that grammar: the scheme forms, the two fragment
forms (literal key and JSON Pointer), the RFC 6901 escaping and array-index
rules, the error table that decides whether `default:` applies, the gotcha
you hit when a value is itself a string containing JSON, the `?decode=`
pipeline that transforms a resolved value before it reaches your field, and
`${VAR}` interpolation of the tag text itself before any of the above is
parsed.

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
aws-sm://prod/db#/~1etc~1passwd               the same trick for a top-level
                                               key that *starts* with a "/":
                                               one token unescaping to
                                               "/etc/passwd". Written
                                               unescaped, "#/etc/passwd" is a
                                               two-token pointer instead
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

The case worth calling out is a top-level key whose name **begins** with `/`,
since that is also the discriminator that makes a fragment a pointer at all:

| Fragment | Selects |
| --- | --- |
| `#/etc/passwd` | a two-token pointer: key `etc`, then key `passwd` |
| `#/~1etc~1passwd` | the single top-level key literally named `/etc/passwd` |

Only the second form reaches such a key. The first is not an error - it is a
well-formed pointer that simply finds no `etc` object, so it reports
`ErrNotFound`, which a field's `default:` or `optional` then absorbs silently.
If a key beginning with `/` seems to have gone missing on upgrade, that is the
reason: escape the leading slash as `~1`.

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
yields its unquoted contents; an object, array, number, or boolean yields its
JSON encoding, byte-for-byte as it appeared in the payload. This holds for
both fragment forms and is what makes the same fragment usable for both a
`secret.String` field and a `flatten:"json"` struct field with no second rule.

**A JSON `null` is the one exception: it selects successfully and yields zero
bytes**, not the four bytes `null`. It is therefore indistinguishable from an
empty string (`""`), and - because it is a success, not an `ErrNotFound` - it
does **not** trigger the field's `default:` or `optional` handling either. If
a value may legitimately be null and has to be told apart from an empty one,
select the *enclosing* object instead and let `flatten:"json"` decode it into
a pointer field: a `null` leaves that pointer `nil`, while `""` gives a
non-nil pointer to an empty string. Note this applies only to a `null` the
fragment selects directly; one nested *inside* a selected object or array is
carried through verbatim, so `#/replicas` over `{"replicas":[null,1]}` still
yields `[null,1]`.

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

`?decode=` is a ref query option declaring that a resolved value is encoded, so
core decodes it before it reaches your struct field:

```go
type Config struct {
	// The Secrets Manager entry holds base64 text; decode it back to raw
	// bytes before TLSKey is populated.
	TLSKey []byte `source:"aws-sm://prod/tls#key?decode=base64"`

	// Stacked codings: the stored value is base64 of a gzip stream.
	Bundle []byte `source:"aws-sm://prod/bundle?decode=base64,gzip"`
}
```

### Codings

| Coding | Applies |
| --- | --- |
| `base64` | `encoding/base64`, standard alphabet, padded (`base64.StdEncoding`) |
| `base64url` | `encoding/base64`, URL-safe alphabet (`base64.URLEncoding`) |
| `hex` | `encoding/hex` |
| `gzip` | `compress/gzip` - decompresses, output capped at 16 MiB |
| `trim` | strips leading/trailing whitespace |

This is a **closed set**, all stdlib, with no extension point: core's minimal
dependency set (validator, mapstructure, fsnotify) is a stated property of the
project layout, and an open coding registry here would duplicate
`WithDecodeHook`, which already exists one layer down for arbitrary per-type
conversion.

The whitespace handling is deliberately asymmetric: `base64`, `base64url`, and
`hex` trim surrounding whitespace before decoding - a trailing newline from
`base64 < key.pem > secret`, an editor's save, or a round trip through a
backend's CLI is common, and rejecting a secret over an invisible byte is a
miserable failure to debug. `gzip` does **not** trim, because its payload is
binary: a valid gzip stream can legitimately end in bytes whose numeric values
are ASCII whitespace (the trailing CRC32/ISIZE bytes are raw integers, not
text), and trimming them would silently corrupt the stream. A genuinely
whitespace-padded gzip payload has its own explicit escape hatch instead:
`?decode=trim,gzip` trims first, then gunzips.

`gzip`'s decompressed output is capped at 16 MiB. Exceeding it is a loud
`ErrInvalid`, never a truncation - handing an application a silently
truncated secret or certificate would fail later, somewhere else, in a way
that looks like anything but a decode problem. The cap is a constant with no
option to override it: a legitimate payload above it is realistically only
possible from an unbounded local source (`file:`, `exec:`), and the remedy
there is to not declare `?decode=gzip` and gunzip in application code instead.

### Ordering: left to right, outermost wrapper first

Codings apply in the order written, outermost wrapper first: `?decode=base64,gzip`
means the stored value is base64 **of** gzip **of** the payload, so it is
base64-decoded first, then gunzipped.

This is why the option is named `decode` rather than `encoding`. HTTP's
`Content-Encoding` header lists codings in the order they were *applied*, and a
client decodes that list in *reverse*. Naming this option after the action
instead removes any question of which direction the list reads: unlike
`Content-Encoding`, `?decode=`'s list reads exactly as written, left to right,
first coding first.

### Failure is loud: `ErrInvalid`, never a silent passthrough

A payload that fails a decode step - malformed base64, a truncated gzip
stream, and so on - wraps `ErrInvalid`. There is no silent passthrough of the
raw, still-encoded bytes: a decode failure is a non-not-found error, so it
stops a precedence chain's walk exactly like any other real error (a
`default:` does not silently mask it - see below). See
[Error kinds](/docs/concepts/error-kinds/) for how `ErrInvalid` maps onto
`mamori.ErrorKind`.

An unrecognized coding name is rejected up front, at spec-walk time, so a typo
(`?decode=base64,gzp`) fails at `Load`, `Watch`, or `Doctor` rather than on
some later poll tick. Every ref in a precedence chain is validated this way,
not just the one that ultimately wins - a typo in a lower-precedence fallback
ref would otherwise stay silent until that ref actually won.

### `Version` is untouched

Decoding only transforms `Value.Bytes`. `Version`, `Sensitive`, `NotAfter`,
and `Metadata` all carry through unchanged, so change detection keeps
comparing the provider's own revision - not the decoded bytes - exactly as it
does for a field with no `?decode=` at all.

### Decoding runs after `#key` selection

Decode applies to whatever `#key` already selected (or the whole payload, if
there is no fragment) - not the other way around. `#tls.crt?decode=base64`
means "select `tls.crt` from the payload, then base64-decode what was
selected."

That ordering has a consequence: `#key` cannot select a field *out of* what
decoding produces. If a JSON Pointer needs to look inside a payload that only
exists after decoding - a base64-encoded JSON document, for instance - drop
the `#key`, decode the whole payload, and use `flatten:"json"` to do the
selection afterward:

```go
type Creds struct {
	User     string        `mapstructure:"user"`
	Password secret.String `mapstructure:"password"`
}

type Config struct {
	// No #key: the whole entry is base64. It decodes to a JSON object, and
	// flatten:"json" does the field selection ?decode= could not.
	Creds Creds `source:"aws-sm://prod/bundle?decode=base64" flatten:"json"`
}
```

### `?decode=` and `default:`

A field whose refs all report not-found falls back to its `default:` tag
value, used **as-is, undecoded** - even if the field also carries `?decode=`.
A default is a literal you wrote in the tag, not an encoded payload from a
backend, so running it through the decode pipeline would be wrong far more
often than right. Write the default already in its final, decoded form.

### `?decode=` is not redacted

`decode` is not on the list of query options [`Status()`](/docs/observability/)
and `mamori doctor` redact, so it stays visible in a `Report`. That is
deliberate: an operator debugging a garbled value needs to see that a
decoding step is in play, and `?decode=` alone is not itself sensitive.

### `?decode=` is a client-side concern

`?decode=` is applied by core, in the process that loads the config, so it
belongs on the ref in *your* `source` tag - including a `mamori://name` ref
pointing at a [config server](/docs/server/). A
[server binding](/docs/server/bindings/) may not carry it: the server serves
what the upstream holds and never runs the pipeline, so it rejects the option
at construction rather than silently serving still-encoded bytes.

## Ref interpolation (`${VAR}`)

`${VAR}` in a `source` tag is expanded from variables you supply through
[`WithRefVars`](#withrefvars-is-the-only-source), before the tag is parsed:

```go
type Config struct {
	DBPassword secret.String `source:"aws-sm://${ENV}/db#password"`
	Bucket     string        `source:"s3://backups-${REGION}/nightly"`
}

cfg, err := mamori.Load[Config](ctx,
	mamori.WithRefVars(map[string]string{"ENV": "prod", "REGION": "eu-west-1"}),
)
```

Only the braced form is recognized. A bare `$VAR` (no braces) is left
untouched, so a password, an `exec:` command, or a path that happens to
contain a literal `$` passes through unaffected. `$$` collapses to one
literal `$`.

### `WithRefVars` is the only source

**Variables come only from `WithRefVars`. mamori never reads the ambient
environment (`os.Getenv`) for `${VAR}` expansion.** This is the central rule
of this feature, not an incidental detail: a `source` tag's ref decides
*which secret a process reads*, so expanding `${VAR}` from ambient state
would let anything able to set an environment variable in the process - a
compromised dependency, a misconfigured entrypoint script, a sibling
container sharing an env file - redirect that read to a secret of its own
choosing. Routing expansion only through an explicit map keeps the set of
things that can influence a ref exactly what the caller passed to
`WithRefVars`: nothing ambient, nothing implicit.

This mirrors the `exec:` provider (see [exec](/docs/providers/exec/)), which
is similarly opt-in via `WithExecProvider` rather than always-on, for the
same reason: a capability that changes which secret gets read should not be
able to activate itself off state the caller never consciously provided.

Applying `WithRefVars` more than once merges the maps, with a later call
winning per key.

### `EnvVars`: the explicit, named opt-in

When a variable's value should in fact come from the environment, opt in by
naming it:

```go
mamori.WithRefVars(mamori.EnvVars("ENVIRONMENT", "REGION"))
```

`EnvVars` takes **named** variables on purpose: it keeps the set of things
that can influence a ref enumerable and greppable at the call site, rather
than "any environment variable at all". A name that is not set in the
environment is **omitted** from the result rather than mapped to `""`, so an
unset variable still surfaces as the undefined-variable error below instead
of silently expanding to nothing.

### When expansion runs

Expansion happens once, when `Load`, `Watch`, or `Doctor` walks the config
struct - before the tag is split into a precedence chain or handed to
`ParseRef`. It runs over the **whole raw tag string**, so a variable can
supply a scheme, a path segment, a fragment, or a query value:

```go
Endpoint string `source:"${SCHEME}://prod/db#${KEY}?region=${REGION}"`
```

It is **not recursive**: a variable's own value is inserted verbatim and
never rescanned. If `${A}` expands to a value that itself contains `${B}`,
that `${B}` is left literal in the output rather than resolved against
whatever `B` is set to. Recursive expansion would risk an infinite loop from
a self-referencing or cyclic variable value, so this is deliberate, not an
oversight.

### Errors: undefined, unterminated, and empty are all hard failures

Three shapes fail loudly rather than silently expanding to nothing:

| Situation | Example | Error text |
| --- | --- | --- |
| Undefined variable | `${NOPE}`, with no `NOPE` key in the vars map | `undefined ref variable "NOPE" (pass it with WithRefVars)` |
| Unterminated `${` | `${NOPE` with no closing `}` | `unterminated ${` |
| Empty name | `${}` | `empty variable name in ${}` |

All three wrap `ErrInvalid`. The reasoning is the same for all three:
expanding an unset or malformed reference to nothing would produce a ref
like `aws-sm:///db` (an empty path segment), which resolves not-found and
then quietly falls back to the field's `default:` - turning a deployment
misconfiguration (someone forgot to pass `ENV`) into a silently wrong value
instead of a loud `Load`/`Watch`/`Doctor`-time error.

### Interpolation and precedence chains

Because expansion runs over the whole tag string before it is split into a
comma-separated [precedence chain](/docs/concepts/source-chains/), a
variable's value can inject a comma and change how that split happens: if a
`${VAR}` value contains a comma followed by text that matches the
[comma-split rule](/docs/concepts/source-chains/#the-comma-split-rule) (it
looks like a new scheme), what was written as one ref becomes two:

```go
// If REGION expands to "eu,vault://kv/data/db", this stops being one ref
// and becomes a two-element chain: "s3://backups-eu" and "vault://kv/data/db".
Bucket string `source:"s3://backups-${REGION}"`
```

This is acceptable under the same trust model as the rest of `WithRefVars`:
the caller supplying the variables is also the one who wrote the ref, so a
variable reshaping the chain is no different in kind from that caller
writing the two-element chain directly. It is worth knowing about, though,
if a variable's value is ever assembled from anything less trusted than the
caller itself.

### A value containing `#` or `?` re-cuts the ref

The comma above is the benign case - it still produces a working chain. `#`
and `?` are the ones to watch, because a value containing either **moves the
fragment or query delimiter** and the result is a valid ref that resolves to
the wrong thing rather than an error. Expansion is textual and runs before
`ParseRef`, which splits the query at the *first* `?` and the fragment at the
*first* `#`, so a delimiter arriving from a variable's value wins over the one
you wrote:

```go
DBUser string `source:"aws-sm://${ENV}/db#/credentials/user"`
```

| `ENV` | Ref actually resolved |
| --- | --- |
| `prod` | path `prod/db`, pointer `/credentials/user` - as written |
| `prod?x` | path `prod`, **no fragment**, and `x/db#/credentials/user` swallowed into the query as an option name |
| `prod#y` | path `prod`, fragment is now the literal key `y/db#/credentials/user` |

Both of the last two rows resolve not-found and then quietly fall through to
the field's `default:` or `optional`. The same applies to a variable in the
fragment or query position, which
[`#${KEY}?region=${REGION}`](#when-expansion-runs) makes an ordinary thing to
write: a `KEY` of `pass?word` yields the fragment `pass` plus a junk
`word?region` option, silently selecting the wrong key *and* dropping the
region.

mamori deliberately does not validate or escape variable values to prevent
this. Supplying a scheme, a fragment, or a query value is an advertised use of
`${VAR}`, so there is no rule that could tell "this `?` starts the query I
meant" from "this `?` came from a value". The rule to hold instead is the one
`WithRefVars` states already: variable values are operator-supplied
identifiers - environment names, regions, tenants - not text assembled from
anything the operator does not control. Keep `#`, `?`, and `,` out of them,
and if a value is ever built from a less trusted source, check it there.

### Compatibility: the scan always runs

Expansion scans every `source` tag whether or not the caller ever calls
`WithRefVars` - `$$` still collapses to a literal `$`, and a stray `${` still
hard-errors, even with no vars supplied at all. This is deliberate rather
than conditioned on "did the caller opt in": gating the scan on
`WithRefVars` having been called would mean a caller who simply forgot to
call it gets `${ENV}` left completely literal in the ref, which resolves
not-found and quietly takes the field's `default:` - exactly the silent
misconfiguration this feature exists to prevent.

The realistic thing this changes on upgrade is a pre-existing tag that
happens to contain a literal `$$` or a stray `${`, most plausibly inside an
`exec:` command line; that text now means something different, or fails,
rather than passing straight through as inert. See
[What changes on upgrade](#what-changes-on-upgrade) for the full list of
behavior changes this grammar brings.

### Visibility: expanded refs are visible, not redacted

After expansion, `Ref.Raw` holds the **expanded** string, so
[`Status()`](/docs/observability/), the
[admin endpoint's](/docs/observability/admin/) `Report`, and
[`mamori doctor`](/docs/observability/doctor/) all show the ref exactly as it
resolved, variable values included.

**`WithRefVars` values must not be secrets.** Use it for environment names,
regions, service names, and tenant identifiers - anything you'd be
comfortable seeing on a status page or in `mamori doctor` output. A secret
still belongs in the *value* a ref resolves to (the payload behind
`aws-sm://...`), never in a variable spliced into the ref's own text.

## What changes on upgrade

Pointer fragments, `?decode=`, and `${VAR}` are new grammar, so almost every
existing tag keeps its meaning. Four things do not, and none of them announces
itself at the call site:

| Existing tag or behavior | Was | Is now |
| --- | --- | --- |
| A tag containing `$$` | two literal `$` characters, passed through | one literal `$` - [the scan always runs](#compatibility-the-scan-always-runs) |
| A tag containing `${` | inert literal text | a variable reference: a hard `ErrInvalid` at `Load`/`Watch`/`Doctor` if it is unterminated or names a variable no `WithRefVars` supplied |
| A fragment naming a top-level key that *starts* with `/` | that literal key | [a JSON Pointer](#escapes), so it reports `ErrNotFound` and a `default:`/`optional` absorbs it. Escape the leading slash: `#/~1etc~1passwd` |
| `SelectKey` against a payload that is not JSON | an unclassified error, `mamori.ErrorKind` of `unknown` | wraps `ErrInvalid`, `ErrorKind` of `invalid` |

The first two are most plausible in an `exec:` command line, which is the one
place a `$` regularly appears in a tag. The last one changes no control flow -
neither kind is not-found, so `default:` was never involved either way - but it
does change the `Kind` a `Status()`, admin `Report`, or `mamori doctor` output
reports, which matters if anything alerts on those strings.

**Provider authors:** `providertest.Run`'s
[`DecodeOption` case](/docs/writing-a-provider/conformance/#decodeoption) is
**not** opt-in - it runs for every provider. An out-of-tree provider that
strips, rewrites, or errors on a query option it does not recognize starts
failing the conformance suite on upgrade. That is the bug it exists to catch:
such a provider silently breaks `?decode=` for its users.

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
