# Engine logging design

**Status:** approved
**Date:** 2026-07-29

Adds structured logging to the mamori engine through `log/slog`, configured
with a new `WithLogger` option.

## The gap

The engine emits nothing. `grep` for `log.` or `slog.` across core returns only
doc-comment examples. An operator running a process that reconciles
configuration in the background has no record of any of it: not a failed
resolve, not a rejected candidate, not a dropped change event.

The existing hooks do not close this. `OnError` receives errors, but a caller
must write the logging themselves, it fires only for errors, and it says
nothing about what succeeded. `Meter` counts things but carries no detail: a
counter reports that three resolves failed, never which refs or why. The admin
endpoint reports current state, not history, and is unavailable precisely when
the process is unhealthy enough to matter.

The result is that mamori's most valuable behaviour, silently keeping
configuration correct while the world changes underneath it, is also its least
inspectable. When it works there is no evidence, and when it stops working
there is no trail.

## Scope

This adds logging only. It deliberately does **not** extend the `Meter`
interface.

Several things worth counting are currently uncounted: dropped change events,
stale values, `PreApply` rejections. Adding methods to `Meter` would break
every existing implementation, including `x/otel`, and that compatibility
question deserves a decision of its own rather than being settled inside a
logging change. Logging covers the same failures in the meantime, with more
detail than a counter could carry.

## The option

```go
func WithLogger(l *slog.Logger) Option
```

The default is `slog.New(slog.DiscardHandler)`, not `slog.Default()`.

A library that writes to the application's stderr because it was linked in has
made a decision that belongs to the application. mamori stays silent until
asked, and `WithLogger(slog.Default())` is the one-line opt-in for callers who
want it to use theirs. `slog.DiscardHandler` reports `Enabled() == false` for
every level, so the cost of a disabled log site is one interface call and no
allocation of the attributes.

A nil logger passed to `WithLogger` resets to the discard logger rather than
panicking on first use, since a caller wiring one up conditionally will pass
nil for the off case.

## What gets logged

Chosen so that the log alone answers "is my configuration current, and if not,
since when and why".

| Event | Level | Why |
| --- | --- | --- |
| Resolve failed | Warn | The field is still serving its last good value, so this is not yet an outage |
| Resolve recovered after failure | Info | A recovery is as operationally significant as the failure, and nothing else reports it |
| Watch channel error | Warn | The provider's native watch broke; mamori falls back but freshness degrades |
| Candidate rejected by validation | Error | New configuration was refused and the old one is still serving, which is a divergence between what the backend holds and what the process uses |
| `PreApply` rejected the change | Warn | Same divergence, deliberately caused by the application's own gate |
| Change applied | Info | The one positive signal: which fields changed and to which versions |
| Value went stale past `WithStale` | Warn | The value has not refreshed for longer than the caller declared acceptable |
| Change event dropped, queue full | Warn | Currently invisible. `WithQueueDepth` drops the oldest event when the queue is full, and no counter, callback, or report records it |
| Provider has no native watch, polling | Debug | Explains an unexpected propagation delay |

The dropped-event line is the one that most justifies this work. A caller whose
`OnChange` handler is slower than the change rate silently loses events today,
with no way to discover it.

## Never log a value

Log records carry the field path, the scheme, the redacted ref, the version,
and the error. They never carry `Value.Bytes`, and never carry a resolved
value in any form.

Refs go through the existing `redactRef`, which strips inline credentials from
query options such as `?token=` and `?password=` and already backs the admin
endpoint's `Report`.

This is a correctness property, not a guideline, and it is tested: a test
resolves a field whose value is a known sentinel string, captures everything
written to the logger at `Debug` level, and asserts the sentinel appears
nowhere in the output. A second asserts a ref carrying `?token=hunter2` logs
the redacted form. Without those, the natural instinct to include "the value
that failed to validate" in an error log would leak a secret into a file that
is, by design, shipped off the host.

## Where the calls go

One unexported helper per event on the engine, so call sites stay one line and
the attribute vocabulary is defined in a single place rather than drifting
across `resolve.go` and `reconciler.go`.

Attribute keys are fixed and shared: `field` (dotted path), `scheme`, `ref`
(redacted), `version`, `kind` (the `mamori.Kind` classification), `err`. A
consistent vocabulary is what makes the output queryable, which is the entire
reason to emit structured records rather than formatted strings.

## Interaction with `OnError`

`WithLogger` and `OnError` are independent and may both be set. `OnError`
remains the programmatic hook for a caller that wants to react; logging is for
the operator reading afterwards. An error reaches both, and neither suppresses
the other.

`OnError` runs on the reconciler goroutine, so a slow callback stalls
reconciliation. Logging has the same property and the same constraint: a
handler that blocks blocks the engine. The `WithLogger` doc comment says so,
because the obvious mistake is passing a logger that writes synchronously to a
remote collector.

## Testing

A `slog.Handler` that records into a slice, asserted against for level,
message, and attributes. Tests cover:

- Each event in the table above fires, with the expected level and attributes.
- The silent default: with no `WithLogger`, nothing is written anywhere, and a
  `nil` logger behaves the same.
- No value ever appears in output, and refs are redacted.
- The dropped-event log fires when the queue overflows, which also gives that
  path its first test of any kind.

Timing-sensitive cases use the existing `FakeClock` rather than sleeps,
matching the rest of the reconciler's tests.

## Documentation

The observability page on the docs site, the core `README.md` where options are
listed, the `skills/` reference, and a doc comment on `WithLogger` covering the
silent default, the blocking-handler constraint, and the promise that values
are never logged.
