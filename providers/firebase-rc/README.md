# mamori Firebase Remote Config provider

`github.com/xavidop/mamori/providers/firebase-rc`

A [mamori](https://github.com/xavidop/mamori) provider for **Firebase Remote
Config**. It registers the `firebase-rc` scheme and resolves the server-side
default value of a Remote Config parameter (optionally selecting a JSON field).

![conformance](https://img.shields.io/badge/providertest-passing-brightgreen)

Passes the mamori provider conformance kit (`providertest.Run`) against an
in-memory fake. See [Verified vs. needs a live backend](#verified-vs-needs-a-live-backend).

## Install

```bash
go get github.com/xavidop/mamori/providers/firebase-rc
```

Import for its side effect (the package `init()` registers the provider):

```go
import _ "github.com/xavidop/mamori/providers/firebase-rc"
```

The package identifier is `firebaserc` (the module directory is `firebase-rc`).

## Scheme

```
firebase-rc://<parameter-key>[#json-key]
```

| Part              | Meaning                                                             |
| ----------------- | ------------------------------------------------------------------- |
| `<parameter-key>` | The name of a parameter in the project's **server** Remote Config template. |
| `#json-key`       | Optional. Select a single field from a JSON-object parameter value. |

The provider reads the current **server** Remote Config template (the one used
by the Admin SDK and server workloads, via the Firebase Remote Config REST API
endpoint `GET /v1/projects/<project>/remoteConfig`) and returns the named
parameter's default (server-side) value.

## Ref examples

```go
type Config struct {
    // A whole parameter value:
    WelcomeMessage string `source:"firebase-rc://welcome_message"`

    // Select a field from a JSON parameter value:
    NewUIEnabled string `source:"firebase-rc://feature_flags#new_ui"`

    // Numeric / boolean fields come back as their JSON encoding:
    MaxItems int `source:"firebase-rc://feature_flags#max_items"`
}
```

Resolved values are **not** marked sensitive (`Value.Sensitive == false`) -
Remote Config parameters are feature flags and configuration, not secrets.

`Value.Version` is set to the template's `versionNumber` (a monotonically
increasing, template-wide identifier), falling back to a content hash when the
backend supplies no version. Because the version is template-wide, it changes
whenever *any* parameter in the template is published; mamori therefore
re-reads (never returns a stale or wrong value) - it may occasionally re-apply
an unchanged value, which is harmless.

Missing parameters, and parameters configured to **use the in-app default**
(i.e. no server-side value), resolve to an error satisfying
`errors.Is(err, mamori.ErrNotFound)`, so mamori applies your default / optional
handling.

This provider is **not watchable**: the server Remote Config template has no
cheap native change notification, so mamori polls it on the configured interval.
(Do not expect push updates.)

## Authentication

The provider uses **Application Default Credentials (ADC)** - the standard
Google credential chain - scoped to
`https://www.googleapis.com/auth/firebase.remoteconfig`. No credentials are read
at registration time; the HTTP client is created lazily on the first `Resolve`,
so importing the package never fails for lack of credentials.

ADC resolves credentials in this order:

1. `GOOGLE_APPLICATION_CREDENTIALS` pointing at a service-account key file.
2. gcloud user credentials from `gcloud auth application-default login`.
3. The attached service account on Google Cloud compute (GKE Workload Identity,
   GCE, Cloud Run, Cloud Functions) via the metadata server.

The principal needs permission to read Remote Config (e.g. the
`roles/firebaseremoteconfig.viewer` role).

### Project ID

The project ID is taken from the resolved credentials by default. Override it
explicitly with `WithProjectID` (required when the credentials do not carry a
project, such as some user credentials):

```go
import (
    "github.com/xavidop/mamori"
    firebaserc "github.com/xavidop/mamori/providers/firebase-rc"
)

mamori.WithProvider(firebaserc.New(firebaserc.WithProjectID("my-project")))
```

### Explicit credentials

To authenticate with a specific service-account key instead of ADC:

```go
key, _ := os.ReadFile("service-account.json")
mamori.WithProvider(firebaserc.New(
    firebaserc.WithCredentialsJSON(key),
    firebaserc.WithProjectID("my-project"), // optional; key usually carries it
))
```

Advanced options: `WithHTTPClient` injects a pre-authenticated `*http.Client`
(custom transport, proxy, or an emulator), and `WithBaseURL` overrides the REST
endpoint base. `WithFetcher` replaces the template fetcher entirely.

## Verified vs. needs a live backend

**Verified in unit tests (no cloud access):** scheme and registration, ref
parsing (`<parameter-key>`, `#json-key`), whole-value and JSON-field resolution,
`Sensitive == false`, template-version reporting and change-on-mutate,
in-app-default and unknown-parameter -> `mamori.ErrNotFound` mapping, REST
response decoding, the real REST fetch path against an `httptest` server (URL,
JSON decoding, non-200 error handling), missing-project-ID handling, lazy
fetcher caching, context cancellation, concurrent resolution, and the full
`providertest.Run` conformance suite (with `SkipWatch`, since the provider is
non-watchable) against an in-memory fake.

**Needs a live backend (not run in CI):** real ADC credential resolution, the
`firebase.remoteconfig` OAuth2 scope and IAM permission behavior, real HTTPS
transport to `firebaseremoteconfig.googleapis.com`, and real project / template
data. These are exercised by the build-tagged live test in
`firebaserc_integration_test.go`:

```bash
gcloud auth application-default login   # or set GOOGLE_APPLICATION_CREDENTIALS

FIREBASE_RC_PROJECT=my-project \
FIREBASE_RC_PARAM=welcome_message \
FIREBASE_RC_EXPECT=Hello \
go test -tags integration -run TestLive ./...
```

The parameter named by `FIREBASE_RC_PARAM` must exist in the project's server
Remote Config template with a concrete (non in-app) default value. Both live
tests skip automatically when the env vars are unset.

## Development

```bash
cd providers/firebase-rc
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test ./...
```
