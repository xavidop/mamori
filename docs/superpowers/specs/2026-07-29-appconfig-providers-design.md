# AppConfig providers design

**Status:** approved
**Date:** 2026-07-29

Adds the AWS and Azure dynamic-configuration services to mamori as two new
schemes, both inside the provider modules that already exist:

- `aws-appconfig://<app>/<env>/<profile>[#key][?opts]` in `providers/aws`
- `azure-appconfig://<store>/<key>[#json-key][?label=<l>]` in `providers/azure`

## Why these two, and not more secret stores

mamori already ships 32 providers, and the great majority are secret stores
that differ only in their auth dance and their URL shape. Infisical, Akeyless,
Bitwarden SM, Conjur, and Nacos were all considered for this batch and cut.
Each would add a module, a `go.mod`, a CI lane, and a maintenance burden while
teaching the framework nothing it cannot already do.

AWS AppConfig and Azure App Configuration are different in kind. They are the
two cloud vendors' answers to the same problem mamori solves - configuration
that changes underneath a running process - and neither is currently reachable
from a `source:` tag. They also land inside `providers/aws` and
`providers/azure`, reusing `classifyAWS`, `classifyAzure`, and both modules'
existing credential plumbing, so the incremental cost is a file each rather
than a module each.

## Sensitivity

Both providers set `Value.Sensitive = false`, matching every other
configuration-shaped provider in the repo (`consul`, `etcd`, `configcat`,
`launchdarkly` all set `false`; the secret stores set `true`). Neither service
is a secret manager, and both vendors document referencing a secret store for
secret material rather than storing it inline.

## AWS AppConfig

### The protocol does not fit `Resolve` directly

`appconfigdata` is a session protocol, not a request/response API, and three
of its properties are hostile to a naive port. All three are quoted from the
SDK's own documentation:

1. `GetLatestConfigurationOutput.Configuration` - "This may be empty if the
   client already has the latest version of configuration."
2. `NextPollConfigurationToken` - "This token should only be used once... If a
   `GetLatestConfiguration` call uses an expired token, the system returns
   `BadRequestException`."
3. `StartConfigurationSessionInput.RequiredMinimumPollIntervalInSeconds` sets a
   floor the *server* enforces on how often the client may call.

Property 1 is the dangerous one. A provider that opened one session and reused
it across `Resolve` calls would return the full payload the first time and
**empty bytes** every time after, and mamori would faithfully apply that empty
value to the field. The failure is silent and looks like a config wipe.

### Resolution: two paths, one session model each

**`Resolve` starts a fresh session per call**, issues one
`GetLatestConfiguration`, and discards the session. A new session has no
client-known version, so its first call always returns the full payload. This
costs two API calls per resolve and buys statelessness: property 1 is
structurally unreachable here, because the provider never holds a token long
enough for the service to consider it current.

#### An empty payload on a fresh session is an error, not an empty value

The paragraph above rests on one assumption: that the first
`GetLatestConfiguration` of a brand-new session always returns data. That
assumption was checked against the API reference and the user guide, which
agree - the payload "may be empty **if the client already has the latest
version**", and a session created moments ago has no version at all. The API
reference's own sample response shows a call returning a full body.

It was worth checking, because a plausible-sounding secondary summary claimed
the exact opposite (that the first call establishes a baseline and returns
empty). That reading is contradicted by both primary sources and is not
believed to be correct.

The provider is nevertheless written to be correct under either reading. If
`Resolve` receives an empty `Configuration` from a session it just created, it
returns an error, never a zero-length `Value`. Under the verified reading that
branch is unreachable. Under the wrong one it fails loudly on the first call
instead of silently applying empty bytes over a live configuration field.

The asymmetry is the whole point: one branch costs a few lines and an error
path that may never fire, and the other costs a silent production config wipe.
Being wrong about the protocol is cheap; trusting it is not.

**There is no `Watch`.** mamori polls this provider through `pollWatch`, like
every other backend without native change notification.

An earlier revision of this design specified a `WatchableProvider`
implementation that owned a long-lived session, threaded
`NextPollConfigurationToken` across calls, and slept
`NextPollIntervalInSeconds`. It was built, and then dropped. Two reasons, in
order of weight.

The first is that `provider.go` forbids it, on the interface itself:

> Providers without native watch support are wrapped by mamori in a polling
> adapter - provider authors must never fake a Watch with an internal ticker.

AppConfig has no push mechanism of any kind; a session is still polling, just
polling that remembers. The two providers in the repo that do implement `Watch`
(`k8s` via informers, `sops` via fsnotify) are genuinely event-driven. A
timer-driven AppConfig watch would have been the first ticker in the tree.

The rule is load-bearing rather than stylistic. `pollWatch` supplies jitter
(`jittered(interval, o.jitter)`), change deduplication, `ErrNotFound`
suppression, and a `Clock` that tests can fake. A provider-side loop forfeits
all four. Jitter matters most here: AppConfig hands every session the same
cadence, so replicas that start together would poll in lockstep forever against
an API that AWS documents as priced.

The second reason is that the strongest argument *for* the session did not
survive checking. That argument was that the service enforces a minimum poll
interval, so mamori's `WithPollInterval` could violate it. It cannot:
`RequiredMinimumPollIntervalInSeconds` constrains calls made *within one
session*, and the `Resolve` path creates a fresh session for every call, so the
floor is never approached.

