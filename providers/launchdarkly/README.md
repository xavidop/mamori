# mamori - LaunchDarkly provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves
configuration values from [LaunchDarkly](https://launchdarkly.com/) feature
flags, with **native hot-reload** driven by LaunchDarkly's streaming flag
tracker.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/launchdarkly"
```

Importing the package registers the `launchdarkly` scheme with mamori. The
LaunchDarkly SDK client is built lazily on first use from the ambient
environment, so importing the package never contacts LaunchDarkly.

## Scheme

```
launchdarkly://<flag-key>[#json-key]
```

- `<flag-key>` - the LaunchDarkly feature flag key, e.g. `new-checkout-enabled`.
- `#json-key` - optional. When present, the flag value is treated as a JSON
  object and the named field is selected via `mamori.SelectKey` (the same
  behavior as every other mamori provider). String fields are returned unquoted;
  objects, arrays, numbers, and booleans are returned as their JSON encoding.

### Ref examples

| Ref | Meaning |
| --- | --- |
| `launchdarkly://new-checkout-enabled` | Value of the boolean flag `new-checkout-enabled` |
| `launchdarkly://log-level` | Value of the string (multivariate) flag `log-level` |
| `launchdarkly://max-retries` | Value of the numeric flag `max-retries` |
| `launchdarkly://api-config#timeout` | Field `timeout` of the JSON-valued flag `api-config` |

```go
type Config struct {
    CheckoutV2 bool   `source:"launchdarkly://new-checkout-enabled"`
    LogLevel   string `source:"launchdarkly://log-level"`
    MaxRetries int    `source:"launchdarkly://max-retries"`
    APITimeout string `source:"launchdarkly://api-config#timeout"`
}
```

### How flag values map to config values

The flag is evaluated with the SDK's JSON "detail" variation, so the provider
receives both the value and the evaluation reason. The value is converted to
`Value.Bytes` by type:

| LaunchDarkly flag value | Bytes |
| --- | --- |
| Boolean | `true` or `false` |
| String | the raw string text (unquoted) |
| Number | its shortest decimal form (e.g. `5432`, `0.25`) |
| JSON object or array | its JSON encoding (e.g. `{"timeout":"5s"}`) |

- `Value.Version` - a content hash of the resolved bytes
  (`mamori.VersionHash`). LaunchDarkly does not expose a server-side flag
  revision to the SDK, so the hash provides cheap change detection: the version
  changes whenever the value changes.
- `Value.Sensitive` - always `false`. LaunchDarkly holds feature flags and
  configuration, not managed secrets. Wrap a field in `secret.String` if you
  want redaction anyway.
- A flag that does not exist (evaluation reason `ERROR` with error kind
  `FLAG_NOT_FOUND`) returns an error satisfying
  `errors.Is(err, mamori.ErrNotFound)`, so defaults and optional fields apply.

### Evaluation context

Every LaunchDarkly evaluation requires an evaluation context. This provider uses
a single, non-anonymous context whose key defaults to `mamori`. A stable key
gives deterministic results for configuration-style flags that are the same for
everyone (served via fallthrough or the off variation).

If your flags are targeted by context, override the key so the flag is evaluated
for the identity you care about (for example, a tenant or environment
identifier):

```go
p := launchdarkly.New(
    launchdarkly.WithSDKKey(os.Getenv("LAUNCHDARKLY_SDK_KEY")),
    launchdarkly.WithContextKey("tenant-acme"),
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

For flags that are not targeted at all, the default `mamori` context is
sufficient.

## Authentication & configuration

The provider needs a LaunchDarkly **server-side SDK key**. It is read, in order
of precedence, from:

1. The `WithSDKKey(...)` option.
2. The `LAUNCHDARKLY_SDK_KEY` environment variable.

| Variable | Purpose |
| --- | --- |
| `LAUNCHDARKLY_SDK_KEY` | Server-side SDK key, e.g. `sdk-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx` |

If no SDK key is configured, `Resolve`/`Watch` return an error. The client is
created on first use and connects to LaunchDarkly (streaming) at that point.

For explicit configuration, construct the provider yourself and register it:

```go
p := launchdarkly.New(launchdarkly.WithSDKKey("sdk-..."))
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

Or inject a fully custom `*ldclient.LDClient` (relay proxy, custom data source,
big segments, ...):

```go
client, _ := ldclient.MakeCustomClient("sdk-...", ldConfig, 10*time.Second)
p := launchdarkly.New(launchdarkly.WithClient(client))
```

### Options

| Option | Effect |
| --- | --- |
| `WithSDKKey(key)` | Set the server-side SDK key (overrides `LAUNCHDARKLY_SDK_KEY`) |
| `WithContextKey(key)` | Set the evaluation context key (defaults to `mamori`) |
| `WithClient(*ldclient.LDClient)` | Inject a pre-configured LaunchDarkly client |

## Native watch (streaming flag tracker)

The provider implements `mamori.WatchableProvider` using **LaunchDarkly's native
flag tracker** - the SDK's streaming change mechanism, not a polling ticker:

1. On `Watch`, the provider calls
   `GetFlagTracker().AddFlagValueChangeListener(flagKey, context, default)`.
2. The SDK re-evaluates the flag whenever its configuration changes and pushes a
   `FlagValueChangeEvent` **only when the value for the evaluation context
   actually changes**. The provider converts each event's new value to bytes
   (applying `#json-key` selection when present) and emits an `Update`.
3. When the watch context is cancelled the provider removes the listener and
   closes the Update channel, so the goroutine exits with no goroutine leaks
   (verified with `goleak`).

Because the flag tracker only fires on subsequent changes, no baseline `Update`
is emitted at subscription time; mamori already holds the value from the initial
`Resolve`.

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-memory fake client with flag evaluation + value-change streaming (`go test ./...`) |
| Value mapping (bool, string, number, JSON object/array), JSON `#key` selection | **Verified** (unit tests) |
| Not-found (`FLAG_NOT_FOUND` reason), version change on value change, context cancellation | **Verified** (unit tests) |
| Native watch (change delivery + cancel/close, no goroutine leak) | **Verified** against the fake (including `-race`) |
| Missing-SDK-key error, context-key option | **Verified** (unit tests) |
| End-to-end against a real LaunchDarkly environment | **Needs a live backend** - see the integration test |

The unit and conformance tests use an in-memory fake that reproduces
LaunchDarkly's flag evaluation (with `FLAG_NOT_FOUND` for unseeded flags) and its
value-change event stream, so `go test ./...` requires **no** LaunchDarkly
connection and **no** SDK key.

### Live integration test

An integration test exercises a real LaunchDarkly environment. It is guarded by a
build tag and skips unless `LAUNCHDARKLY_SDK_KEY` and `LAUNCHDARKLY_TEST_FLAG`
(the key of an existing flag) are set. It cannot create or toggle flags - that
requires the LaunchDarkly management REST API - so it verifies the read path and,
best-effort, the streaming watch:

```sh
export LAUNCHDARKLY_SDK_KEY=sdk-xxxxxxxx
export LAUNCHDARKLY_TEST_FLAG=my-existing-flag-key
GOWORK=off go test -tags integration -run Integration ./...
```

Toggle `LAUNCHDARKLY_TEST_FLAG` in the LaunchDarkly dashboard while the watch
test is waiting to observe a live `Update`.

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/launchdarkly
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
