# mamori OpenFeature provider

`github.com/xavidop/mamori/providers/openfeature`

A [mamori](https://github.com/xavidop/mamori) provider for
**[OpenFeature](https://openfeature.dev)**, the vendor-neutral feature-flag
standard (a CNCF project). It registers the `openfeature` scheme and resolves
the evaluated value of a flag through whatever OpenFeature provider your
application has installed - LaunchDarkly, Flagsmith, GO Feature Flag, a
flat-file provider, or your own.

![conformance](https://img.shields.io/badge/providertest-passing-brightgreen)

Passes the mamori provider conformance kit (`providertest.Run`) against an
in-memory fake `openfeature.IClient`. See
[Verified vs. needs a live backend](#verified-vs-needs-a-live-backend).

## Install

```bash
go get github.com/xavidop/mamori/providers/openfeature
```

Import for its side effect (the package `init()` registers the provider):

```go
import _ "github.com/xavidop/mamori/providers/openfeature"
```

The package identifier is `openfeature`.

## Why use this instead of a vendor-specific provider

mamori already ships providers for individual flag vendors (`launchdarkly`,
`flagsmith`, `unleash`, `configcat`, `split`, `growthbook`, `flipt`, `goff`).
Use this one instead when your application already depends on the
[OpenFeature Go SDK](https://openfeature.dev/docs/reference/technologies/server/go)
directly - it lets a `source` tag read a flag through the exact same
`openfeature.SetProvider` wiring the rest of your code uses, with no second
vendor SDK or credential set to configure just for mamori.

## Scheme

```
openfeature://<flag-key>[#json-key][?type=bool|string|int|float|object]
```

| Part | Required | Meaning |
| --- | --- | --- |
| `<flag-key>` | yes | The flag key, exactly as passed to the OpenFeature client. |
| `#json-key` | no | Select one field from an object-valued flag with `mamori.SelectKey` (an RFC 6901 JSON Pointer such as `#/limits/maxMB`, or a literal top-level key). |
| `?type=` | no | Pin which OpenFeature evaluation method resolves the flag: `bool`, `string`, `int`, `float`, or `object`. See below. |

### Why `?type=` exists, and why you should set it

OpenFeature evaluates a flag **by type**: a caller has to pick
`BooleanValueDetails`, `StringValueDetails`, `IntValueDetails`,
`FloatValueDetails`, or `ObjectValueDetails` - there is no single "evaluate
this and tell me what it is" call, because a mamori ref alone (a bare flag
key) does not say what type the flag is.

- **With `?type=`**, this provider calls exactly the one evaluation method you
  named. This is the recommended, production setting: it costs exactly one
  evaluation against your OpenFeature provider, the same way any other
  mamori provider costs one backend call per resolve.
- **Without `?type=`**, the provider tries, in order, `object`, then `bool`,
  then `string`, stopping at the first one that does not report
  `TYPE_MISMATCH`. This is a convenience for exploration, not something to
  ship: a flag that only resolves via the untyped fallback pays for up to
  three evaluations against your vendor on every single resolve, and a vendor
  that rate-limits or bills per evaluation call will see that multiplication
  on every poll. `int` and `float` are deliberately **not** part of the
  fallback chain: OpenFeature itself has no single "numeric" evaluation
  method, only separate `IntValueDetails` and `FloatValueDetails` calls, so
  an auto chain that included them would have to guess int vs. float on top
  of guessing that the flag is numeric at all. A numeric flag always needs an
  explicit `?type=int` or `?type=float`.

If the fallback chain is exhausted (every one of object, bool, and string
reports `TYPE_MISMATCH`), Resolve returns an error satisfying
`errors.Is(err, mamori.ErrInvalid)` that tells you to pin `?type=`. It
deliberately does **not** report `mamori.ErrNotFound`: the flag exists (a
real `TYPE_MISMATCH` proves that), so treating this as not-found would make
mamori silently apply a `default:` value instead of surfacing a
misconfigured ref.

```go
type Config struct {
    NewCheckout bool          `source:"openfeature://new-checkout?type=bool"`
    LogLevel    string        `source:"openfeature://log-level?type=string"`
    MaxRetries  int           `source:"openfeature://max-retries?type=int"`
    Limits      LimitsConfig  `source:"openfeature://limits?type=object" flatten:"json"`
    MaxUploadMB int           `source:"openfeature://limits#/upload/maxMB?type=object"`
}
```

The evaluated value is rendered as bytes by type: a bool becomes `true` /
`false`, a string is returned raw, an int or float becomes its decimal form,
and an object becomes its JSON encoding (which `#json-key` then selects
from, exactly like every other mamori provider).

## Authentication and setup

This provider does not talk to any flag backend itself. It evaluates through
whatever `openfeature.FeatureProvider` **your application** has already
installed with `openfeature.SetProvider(...)` - LaunchDarkly's, Flagsmith's,
a flat-file provider, an in-memory one for tests, or your own. There is
nothing to configure here beyond that:

```go
import (
    "github.com/open-feature/go-sdk/openfeature"
    ofprov "github.com/xavidop/mamori/providers/openfeature"
)

// Your application wires up OpenFeature exactly as it already does, with
// whichever vendor SDK backs it:
openfeature.SetProvider(myVendorProvider)

// mamori then reads flags through that same provider - no separate mamori
// configuration needed:
cfg, err := mamori.Load[Config](ctx)
```

If no provider has been set yet when a ref is resolved, the OpenFeature SDK's
default no-op provider answers `PROVIDER_NOT_READY` for every flag, which
this package maps to `mamori.ErrUnavailable` - the same transient-failure
handling mamori gives any other backend that is not reachable yet, retried
on the next poll rather than treated as a permanent failure.

`New` performs no I/O: it never contacts anything, and the underlying
`openfeature.NewClient(...)` client is built lazily on first `Resolve`, not
at construction. Registering the provider from `init()` (via a blank import)
is always safe, even before your application has called `SetProvider`.

### Targeting key and attributes

Every OpenFeature evaluation is made against an **evaluation context**: a
targeting key plus optional attributes. This provider fixes one evaluation
context for the whole process:

```go
p := ofprov.New(
    ofprov.WithTargetingKey("checkout-service"),
    ofprov.WithAttributes(map[string]any{"region": "eu-west-1"}),
)
mamori.WithProvider(p)
```

- `WithTargetingKey` sets the context's targeting key. It defaults to
  `"mamori"` - a stable, non-anonymous key, the same default
  `providers/launchdarkly` uses for the identical reason: deterministic
  evaluation for configuration-style flags that are not meant to vary per
  end user.
- `WithAttributes` adds static attributes constant for the process (region,
  deployment tier, service name, ...).

**This is not per-user targeting.** A mamori field holds exactly one value
for the whole process - there is no per-request evaluation context, and
there cannot be: mamori resolves configuration into long-lived struct
fields, not into a value computed fresh for each incoming request. If you
need a flag whose value varies per end user, request, or tenant, evaluate it
directly through your application's own OpenFeature client at the point you
handle that request; do not route per-user targeting through a mamori config
field.

### Injecting a client

For tests, or to route through a specific named OpenFeature client instead of
the default, inject one with `WithClient`:

```go
p := ofprov.New(ofprov.WithClient(myClient)) // myClient implements openfeature.IClient
```

## Sensitivity

Resolved values are **not** marked sensitive (`Value.Sensitive == false`) -
an OpenFeature flag is configuration, the same as every other mamori flag
provider. Wrap a field in `secret.String` yourself if you want redaction
anyway.

`Value.Version` is the evaluation's `Variant` when the provider reports one
(non-empty), and falls back to `mamori.VersionHash` of the resolved bytes
otherwise - cheap change detection either way, since not every OpenFeature
provider returns a variant for every flag.

## Watch (polling, no native push)

This provider is **not watchable**. OpenFeature does define provider-level
change events, but there is no cross-vendor, per-flag "this value changed"
signal exposed uniformly enough at the `IClient` level for a generic
provider to subscribe to. mamori polls it on the configured interval
instead, re-evaluating on each poll. Do not expect push updates.

## Error classification

Beyond not-found, `Resolve` classifies an evaluation failure from the
[OpenFeature error code](https://openfeature.dev/specification/types#error-code)
carried on the evaluation's `ResolutionDetail`, which is the only place a
real OpenFeature client exposes a stable, cross-vendor failure signal (the
SDK's `ResolutionError` keeps its code unexported and offers no accessor):

| OpenFeature error code | mamori kind |
| --- | --- |
| `FLAG_NOT_FOUND` | `not_found` |
| `TYPE_MISMATCH` | `invalid` |
| `PARSE_ERROR` | `invalid` |
| `INVALID_CONTEXT` | `invalid` |
| `TARGETING_KEY_MISSING` | `invalid` |
| `PROVIDER_NOT_READY` | `unavailable` |
| `PROVIDER_FATAL` | `unavailable` |
| `GENERAL` | `unknown` (deliberately unmapped) |

`GENERAL` is OpenFeature's catch-all error code and, by the spec, carries no
committed meaning beyond "something went wrong" - it says nothing about
whether the underlying cause is transient, a permission problem, or a bug in
the provider being evaluated. Guessing at a more specific kind for it would
send an operator investigating a failure down the wrong path, so it is left
unclassified rather than guessed at, the same way `providers/aws` leaves
`DecryptionFailure` unclassified for an analogous reason. The underlying
error is never lost either way: `Resolve` wraps it with `%w`, so
`errors.Is` / `errors.As` still reach it and any mamori sentinel it happens
to already carry survives.

## Verified vs. needs a live backend

**Verified in unit tests (no live OpenFeature provider needed):** scheme and
registration, `?type=` parsing (including the empty and unrecognized cases),
value rendering for every type, the untyped fallback chain's order and its
stopping behavior (verified by call-count assertions, not just the returned
value), the exhausted-chain error's classification, `#json-key` selection,
`Sensitive == false`, `Value.Version` preferring `Variant` over
`VersionHash`, targeting key and attributes wiring, not-found, all eight
OpenFeature error codes' classification, and the full `providertest.Run`
conformance suite against an in-memory fake `openfeature.IClient`.

**Needs a live backend:** none. Because this provider evaluates through
whatever `openfeature.FeatureProvider` your application installs, "a live
backend" is a choice your application already makes independently of
mamori - there is no vendor-specific integration test to run here. If you
want to exercise this provider against a real flag vendor, install that
vendor's OpenFeature provider with `openfeature.SetProvider` in your own
application and let `openfeature.New()` (a blank import, or an explicit
`ofprov.New()`) evaluate through it.

## Development

```bash
cd providers/openfeature
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
