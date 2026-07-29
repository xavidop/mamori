# OpenFeature provider design

**Status:** approved
**Date:** 2026-07-29

Adds OpenFeature as an ordinary mamori provider module, `providers/openfeature`,
registering the scheme:

```
openfeature://<flag-key>[#json-key][?type=<t>]
```

## Direction, and why the other one was rejected

Two bridges were possible and only one is safe.

The rejected one was mamori acting as an OpenFeature `FeatureProvider`, so that
application code calls `client.Boolean(ctx, "flag", false, evalCtx)` and mamori
answers. OpenFeature evaluates per call, with a targeting context naming *who
is asking*. mamori resolves a ref once, at startup, into a single struct field.
There is one value, not one per user.

A caller writing `client.Boolean(ctx, "new-checkout", false, aliceCtx)` would
therefore get an answer that ignores `aliceCtx` entirely, while the code reads
as though targeting happened. Nothing errors and nothing looks wrong in review.
The failure appears only in production, as a rollout intended for ten percent
of users reaching all of them. OpenFeature does define a `STATIC` reason for
"not computed per evaluation", so the bridge could be built honestly, but a
warning label on a footgun is still a footgun, and the benefit it buys is
vendor-neutral application code, which is largely what writing mamori refs
already buys.

This design takes the other direction. OpenFeature becomes a backend mamori
reads from, like the thirty-three others. A `source:"openfeature://new-checkout"`
tag obviously fills one field; nobody expects per-user targeting from it, so
there is nothing to be misled by. What it adds is reach: every OpenFeature
vendor becomes usable from mamori, including flagd and the many for which
mamori has no dedicated provider, in one small module.

## Conventions

This provider follows the eight feature-flag providers already in the tree
(`configcat`, `launchdarkly`, `growthbook`, `flagsmith`, `split`, `unleash`,
`flipt`, `goff`) rather than inventing anything:

- Ref shape `<scheme>://<flag-key>[#json-key]`, matching `launchdarkly` and
  `growthbook` exactly.
- The evaluated value is rendered as text and decoded into the field's Go type
  by core, as every flag provider does.
- Evaluation happens against one fixed identity, configurable by an option,
  exactly as `launchdarkly`'s `WithContextKey` does (defaulting to `"mamori"`).
- `Value.Sensitive = false`. A flag is configuration, not a secret.
- No `Watch`. All eight sibling flag providers are polled by mamori, and
  `provider.go` reserves `WatchableProvider` for backends with native change
  notification. The OpenFeature SDK does expose a provider-event API, which
  would be a legitimate basis for a future watch, but it is out of scope here.

## Client and identity

The provider evaluates through `openfeature.IClient`, the SDK's own interface,
which is the injection seam for tests.

By default it uses `openfeature.NewClient("mamori")`, so the zero-config path
resolves against whatever provider the application has already installed with
`openfeature.SetProvider`. This means construction performs no I/O and cannot
fail, so `init()` registration is safe even when no OpenFeature provider has
been set yet. An evaluation made before one is ready returns
`PROVIDER_NOT_READY`, which maps to `ErrUnavailable` and is retried by mamori
like any other transient backend failure.

Options:

- `WithClient(c openfeature.IClient)` injects a client, for tests or for an
  application that keeps a named client of its own.
- `WithTargetingKey(k string)` sets the evaluation context's targeting key,
  defaulting to `"mamori"`.
- `WithAttributes(m map[string]any)` adds static evaluation-context attributes,
  so a deployment can target by region or tier even though the identity is
  fixed for the process.

## Flag types

OpenFeature evaluates by type: a caller must pick `BooleanValueDetails`,
`StringValueDetails`, and so on. A mamori ref does not say what type a flag is,
and the Go field type is not visible to a provider.

`?type=` resolves this explicitly, accepting `bool`, `string`, `int`, `float`,
and `object`.

When `?type=` is absent the provider tries `object`, then `bool`, then
`string`, stopping at the first that does not report `TYPE_MISMATCH`. This
mirrors the documented fallback `flipt` already performs (variant evaluation
first, boolean on an invalid-type error), so it is an established pattern in
this tree rather than a novelty. The chain is bounded at three calls and the
documentation recommends pinning `?type=` in production to make it one.

`#json-key` selects a field out of an object-typed flag's value using
`mamori.SelectKey`, so literal keys and RFC 6901 JSON Pointers both work and
`providertest.Config.PointerRef` applies.

## Rendering and versioning

A boolean, string, or numeric flag is rendered as its plain text form. An
object flag is rendered as JSON, which is also what `#json-key` selects
against.

`Value.Version` prefers the resolution detail's `Variant`, which is
OpenFeature's own name for which variation was served and is exactly the right
notion of a version. It falls back to `mamori.VersionHash(data)` when a
provider returns no variant, which is common for flags with no named
variations.

## Error classification

OpenFeature normalizes backend failures into its own `ErrorCode`, so the
mapping is a small closed set rather than a per-vendor guess:

| OpenFeature `ErrorCode` | mamori kind |
| --- | --- |
| `FLAG_NOT_FOUND` | `not_found` |
| `TYPE_MISMATCH` | `invalid` |
| `PARSE_ERROR` | `invalid` |
| `INVALID_CONTEXT` | `invalid` |
| `TARGETING_KEY_MISSING` | `invalid` |
| `PROVIDER_NOT_READY` | `unavailable` |
| `PROVIDER_FATAL` | `unavailable` |
| `GENERAL` | unclassified, reports `unknown` |

`GENERAL` is deliberately left unmapped. It is OpenFeature's catch-all and
carries no information about whether the cause is transient, a permission
problem, or a bug, so guessing would send an operator down the wrong path.
This matches how `classifyAWS` leaves `DecryptionFailure` unmapped for the same
reason.

## Testing

The provider is tested against an in-memory fake implementing
`openfeature.IClient`, as every other provider in the tree injects a client
interface.

It passes `providertest.Run` with `Seed`, `Mutate`, `Fail`, `Clear`, and
`PointerRef` supplied, and adds a table test mapping every `ErrorCode` above to
its mamori kind, as `CONTRIBUTING.md` step 3 requires.

Two behaviours get dedicated tests beyond the kit:

- The untyped fallback chain tries `object`, then `bool`, then `string`, and
  stops at the first success. A test asserts both the ordering and that it
  stops, since a chain that kept going would turn one evaluation into three
  against a rate-limited vendor.
- `?type=` pins the evaluation to exactly one call, with no fallback. This is
  the property the documentation tells operators to rely on.

## Documentation

Module `README.md`, a docs-site page, a row in both coverage tables, an
`## Error classification` section in README and site page, and the `skills/`
provider reference. The README states plainly that evaluation identity is fixed
per process and that per-user targeting is not what this provider does, so the
rejected direction's confusion cannot re-enter through the docs.
