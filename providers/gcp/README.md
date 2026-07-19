# mamori GCP Secret Manager provider

`github.com/xavidop/mamori/providers/gcp`

A [mamori](https://github.com/xavidop/mamori) provider for **Google Cloud
Secret Manager**. It registers the `gcp-sm` scheme and resolves secret payloads
(optionally selecting a JSON field, optionally pinning a version).

![conformance](https://img.shields.io/badge/providertest-passing-brightgreen)

Passes the mamori provider conformance kit (`providertest.Run`) against an
in-memory fake. See [Verified vs. needs a live backend](#verified-vs-needs-a-live-backend).

## Install

```bash
go get github.com/xavidop/mamori/providers/gcp
```

Import for its side effect (the package `init()` registers the provider):

```go
import _ "github.com/xavidop/mamori/providers/gcp"
```

## Scheme

```
gcp-sm://<project>/<secret>[#json-key][?version=<version>]
```

| Part          | Meaning                                                                 |
| ------------- | ----------------------------------------------------------------------- |
| `<project>`   | GCP project ID or number.                                               |
| `<secret>`    | Secret ID within the project.                                           |
| `#json-key`   | Optional. Select a single field from a JSON-object payload.             |
| `?version=`   | Optional. Secret version number (e.g. `3`) or `latest`. Default `latest`. |

Under the hood the provider calls `AccessSecretVersion` with the resource name
`projects/<project>/secrets/<secret>/versions/<version|latest>`.

## Ref examples

```go
type Config struct {
    // Latest version, whole payload:
    DBPassword string `source:"gcp-sm://my-project/db-password"`

    // Select a field from a JSON secret:
    APIKey string `source:"gcp-sm://my-project/app-config#api_key"`

    // Pin a specific version:
    SigningCert string `source:"gcp-sm://my-project/signing-cert?version=3"`

    // Project by number is also valid:
    Token string `source:"gcp-sm://123456789012/service-token"`
}
```

Resolved values are marked **sensitive** (`Value.Sensitive == true`), so mamori
redacts them in logs and diagnostics. `Value.Version` is set to the resolved
version resource name (e.g. `projects/my-project/secrets/db-password/versions/7`),
which changes on every new secret version - giving mamori cheap change detection.
Missing secrets, missing versions, and absent `#json-key` fields all resolve to
an error satisfying `errors.Is(err, mamori.ErrNotFound)`.

This provider is **not watchable**: Secret Manager has no native change
notification, so mamori polls it on the configured interval. (Do not expect
push updates.)

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

The principal needs the `roles/secretmanager.secretAccessor` role (or the
`secretmanager.versions.access` permission) on each secret it reads.

### Explicit configuration

For most uses the zero-config registered instance is enough. To supply your own
client (custom `option.ClientOption`s, a specific credentials file, an emulator
endpoint), build one and register it:

```go
import (
    "context"

    secretmanager "cloud.google.com/go/secretmanager/apiv1"
    "google.golang.org/api/option"

    "github.com/xavidop/mamori"
    gcp "github.com/xavidop/mamori/providers/gcp"
)

client, err := secretmanager.NewClient(ctx, option.WithCredentialsFile("sa.json"))
// ...
mamori.WithProvider(gcp.New(gcp.WithClient(client)))
```

`WithClientFactory` is also available if you want mamori to build the client
lazily on first use with your own constructor.

## Verified vs. needs a live backend

**Verified in unit tests (no cloud access):** scheme, ref parsing
(`<project>/<secret>`, `#json-key`, `?version=`), `latest` and pinned-version
resolution, JSON key selection, `Sensitive` flagging, native version reporting,
`NotFound` → `mamori.ErrNotFound` mapping, context cancellation, concurrent
resolution, lazy client construction/caching, and the full
`providertest.Run` conformance suite - all against an in-memory fake that models
Secret Manager's `AccessSecretVersion` semantics.

**Needs a live backend (not run in CI):** real gRPC transport, ADC credential
resolution, IAM permission behavior, and real project/secret naming. These are
exercised by the build-tagged live test in `gcp_integration_test.go`:

```bash
gcloud auth application-default login   # or set GOOGLE_APPLICATION_CREDENTIALS

GCP_TEST_PROJECT=my-project \
GCP_TEST_SECRET=my-secret \
GCP_TEST_EXPECT=expected-latest-payload \
go test -tags integration -run TestLive ./...
```

The secret named by `GCP_TEST_SECRET` must already exist with at least one
enabled version. Both live tests skip automatically when the env vars are unset.

## Development

```bash
cd providers/gcp
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
