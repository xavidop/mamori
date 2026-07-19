# mamori - MongoDB provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves configuration
and secret values from documents stored in [MongoDB](https://www.mongodb.com/),
with **native hot-reload** driven by MongoDB change streams.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/mongodb"
```

Importing the package registers the `mongodb` scheme with mamori. The MongoDB
client is connected lazily on first use, so importing the package never contacts
MongoDB.

## Scheme

```
mongodb://<collection>/<docid>[#field][?key=<field>]
```

- `<collection>` - the collection to look in.
- `<docid>` - identifies the document. By default the document whose `_id` equals
  `<docid>` is selected. When `<docid>` is a valid 24-character hex string it is
  matched as an `ObjectID`; otherwise it is matched as a plain value.
- `#field` - optional. Selects a single field from the matched document via
  `mamori.SelectKey` (the same behavior as every other mamori provider). Scalars
  are returned unquoted; objects, arrays, numbers, and booleans are returned as
  their JSON encoding.
- `?key=<field>` - optional. Selects the document by an arbitrary field instead of
  `_id`, i.e. the document where `<field> == <docid>`.

> Per the mamori ref grammar the `#field` fragment comes **before** the `?opts`
> query: `mongodb://coll/id#field?key=email`.

### Ref examples

| Ref | Meaning |
| --- | --- |
| `mongodb://config/app` | The whole `config` document with `_id == "app"`, as JSON |
| `mongodb://config/app#logLevel` | Field `logLevel` of that document |
| `mongodb://secrets/app-db#password` | Field `password` of the `secrets` doc with `_id == "app-db"` |
| `mongodb://users/507f1f77bcf86cd799439011#email` | Field `email` of the user whose ObjectID `_id` is `507f...` |
| `mongodb://users/ada@example.com#apiKey?key=email` | Field `apiKey` of the user whose `email == "ada@example.com"` |

```go
type Config struct {
    LogLevel   string `source:"mongodb://config/app#logLevel"`
    DBHost     string `source:"mongodb://secrets/app-db#host"`
    DBPassword string `source:"mongodb://secrets/app-db#password"`
    APIKey     string `source:"mongodb://users/ada@example.com#apiKey?key=email"`
}
```

### Value semantics

- `Value.Bytes` - when no `#field` is given, the whole document encoded as
  deterministic JSON (relaxed MongoDB Extended JSON re-encoded with sorted keys,
  so BSON types like `ObjectID` and dates get a stable representation). With a
  `#field`, the selected field's value.
- `Value.Version` - the document's `version` field when present (stringified),
  otherwise `mamori.VersionHash` over the document JSON. It changes whenever the
  document changes, giving cheap change detection.
- `Value.Sensitive` - always `false`. Wrap a field in `secret.String` if you want
  redaction.
- A missing document (or a missing `#field`) returns an error satisfying
  `errors.Is(err, mamori.ErrNotFound)`.

## Authentication & configuration

The connection string is read from the standard `MONGODB_URI` environment
variable, and the database name is set explicitly:

| Variable | Purpose |
| --- | --- |
| `MONGODB_URI` | MongoDB connection string, e.g. `mongodb://user:pass@host:27017/?replicaSet=rs0` |

```go
p := mongodb.New(mongodb.WithDatabase("app")) // URI from MONGODB_URI
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

For explicit configuration, set the URI directly or inject a fully custom client
(custom TLS, auth, pool settings):

```go
// Explicit URI + database.
p := mongodb.New(
    mongodb.WithURI("mongodb://user:pass@host:27017/?replicaSet=rs0"),
    mongodb.WithDatabase("app"),
)

// Or inject a pre-built client.
client, _ := mongo.Connect(ctx, options.Client().ApplyURI(uri))
p := mongodb.New(mongodb.WithClient(client), mongodb.WithDatabase("app"))
```

### Options

| Option | Effect |
| --- | --- |
| `WithURI(uri)` | Set the connection string (default: `MONGODB_URI`) |
| `WithDatabase(name)` | Set the database name (**required**) |
| `WithClient(*mongo.Client)` | Inject a pre-configured MongoDB client |

## Native watch (change streams)

The provider implements `mamori.WatchableProvider` using **MongoDB change
streams** - the idiomatic MongoDB push mechanism, not a polling ticker:

1. On `Watch`, the current value is emitted immediately as a baseline.
2. The provider opens a change stream on the collection with a `$match` stage
   scoped to the target document (by `documentKey._id`, or by the looked-up
   `fullDocument.<field>` when `?key=` is used).
3. On each change the target document is re-read and emitted as an `Update`.
   Transient stream errors are delivered as `Update{Err: ...}`; a deleted
   document surfaces as an `Update` whose `Err` satisfies `mamori.ErrNotFound`.
4. When the watch context is cancelled the change stream is closed, the goroutine
   exits, and the channel is closed - no goroutine leaks (verified with `goleak`).

> **Change streams require a replica set (or sharded cluster).** Against a
> standalone `mongod`, `Watch` returns an error and mamori transparently falls
> back to its polling adapter. To use native watch locally, run a single-node
> replica set (see the integration test below).

## Testing status

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-memory fake backend (`go test ./...`) |
| Resolve (whole doc + `#field` + `?key=` selection), not-found, version (field & hash), context cancellation, bad path | **Verified** (unit tests) |
| Native change-stream watch (baseline + change + unrelated-write isolation + delete + cancel/close, no goroutine leak) | **Verified** against the fake |
| End-to-end against a real MongoDB replica set (real `*mongo.ChangeStream`, ObjectID `_id` matching) | **Needs a live backend** - see the integration test |

The unit and conformance tests use an in-memory fake that reproduces MongoDB's
point lookups and change-stream signalling, so `go test ./...` requires **no**
running MongoDB.

### Live integration test

An integration test exercises a real MongoDB deployment. It is guarded by a build
tag and skips unless `MONGODB_URI` is set. Change streams need a replica set:

```sh
docker run -d --name mongo -p 27017:27017 mongo:7 --replSet rs0
docker exec mongo mongosh --eval 'rs.initiate()'
export MONGODB_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0'
export MONGODB_DATABASE=mamori_it
GOWORK=off go test -tags integration -run Integration ./...
```

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/mongodb
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
