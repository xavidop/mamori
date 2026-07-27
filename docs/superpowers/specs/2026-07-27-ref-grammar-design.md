# mamori ref grammar and value pipeline: nested selection, decoding, interpolation

**Date:** 2026-07-27
**Status:** draft, not yet implemented
**Scope:** core module (`helpers.go`, `ref.go`, `resolve.go`, `decode.go`, `reconciler.go`), `providertest`, README and docs site

---

## 1. Context

mamori resolves a `source` tag into a `Ref` and hands it to a provider. The
grammar that tag speaks has been stable since the original design:

```
scheme://path[#key][?opts]   (hierarchical: aws-sm, vault, file, ...)
scheme:path[#key][?opts]     (opaque: env, exec)
```

Three things it cannot express today show up repeatedly in real
configurations, and each currently pushes work onto the application:

**Nested payloads.** `SelectKey` (`helpers.go:27`) unmarshals into
`map[string]json.RawMessage` and looks up exactly one top-level key. A secret
shaped `{"credentials": {"password": "..."}}` cannot be addressed at all. The
user's options are to restructure the secret in the backend, or to give the
field a struct type with `flatten:"json"` and pull one leaf out of it in
application code.

**Encoded payloads.** Certificates, keystores, and binary blobs are routinely
stored base64-encoded or gzipped inside a secret manager. mamori hands the raw
bytes through, so every such field needs a `WithDecodeHook` in the application,
which is per-type rather than per-field and therefore cannot distinguish a
base64 field from a plain one of the same Go type.

**Per-environment refs.** A `source` tag is a compile-time string constant. A
service deployed to three environments whose secrets live at
`aws-sm://prod/db`, `aws-sm://staging/db`, `aws-sm://dev/db` has no way to
express that in one struct. `middleware.Prefix` can rewrite a namespace, but it
is a multi-tenancy tool applied to a whole provider, not a per-field
substitution, and it is invisible at the field's declaration site.

All three are resolve-time concerns that belong in the grammar rather than in
every application that uses it.

## 2. Goals

1. Address a value nested at any depth inside a structured payload, including
   through array elements.
2. Decode an encoded payload declaratively, at the field that needs it, with no
   new dependency in core.
3. Substitute operator-supplied variables into a ref, without introducing
   ambient authority over which secret a process reads.
4. Break none of the 32 provider modules, and change the meaning of no
   existing ref.

## 3. Non-goals

- **Not a query language.** JSON Pointer addresses one location. There is no
  filtering, no wildcards, no JSONPath `$..` recursion. A ref names a value; it
  does not search for one.
- **Not a transform framework.** `?decode=` covers a closed set of five
  stdlib codings. It is not an extension point, and there is no
  `WithDecoder(name, func)` hook. A caller needing arbitrary transformation
  already has `WithDecodeHook` at the decode layer.
- **No moving of `SelectKey` into core.** Providers keep calling it. That
  refactor is real and possibly worth doing (see decision D3), but it is a
  breaking `Provider` and `BatchProvider` contract change across 26 provider
  files and belongs in its own spec.
- **No derived or computed fields.** Composing a DSN from other decoded fields
  is a post-decode concern with its own questions (re-run on unpin? participate
  in `Change.Fields`?). Deliberately deferred to a separate spec.
- **No expansion from the ambient environment.** See D6.

## 4. Decisions

