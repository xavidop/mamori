---
layout: ../../../layouts/DocsLayout.astro
title: Nacos provider
---

# Nacos

Application configuration from [Alibaba Nacos](https://nacos.io/). This is one of the few mamori providers with a **native watch** rather than polling: Nacos's long-poll listener tells mamori when a configuration moves, so a change is picked up in about as long as it takes to read it back, not on the next poll tick.

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

- `nacos://application.properties` is the whole configuration `application.properties` in `DEFAULT_GROUP`.
- `nacos://prod/db.json#password` is the `password` field of the JSON at dataId `db.json` in group `prod`.
- `nacos://app.json#/database/dsn` is an RFC 6901 pointer into `app.json`.

```go
type Config struct {
	Raw    string        `source:"nacos://application.properties"`
	DBPass secret.String `source:"nacos://prod/db.json#password"`
	DSN    string        `source:"nacos://app.json#/database/dsn"`
}
```

A path with more than two segments is rejected with `mamori.ErrInvalid` rather than guessed at, so `mamori doctor` catches the typo before deployment.

A Nacos configuration has a third coordinate, the namespace (`tenant` on the wire), and it is **not** in the ref. It lives on the provider (`WithNamespace`, or `NACOS_NAMESPACE`) because it is the boundary a set of credentials is issued against: one server address, one namespace, one login. One provider therefore serves one namespace; register a second provider to reach a second one.

The read endpoint answers with the configuration content itself and no JSON wrapper, so the bytes on the wire are the value and a `#json-key` selects out of them. `Value.Version` is a hash of the selected bytes, not the response's `Last-Modified`: that header has one-second resolution, so a bad configuration corrected seconds after it was pushed would otherwise look unchanged. Hashing the selected bytes also means a field bound to `#log_level` does not fire its `OnChange` because an unrelated key in the same document moved.

## Watch

Nacos's change notification is a **comparison**, not a stream. One round POSTs the MD5 mamori believes the configuration currently has, and the server holds the request open until either that belief becomes wrong or the hold elapses (30s by default, `WithLongPollTimeout`). The response names only *which* configuration moved, never its content, so a round that reports a change is followed by an ordinary read.

Two consequences worth knowing:

- **No update is missed at startup.** The baseline read and the MD5 the first round subscribes with come from the same response, so a write landing between them is answered immediately rather than dropped. A stream-based watch has to attach before it reads; this one carries the "before" with it in every request.
- **A configuration that disappears emits nothing.** The field keeps its last good value, matching mamori's polling adapter, which is likewise silent for a key that does not exist. Republishing the configuration fires the watch again.

The loop itself is [`httpcore.LongPoll`](/docs/writing-a-provider/httpcore/): one goroutine, one round at a time, closing on context cancellation. Because the watch is native, you do not need `WithPollInterval` for a `nacos://` ref.

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

A configuration that does not exist is a 404, which becomes `mamori.ErrNotFound`, so the field's `default:` applies. A 409 means the configuration is being modified right now; it lands on `unavailable`, the transient kind mamori retries. 401, 408, 422 and 429 come from `httpcore`'s general table rather than from Nacos itself, and a fronting gateway or an auth-enabled deployment can produce them.

**No response body ever reaches an error message**, because on a 200 that same body *is* the configuration. Errors name the coordinates instead (`dataId=... group=...`), so a wrong group is distinguishable from a wrong dataId.

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

Supplying only one of username and password is a configuration error rather than a silent fallback to unauthenticated requests, which would work against a server with auth disabled and fail with an opaque 403 against every other one. For any other Nacos auth mode, including the accessKey/secretKey signature Alibaba Cloud's hosted MSE Nacos issues, pass an `httpcore.Authenticator` to `WithAuth`.

**Nacos carries its access token in the query string**, which this provider follows, and a stock Nacos deployment is cleartext `http` on port 8848. A query parameter is in the request line, which a proxy's access log and the server's own request log record in plaintext. Put a TLS terminator in front of Nacos and point `NACOS_SERVER_ADDR` at the `https://` address unless the network between your application and Nacos is already private. Neither the password nor the token is held in a readable struct field, and `httpcore` strips the query from every error it returns, including the `*url.Error` that `net/http` wraps a transport failure in.

This provider speaks the **v1 Open API**, which is what publishes the HTTP long-poll listener, and Nacos documents 2.x as compatible with it, so one code path serves 1.x and 2.x servers. Nacos 3.0 rebuilt the admin surface; on 3.x, confirm the v1 compatibility endpoints are enabled before relying on this provider.

Verified with an in-process fake `http.RoundTripper` that implements the listener protocol as the real server does, so the conformance kit runs without a live backend and both watch cases execute for real. A `//go:build integration` test exercises a real Nacos server when `MAMORI_NACOS_ADDR` is set. Rows above follow Nacos's published documentation where nobody here could verify them live.
