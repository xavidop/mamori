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
| Used by | `providers/https`, `providers/nacos`, and the HTTP providers migrating onto it |

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
| `LongPoll` | Drives the watch loop for a backend that holds a request open. |
| `SSEDecoder` | Frames a server-sent-events byte stream, with bounded memory. |
| `SSEStream` | The same over an `*http.Response`, tied to a context. |

`OAuth2ClientCredentials` requires an `https://` `TokenURL` and rejects any other scheme at construction, wrapping `mamori.ErrInvalid`. The grant POSTs `client_secret` in the form body, so a cleartext token endpoint hands that secret to anything on the path, and the scheme is checked against a closed set so an `ftp://` typo fails at startup rather than on every exchange. `OAuth2Config.AllowInsecure` opts into `http://` for a local test identity provider, and into cleartext `http` only.

## Four guarantees you inherit

**A path cannot escape your `BaseURL`.** `Do` rejects a `Request.Path` containing a `.` or `..` segment, in either its literal or its percent-encoded form, and treats `\` as a separator too, because IIS and ASP.NET decode `%5C` and honor it as one. Handing `ref.Path` straight to `Request.Path` is safe from traversal for exactly that reason, but that safety is scoped to traversal alone. `Request.Path` is an ESCAPED path, the same form `net/url` calls `RawPath`, so a literal `%` in it must already be written `%25`. Forwarding `ref.Path` unmodified satisfies that automatically, because mamori never decodes a ref's path; a provider that instead builds its own path by concatenating pieces must escape it itself, with `url.PathEscape`, before handing it to `Request.Path`. This is enforced in `httpcore` rather than left to each provider precisely so that no provider can be the one that forgets, and it matters because `${VAR}` interpolation means a ref path carries values the application supplies at runtime.

**The response body never reaches an error unless you say so.** A body can be the resolved value itself - a config value, a secret, a token - so `ClassifyStatus` carries the status alone by default. `Config.ErrorDetail` is the hook through which your backend's error envelope reaches the message, called only on a failing status and only with at most `MaxBody` bytes. Parse it, return the one field you have decided cannot carry the value, and return `""` when in doubt.

**Every response body is drained and closed, on every path.** Including the ones that return an error. That is the hygiene issue #107 was about.

**A repeated poll is a conditional GET, when the backend gives you something to revalidate with.** `Revalidator` remembers the last `ETag` / `Last-Modified` and body per key, so an unchanged value costs a `304` instead of a full payload. That precondition is real: a response carrying neither `ETag` nor `Last-Modified` is not cached at all, since there would be no validator to send next time, so against a backend that emits neither, every poll stays unconditional and returns the full body. When there is a validator, `Revalidator` reports the one the cache holds rather than whatever the `304` carried, since a CDN that omits them would otherwise make `Version` change on a poll that changed nothing.

## Long polling

If your backend answers a *held-open* request, one where the server keeps the connection until something changes or a hold deadline elapses, you have a native watch and should implement `mamori.WatchableProvider` rather than letting mamori poll you. `LongPoll` is that loop.

```go
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
    var known string // this watch's belief about the current value

    return httpcore.LongPoll(ctx, httpcore.LongPollConfig{
        Hold: 30 * time.Second,
        Baseline: func(ctx context.Context) (httpcore.LongPollResult, error) {
            v, digest, err := p.read(ctx, ref)
            if err != nil {
                return httpcore.LongPollResult{}, err
            }
            known = digest
            return httpcore.LongPollResult{Changed: true, Value: v}, nil
        },
        Round: func(ctx context.Context, hold time.Duration) (httpcore.LongPollResult, error) {
            changed, err := p.poll(ctx, ref, known, hold)
            if err != nil || !changed {
                return httpcore.LongPollResult{}, err
            }
            v, digest, err := p.read(ctx, ref)
            if err != nil {
                return httpcore.LongPollResult{}, err
            }
            known = digest
            return httpcore.LongPollResult{Changed: true, Value: v}, nil
        },
    })
}
```

You write `Round`, and optionally `Baseline`. Everything else is the part that is the same for every long-poll backend, and each piece of it is a bug somebody has shipped:

- **One goroutine, gone by the time the channel closes.** `providertest`'s `NoGoroutineLeak` case runs `goleak.VerifyNone`, so a watch that leaks fails conformance. A cancellation is never delivered as an `Update`: your in-flight request fails with a context error on every clean shutdown, and reporting that would make every shutdown look like a backend fault.
- **A transient failure never closes the channel.** mamori does not resubscribe a closed watch, so closing on the first 503 silently downgrades your native watch to nothing at all. Failures arrive as an `Update` with a non-nil `Err` and the loop continues.
- **The client always outlasts the server.** `Hold` is handed to `Round` rather than read from the config by your code, so the number that goes into the request (a `Long-Pulling-Timeout` header, a `wait` query parameter) and the number that bounds the round are one number. A client deadline shorter than the hold you asked the server for fails every idle round with a timeout that is indistinguishable from an unhealthy backend.
- **`Changed: false` emits nothing.** That is the ordinary outcome of a hold elapsing, several times an hour for every watched value.
- **`Baseline` runs strictly before the first `Round`.** For a comparison-based protocol, where each round carries what the client believes the value to be, that ordering is what lets you honestly set `providertest.Config.WatchDeliversBaseline`: the emitted baseline and the state the first round subscribes with come from one observation, so a write landing between them is answered rather than dropped.

`ErrorPause` is pacing, not retry. Every failure reaches you before the pause, and no round is ever re-attempted; the floor exists only so a backend that rejects a round instantly cannot turn the watch into a hot loop, and it never grows.

[`providers/nacos`](/docs/providers/nacos/) is the worked example.

## Server-sent events

If your backend pushes a stream of events rather than answering a held-open request, you have a native watch of a different shape. `SSEDecoder` frames the stream; `SSEStream` is the same thing over an `*http.Response`.

```go
resp, err := p.openStream(ctx, ref) // your request, your auth
if err != nil {
    return nil, err
}