| ID | Decision | Rationale |
|----|----------|-----------|
| D1 | A `#` fragment beginning with `/` is an **RFC 6901 JSON Pointer**; anything else stays a literal top-level key | `#ca.crt` must keep meaning the key `ca.crt`. That is not hypothetical: it is the shipped example in `providers/k8s/README.md:23` and `site/src/pages/docs/providers/kubernetes.md:46`, because `tls.crt` / `tls.key` / `ca.crt` are the canonical Kubernetes TLS secret keys, and Java properties and most ConfigMaps have the same shape. A leading `/` is an unambiguous discriminator that no existing ref can collide with, and RFC 6901 brings defined escaping (`~0`, `~1`) and array indexing for free rather than inventing either. |
| D2 | Absence is `ErrNotFound`; structural mismatch is `ErrInvalid` | Only `ErrNotFound` triggers `default:` / `optional`. A path that is simply not present is genuine absence and should fall back exactly as a missing top-level key does today. A pointer that walks *through* a scalar, or a non-numeric segment against an array, is a malformed request against this payload and must fail loudly. This also matches what `SelectKey` already does: a non-object payload is currently a plain error, not a not-found. |
| D3 | `?decode=` is applied by **core**, after the provider returns | Providers call `SelectKey` themselves inside `Resolve` (26 provider files do), so by the time `resolveRef` (`resolve.go:140`) sees a `Value`, selection has already happened and core can only transform after it. Accepting that fixed order costs one expressible case (decode-then-select), which already has an answer: drop the `#key`, decode the whole payload, and use `flatten:"json"`. The alternative, moving selection into core, is a breaking SPI change across every provider for a case with an existing workaround. |
| D4 | The option is named `?decode=`, not `?encoding=` | HTTP's `Content-Encoding` lists codings **in the order they were applied**, so a receiver decodes in reverse. Naming this `encoding=` would inherit that expectation and make `encoding=base64,gzip` mean the opposite of left-to-right. `decode=` names the action, so `decode=base64,gzip` reads "base64-decode, then gunzip" with no convention to recall. |
| D5 | Codings are limited to `base64`, `base64url`, `hex`, `gzip`, `trim` | All are `encoding/base64`, `encoding/hex`, `compress/gzip`, and `strings`. Core's dependency set (`validator`, `mapstructure`, `fsnotify`) is a stated property of the project layout and stays untouched. `zstd` and `brotli` would each add one. |
| D6 | Interpolation variables come **only** from `WithRefVars`; never from the ambient environment | A ref is what decides which secret a process reads. Expanding it from `os.Getenv` means anything able to set an environment variable can redirect that read. mamori already refuses ambient authority of this kind: `exec:` is opt-in via `WithExecProvider` "for security reasons", and `default:` never fires on an error because "an error must fail loudly unless the field explicitly opts in". `WithRefVars(mamori.EnvVars("ENVIRONMENT"))` keeps the environment available, named variable by named variable, and greppable at the call site. |
| D7 | Expansion happens **once**, at spec-walk time | The reconciler keys per-field watch state on ref identity (`recordSourceUpdate`, `reconciler.go:670`, is indexed by spec and chain position). A ref whose target could move mid-process would require tearing down and restarting a watch, for no use case anyone has asked for. |
| D8 | An unset variable is a hard error from `Load` / `Watch` / `Doctor` | Expanding to empty would silently produce `aws-sm:///db`, which resolves not-found and then quietly takes the `default:`. That is the exact footgun D2 and the existing `onfail` design both exist to prevent. |

## 5. Feature 1: JSON Pointer nested key selection

### 5.1 Grammar

`SelectKey(data []byte, key string) ([]byte, error)` gains one rule at its head:
if `key` begins with `/`, it is an RFC 6901 JSON Pointer. Otherwise the existing
literal top-level lookup runs unchanged. An empty `key` still returns `data`
untouched.

```
aws-sm://prod/db#password                     literal key "password"     (unchanged)
k8s-secret://prod/tls#ca.crt                  literal key "ca.crt"       (unchanged)
aws-sm://prod/db#/credentials/password        nested
aws-sm://prod/db#/replicas/5/creds/password   through an array element
aws-sm://prod/db#/a~1b                        literal key "a/b"
aws-sm://prod/db#/m~0n                        literal key "m~n"
```

Object keys and array indices interleave freely at any depth.

### 5.2 Array indices

Indices are **zero-based**: `/replicas/5` is the sixth element. Given

```json
{"replicas": [
  {"host": "r0.db", "creds": {"user": "app", "password": "p0"}},
  ...
  {"host": "r5.db", "creds": {"user": "app", "password": "p5"}}
]}
```

```go
type Config struct {
    Host string        `source:"aws-sm://prod/db#/replicas/5/host"`
    Pass secret.String `source:"aws-sm://prod/db#/replicas/5/creds/password"`
}
```

Per RFC 6901, an index token is either `0` or a non-zero digit followed by
digits: **leading zeros are rejected**, so `/05` is an error rather than a
silent alias for `/5`. The `-` token (one-past-the-end) is defined by RFC 6901
only for JSON Patch's add operation; it can never address an existing value, so
it is rejected as invalid rather than reported as not-found.

### 5.3 Errors

