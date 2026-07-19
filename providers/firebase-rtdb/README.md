# mamori - Firebase Realtime Database provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves configuration
values from the [Firebase Realtime Database](https://firebase.google.com/docs/database),
with **native hot-reload** driven by the database's REST streaming (Server-Sent
Events) endpoint.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/firebase-rtdb"
```

Importing the package registers the `firebase-rtdb` scheme with mamori. The
backend (Admin SDK client + streaming HTTP client) is built lazily on first use
from Application Default Credentials and the configured database URL, so importing
the package never contacts Firebase.

## Scheme

```
firebase-rtdb://<path>[#json-key]
```

- `<path>` - the Realtime Database location, e.g. `config/service/db`. No leading
  slash.
- `#json-key` - optional. When present, the value at `<path>` is parsed as a JSON
  object and the named field is selected via `mamori.SelectKey` (the same behavior
  as every other mamori provider). String fields are returned unquoted; objects,
  arrays, numbers, and booleans are returned as their JSON encoding.

### Ref examples

| Ref | Meaning |
| --- | --- |
| `firebase-rtdb://config/service/log_level` | Value at path `config/service/log_level` |
| `firebase-rtdb://config/service/db#host` | Field `host` of the JSON object at `config/service/db` |
| `firebase-rtdb://config/service/db#password` | Field `password` of the JSON object at `config/service/db` |
| `firebase-rtdb://features/flags#dark_mode` | Field `dark_mode` of the JSON object at `features/flags` |

```go
type Config struct {
    LogLevel   string `source:"firebase-rtdb://config/service/log_level"`
    DBHost     string `source:"firebase-rtdb://config/service/db#host"`
    DBPassword string `source:"firebase-rtdb://config/service/db#password"`
    DarkMode   bool   `source:"firebase-rtdb://features/flags#dark_mode"`
}
```

### Value semantics

- `Value.Bytes` - the JSON of the value at the path. A JSON string leaf is returned
  unquoted (matching `mamori.SelectKey`); objects, arrays, numbers, and booleans
  are returned as their JSON encoding.
- `Value.Version` - the database **ETag** for the path (requested with
  `X-Firebase-ETag`), a native revision, so change detection is exact and cheap.
  If the backend returns no ETag, it falls back to `mamori.VersionHash` of the
  payload.
- `Value.Sensitive` - always `false`. The Realtime Database stores configuration,
  not managed secrets. Wrap a field in `secret.String` if you want redaction anyway.
- A `null` or missing path returns an error satisfying
  `errors.Is(err, mamori.ErrNotFound)`.

## Authentication & configuration

Authentication uses **Application Default Credentials (ADC)**:

| Source | How |
| --- | --- |
| Service-account key | `GOOGLE_APPLICATION_CREDENTIALS=/path/to/key.json` |
| User credentials | `gcloud auth application-default login` |
| Workload identity / metadata server | Automatic on GCP / GKE / Cloud Run |

The database URL is required and is taken from `WithDatabaseURL` or the
`FIREBASE_DATABASE_URL` environment variable, e.g.
`https://my-project-default-rtdb.firebaseio.com`.

Streaming uses the ADC token with the `firebase.database` and `userinfo.email`
OAuth scopes.

For explicit configuration, construct the provider yourself and register it:

```go
p := firebasertdb.New(
    firebasertdb.WithDatabaseURL("https://my-project-default-rtdb.firebaseio.com"),
    firebasertdb.WithProjectID("my-project"),          // optional; ADC usually supplies it
    firebasertdb.WithReconnectBackoff(5*time.Second),  // optional; stream reconnect delay
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

### Options

| Option | Effect |
| --- | --- |
| `WithDatabaseURL(url)` | Set the Realtime Database URL (else `FIREBASE_DATABASE_URL`) |
| `WithProjectID(id)` | Set the Firebase/GCP project ID (optional) |
| `WithReconnectBackoff(d)` | Delay before reconnecting a dropped stream / retrying (default 2s) |

## Native watch (SSE streaming)

The provider implements `mamori.WatchableProvider` using the Realtime Database
**REST streaming** endpoint - the idiomatic Firebase push mechanism, not a polling
ticker:

1. On `Watch`, a `GET <db-url>/<path>.json` request is opened with
   `Accept: text/event-stream` and an ADC bearer token.
2. The current value is emitted immediately as a baseline.
3. The server pushes Server-Sent Events (`put` for a replace, `patch` for a merge,
   plus `keep-alive` heartbeats). On each `put`/`patch`, the provider **re-resolves**
   the path to obtain a consistent value plus a fresh ETag and emits an `Update`.
   Re-resolving on the server's push (rather than reconstructing the value from the
   event's relative path and merge payload) keeps the implementation simple and
   correct while remaining native push, not polling.
4. A `keep-alive` is a no-op; a server `cancel` terminates the watch; an
   `auth_revoked` reconnects with a fresh token; a dropped connection is surfaced
   as `Update{Err: ...}` and reconnected after `WithReconnectBackoff`.
5. When the watch context is cancelled the in-flight request is aborted, the
   goroutine exits, and the channel is closed - no goroutine leaks (verified with
   `goleak`).

A delete of the watched path arrives as a `put` of `null` and is delivered as an
`Update` whose `Err` satisfies `errors.Is(err, mamori.ErrNotFound)`; the watch
stays open.

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-memory fake backend (`go test ./...`) |
| Resolve, scalar unquoting, JSON `#key` selection, not-found, version monotonicity, context cancellation | **Verified** (unit tests) |
| Native SSE watch (baseline + change + delete + cancel/close, no goroutine leak) | **Verified** against the fake |
| Server-Sent-Events parser (`event:`/`data:`, multi-line data, comments, keep-alive) | **Verified** (unit test over byte streams) |
| End-to-end against a real Firebase Realtime Database | **Needs a live backend** - see the integration test |

The unit and conformance tests use an in-memory fake that reproduces the database's
ETag bump-on-write and push-on-change behavior, so `go test ./...` requires **no**
Firebase project and **no** credentials. The live SSE streamer (Admin SDK read +
REST streaming with an ADC token) is exercised only by the integration test.

### Live integration test

An integration test exercises a real Firebase Realtime Database. It is guarded by a
build tag and skips unless `FIREBASE_DATABASE_URL` is set:

```sh
gcloud auth application-default login           # or set GOOGLE_APPLICATION_CREDENTIALS
export FIREBASE_DATABASE_URL=https://my-project-default-rtdb.firebaseio.com
export FIREBASE_TEST_PATH=config/service/log_level
export FIREBASE_TEST_EXPECT=info                # optional
GOWORK=off go test -tags integration -run TestLive ./...

# To exercise the native watch, also set FIREBASE_TEST_WATCH=1 and mutate the
# value in the Firebase console within the timeout:
FIREBASE_TEST_WATCH=1 GOWORK=off go test -tags integration -run TestLiveWatch ./...
```

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/firebase-rtdb
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
