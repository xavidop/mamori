# mamori Google Cloud Storage provider

`github.com/xavidop/mamori/providers/gcs`

A [mamori](https://github.com/xavidop/mamori) provider for **Google Cloud
Storage (GCS)**. It registers the `gcs` scheme and resolves object contents
(optionally selecting a JSON field). Use it to source configuration files -
JSON, YAML, plain text, certificates - straight from a bucket.

![conformance](https://img.shields.io/badge/providertest-passing-brightgreen)

Passes the mamori provider conformance kit (`providertest.Run`) against an
in-memory fake. See [Verified vs. needs a live backend](#verified-vs-needs-a-live-backend).

## Install

```bash
go get github.com/xavidop/mamori/providers/gcs
```

Import for its side effect (the package `init()` registers the provider):

```go
import _ "github.com/xavidop/mamori/providers/gcs"
```

## Scheme

```
gcs://<bucket>/<object>[#json-key]
```

| Part          | Meaning                                                                       |
| ------------- | ----------------------------------------------------------------------------- |
| `<bucket>`    | GCS bucket name.                                                              |
| `<object>`    | Object name within the bucket. **May contain slashes** (e.g. `env/prod/app.json`). |
| `#json-key`   | Optional. Select a single field from a JSON-object payload.                  |

Only the **first** `/` after the bucket splits bucket from object, so an object
name with any number of `/` segments resolves correctly.

Under the hood the provider calls `bucket.Object(name).NewReader(ctx)` and reads
the full object body.

## Ref examples

```go
type Config struct {
    // Whole object as raw bytes:
    Settings []byte `source:"gcs://my-bucket/app/config.json"`

    // Object name with nested "directories":
    Cert string `source:"gcs://my-bucket/env/prod/tls/server.crt"`

    // Select a single field from a JSON object:
    APIKey string `source:"gcs://my-bucket/app/config.json#api_key"`

    // YAML / plain-text objects work too (decoded by mamori downstream):
    LogLevel string `source:"gcs://my-bucket/settings.yaml"`
}
```

`Value.Version` is set to the object's **generation** number, which changes on
every overwrite - giving mamori cheap change detection without a byte
comparison. If a generation is somehow unavailable it falls back to the object's
entity tag (etag), then to a content hash. Missing objects and absent
`#json-key` fields both resolve to an error satisfying
`errors.Is(err, mamori.ErrNotFound)`.

### Sensitivity

Resolved values are **not** marked sensitive by default, since GCS objects are
commonly plain configuration. If a bucket holds secret material, mark the whole
provider sensitive so mamori redacts its values in logs and diagnostics:

```go
mamori.WithProvider(gcs.New(gcs.WithSensitive()))
```

## Watching / change detection

This provider is **not watchable**: Google Cloud Storage has no cheap native
change-notification suitable for an in-process watch, so mamori **polls** it on
the configured interval. Each poll reads the object and reports its generation
as `Value.Version`, so unchanged objects are detected cheaply (the generation is
compared; the bytes are not re-diffed).

**Future push mode.** GCS can emit
[Pub/Sub object-change notifications](https://cloud.google.com/storage/docs/pubsub-notifications)
when an object is created, updated, or deleted. A future version of this
provider could implement `WatchableProvider` by subscribing to such a topic and
emitting a `mamori.Update` on each relevant event, turning polling into true
push. That is intentionally out of scope today - it requires a Pub/Sub topic,
subscription, and notification config to exist, which the plain read path does
not.

## Authentication

The provider uses **Application Default Credentials (ADC)** - the standard
Google credential chain. No credentials are read at registration time; the
client is created lazily on the first `Resolve`, so importing the package never
fails for lack of credentials.

ADC resolves credentials in this order:

1. `GOOGLE_APPLICATION_CREDENTIALS` pointing at a service-account key file.
2. gcloud user credentials from `gcloud auth application-default login`.
3. The attached service account on GCP compute (GKE Workload Identity, GCE, Cloud
   Run, Cloud Functions) via the metadata server.

The principal needs read access to the objects it resolves - the
`roles/storage.objectViewer` role (or the `storage.objects.get` permission) on
each bucket or object.

### Explicit configuration

For most uses the zero-config registered instance is enough. To supply your own
client (custom `option.ClientOption`s, a specific credentials file, an emulator
endpoint), build one and register it via `NewClientReader`:

```go
import (
    "context"

    "cloud.google.com/go/storage"
    "google.golang.org/api/option"

    "github.com/xavidop/mamori"
    gcs "github.com/xavidop/mamori/providers/gcs"
)

client, err := storage.NewClient(ctx, option.WithCredentialsFile("sa.json"))
// ...
mamori.WithProvider(gcs.New(gcs.WithClient(gcs.NewClientReader(client))))
```

`WithClientFactory` is also available if you want mamori to build the client
lazily on first use with your own constructor.

## Verified vs. needs a live backend

**Verified in unit tests (no cloud access):** scheme, ref parsing
(`<bucket>/<object>`, object names with slashes, `#json-key`), object
resolution, JSON key selection, the generation → etag → content-hash version
chain, default (non-sensitive) and `WithSensitive` flagging, `NotFound` →
`mamori.ErrNotFound` mapping, malformed-ref rejection, context cancellation,
concurrent resolution, lazy client construction/caching, the
not-watchable guarantee, and the full `providertest.Run` conformance suite
(with `SkipWatch`) - all against an in-memory fake that models GCS read and
generation semantics.

**Needs a live backend (not run in CI):** real HTTP/gRPC transport, ADC
credential resolution, IAM permission behavior, and real bucket/object naming.
These are exercised by the build-tagged live test in `gcs_integration_test.go`:

```bash
gcloud auth application-default login   # or set GOOGLE_APPLICATION_CREDENTIALS

GCS_TEST_BUCKET=my-bucket \
GCS_TEST_OBJECT=path/to/object.json \
GCS_TEST_EXPECT=expected-object-contents \
go test -tags integration -run TestLive ./...
```

The object named by `GCS_TEST_OBJECT` must already exist. Both live tests skip
automatically when the env vars are unset.

## Development

```bash
cd providers/gcs
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