| Situation | Sentinel | `default:` applies |
|---|---|---|
| Object key absent | `ErrNotFound` | yes |
| Array index out of range | `ErrNotFound` | yes |
| Pointer descends into a string, number, boolean, or null | `ErrInvalid` | no |
| Non-numeric token against an array (`#/replicas/five`) | `ErrInvalid` | no |
| Index with a leading zero, or the `-` token | `ErrInvalid` | no |
| Root payload is not valid JSON | `ErrInvalid` | no |
| Malformed escape (a `~` not followed by `0` or `1`) | `ErrInvalid` | no |

Error text names the pointer and the token that failed, e.g.
`mamori: pointer "/replicas/5/creds" : token "5" out of range (array has 3 elements)`.

The `ErrInvalid` row for descending into a string is the one users will meet by
accident, because a replica entry stored as a *string containing JSON* looks like
it should be walkable. The docs will state the remedy: select the string with
`#/replicas/5` and give the field a struct type with `flatten:"json"`.

### 5.4 Return semantics

Unchanged from today, and deliberately so: a JSON string yields its unquoted
contents; an object, array, number, or boolean yields its JSON encoding. This is
what makes `#/a/b` usable for both a `secret.String` and a `flatten:"json"`
struct field without a second rule.

### 5.5 Blast radius

All 26 provider files calling `mamori.SelectKey` inherit this with no change.

**The fragment slot does not mean the same thing in every provider.** This was
underestimated in the first draft of this spec and is recorded here because the
conformance case depends on it. Three distinct meanings exist today:

1. **A JSON selector.** `aws-sm`, `s3`, `consul`, `redis`, and most others treat
   the fragment as a key into a JSON payload and route it through `SelectKey`.
   These gain pointer support.
2. **A backend-native key.** The Kubernetes provider reads `Secret.Data`, a Go
   map, so `#ca.crt` names a map entry, not a JSON path. Doppler's fragment
   names the secret itself. These correctly do their own lookup and gain
   nothing, because there is no document to point into.
3. **Nothing.** `providers/mamori`'s client never reads `ref.Key` at all;
   per decision D9 of the operational-layer spec, field selection is
   server-side only. Feature-flag providers returning a bare `bool` or `string`
   (`unleash`, `split`, `flipt`) have no payload to select from either.

Only category 1 can be conformance-tested for pointer support, and a provider
in category 2 or 3 is not defective for failing such a test. The conformance
case is therefore **opt-in**: `providertest.Config.PointerRef` is supplied only
by a provider whose fragment slot is a JSON selector, and the case skips when it
is nil.

`PointerRef` is a ref *builder* rather than a boolean because
`providertest.Config.Ref` is not fragment-free by convention. Several providers
bake a fixed fragment into it (`vault://secret/<key>#value` at
`providers/vault/vault_test.go:137`; the same shape in `mongodb`, `firestore`,
and `k8s`), so appending a second fragment to `Ref`'s output would produce
`vault://secret/x#value#/outer/inner`. A builder lets each provider say where
its selector goes.

## 6. Feature 2: `?decode=` value transforms

### 6.1 Grammar

A new core-recognized ref option, joining `debounce`, `optional`, and `version`:

```
aws-sm://prod/certs#tls.crt?decode=base64
s3://bucket/keystore?decode=base64,gzip
env:PADDED_TOKEN?decode=trim
```

The value is a comma-separated list applied **left to right**: `base64,gzip`
means base64-decode the payload, then gunzip the result.

| Coding | Implementation |
|---|---|
| `base64` | `encoding/base64.StdEncoding` (padded) |
| `base64url` | `encoding/base64.URLEncoding` |
| `hex` | `encoding/hex` |
| `gzip` | `compress/gzip` |
| `trim` | `strings.TrimSpace` on the bytes |

An unrecognized coding name is rejected at **spec-walk time**, not at resolve
time, so a typo fails at `Watch()` alongside every other tag error rather than
on a poll tick an hour later.

### 6.2 Semantics

- A payload that fails to decode produces `ErrInvalid`, wrapping the underlying
  codec error with `%w`. This is exactly what `KindInvalid` already documents:
  "the returned payload could not be parsed". It is never a silent passthrough.
- **`Value.Version` is left untouched.** The provider's revision identifier
  describes the source, not the decoded form, so `Value.changed` keeps working
  byte-for-byte and a decoded field detects change exactly as an undecoded one
  does.
