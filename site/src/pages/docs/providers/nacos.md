---
layout: ../../../layouts/DocsLayout.astro
title: Nacos provider
---

# Nacos

Application configuration from [Alibaba Nacos](https://nacos.io/), with **native watch** via its long-poll listener. It speaks the Nacos Open API directly over the standard library: `nacos-sdk-go` is deliberately not a dependency, because the two endpoints this needs are plain HTTP.

| | |
| --- | --- |
| Scheme | `nacos://` |
| Module | `github.com/xavidop/mamori/providers/nacos` |
| Sensitive | no |
| Watch | native (long-poll listener) |
| Auth | `NACOS_USERNAME` / `NACOS_PASSWORD`, or `WithAuth` |

## Install

```bash
go get github.com/xavidop/mamori/providers/nacos
```

```go
import _ "github.com/xavidop/mamori/providers/nacos"
```

## Using the ref

A `nacos://` ref names one configuration by its group and dataId, optionally selecting a field from a JSON value stored there.

```text
nacos://[<group>/]<dataId>[#json-key]
```

| Part | Required | What it means |
| --- | --- | --- |
| `<dataId>` | yes | The configuration's dataId, e.g. `application.properties`. Dots are ordinary characters, so `com.example.svc.yaml` is one dataId. |
| `<group>` | no | The Nacos group. When absent, the provider's group is used, which defaults to Nacos's own `DEFAULT_GROUP`. |
| `#json-key` | no | When the configuration is JSON, return one field from it (via `mamori.SelectKey`). A fragment beginning with `/` is an RFC 6901 JSON Pointer. |

**Examples**

- `nacos://application.properties` - the whole configuration `application.properties` in `DEFAULT_GROUP`.
- `nacos://prod/db.json#password` - the `password` field of the JSON at dataId `db.json` in group `prod`.
- `nacos://app.json#/database/dsn` - an RFC 6901 pointer into `app.json`.

A path with more than two segments is rejected with `mamori.ErrInvalid` rather than guessed at, so `mamori doctor` catches the typo before deployment.

## The namespace is not in the ref

A Nacos configuration is addressed by three coordinates: namespace (called `tenant` on the wire), group, and dataId. Only the last two are in the ref. The namespace lives on the provider (`WithNamespace`, or `NACOS_NAMESPACE`) because it is the boundary a set of credentials is issued against: one server address, one namespace, one login. A ref that could name any namespace would make every struct tag able to reach another tenant's configuration whenever the credentials happened to span both, which is the same reasoning that keeps a raw URL out of a `https://` ref.

## A raw body, not an envelope

Nacos's v1 read endpoint answers with the configuration content itself, with no JSON wrapper. That is unusual among mamori's HTTP-backed providers, which nearly all unwrap a `{"data": ...}` envelope, and it is why this provider decodes nothing: the bytes on the wire **are** the value. A `#json-key` fragment then selects out of them.

`Value.Version` is a hash of the bytes actually returned, not the response's `Last-Modified`. Nacos sends no `ETag`, and `Last-Modified` has one-second resolution, so two publishes inside the same second are indistinguishable through it. During a rollout, where a bad configuration is commonly corrected seconds after it is pushed, mamori would compare two identical versions and skip the correction entirely. Hashing the **selected** bytes rather than the whole document is equally deliberate: a field bound to `#log_level` must not report a change because an unrelated key in the same document moved, which would fire its `OnChange` and, for a credential, force a reconnect for nothing.

## Watch

Nacos's change notification is neither a stream nor a blocking read of the value. It is a **comparison**: one round POSTs "here is the MD5 I believe this configuration currently has", and the server holds the request open until either that belief becomes wrong or the hold elapses. Nothing is pushed, and the response names only *which* configuration moved, never its content, so a round that reports a change is followed by an ordinary read.

The loop is [`httpcore.LongPoll`](/docs/writing-a-provider/httpcore/): one goroutine, one round at a time, a client deadline strictly longer than the hold the server was given, closure on context cancellation, and no re-attempt of a round already reported. This provider supplies only what a baseline is and what a round sends.

That protocol shape is also why the provider can honestly declare `providertest.Config.WatchDeliversBaseline`. The baseline read and the MD5 the first round subscribes with come from the same response, so a write landing between them is answered rather than dropped: the round simply carries an MD5 that is already stale, and the server replies immediately instead of parking. A stream-based watch has to attach before it reads, because its notifications only cover what happens afterwards; a comparison-based one carries the "before" with it in every request.

A configuration that disappears emits nothing, matching mamori's polling adapter, which is silent for a key that does not exist: the field keeps its last good value. The watch resets its remembered MD5 to the empty digest, which is what a Nacos client sends for content it does not hold, so the round parks properly and a republish fires.

### The wire format

The probe is the value of the `Listening-Configs` form field on `POST /nacos/v1/cs/configs/listener`:

```text
dataId  0x02  group  0x02  contentMD5  0x02  tenant  0x01
```

with the tenant field and its preceding separator omitted when there is no namespace. `0x02` is ASCII STX and `0x01` is ASCII SOH, both invisible; because the probe travels as an ordinary `application/x-www-form-urlencoded` value they reach the wire as `%02` and `%01`. The header `Long-Pulling-Timeout: 30000` says how long to hold, in milliseconds.

`Long-Pulling-Timeout` is spelled that way on purpose. It is Nacos's own spelling, and the server decides whether to park a request purely on whether that header is present. Correcting the spelling does not break the watch, which is what makes it dangerous: Nacos falls back to *short* polling and answers immediately either way, so every behavioural test would still pass while the loop hammered the server as fast as it could.

The response is the part most easily got wrong. The server assembles `dataId 0x02 group 0x02 tenant 0x01` and then URL-encodes the whole string, so the body on the wire carries the **literal characters** `%02` and `%01`, not the control bytes. Splitting the raw body on `\x01` finds nothing, and the watch reports "unchanged" forever: it never fires, never errors, and never logs. This provider decodes first, tolerates the un-encoded form as a fallback for whatever proxy sits in front of the endpoint, and pins the encoded shape with a test.

An empty body means the hold elapsed with nothing to report, which is the ordinary outcome several times an hour for every watched configuration.

## Error classification

Every failure goes through `httpcore.ClassifyStatus`:

| HTTP status | mamori kind |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else | `unavailable` |

400, 403, 404 and 500 are named in Nacos's own error table for these endpoints. 409 is not, but the server writes it with `requested file is being modified, please try later.`, and it lands on `unavailable`, which is the transient kind mamori retries. 401, 408, 422 and 429 are the general HTTP mappings inherited from `httpcore` rather than a claim that Nacos itself emits them; a fronting gateway or an auth-enabled deployment can.

A configuration that does not exist is a 404 with the body `config data not exist`, which becomes `mamori.ErrNotFound`, so the field's `default:` applies.

**No response body ever reaches an error.** `httpcore`'s `ErrorDetail` hook is left nil, because on a 200 that same body *is* the configuration: there is no envelope field this provider could select and be certain it is not the value. The error names the coordinates instead (`dataId=... group=...`), so a wrong group is distinguishable from a wrong dataId.

## Configuration

| Setting | Option | Environment | Default |
| --- | --- | --- | --- |
| Server address | `WithServerAddr` | `NACOS_SERVER_ADDR` | `http://127.0.0.1:8848` |
| Servlet context path | `WithContextPath` | `NACOS_CONTEXT_PATH` | `/nacos` |
| Namespace (`tenant`) | `WithNamespace` | `NACOS_NAMESPACE` | empty, i.e. `public` |
| Default group | `WithGroup` | `NACOS_GROUP` | `DEFAULT_GROUP` |
| Username / password | `WithCredentials` | `NACOS_USERNAME` / `NACOS_PASSWORD` | none |
| Arbitrary authenticator | `WithAuth` | | none |
| HTTP client | `WithHTTPClient` | | one whose timeout outlasts the long-poll hold |
| Long-poll hold | `WithLongPollTimeout` | | 30s |
| Response ceiling | `WithMaxBody` | | 1 MiB |

```go
p := nacos.New(
    nacos.WithServerAddr("http://nacos.svc.cluster.local:8848"),
    nacos.WithNamespace("2f9d1b0c-..."),
    nacos.WithCredentials(os.Getenv("NACOS_USERNAME"), os.Getenv("NACOS_PASSWORD")),
)
cfg, err := mamori.Load[Config](ctx, mamori.WithProvider(p))
```

Supplying only one of username and password is a configuration error rather than a silent fallback to unauthenticated requests, which would work against a server with auth disabled and fail with an opaque 403 against every other one.

### The access token travels in the query string

Nacos documents carrying its token as a query parameter, and this provider does that. It is worth knowing what it costs: a query parameter is in the request line, which a proxy's access log and the server's own request log record in plaintext, and a stock Nacos deployment is cleartext `http` on port 8848. Put a TLS terminator in front of Nacos and point `NACOS_SERVER_ADDR` at the `https://` address if the network between your application and Nacos is not already private.

Neither the password nor the token is held in any readable struct field, and `httpcore` strips the query from every error it returns, including the `*url.Error` that `net/http` wraps a transport failure in. A test asserts that neither value appears in an error's text.

### Other authentication modes

Nacos's auth is pluggable. Username/password is implemented here; the others are header injection, which is exactly what an `httpcore.Authenticator` is, so `WithAuth` is the seam. That covers the accessKey/secretKey signature Alibaba Cloud's hosted MSE Nacos issues, and the server identity header used for server-to-server calls.

## Which Nacos version this targets

**Nacos 2.x, using the v1 Open API, which works unchanged against a 1.x server.** The reason is the listener: Nacos publishes an HTTP long-poll listener only at v1. The v2 API added a JSON envelope for reads and moved change notification to gRPC, which this module cannot speak within its dependency budget, and Nacos's own documentation states that 2.X is compatible with the 1.X OpenAPI, so the v1 read and the v1 listener are one coherent pair. Nacos 3.0 rebuilt the admin surface, but its `config/listener` endpoint is a query for *which clients are listening*, not a long-poll listener; on 3.x, verify the v1 compatibility endpoints are enabled before relying on this provider.

Verified with an in-process fake `http.RoundTripper` that implements the listener protocol as the real server does, including the URL-encoded response, so the conformance kit runs without a live backend and both watch cases execute for real rather than skipping. A separate test builds a listener that never reports a change and asserts the watch harness fails against it, which is what keeps the watch assertions from being vacuous. A `//go:build integration` test exercises a real Nacos server when `MAMORI_NACOS_ADDR` is set. See the [module README](https://github.com/xavidop/mamori/tree/main/providers/nacos) for the documentation sources behind every row above, and for which of them are documented rather than live-verified.
