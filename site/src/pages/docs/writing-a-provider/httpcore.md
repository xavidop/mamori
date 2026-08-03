---
layout: ../../../layouts/DocsLayout.astro
title: HTTP core
---

# HTTP core

If your backend is a REST API, do not hand-roll the HTTP. `providers/httpcore` is a small, dependency-free library that does the request building, credential injection, status classification, conditional GET, and response-body hygiene, so your provider is only the part that is actually specific to your backend: the URL shape and the response envelope.

| | |
| --- | --- |
| Module | `github.com/xavidop/mamori/providers/httpcore` |
| Registers a scheme | no - it is a library, not a provider |
| Dependencies | the standard library and `github.com/xavidop/mamori` |
| Used by | `providers/https`, and the HTTP providers migrating onto it |

It exists because sixteen of mamori's providers speak HTTP and each one used to hand-roll the same four things. [Issue #107](https://github.com/xavidop/mamori/issues/107) was that duplication surfacing as a bug: body draining was inconsistent across providers, so some connections were abandoned instead of reused. A shared core fixes that class once.

```bash
go get github.com/xavidop/mamori/providers/httpcore
```

```go
import "github.com/xavidop/mamori/providers/httpcore"
```

An ordinary import, never a blank one: `httpcore` registers no scheme, because it is not a provider.

## What you get

```go
c, err := httpcore.New(httpcore.Config{
	BaseURL: "https://api.example.com/v1",
	Auth:    httpcore.Bearer(os.Getenv("EXAMPLE_TOKEN")),
})
if err != nil {
	return err
}

resp, err := c.Do(ctx, httpcore.Request{Path: ref.Path})
```

`Do` performs one round trip: it applies `Auth`, joins `Request.Path` onto `BaseURL`, bounds the response at `MaxBody` (1 MiB by default), always drains and closes the body, and classifies a non-2xx status onto a mamori sentinel.

| Unit | What it does |
| --- | --- |
| `Client` / `Do` | One bounded, always-drained, classified round trip. |
| `Authenticator` | `Bearer`, `HeaderAuth`, `BasicAuth`, `QueryAuth`, `OAuth2ClientCredentials`. |
| `ClassifyStatus` | Maps an HTTP status onto a `mamori` error sentinel. |
| `StatusForKind` | The exact inverse, for your `providertest` `Fail` hook. |
| `Revalidator` | Turns a repeated poll into a conditional GET. |
| `Version` | Derives `Value.Version` from ETag, then Last-Modified, then a body hash. |

## Four guarantees you inherit

**A path cannot escape your `BaseURL`.** `Do` rejects a `Request.Path` containing a `.` or `..` segment, in either its literal or its percent-encoded form, and treats `\` as a separator too, because IIS and ASP.NET decode `%5C` and honor it as one. Handing `ref.Path` straight to `Request.Path` is safe from traversal for exactly that reason, but that safety is scoped to traversal alone. `Request.Path` is an ESCAPED path, the same form `net/url` calls `RawPath`, so a literal `%` in it must already be written `%25`. Forwarding `ref.Path` unmodified satisfies that automatically, because mamori never decodes a ref's path; a provider that instead builds its own path by concatenating pieces must escape it itself, with `url.PathEscape`, before handing it to `Request.Path`. This is enforced in `httpcore` rather than left to each provider precisely so that no provider can be the one that forgets, and it matters because `${VAR}` interpolation means a ref path carries values the application supplies at runtime.

**The response body never reaches an error unless you say so.** A body can be the resolved value itself - a config value, a secret, a token - so `ClassifyStatus` carries the status alone by default. `Config.ErrorDetail` is the hook through which your backend's error envelope reaches the message, called only on a failing status and only with at most `MaxBody` bytes. Parse it, return the one field you have decided cannot carry the value, and return `""` when in doubt.

**Every response body is drained and closed, on every path.** Including the ones that return an error. That is the hygiene issue #107 was about.

**A repeated poll is a conditional GET.** `Revalidator` remembers the last `ETag` / `Last-Modified` and body per key, so an unchanged value costs a `304` instead of a full payload. It reports the validators the cache holds rather than whatever the `304` carried, since a CDN that omits them would otherwise make `Version` change on a poll that changed nothing.

## Error classification

| HTTP status | mamori kind |
| --- | --- |
| 400, 422 | `invalid` |
| 401 | `unauthenticated` |
| 403 | `permission_denied` |
| 404 | `not_found` |
| 408, 429 | `rate_limited` |
| anything else | `unavailable` |

2xx and 304 are not failures and return `nil`.

`StatusForKind` is the exported inverse, and it exists for your conformance test: `providertest`'s `ErrorClassification` case injects a mamori sentinel, but an HTTP fake can only fail a request with a status code, so your `Fail` hook needs to turn the sentinel back into the status that produces it. Hand-rolling that inverse per provider is what lets the two tables drift, and a drifted inverse does not fail loudly - it quietly makes the conformance case exercise one classification five times instead of five once.

```go
Fail: func(ctx context.Context, key string, err error) error {
	fake.fail(key, httpcore.StatusForKind(mamori.ErrorKind(err)))
	return nil
},
```

## What it deliberately does not do

- **No retry.** mamori's reconciler already backs off and retries a failed resolve. A second retry layer inside the provider would multiply against it, turning a configured five attempts into twenty-five.
- **No vendor error-envelope parsing.** Only you know which field of your backend's error shape is safe to surface, hence the `ErrorDetail` hook rather than a built-in guess.
- **No SSE.** Server-sent events are a separate, planned capability.

## Next

- [Resolve and errors](/docs/writing-a-provider/resolve/) - the contract `Do` helps you satisfy.
- [Conformance](/docs/writing-a-provider/conformance/) - where `StatusForKind` gets used.
- [Generic HTTPS provider](/docs/providers/https/) - a complete provider built on `httpcore`, worth reading as a worked example.
- The module [README](https://github.com/xavidop/mamori/tree/main/providers/httpcore) carries the full API notes, every code block of which is compile-checked by `example_test.go`.