- `Value.Sensitive` is preserved across the pipeline.
- `Value.NotAfter` and `Value.Metadata` are preserved.
- Decoding runs after the provider's own `#key` selection (D3), so
  `#tls.crt?decode=base64` means "select `tls.crt`, then base64-decode it".

### 6.3 Where it is applied

This is the correctness risk of the feature. A `Value` enters the engine at
three places, and applying the transform at only the first would mean a watched
field decodes correctly on load and then silently stops decoding on its first
update.

One helper, `applyDecode(ref Ref, v Value) (Value, error)`, called from:

| Site | File | Path it covers |
|---|---|---|
| `resolveRef` | `resolve.go:140` | `Load`, `Doctor` (via `resolveChain`), and the reconciler's own re-resolves |
| `resolveBatchScheme` | `resolve.go:192` | any `BatchProvider` |
| `recordSourceUpdate` | `reconciler.go:670` | every native watch and poll update |

`recordSourceUpdate` is the single funnel for watch-path values (called from
`loop` at `reconciler.go:535`), so hooking it there covers native watches and
the polling adapter together. A decode failure arriving on the watch path is
delivered as an `Update.Err` classified `KindInvalid` and leaves the last-good
value in place, matching how the engine already treats a transient resolve
failure.

### 6.4 Redaction

`decode` carries no secret and is not added to the redaction denylist, so it
stays visible in `Status()` and `mamori doctor` output. That is deliberate: an
operator debugging a garbled value needs to see that a decoding step is in play.

## 7. Feature 3: Ref interpolation

### 7.1 Grammar

`${VAR}` substitution over the raw tag string, performed in `walkSpecs`
(`decode.go`) **before** `ParseRefs` (`ref.go:149`). Expanding the whole tag
rather than a parsed `Ref` means a variable can supply a scheme, any part of a
path, a fragment, or a query value, with one rule instead of four.

```go
type Config struct {
    Pass secret.String `source:"aws-sm://${ENVIRONMENT}/db#password"`
    Port string        `source:"env:PORT,aws-ps://${SERVICE}/port" default:"8080"`
}

w, err := mamori.Watch[Config](ctx,
    mamori.WithRefVars(map[string]string{
        "ENVIRONMENT": "prod",
        "SERVICE":     "checkout",
    }),
)
```

- Only the braced form is recognized. Bare `$VAR` is left alone, so passwords,
  `exec:` commands, and any path containing `$` are unaffected.
- `$$` is a literal `$`.
- An unterminated `${` is a spec-walk error, not a literal.

### 7.2 API

```go
// WithRefVars supplies the variables available to ${VAR} expansion in source
// tags. Nothing is expanded unless it appears here: mamori never reads the
// ambient environment for this, because a ref decides which secret a process
// reads and expanding it from ambient state would let anything able to set an
// environment variable redirect that read.
func WithRefVars(vars map[string]string) Option

// EnvVars reads the named environment variables into a map suitable for
// WithRefVars. Naming each variable is the point: it keeps the set of things
// that can influence a ref enumerable and greppable at the call site.
func EnvVars(names ...string) map[string]string
```

Applying `WithRefVars` more than once merges, with later calls winning per key.
This differs from `WithAuth`, which rejects a second application, because
merging maps has one obvious meaning while "which authenticator wins" has two.

### 7.3 Failure

An unset variable fails `Load`, `Watch`, and `Doctor` at construction, naming
the field, the ref, and the variable:

```
mamori: field Pass: source "aws-sm://${ENVIRONMENT}/db#password":
        undefined ref variable "ENVIRONMENT" (pass it with WithRefVars)
```

Expanding to empty instead would yield `aws-sm:///db`, which resolves
not-found and then quietly takes the field's `default:`, converting a
deployment misconfiguration into a silently wrong value. That is precisely what
`applyOnFail`'s existing "an error must fail loudly unless the field explicitly
opts in" rule (`resolve.go:161-190`) exists to prevent.

### 7.4 Visibility

After expansion, `Ref.Raw` holds the **expanded** string, so `Status()`,
`Report`, and `mamori doctor` show the ref that was actually resolved, which is
what an operator debugging a bad deploy needs.

The consequence goes in the docs as an explicit warning: **`WithRefVars` values
must not be secrets**, because they become visible in the admin report. Variables
are for environment names, regions, service names, and tenant identifiers.

