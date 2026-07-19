# mamori - Unleash provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves feature-flag
state from an [Unleash](https://www.getunleash.io) server using the official
[`unleash-client-go`](https://github.com/Unleash/unleash-client-go) SDK (v4).

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```bash
go get github.com/xavidop/mamori/providers/unleash
```

```go
import _ "github.com/xavidop/mamori/providers/unleash" // registers unleash://
```

Importing the package registers the `unleash` scheme with mamori. The Unleash
client is built lazily on first use, so importing the package never contacts the
network and never fails for lack of configuration.

## Scheme

```
unleash://<feature-toggle-name>[#variant|#payload]
```

- `<feature-toggle-name>` - the name of an Unleash feature toggle.
- No fragment - resolves to the toggle's **enabled state**, the string `"true"`
  or `"false"` (`Client.IsEnabled`).
- `#variant` - resolves to the active **variant name** (`Client.GetVariant`).
- `#payload` - resolves to the active variant's **payload value**.

### Ref examples

| Ref | Resolves to |
| --- | --- |
| `unleash://new-checkout` | `"true"` / `"false"` - whether the toggle is enabled |
| `unleash://new-checkout#variant` | the active variant name, e.g. `"blue"` |
| `unleash://new-checkout#payload` | the active variant payload value, e.g. `"#0000ff"` |

```go
type Config struct {
    NewCheckout    bool   `source:"unleash://new-checkout"`
    CheckoutColor  string `source:"unleash://new-checkout#variant"`
    CheckoutCopy   string `source:"unleash://new-checkout#payload"`
}
```

## Evaluation

- The enabled state and variant are evaluated by the Unleash SDK against the
  toggle's activation strategies, exactly as any other Unleash client would
  evaluate them.
- `Value.Sensitive` is always `false`. Unleash feature toggles carry
  configuration / rollout state, not managed secrets.
- `Value.Version` is a content hash (`mamori.VersionHash`) of the resolved bytes.
  Unleash exposes no per-toggle revision identifier through the client, and the
  hash still gives mamori cheap, correct change detection: the version changes
  whenever the resolved value changes.

### Not found

A ref that names a toggle the client does not know about resolves to an error
satisfying `errors.Is(err, mamori.ErrNotFound)`, so mamori applies defaults /
optional handling. Unleash's `IsEnabled` returns `false` (not an error) for
unknown toggles, so the provider inspects the client's loaded feature repository
(`Client.ListFeatures`) to distinguish a genuinely-missing toggle from one that
exists and is simply disabled - a disabled-but-existing toggle resolves to
`"false"`, never to not-found.

## Authentication

The provider connects to an Unleash server using three settings, supplied either
explicitly via options or, when unset, read lazily from the environment at first
use:

| Setting | Option | Environment variable | Default |
| --- | --- | --- | --- |
| Server URL | `WithURL` | `UNLEASH_URL` | (required) |
| API token | `WithToken` | `UNLEASH_API_TOKEN` | (sent as `Authorization` header) |
| App name | `WithAppName` | `UNLEASH_APP_NAME` | `mamori` |

```go
// From the environment (UNLEASH_URL / UNLEASH_API_TOKEN / UNLEASH_APP_NAME):
mamori.WithProvider(unleash.New())

// Explicit configuration:
mamori.WithProvider(unleash.New(
    unleash.WithURL("https://unleash.example.com/api"),
    unleash.WithToken("*:development.xxxxxxxx"),
    unleash.WithAppName("my-service"),
))

// Bring your own fully-configured client (custom strategies, storage, HTTP client):
c, _ := unleash.NewClient(/* ... */)
c.WaitForReady()
mamori.WithProvider(unleash.New(unleash.WithClient(c)))
```

### Lazy start and synchronization

The Unleash client fetches feature toggles from the server in a background
goroutine after it is created; it is not usable until that first fetch has
completed. This provider builds the client lazily on the first `Resolve` and
calls `Client.WaitForReady()` before returning it, so the first resolve blocks
until the feature repository is populated rather than reporting spurious
not-found results. If you inject your own client via `WithClient`, call
`WaitForReady()` on it yourself before handing it over.

## Watch

No native per-toggle change notification - mamori polls (interval + jitter).
Configure the poll interval with `mamori.WithPollInterval`. The Unleash client
also refreshes its own in-memory feature repository on an internal interval
(`WithRefreshInterval`, 15s by default), so the value mamori polls stays current
with the server independently of the mamori poll cadence.

## What is verified

- The unit tests and the [`providertest`](../../providertest) conformance kit run
  against an in-memory fake of the minimal Unleash client surface
  (is-enabled / get-variant / toggle-exists), so no network and no live Unleash
  server are required. The fake reports un-seeded toggles as missing, so the
  not-found contract is exercised for real.
- Live Unleash behavior (real server, real token, real toggle) is exercised by
  `//go:build integration` tests, **not** run in CI by default. See the header of
  `unleash_integration_test.go` for the environment variables it needs.

Passes the mamori conformance kit.
