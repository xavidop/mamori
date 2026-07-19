# mamori - GO Feature Flag (goff) provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves values from
[GO Feature Flag](https://gofeatureflag.org/) (`github.com/thomaspoignant/go-feature-flag`),
the standalone, file-driven feature-flag engine (the embedded `ffclient`, not a
remote relay or vendor SaaS). Each ref evaluates a flag's variation and returns
it as a config value.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/goff"
```

Importing the package registers the `goff` scheme with mamori. The go-feature-flag
client is built lazily on first use from the ambient configuration (the
`GOFF_CONFIG` environment variable), so importing the package never reads a file
or contacts an endpoint.

## Scheme

```
goff://<flag-key>[#json-key]
```

- `<flag-key>` - the feature-flag key as defined in your go-feature-flag
  configuration, e.g. `new-checkout`.
- `#json-key` - optional. When the flag's variation is a JSON object, the named
  field is selected via `mamori.SelectKey` (the same behavior as every other
  mamori provider). String fields are returned unquoted; objects, arrays,
  numbers, and booleans are returned as their JSON encoding.

A ref resolves to the flag's evaluated variation for an evaluation context (see
[Targeting key](#targeting-key)). The variation value is rendered to bytes by
type:

| Variation type | Rendered bytes |
| --- | --- |
| bool | `"true"` / `"false"` |
| string | the raw string |
| number | its decimal string form, e.g. `"100"`, `"3.5"` |
| JSON object / array | its JSON encoding, e.g. `{"theme":"dark"}` |

### Ref examples

| Ref | Meaning |
| --- | --- |
| `goff://new-checkout` | Evaluated variation of flag `new-checkout` |
| `goff://api-ratelimit` | Number variation of flag `api-ratelimit`, e.g. `100` |
| `goff://ui-config#theme` | Field `theme` of the JSON variation of flag `ui-config` |

```go
type Config struct {
    Beta      bool   `source:"goff://new-checkout"`
    RateLimit int    `source:"goff://api-ratelimit"`
    Theme     string `source:"goff://ui-config#theme"`
}
```

### Value semantics

- `Value.Bytes` - the rendered variation (see the table above).
- `Value.Version` - `mamori.VersionHash` of the rendered bytes. go-feature-flag
  has no per-evaluation native revision, so the hash gives cheap, exact change
  detection: the version changes when, and only when, the evaluated value
  changes.
- `Value.Sensitive` - always `false`. Feature flags are configuration, not
  managed secrets. Wrap a field in `secret.String` if you want redaction anyway.
- `Value.Metadata` - carries `variationType`, `reason`, and (when the flag
  declares one) `flagVersion`. It never contains the value payload.
- A flag that does not exist returns an error satisfying
  `errors.Is(err, mamori.ErrNotFound)`. go-feature-flag reports this as a
  `FLAG_NOT_FOUND` error code / `ERROR` reason, which the provider detects. A
  missing `#json-key` inside an existing JSON variation is likewise a typed
  not-found.

## Configuration (retriever)

go-feature-flag loads its flag definitions from a **retriever** - a local file,
an HTTP(S) URL, S3, GCS, a Kubernetes ConfigMap, and so on - and reloads them on
a polling interval. This provider wires up the two most common retrievers and
defaults from an environment variable:

| Source | How to set it |
| --- | --- |
| Local file (YAML / JSON / TOML) | `goff.WithConfigFile("flags.yaml")` |
| HTTP(S) endpoint | `goff.WithConfigURL("https://example.com/flags.yaml")` |
| Environment default | `GOFF_CONFIG=...` (a value beginning with `http://` or `https://` is treated as a URL; anything else as a file path) |

If no source is configured and `GOFF_CONFIG` is unset, `Resolve` returns a
descriptive error (not a not-found).

For explicit configuration, construct the provider yourself and register it:

```go
p := goff.New(
    goff.WithConfigFile("/etc/app/flags.yaml"),
    goff.WithTargetingKey("service-a"),
    goff.WithPollingInterval(30*time.Second),
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

Need a retriever this provider does not expose directly (S3, GCS, ConfigMap,
Git, ...)? Build the `*ffclient.GoFeatureFlag` client yourself with the
retriever of your choice and inject it - the client satisfies the provider's
evaluator interface. (`WithConfigFile` / `WithConfigURL` cover the common cases;
richer wiring is a small amount of glue against `ffclient.New`.)

### Targeting key

Every resolution uses an **anonymous** evaluation context whose targeting (user)
key selects which percentage bucket / targeting rule a flag evaluates to. It
defaults to `"mamori"` and is overridden with `goff.WithTargetingKey("...")`. Use
a stable key per service/tenant if your flags use percentage rollouts or
targeting rules and you want deterministic, consistent evaluation.

### Options

| Option | Effect |
| --- | --- |
| `WithConfigFile(path)` | Load flag definitions from a local file |
| `WithConfigURL(url)` | Load flag definitions from an HTTP(S) endpoint |
| `WithTargetingKey(key)` | Targeting key of the anonymous context (default `"mamori"`) |
| `WithPollingInterval(d)` | How often go-feature-flag reloads definitions (default 60s) |

## Authentication

go-feature-flag itself needs no credentials to evaluate flags - evaluation is
local, against an in-memory cache of the flag definitions. Any credentials are a
property of the **retriever**: an HTTP endpoint may need headers, an S3/GCS
bucket needs the ambient cloud credential chain, and so on. The file and HTTP
retrievers exposed here are typically used against an unauthenticated or
network-protected endpoint.

## Reload (polling, no native watch)

This provider does **not** implement `mamori.WatchableProvider`. go-feature-flag
has no native change-push mechanism; instead it **polls** its retriever on the
configured interval and refreshes its in-memory cache. mamori independently polls
`Resolve`. So a changed flag file is picked up automatically - by go-feature-flag
on its poll, then surfaced by mamori on its own poll - with no faked ticker in
the provider (per the SPI, providers must never simulate a watch). Tune
freshness with `WithPollingInterval` on this side and mamori's poll interval on
the other.

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-memory fake evaluator (`GOWORK=off go test ./...`), no flag file needed |
| Resolve, type mapping (bool / string / number / JSON), JSON `#key` selection, not-found, version monotonicity, context cancellation, concurrency, goroutine hygiene | **Verified** (unit + conformance tests) |
| Targeting-key propagation into the evaluation context | **Verified** (unit test) |
| End-to-end against a **real** `ffclient` loaded from a file, including FLAG_NOT_FOUND and poll-interval hot-reload | **Needs the integration build tag** - see below |

The unit and conformance tests use an in-memory fake that returns `FLAG_NOT_FOUND`
for un-seeded flags, so `go test ./...` requires **no** flag-configuration file
and starts **no** background poller (keeping the goroutine-leak check clean).

### Live integration test

An integration test exercises a real go-feature-flag client loaded from an
on-disk flag file (no external service required). It is guarded by a build tag:

```sh
GOWORK=off go test -tags integration -run Integration ./...
```

It seeds a temp flag file, resolves bool/string/number/JSON variations, asserts a
missing flag is `ErrNotFound`, and rewrites the file to prove poll-interval
hot-reload.

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/goff
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
