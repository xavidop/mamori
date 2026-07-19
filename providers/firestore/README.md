# mamori - Google Cloud Firestore provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves configuration
values from [Google Cloud Firestore](https://cloud.google.com/firestore) documents,
with **native hot-reload** driven by Firestore snapshot listeners.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/firestore"
```

Importing the package registers the `firestore` scheme with mamori. The Firestore
client is built lazily on first use from Application Default Credentials, so
importing the package never contacts Google Cloud.

## Scheme

```
firestore://<collection>/<doc>[#field]
```

- `<collection>` - the Firestore collection ID, e.g. `config`.
- `<doc>` - the document ID within that collection, e.g. `app`.
- `#field` - optional. When present, the document is JSON-encoded and the named
  top-level field is selected via `mamori.SelectKey` (the same behavior as every
  other mamori provider). String fields are returned unquoted; objects, arrays,
  numbers, and booleans are returned as their JSON encoding.

### Ref examples

| Ref | Meaning |
| --- | --- |
| `firestore://config/app` | The whole `config/app` document, encoded as JSON |
| `firestore://config/app#log_level` | Field `log_level` of the `config/app` document |
| `firestore://config/db#password` | Field `password` of the `config/db` document |
| `firestore://features/flags#dark_mode` | Field `dark_mode` of the `features/flags` document |

```go
type Config struct {
    // Whole document as JSON (decoded into a struct/map by mamori).
    App        map[string]any `source:"firestore://config/app"`
    LogLevel   string         `source:"firestore://config/app#log_level"`
    DBHost     string         `source:"firestore://config/db#host"`
    DBPassword string         `source:"firestore://config/db#password"`
    DarkMode   bool           `source:"firestore://features/flags#dark_mode"`
}
```

### Value semantics

- `Value.Bytes` - without a `#field`, the document data (`snap.Data()`) encoded as
  JSON; with a `#field`, the selected field.
- `Value.Version` - the document's `UpdateTime` formatted as RFC3339Nano. This is
  Firestore's native revision, so change detection is exact and cheap - no byte
  comparison. If `UpdateTime` is unavailable the provider falls back to a content
  hash (`mamori.VersionHash`). The version reflects the whole document, so a change
  to any field is detected even for a `#field` ref.
- `Value.Sensitive` - always `false`. Firestore stores configuration, not managed
  secrets. Wrap a field in `secret.String` if you want redaction anyway.
- A missing document or field returns an error satisfying
  `errors.Is(err, mamori.ErrNotFound)`.

## Authentication & configuration

The provider authenticates with **Application Default Credentials (ADC)** and needs
a Google Cloud project ID:

| Source | Purpose |
| --- | --- |
| `GOOGLE_APPLICATION_CREDENTIALS` | Path to a service-account key file |
| `gcloud auth application-default login` | Local user credentials |
| Workload identity / metadata server | Credentials on GCP (GKE, Cloud Run, GCE) |
| `GOOGLE_CLOUD_PROJECT` / detected from the credentials | Project ID (unless set with `WithProjectID`) |

By default the project ID is detected from the environment. To set it explicitly,
construct the provider yourself and register it:

```go
p := firestore.New(firestore.WithProjectID("my-project"))
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

Or inject a fully custom `*firestore.Client` (custom database ID, credentials, or
the local emulator):

```go
client, _ := firestore.NewClient(ctx, "my-project")
p := firestore.New(firestore.WithClient(client))
```

### Options

| Option | Effect |
| --- | --- |
| `WithProjectID(id)` | Set the Google Cloud project ID (default: detected) |
| `WithClient(*firestore.Client)` | Inject a pre-built Firestore client |

## Native watch (snapshot listeners)

The provider implements `mamori.WatchableProvider` using **Firestore snapshot
listeners** - the idiomatic Firestore push mechanism, not a polling ticker:

1. On `Watch`, the provider opens `DocumentRef.Snapshots(ctx)` and emits the
   current document as a baseline.
2. Firestore streams a fresh snapshot every time the document changes (or is
   created/deleted); each is emitted as an `Update` the instant it happens, with
   no busy polling.
3. Transient listener failures are delivered as `Update{Err: ...}`.
4. When the watch context is cancelled the listener is stopped (`Stop`), the
   goroutine exits, and the channel is closed - no goroutine leaks (verified with
   `goleak`).

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-memory fake with snapshot-listener semantics (`go test ./...`) |
| Resolve (whole document + JSON `#field`), not-found, version monotonicity, context cancellation, malformed ref | **Verified** (unit tests) |
| Native snapshot-listener watch (baseline + change + cancel/close, no goroutine leak) | **Verified** against the fake, including under `-race` |
| End-to-end against a real Firestore / emulator | **Needs a live backend** - see the integration test |

The unit and conformance tests use an in-memory fake that reproduces Firestore's
`UpdateTime` bump-on-write and snapshot-listener behavior, so `go test ./...`
requires **no** Google Cloud credentials and **no** running Firestore.

### Live integration test

An integration test exercises a real Firestore (or the Firestore emulator). It is
guarded by a build tag and skips unless `FIRESTORE_TEST_PROJECT` is set:

```sh
# Against a real project (ADC configured):
FIRESTORE_TEST_PROJECT=my-project \
  GOWORK=off go test -tags integration -run TestLive ./...

# Against the local emulator:
gcloud emulators firestore start --host-port=127.0.0.1:8080 &
export FIRESTORE_EMULATOR_HOST=127.0.0.1:8080
FIRESTORE_TEST_PROJECT=demo-project \
  GOWORK=off go test -tags integration -run TestLive ./...
```

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/firestore
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