stream := httpcore.NewSSEStream(ctx, resp, httpcore.SSEConfig{})
defer stream.Close()

for {
    ev, err := stream.Next()
    if err != nil {
        return err // io.EOF at a clean end, a context error on cancel
    }
    switch ev.Name {
    case "put", "patch":
        // ev.Data is the frame's data fields joined with "\n".
    }
}
```

You write the request and decide what each event name means. The framing, and the two bounds below, are the part that is the same for every SSE backend.

- **Two bounds, not one, and they guard different attacks.** `MaxLine` stops a server that opens a line and never sends a newline. `MaxFrame` stops one that sends `data:` forever and never the blank line that would dispatch the frame. Per-line bounding alone does not prevent the second: every line stays small while the accumulated frame grows without limit. Both default to 1 MiB.
- **Peak memory is `MaxFrame + MaxLine`, not `MaxFrame`.** The line buffer exists alongside the accumulating frame. `MaxFrame` also does not count the event name, which arrives on a line and is bounded by `MaxLine`.
- **A breach is retryable, not terminal.** Both bound errors wrap `mamori.ErrUnavailable` rather than `ErrInvalid`, because `Report` treats an invalid field as terminal: a hostile or misconfigured server would otherwise mark the field permanently unhealthy instead of letting your loop reconnect.
- **`ev.Data` is yours to keep.** It is freshly allocated per frame and never reused, so you can retain it past the next `Next` without copying. A decoder that recycled one buffer would be faster and would corrupt any consumer that decodes JSON out of a frame it still holds.
- **No goroutine, and the body is closed exactly once.** `Close` is safe to call alongside a blocked `Next`, and cancelling the context ends the stream.

**There is no reconnect loop here, deliberately.** The two providers that stream disagree about every part of one: a fixed versus an exponential backoff, one endpoint versus rotation, whether a disconnect is reported to the caller, whether a freshness guard outlives the connection. A shared loop would need a hook for each and end up larger than either. Write the loop in your provider, and give it a growing, capped wait so a stream that fails identically every time cannot become a hot loop.

[`providers/firebase-rtdb`](/docs/providers/firebase-rtdb/) is the worked example.

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
- **No reconnect loop.** The framing is here (`SSEDecoder` / `SSEStream`, above) and so is the long-poll loop (`LongPoll`), but reconnecting a dropped stream is your provider's job, for the reason given in that section.

## Next

- [Resolve and errors](/docs/writing-a-provider/resolve/) - the contract `Do` helps you satisfy.
- [Conformance](/docs/writing-a-provider/conformance/) - where `StatusForKind` gets used.
- [Generic HTTPS provider](/docs/providers/https/) - a complete provider built on `httpcore`, worth reading as a worked example.
- [Nacos provider](/docs/providers/nacos/) - the worked example for `LongPoll` and a native watch.
- The module [README](https://github.com/xavidop/mamori/tree/main/providers/httpcore) carries the full API notes, every code block of which is compile-checked by `example_test.go`.
