# Documentation gaps design

**Status:** approved
**Date:** 2026-07-30

Adds four pages to the docs site at mamorigo.dev, closing gaps found by auditing
the shipped API surface against what the site actually documents:

| Page | Gap it closes |
| --- | --- |
| `usage/watch-ref.md` | `WatchRef` is exported public API with zero mentions anywhere on the site |
| `usage/options.md` | No single reference for the 22 `Option`s and 3 `HandlerOption`s, or their defaults |
| `migrating-from-viper.md` | `comparison.md` argues why mamori beats Viper, nothing shows how to get there |
| `troubleshooting.md` | No symptom-first page; `getting-help.md` is a links page, not a diagnostic one |

This is documentation only. No Go file changes, so semantic-release cuts no
version, which is correct.

## What this is not

Not a restructuring. These are four additions plus four navigation entries.
Existing pages keep their scope, and `getting-help.md` in particular stays a
links page rather than absorbing the troubleshooting content: "where do I ask a
human" and "why is my value stale" are different questions asked in different
moods, and merging them makes the first harder to find.

## Page 1: WatchRef

`WatchRef` ([watchref.go](../../../watchref.go)) is the library's single-ref
entry point:

```go
func WatchRef(ctx context.Context, p Provider, ref Ref, opts ...Option) <-chan Update
```

It is genuinely public API, it is what the config server itself uses, and it has
never been mentioned on the site. A reader who wants to watch one value and does
not want to declare a config struct currently has no way to discover it exists.

The page must cover:

- **What it does.** Watches one ref, returning a raw `<-chan Update`. It selects
  a provider's native watch when the provider implements `WatchableProvider`, and
  falls back to the shared polling adapter otherwise. This is the identical
  per-position selection the reconciler performs for every ref in a `Watch[T]`,
  exported rather than reimplemented, so the two can never drift.
- **What you give up**, stated plainly, because this is the page's real job.
  There is no struct, so no `validate:` tags and no validation. No atomic swap
  and no last-good snapshot: you get raw updates, including error updates, and
  you decide what to do with them. No `PreApply` gate, no `Change` diffing, no
  `Status`, no `Health`, no history or pinning. A reader who needs any of those
  wants `Watch[T]`.
- **Which options actually apply.** Of the whole `Option` surface, only `clock`,
  `pollInterval`, `jitter`, and the `WithBackoff` window reach `WatchRef`, since
  those are what the polling adapter reads. The full surface is accepted so a
  caller needs no second, narrower vocabulary, but the page should say which ones
  do nothing here rather than let a reader assume `WithValidator` has an effect.
- **The channel contract.** Closed when `ctx` is cancelled. A transient error
  arrives as an `Update` with a non-nil `Err` and the channel stays open;
  closure is what signals termination.

Placement: under "Loading & watching", after `usage/refresh`.

## Page 2: Options reference

Every option is currently documented where it happens to be relevant, which
means a reader tuning behavior has no single place to look and no way to learn a
default without reading source. The page is one reference table.

Grouping is **by what the reader is trying to do**, not alphabetically, because
nobody arrives already knowing the option's name:

- **Sources and decoding:** `WithProvider`, `WithRefVars`, `WithDecodeHook`,
  `WithValidator`, `WithExecProvider`
- **Cadence and retry:** `WithPollInterval`, `WithJitter`, `WithBackoff`,
  `WithStale`, `WithClock`
- **Reacting to change:** `OnChange`, `OnError`, `PreApply`,
  `WithPreApplyTimeout`, `WithDebounce`, `WithQueueDepth`, `WithHistory`
- **Observability:** `WithLogger`, `WithMeter`, `WithTracer`
- **Admin endpoint:** `WithAdminHTTP`, `WithAdminTLS`, and the three
  `HandlerOption`s (`WithAuth`, `HandlerPrefix`, `HandlerMiddleware`), with the
  `Option` versus `HandlerOption` distinction made explicit since they are not
  interchangeable

Every default is stated, taken from `defaultOptions()` in
[reconcile.go](../../../reconcile.go):

| Option | Default |
| --- | --- |
| `WithPollInterval` | 30s |
| `WithJitter` | 0.2 |
| `WithDebounce` | 500ms |
| `WithQueueDepth` | 16 |
| `WithPreApplyTimeout` | 10s |
| `WithBackoff` | **disabled** unless set |
| `WithStale` | unset (no staleness threshold) |
| `WithHistory` | unset (no retained snapshots) |
| `WithLogger` | a discard logger (silent) |
| `WithMeter` / `WithTracer` | no-op |
| `WithClock` | `SystemClock()` |
| `WithValidator` | go-playground/validator |

`WithBackoff` being off by default is the entry most worth stating loudly: a
reader tuning retry behavior would reasonably assume some backoff already exists.

The page links to the deeper page for options that have one rather than
duplicating it, so this stays a reference and does not become a second copy of
`usage/watching` or `observability`.

## Page 3: Migrating from Viper

