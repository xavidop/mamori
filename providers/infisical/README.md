# mamori - Infisical provider

A [mamori](https://github.com/xavidop/mamori) provider that resolves secrets from
[Infisical](https://infisical.com), the open-source secret manager. Infisical
publishes a Go SDK, but it is not used here: the read path is a documented HTTPS
API, so this provider is built on
[`providers/httpcore`](../httpcore/) and the standard library only, keeping the
SDK's dependency tree out of every consumer's build.

[![conformance](https://img.shields.io/badge/mamori%20conformance-passing-brightgreen)](https://github.com/xavidop/mamori)

```go
import _ "github.com/xavidop/mamori/providers/infisical"
```

Importing the package registers the `infisical` scheme with mamori. The provider
reads `INFISICAL_CLIENT_ID`, `INFISICAL_CLIENT_SECRET`, `INFISICAL_PROJECT_ID`,
`INFISICAL_ENVIRONMENT` and `INFISICAL_SECRET_PATH` lazily at resolve time, so it
is safe to register from a blank import even when no credentials exist at process
start.

## Scheme

```text
infisical://<secretName>                    secret from the configured project
infisical://<secretName>?project=<id>       explicit project
infisical://<secretName>?env=<slug>         explicit environment
infisical://<secretName>?path=/folder       explicit folder
infisical://<secretName>#field              select a field of a JSON-valued secret
infisical://<secretName>#/json/pointer      select a nested field by RFC 6901 JSON Pointer
```

- `<secretName>` - the **entire ref path**, including any slashes it contains.
  Infisical secret names are conventionally `SCREAMING_SNAKE_CASE`, but the API
  does not forbid a slash, and this follows
  [`providers/cloudflare-kv`](../cloudflare-kv/)'s precedent: a segment-count rule
  that split the path into "scope plus name" would silently misread one name
  containing a slash as two. The project, environment and folder come from
  options and ref query parameters instead, never from the path.
- `?project=`, `?env=`, `?path=` - optional. Each overrides the corresponding
  provider option for this ref only, letting one provider serve refs that point
  at different projects, environments or folders.
- `#field` / `#/json/pointer` - optional. When present, the secret value is
  parsed as JSON and the field is selected via `mamori.SelectKey` (identical to
  every other mamori provider): a fragment starting with `/` is an RFC 6901 JSON
  Pointer for nested selection (`#/creds/password`); anything else is a literal
  top-level key (`#password`).

**A secret name containing a literal `#` cannot be expressed as a ref.** mamori's
ref grammar parses the `#key` fragment before the `?opts` query, claiming `#` for
field selection. That is not something this provider imposes and there is no
escape hatch around it.

**A secret name containing a `.` or `..` path segment is refused.**
`httpcore.Client.Do` rejects a dot segment on the decoded path for every provider
built on it, so a name of `../x` fails with `mamori.ErrInvalid` even though this
provider would have kept it inside one escaped segment. That is a deliberate cost
of inheriting a check no provider can forget rather than re-deriving a safe one
here. A name of `%2e%2e/x`, by contrast, resolves normally: this provider escapes
the whole name, so it travels as the single segment `%252e%252e%2Fx` and is a
literal name rather than a traversal.

### Ref examples

| Ref | Meaning |
| --- | --- |
| `infisical://DB_PASSWORD` | Secret `DB_PASSWORD` in the configured project, environment and folder |
| `infisical://DB_PASSWORD?env=staging` | The same secret from the `staging` environment |
| `infisical://DB_PASSWORD?path=/backend` | The same secret from the `/backend` folder |
| `infisical://DB_PASSWORD?project=abc123` | The same secret from an explicit project |
| `infisical://DB_CREDS#password` | Field `password` of the JSON-valued secret `DB_CREDS` |
| `infisical://DB_CREDS#/creds/password` | A nested field of `DB_CREDS`, selected by JSON Pointer |

```go
type Config struct {
    DBPassword secret.String `source:"infisical://DB_PASSWORD"`
    APIKey     secret.String `source:"infisical://API_KEY?env=staging"`
    StripeKey  secret.String `source:"infisical://PAYMENTS#/stripe/secretKey"`
}
```

## Fetching a value

One ref is one authenticated `GET /api/v4/secrets/{secretName}`, scoped by
`projectId`, `environment` and `secretPath` query parameters. The response nests
the value one level down, `{"secret":{"secretKey":"...","secretValue":"...","version":3}}`,
so there is always an unwrapping step, unlike `providers/cloudflare-kv`'s
single-key GET which answers with raw bytes.

**The read path is `v4`, not the `v3` most third-party write-ups still describe.**
`WithAPIVersion` is deliberately not offered: a second version means a second
response shape, and guessing at one nobody here has tested would be worse than
requiring a provider update.

There is **no value cache**. `mamori.Refresh` and `mamori.Doctor` both call
`Resolve` directly, and Infisical exposes no ETag or digest to gate a held
snapshot on, so every call is a live read. The **access token is** cached, because
it is a credential rather than a value and re-buying it on every poll would double
the request count against Infisical's identity endpoint.

## Value mapping

| Field | Value |
| --- | --- |
| `Value.Bytes` | `secret.secretValue`, after `#field` / `#/json/pointer` selection when the ref asks for one |
| `Value.Version` | `secret.version` rendered as a string, or a content hash (`mamori.VersionHash`) when the backend sends no revision |
| `Value.Sensitive` | Always `true` |

Three things are worth stating, because each is a decision a reader might assume
went the other way:

- **`Value.Sensitive` is always `true`, with no switch.** This is a secret
  manager; there is no configuration-only mode of Infisical for a per-ref or
  per-provider flag to describe. `providers/https` has `Endpoint.Sensitive`
  because a generic HTTP endpoint may serve either.
- **`Value.Version` is the backend's own revision**, so mamori gets change
  detection from Infisical rather than from a content hash. That is strictly
  better than a hash: it costs nothing to compare, and it distinguishes a rewrite
  of identical bytes from no write at all. The hash fallback exists for a backend
  that sends no `version`, because rendering an absent revision as `"0"` would pin
  `Version` to a constant and make change detection impossible for every ref at
  once, which is the one failure mode a poller cannot report.
- **The version describes the whole secret, not the selected fragment.** Two refs
  selecting different `#field`s of one secret therefore agree on when it changed.

## Authentication

Machine identity **Universal Auth**: a client id and client secret are exchanged
for a short-lived access token, cached and refreshed 30 seconds before its stated
expiry so a request is never sent with a token that dies in flight.

| Source | Option | Environment variable |
| --- | --- | --- |
| Client id | `WithClientID(id)` | `INFISICAL_CLIENT_ID` |
| Client secret | `WithClientSecret(secret)` | `INFISICAL_CLIENT_SECRET` |

An explicit option wins over its environment variable, and the environment is read
lazily at the first resolve.

### Why this provider does not use `httpcore.OAuth2ClientCredentials`

It does not fit, in both directions:

| | RFC 6749 client credentials | Infisical Universal Auth |
| --- | --- | --- |
| Request | form-encoded `grant_type`, `client_id`, `client_secret` | JSON `{"clientId":..., "clientSecret":...}` |
| Path | the configured token endpoint | `/api/v1/auth/universal-auth/login` |
| Response | `access_token`, `expires_in` | `accessToken`, `expiresIn` |

So this provider writes its own token authenticator, following `httpcore`'s
structure decision for decision rather than reinventing it:

- **The lock is never held across the login round trip.** Concurrent callers
  arriving while an exchange is in flight share that one exchange, and a caller
  waiting on someone else's exchange is released by its own context.
  `sync.Mutex` has no context-aware `Lock`, and mamori's reconciler runs on a
  single goroutine, so an `Apply` wedged behind a hung Infisical would stall
  reconciliation for every field rather than only the one being resolved.
- **No credential is held in a readable struct field**, neither the client secret
  nor the access token it buys. `fmt`'s `%+v` and `%#v` walk unexported fields by
  reflection, and reflection cannot call a `String` or `GoString` method on a
  value it reaches that way, so a redaction method would not have protected
  either one: `fmt` falls back to printing the raw contents. Both live inside
  closures, which reflection renders as bare function pointers. **`Provider`
  itself gets the same treatment**, because a `Provider` is exactly the value an
  application passes to `mamori.WithProvider` and so is a plausible thing to log.

The only genuine divergence is `httpcore`'s `TokenURL`. Infisical's login path is
fixed relative to the base URL, so there is nothing per-deployment to configure and
nothing to validate beyond the base URL itself.

## Self-hosted installs

`WithBaseURL(u)` overrides `https://app.infisical.com`. The scheme is checked
against a closed set of `https` and `http`: an `ftp://` typo or a `ws://` paste
would otherwise satisfy `httpcore.New`, which requires only a scheme and a host,
and then fail on every single resolve with `net/http`'s "unsupported protocol
scheme".

`http://` additionally requires `WithAllowInsecure()`. Universal Auth POSTs the
client secret in the request body, so a cleartext base URL hands that secret to
anything on the path, and that secret is the key to every value the backend
serves. `WithAllowInsecure()` takes no argument deliberately: an opt-in that
cannot be switched on by a `bool` variable that happened to default to `true` is
the safer shape for a decision of that weight. It permits cleartext `http` and
nothing else, never a way to skip the scheme check itself.

Both checks run on the first `Resolve` rather than in `New`, because `New` returns
no error: keeping it single-valued is what lets a blank import register the scheme
from `init`. `mamori doctor` resolves every ref before deployment, so a
misconfiguration still surfaces before production.

## Error classification

Status classification is `httpcore.ClassifyStatus`, shared with every other
REST-backed provider, so this table is not re-derived here and cannot drift from
the one the conformance `Fail` hook inverts:

| HTTP status | mamori kind |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else (500, 502, ...) | `unavailable` |

Two rows carry weight beyond their status:

- **404 is `not_found`, and nothing else.** That is the one kind that changes
  mamori's behaviour rather than only its reporting: it is what makes a field's
  `default:` and `optional` handling apply instead of failing the whole snapshot.
  A misconfiguration must therefore never be reported as `not_found`, which is
  why a missing project id, an empty secret name and a malformed 200 response all
  wrap `mamori.ErrInvalid` instead.
- **422 is `invalid`, not `unavailable`.** Infisical is the backend in this
  ecosystem that answers `422 Unprocessable Entity`, and the default kind is
  transient: mamori would back off and retry a request that was well formed and
  semantically wrong, which no amount of retrying can fix. `httpcore` names 422
  explicitly for exactly this provider.

A failure on the **login** leg keeps its own kind rather than being flattened into
`unauthenticated`. `httpcore`'s `authError` adds `ErrUnauthenticated` only to an
authenticator error that carried no classification, because `mamori.ErrorKind`
tests `KindUnauthenticated` before `KindUnavailable`: adding it unconditionally
would report a passing blip at Infisical's identity endpoint as a terminal
credential failure, and mamori treats `unauthenticated` as terminal while
`unavailable` is expected to heal on its own.

### Nothing secret reaches an error

- **The vendor's `message` field, and only that field, is surfaced.** A response
  body can be the resolved secret itself, which is why `httpcore` leaves its
  `ErrorDetail` hook nil by default and makes each provider decide. On the read
  path the error envelope is
  `{"statusCode":..,"message":"..","error":".."}`, and the message names the
  secret and the reason rather than a value. A `message` that is not a string
  (Infisical answers some validation failures with an array) suppresses the detail
  rather than being guessed at.
- **The login response never reaches an error at all.** That one client sets no
  `ErrorDetail` hook: it is the only response in this provider that is a reply to
  a request which *contained* the client secret, and no vendor guarantee says an
  error envelope cannot echo part of what it rejected.
- **A JSON decode failure drops its cause.** `encoding/json` quotes the offending
  byte in a syntax error, and on the read path a 200 body *is* the secret. The
  status and the secret name are enough to act on.
- **The access token travels in an `Authorization` header, never a query
  parameter**, so `httpcore`'s URL redaction has nothing to catch on this path
  and no request URL in an error can carry it.

## No native watch

The Infisical read API exposes no streaming read, no blocking read, and no ETag
this provider could gate a conditional GET on, so unlike `providers/https` there
is no `httpcore.Revalidator` here: every poll is a full read. This provider
deliberately does not implement `mamori.WatchableProvider`, and mamori wraps it in
the polling adapter instead, using `Value.Version` (the backend revision) to
detect a change between ticks. Pinned by `TestProviderIsNotWatchable`.

If many refs share a poll interval and you want to avoid a round trip on every
tick, compose [`middleware.Cache`](../../middleware/) in front of this provider to
coalesce reads over a TTL you choose.

## Configuration

```go
import infisical "github.com/xavidop/mamori/providers/infisical"

p := infisical.New(
    infisical.WithClientID(os.Getenv("INFISICAL_CLIENT_ID")),
    infisical.WithClientSecret(os.Getenv("INFISICAL_CLIENT_SECRET")),
    infisical.WithProjectID("abc123"),
    infisical.WithEnvironment("prod"),
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

### Scope and its precedence

A ref option wins over a provider option, which wins over the environment
variable. That is exactly `providers/cloudflare-kv`'s `?namespace=` rule, so an
operator who knows one knows the other.

| Scope | Provider option | Ref option | Environment variable | Default |
| --- | --- | --- | --- | --- |
| Project id | `WithProjectID` | `?project=` | `INFISICAL_PROJECT_ID` | none, **required** |
| Environment | `WithEnvironment` | `?env=` | `INFISICAL_ENVIRONMENT` | omitted from the request |
| Secret path | `WithSecretPath` | `?path=` | `INFISICAL_SECRET_PATH` | `/` |

An unconfigured environment is **omitted** from the request rather than sent
empty, because the API treats the parameter as optional and "absent" is not the
same request as "present and empty".

### Options

| Option | Effect |
| --- | --- |
| `WithClientID(id)` | Set the machine identity's client id; an empty id falls back to `INFISICAL_CLIENT_ID` |
| `WithClientSecret(secret)` | Set the machine identity's client secret, captured in a closure rather than a field; an empty secret falls back to `INFISICAL_CLIENT_SECRET` |
| `WithProjectID(id)` | Set the default project id for refs with no `?project=` |
| `WithEnvironment(slug)` | Set the default environment slug for refs with no `?env=` |
| `WithSecretPath(path)` | Set the default folder for refs with no `?path=` |
| `WithBaseURL(u)` | Override `https://app.infisical.com` for a self-hosted install; a trailing slash is trimmed |
| `WithAllowInsecure()` | Permit an `http://` base URL, and nothing else |
| `WithHTTPClient(c)` | Inject a custom `*http.Client` for both the login and the read; a nil client is a no-op |

## Testing status

Wire shapes are pinned from Infisical's own API reference on 2026-08-04:
<https://infisical.com/docs/api-reference/endpoints/secrets/read> and
<https://infisical.com/docs/api-reference/endpoints/universal-auth/login>.
Nobody on this project has Infisical credentials, so the rows below marked
**Documented, not live-verified** are exactly that: taken from the vendor
reference rather than confirmed against a live call. They are not claimed to be
more than that, and the integration test below is how the first person with a
machine identity closes the gap.

| Aspect | Status |
| --- | --- |
| `providertest.Run` conformance suite | **Verified** - runs against an in-process fake `http.RoundTripper` (`go test ./...`) |
| Login request shape (JSON `{clientId, clientSecret}`, not the RFC 6749 form) and endpoint path | **Documented, not live-verified** ([vendor reference](https://infisical.com/docs/api-reference/endpoints/universal-auth/login)); the shape this provider sends is pinned by `TestLoginPostsTheDocumentedJSONBody` |
| Login response shape (`accessToken`, `expiresIn`) | **Documented, not live-verified** ([vendor reference](https://infisical.com/docs/api-reference/endpoints/universal-auth/login)) |
| Read endpoint path and query (`/api/v4/secrets/{name}?projectId=&environment=&secretPath=`) | **Documented, not live-verified** ([vendor reference](https://infisical.com/docs/api-reference/endpoints/secrets/read)); what this provider sends is pinned by `TestResolveSendsProjectEnvironmentAndPath` |
| Read response shape (`{"secret":{"secretKey","secretValue","version"}}`) | **Documented, not live-verified** ([vendor reference](https://infisical.com/docs/api-reference/endpoints/secrets/read)) |
| Value mapping: `secretValue` to `Bytes`, `version` to `Version`, `Sensitive` always true | **Verified** (`TestResolveReturnsSecretValue`, `TestResolveUsesBackendVersion`, `TestResolveMarksSensitive`) |
| Content-hash `Version` fallback when the backend sends no revision, and one version per secret rather than per selected fragment | **Verified** (`TestResolveHashesWhenBackendSendsNoVersion`, `TestResolveVersionIgnoresKeySelection`) |
| Scope precedence: ref option over provider option over environment, for project, environment and secret path | **Verified** (`TestSettingsPrecedence`) |
| Credential precedence: explicit option over environment variable, read lazily at resolve time | **Verified** (`TestCredentialPrecedence`, `TestCredentialsReadFromEnvironment`) |
| An unconfigured environment is omitted rather than sent empty; the secret path defaults to `/` | **Verified** (`TestResolveOmitsEmptyEnvironment`, `TestResolveDefaultsSecretPathToRoot`) |
| A secret name containing slashes travels as one `url.PathEscape`'d segment; a percent-encoded name stays a literal name | **Verified** (`TestResolveSecretNameWithSlashIsEscapedNotSplit`, `TestResolvePercentEncodedTraversalIsALiteralName`) |
| A `.`/`..` segment, including a backslash-separated one, is refused before anything is sent | **Verified** (`TestResolveRejectsDotSegments`, plus `httpcore`'s own `TestDoRejectsDotSegments`) |
| `#field` and `#/json/pointer` selection through `mamori.SelectKey` | **Verified** (`TestResolveSelectsTopLevelKey`, `TestResolveSelectsWithPointer`, plus `providertest`'s `JSONPointerSelection` case) |
| Every status the backend can return (400/401/403/404/422/500, plus 502 and 429 from a proxy) maps to its documented `mamori.Kind` | **Verified** (`TestStatusToKind`, plus `providertest`'s `ErrorClassification` case) |
| A 404 is `not_found` and nothing else; a misconfiguration is never reported as `not_found` | **Verified** (`TestNotFoundIsNotFoundAndNothingElse`, `TestResolveRequiresProjectID`, `TestResolveResponseWithoutSecretObject`) |
| A login failure keeps its own kind rather than being flattened to `unauthenticated` | **Verified** (`TestLoginFailureIsClassified`) |
| The vendor `message` is surfaced; a non-string `message` suppresses the detail | **Verified** (`TestErrorDetailSurfacesTheVendorMessage`, `TestErrorMessageIgnoresANonStringMessage`) |
| No secret value, access token or client secret ever reaches an error, including when the backend echoes them in its error envelope | **Verified** (`TestResolveErrorCarriesNoSecretValue`, `TestResolveErrorCarriesNoAccessToken`, `TestLoginErrorNeverCarriesTheSecret`, `TestResolveMalformedResponseNeverEchoesTheBody`, `TestLoginResponseThatIsNotJSONNeverEchoesTheBody`) |
| Neither credential appears in `%v`, `%+v` or `%#v` of the `Provider` or the authenticator, before or after an exchange | **Verified** (`TestProviderNeverPrintsTheClientSecret`, `TestAuthenticatorNeverPrintsItsCredentials`) |
| The access token is cached across resolves; the VALUE never is | **Verified** (`TestResolveReusesTheAccessToken`, `TestResolveIsNotCached`) |
| Token refresh at the leeway boundary, one shared exchange under concurrency, and a waiter released by its own context | **Verified** (`TestTokenIsCachedUntilLeewayBeforeExpiry`, `TestConcurrentApplyPerformsOneLogin`, `TestWaiterIsReleasedByItsOwnContext`) |
| Base URL scheme is a closed set; `http://` needs `WithAllowInsecure()`, which rescues nothing else | **Verified** (`TestResolveRejectsNonHTTPBaseURL`, `TestResolveRejectsInsecureBaseURL`, `TestAllowInsecurePermitsHTTPAndNothingElse`) |
| `WatchableProvider` is deliberately not implemented | **Verified** (`TestProviderIsNotWatchable`) |
| End to end against a real Infisical instance: that the documented wire shapes above are what the backend actually sends | **Needs a live backend** - see the integration test below |

The unit and conformance tests run against an in-process fake `http.RoundTripper`
that emulates both endpoints (including injectable per-leg failures whose error
envelopes deliberately echo the credential and the secret value, so the
"nothing secret reaches an error" assertions are falsifiable). `go test ./...`
therefore requires **no** network access and **no** Infisical credentials.

### Live integration test

An integration test exercises a real Infisical instance. It is guarded by a build
tag and skips unless `INFISICAL_CLIENT_ID`, `INFISICAL_CLIENT_SECRET`,
`INFISICAL_PROJECT_ID` and `INFISICAL_TEST_SECRET_NAME` (the name of an existing
secret) are all set. It never logs a client secret, an access token or a resolved
value: only the secret name and a byte count.

```sh
export INFISICAL_CLIENT_ID=...
export INFISICAL_CLIENT_SECRET=...
export INFISICAL_PROJECT_ID=...
export INFISICAL_TEST_SECRET_NAME=SOME_EXISTING_SECRET
export INFISICAL_ENVIRONMENT=dev          # optional
export INFISICAL_SECRET_PATH=/            # optional, defaults to /
export INFISICAL_BASE_URL=https://...     # optional, self-hosted installs
GOWORK=off go test -tags integration -run Integration ./...
```

## Development

This provider is its own Go module. Run all commands with the workspace disabled:

```sh
cd providers/infisical
GOWORK=off go mod tidy
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go vet -tags integration ./...
GOWORK=off go test ./...
GOWORK=off go test -race ./...
```