What remains is a cost difference. Polling `Resolve` spends two API calls
(`StartConfigurationSession` plus `GetLatestConfiguration`) where a resident
session would spend one. That is a real cost against a priced API, but it does
not outweigh an explicit project rule plus the loss of jitter on a
thundering-herd-prone workload.

If the cost later justifies acting, the principled fix is in core, not here: an
optional interface by which a provider suggests a poll interval, with
`pollWatch` honoring it. That keeps jitter and backoff centralized and needs no
exception to the rule.

### Ref grammar

```
aws-appconfig://<application>/<environment>/<profile>[#key][?minPoll=<seconds>]
```

All three path segments are required by `StartConfigurationSession` and may be
either IDs or names; the provider passes them through verbatim and lets the
service resolve them. A path with anything other than three non-empty segments
is `ErrInvalid`.

`#key` selects into the payload with `mamori.SelectKey`, so both literal keys
and RFC 6901 JSON Pointers work and `providertest.Config.PointerRef` applies.

`?minPoll=<seconds>` sets `RequiredMinimumPollIntervalInSeconds` on the
session. With no `Watch` path it has no observable effect, since every session
is discarded after one call and the floor constrains only a session's second
and later calls. It is kept because it is the correct plumbing for the field,
costs nothing, and would become meaningful the moment a resident session
exists. It is documented as inert rather than presented as a tuning knob.

### Version

`VersionLabel` when the service supplies one, `mamori.VersionHash(data)`
otherwise. The fallback is load-bearing rather than defensive: the SDK
documents `VersionLabel` as empty for any configuration that is not an
AppConfig-hosted version, and empty again whenever the client already has the
latest version.

## Azure App Configuration

A conventional request/response provider, shaped like the existing `azure-kv`
provider in the same module: `<store>` becomes `https://<store>.azconfig.io`,
the same lazily-resolved ambient credential is used, and the same
`classifyAzure` maps HTTP status onto mamori's sentinels.

### Ref grammar

```
azure-appconfig://<store>/<key>[#json-key][?label=<label>]
```

An absent `?label=` means Azure's *null label*, not "any label". The two are
distinct in the service, and a setting stored under the null label is a
different setting from one stored under the label `prod`.

`Version` comes from the setting's ETag.

### Key Vault references are rejected, not resolved

Azure App Configuration supports storing a *reference* to a Key Vault secret: a
setting whose content type is
`application/vnd.microsoft.appconfig.keyvaultref+json` and whose value is
`{"uri":"https://<vault>.vault.azure.net/secrets/<name>"}`. Resolving one means
a second authenticated call into Key Vault.

This provider does not resolve them. It detects that content type and fails
with `ErrInvalid`, naming the equivalent `azure-kv://` ref in the error
message.

Returning the raw JSON would be worse than failing. A caller whose field is
`Password string` would receive the literal text `{"uri":"https://..."}` and
mamori would apply it, validate it as a non-empty string, and hand it to a
database driver. The failure would surface as an authentication error far from
its cause. Failing at resolve time, with the ref the user should have written,
is the strictly better outcome. Resolution can be added later without breaking
anyone, since every ref that would newly succeed currently errors.

## Testing

Both providers inject a client interface and are tested against an in-memory
fake, matching `SMProvider`, `PSProvider`, and the Key Vault provider.

Both pass `providertest.Run` with `Seed`, `Mutate`, `Fail`, and `Clear`
supplied, and both set `PointerRef` since both select into JSON payloads with
`SelectKey`.

Both add a table test mapping real SDK error values onto mamori's error kinds,
as `CONTRIBUTING.md` step 3 requires - the conformance kit's
`ErrorClassification` case proves a mapping survives transit, not that one
exists.

The AWS fake models the session protocol itself, since that is where the risk
lives. It must issue single-use tokens, reject a reused or unknown token the
way the service does, return the full payload on a fresh session's first call,
and return empty `Configuration` on any later call whose session already has
the current version. A fake that always returns the payload would pass a
provider carrying the exact bug this design exists to avoid.

Two tests target the session semantics directly, because the conformance kit
cannot see them:

- Resolving the same ref twice returns the full payload **both** times. This is
  the regression test for the reuse-a-session bug: a provider that cached its
  session would return empty on the second call and fail here.
- A fresh session whose first call returns empty produces an error and not an
  empty `Value`, per the defensive branch above.

## Documentation

Per repo convention, each provider ships with its module `README.md` updated, a
docs-site page under `site/`, a row in both provider coverage tables (root
`README.md` and `site/src/pages/docs/providers/index.md`), and an
`## Error classification` section in both the module README and the site page.

## Delivery

Two stacked PRs on top of #62:

1. `aws-appconfig` - resolve only; mamori polls it.
2. `azure-appconfig` - independent of it.

A third PR for `aws-appconfig` `Watch` was planned and cut, for the reasons in
the AWS section above. Splitting it out is what made cutting it cheap: task 1
was complete and reviewed on its own, so dropping the watch work cost nothing
already delivered.

The stack base is #62 rather than `main` because
`providertest.Config.PointerRef` is introduced in #52/#53 and has not merged
yet.
