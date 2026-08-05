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

## Error classification

Beyond the not-found case above, this provider has no Firebase-specific SDK
error taxonomy to map, so any other backend failure (a rejected REST call, a
network error, ...) reports `unknown` via `mamori.ErrorKind`. `Resolve` still
wraps the backend error with `%w`, so the classification chain (`errors.Is`,
`errors.As`, and any kind a *caller* attaches to the underlying error) is
preserved rather than flattened.

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
    firebasertdb.WithReconnectBackoff(5*time.Second),  // optional; base stream reconnect delay
    firebasertdb.WithMaxFrameBytes(4<<20),             // optional; ceiling on one streamed change
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

### Options

| Option | Effect |
| --- | --- |
| `WithDatabaseURL(url)` | Set the Realtime Database URL (else `FIREBASE_DATABASE_URL`) |
| `WithProjectID(id)` | Set the Firebase/GCP project ID (optional) |
| `WithReconnectBackoff(d)` | Base delay before reconnecting a dropped stream / retrying (default 2s) |
| `WithMaxFrameBytes(n)` | Ceiling on one streamed change, in bytes (default 1 MiB) |

#### `WithMaxFrameBytes`

The Realtime Database opens **every** stream with a `put` of the whole watched
node, so this ceiling is in practice the largest node the provider can watch. A
node whose pushed frame is over it never gets past the first frame of a
connection: each attempt reports an `Update{Err: ...}` and reconnects, so the
watch keeps running but never delivers a change. `Resolve` reads through the
Admin SDK and is unaffected.

Set it to the size of the largest node you watch, plus headroom. Values that are
not positive are ignored.

#### `WithReconnectBackoff`

This is the **floor**, not a fixed delay. Each consecutive attempt that delivers
nothing doubles the wait, up to 30 seconds (or up to your value, if you set one
longer than that), and the wait drops straight back to the floor as soon as a
stream delivers anything. Every wait is jittered into the upper half of its
range.

Left alone, a database that is down is retried at 2s, 4s, 8s, ... up to every
30s, and a healthy stream that drops is retried after about 2s.

`Close()` marks the provider closed; afterwards every `Resolve`, and any
`Watch` started after `Close`, report `errors.Is(err, mamori.ErrUnavailable)`
locally, without contacting the Realtime Database. It releases no connection:
this provider holds no `*http.Client` of its own, and a running `Watch`'s SSE
stream is owned end to end by that watch's own goroutine, which already closes
the stream on context cancellation. `Close` deliberately adds no second
teardown path for a running watch.

A `Watch` that was **already running** when `Close` was called therefore keeps
its stream open, and `Close` does not end it. It does not go quiet either:
every `put`/`patch` the server pushes is re-resolved through the same closed
check, so from then on each change arrives as an error update carrying
`errors.Is(err, mamori.ErrUnavailable)` instead of a value, for as long as the
stream lives. Cancelling that watch's own context is the only way to stop it.
See
[Close does not stop a Watch](https://mamorigo.dev/docs/writing-a-provider/#close-does-not-stop-a-watch)
for what every other provider does here.

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
   as `Update{Err: ...}` and reconnected after a wait that starts at
   `WithReconnectBackoff` and doubles, capped at 30s, until a stream delivers
   something. The stream is parsed by `httpcore`'s bounded SSE decoder, which
   caps a single line and one frame's total data at `WithMaxFrameBytes` each,
   1 MiB by default; a frame over that ceiling is surfaced as `Update{Err: ...}`
   and the connection re-established.
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
| Server-Sent-Events parser (`event:`/`data:`, multi-line data, comments, keep-alive) | **Verified** in `providers/httpcore` (unit tests over byte streams), which now owns the decoder |
| Live SSE streaming path (frame decoding, both memory ceilings, cancellation, non-200) | **Verified** (unit tests over a local HTTP server, no credentials) |
| End-to-end against a real Firebase Realtime Database | **Needs a live backend** - see the integration test |

The unit and conformance tests use an in-memory fake that reproduces the database's
ETag bump-on-write and push-on-change behavior, so `go test ./...` requires **no**
Firebase project and **no** credentials. The REST streaming half of the live
backend is covered without credentials too; only the Admin SDK read path needs
the integration test.

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