A standalone top-level page, not a section inside `comparison.md`.
`comparison.md` makes the case for mamori; a migration guide answers a different
question for a reader who is already convinced. Mixing the two serves neither.

The spine is the incremental path the `providers/viper` module already enables,
and the guide's whole point is that no big-bang rewrite is required:

1. **Keep Viper exactly as it is.** It stays the source of truth for everything
   it already resolves, with its precedence rules intact.
2. **Declare a config struct** whose fields point at `viper://` refs. Behavior is
   unchanged, because Viper is still doing the resolution.
3. **Move secrets first**, since that is what Viper does worst: a `secret.String`
   field pointed at `aws-sm://` or `vault://` gains redaction, rotation, and the
   `PreApply` gate immediately, while the rest of the config keeps flowing
   through Viper.
4. **Migrate the remainder opportunistically**, field by field, as it becomes
   worth it. Some of it may never need to move.

It should also be honest about what does not carry over: mamori has no
equivalent of Viper's `Set`/`BindPFlag` mutation API, because the config struct
is resolved from sources rather than assembled imperatively. A reader relying
heavily on runtime `Set` calls should know that before starting.

Placement: top level, after `comparison` (Why mamori), so the argument and the
path sit next to each other without being the same page.

## Page 4: Troubleshooting

Organized symptom-first, because that is what a reader actually has. Each entry
names the symptom, the likely causes in the order worth checking, and the
specific knob or diagnostic that confirms it.

- **My value is not updating.** Causes: the field is polled rather than natively
  watched and the interval has not elapsed (default 30s); the provider has no
  native watch at all; the value genuinely has not changed, since the poller
  emits only on change (by `Version`, or by bytes when `Version` is empty).
  Diagnostics: `w.Status()` for per-field age, `WithStale` to make staleness
  visible, `w.Refresh(ctx)` to force a re-resolve now.
- **My `OnChange` never fires, or fires once for several changes.** Causes:
  debounce coalescing a burst into one event (default 500ms); a handler slow
  enough that the bounded dispatch queue filled and the oldest event was dropped.
  Diagnostics: `WithDebounce`, `WithQueueDepth`, and `Meter.RecordChangeDropped`,
  which exists precisely so this is alertable rather than invisible.
- **My update was rejected and `Get()` still returns the old config.** That is
  the designed behavior, not a bug. Causes: the candidate failed validation, or a
  `PreApply` gate rejected it. Diagnostics: `OnError` receives a `*PreApplyError`
  in the second case, and `Meter.RecordApplyRejected` reports a `RejectReason`
  of exactly `RejectValidation` or `RejectPreApply`.
- **My process will not start.** `Watch` performs a fail-fast initial load, so an
  unreachable backend at startup fails outright rather than serving a partial
  config. Diagnostics: `mamori.Doctor[T]` reports every failing ref at once
  rather than just the first; error kinds distinguish a terminal cause
  (`not_found`, `permission_denied`, `unauthenticated`, `invalid`) from a
  transient one (`unavailable`, `rate_limited`). Consider `optional:"true"` or an
  `onfail:` policy for fields that should not be able to block startup.
- **My secret is showing up in logs.** Causes: the field is a plain `string`
  rather than `secret.String`/`secret.Bytes`, or something calls `Reveal()` and
  passes the result somewhere logged. Diagnostic: `mamori vet ./...`, which flags
  exactly the first case.

Placement: under "More", before `getting-help`, so the diagnostic page is reached
before the ask-a-human page.

## Navigation

Four entries in `site/src/layouts/DocsLayout.astro`:

- `usage/watch-ref` titled "Watching one ref", indented under "Loading & watching"
- `usage/options` titled "Options reference", indented in the same group
- `migrating-from-viper` titled "Migrating from Viper", top level in the first group after `comparison`
- `troubleshooting` titled "Troubleshooting", top level in "More" before `getting-help`

## Accuracy requirement

Every option name, default, type signature, and behavioral claim in these pages
must be verified against the source at the time of writing, not recalled. A
reference page that states a wrong default is worse than no reference page,
because a reader will trust it and stop checking. The implementation plan
carries an explicit verification step for the defaults table.

## Cross-links to add

Only where a link closes the discovery gap that motivated the page:

- `usage/index.md` gains a pointer to `usage/watch-ref` for the single-ref case.
- `usage/watching.md` gains a pointer to `usage/options` for the tuning knobs it
  mentions in passing.
- `comparison.md` gains a pointer to `migrating-from-viper` from its Viper row or
  the surrounding prose.
- `providers/viper.md` gains a pointer to `migrating-from-viper`.
- `getting-help.md` gains a pointer to `troubleshooting` as the thing to read
  before filing an issue.

## Testing

There is no test suite for documentation. Verification is:

1. `cd site && npm run build` succeeds and emits all four pages into `site/dist`.
2. Every internal link added resolves to a real page.
3. No em-dash (U+2014) in any file touched.
4. Every default in the options table matches `defaultOptions()` and the
   `default*` constants in the source.