### 7.5 Interaction with chains

Expansion runs on the whole tag before `ParseRefs` splits it, so a variable may
appear in any position of a precedence chain. A variable whose value contains a
comma followed by a scheme-like token would change how the chain splits; since
values come only from the operator via `WithRefVars` (D6), this is a caller
error rather than an injection vector, and it is documented alongside
`ParseRefs`' existing `%2C` escape guidance.

## 8. Testing

- **`SelectKey` table tests** against RFC 6901's own example document, covering
  every escape, every array form, and each row of the 5.3 error table.
- **The array-of-objects document from 5.2** as a dedicated case, exercising
  index parsing, key/index interleaving, and the out-of-range branch together.
- **Backward-compatibility assertions**: `#ca.crt`, `#tls.key`, and
  `#application.properties` still resolve as literal top-level keys. This is the
  test that guards D1.
- **Round-trip and failure test per coding**, asserting `KindInvalid` on a
  corrupt payload for each.
- **A watch-path decode test**, proving `?decode=` still applies to the second
  and subsequent values a watched field receives. This guards the 6.3 risk
  directly and is the single most important new test in the spec.
- **A batch-path decode test**, same reasoning, through a `BatchProvider`.
- **Interpolation tests**: expansion in scheme, path, fragment, and query
  position; `$$` escaping; unset-variable error text; merge order across two
  `WithRefVars` calls; expansion inside a chain.
- **Two new `providertest` cases**: `JSONPointerSelection` and `DecodeOption`.
  `JSONPointerSelection` is opt-in via `providertest.Config.PointerRef`
  (see 5.5): a provider supplies a builder saying how to construct a ref whose
  fragment is a JSON selector, and the case skips when it is nil, because a
  provider whose fragment slot is a backend-native key is not defective for
  lacking pointer support. `DecodeOption` needs no opt-out: `?decode=` is a
  query option every provider must pass through untouched regardless of what
  its fragment means.
- **A fuzz target over `ParseRef` + `SelectKey`.** The repo has no fuzzing
  today; a grammar this size with hand-rolled parsing warrants one, and it costs
  a single `FuzzRefGrammar` function.
- Race detector on, `goleak` unchanged.

## 9. Documentation

Per the project rule that documentation ships with the feature, not after it.

| File | Change |
|---|---|
| `site/src/pages/docs/concepts/ref-grammar.md` | **New page.** There is no dedicated grammar page today; after this the grammar is large enough to need one. Full BNF, both fragment forms, the decode table, interpolation, and the 5.3 error table. |
| `site/src/pages/docs/concepts/index.md` | Link the new page. |
| `site/src/pages/docs/usage/index.md` | Nested selection and `?decode=` in the field-tag walkthrough. |
| `site/src/pages/docs/writing-a-provider/resolve.md` | `SelectKey`'s new contract, and that providers get pointer support for free by calling it. |
| `site/src/pages/docs/writing-a-provider/conformance.md` | The two new cases and `NoStructuredPayload`. |
| `site/src/pages/docs/providers/kubernetes.md` | An explicit note that `#ca.crt` remains a literal key and why. |
| `README.md` | Grammar bullets under "What makes it different"; the quick-start struct gains one nested example. |
| `providers/k8s/README.md` | Same literal-key note as the docs-site page. |
| `skills/mamori/SKILL.md` | The agent skill teaches the tag grammar; all three additions belong there or agents will keep emitting the old forms. |
| `CONTRIBUTING.md` | The provider checklist gains the two conformance cases. |
| `doc.go` | Package doc grammar summary. |

## 10. Delivery

Four stacked PRs on `xavier/new-features-2`, each self-contained and each
carrying its own documentation:

```
xavier/new-features-2
 ├─ PR 1  docs: spec for ref grammar and value pipeline      (this document)
 ├─ PR 2  feat: JSON Pointer nested key selection
 ├─ PR 3  feat: ?decode= value transforms
 └─ PR 4  feat: ref interpolation via WithRefVars
```

PR 4 goes last because it is the only one that changes construction-time
behavior; PRs 2 and 3 are pure resolve-path additions and are independently
revertible.

All three are additive: no existing ref changes meaning, no provider requires a
change, and the `Provider` and `BatchProvider` interfaces are untouched. The
release is therefore a minor version, not a major one.
