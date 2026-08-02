---
layout: ../../layouts/DocsLayout.astro
title: Testing
---

# Testing

`mamoritest` is an in-memory, scriptable `mamori.Provider` for testing code that consumes `mamori` (an `OnChange` handler, an `OnError` sink) without a real AWS account, Vault, or Kubernetes cluster. Its wait helpers block until a change is actually applied instead of sleeping and hoping.

```bash
go get github.com/xavidop/mamori/mamoritest
```

## Quick start

Create a provider, script a value, `Watch` it, then push a change and wait for it to land before asserting:

```go
func TestConfigReactsToChange(t *testing.T) {
	p := mamoritest.NewProvider("mt")
	p.Set("cfg/level", "info")

	type Config struct {
		Level string `source:"mt://cfg/level"`
	}

	w, err := mamori.Watch[Config](context.Background(), mamori.WithProvider(p))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if w.Get().Level != "info" {
		t.Fatalf("Level = %q, want info", w.Get().Level)
	}

	p.Set("cfg/level", "debug")          // push a change
	mamoritest.WaitForSnapshot(t, w, 2)  // block until it lands (1 = initial load)

	if w.Get().Level != "debug" {
		t.Fatalf("Level = %q, want debug", w.Get().Level)
	}
}
```

`Provider` implements `mamori.Provider` and `mamori.WatchableProvider`, so it works with `Load`, `Watch`, and `Doctor` like a real provider. Register it with `mamori.WithProvider` and reference keys with `source:"<scheme>://<key>"`.

## Drive the fake provider

Five methods script what the provider returns for a key:

```go
func (p *Provider) Set(key, val string)
func (p *Provider) SetBytes(key string, b []byte)
func (p *Provider) Del(key string)
func (p *Provider) Fail(key string, err error)
func (p *Provider) Clear(key string)
```

| Method | Effect |
| --- | --- |
| `Set` | Store a string value and push it to any watcher of `key`. |
| `SetBytes` | Byte-oriented `Set`, for binary payloads. |
| `Del` | Remove the value; later resolves and watchers see `mamori.ErrNotFound`. |
| `Fail` | Every future resolve of `key` returns `err` (and pushes it) until `Clear`. |
| `Clear` | Remove an injected failure and push the restored state. |

- `key` must match the ref's path exactly: `source:"mt://cfg/level"` is keyed by `Set("cfg/level", ...)`, not the full `mt://...` URL.
- A key with neither `Set` nor `Fail` called on it resolves as `mamori.ErrNotFound`.
- After `Del`, a field with `default:` or `optional` falls back to its default; a required field with no default becomes an unhealthy, erroring field.

## Wait for async propagation

A push from `Set`, `Del`, or `Fail` reaches a real `mamori.Watcher` asynchronously on the reconciler's goroutine. These helpers block until the effect is observable, so a test never needs a fixed `time.Sleep`.

### Wait for a change to land

```go
func WaitForSnapshot[T any](tb testing.TB, w *mamori.Watcher[T], v uint64)
```

Blocks until `w.Status().Snapshot` reaches version `v`, then returns; fails via `tb.Fatalf` if `v` never arrives in time. The initial load is version **1**, the first applied change is **2**, the next is **3**, and so on.

```go
p.Set("db/password", "hunter2")

w, err := mamori.Watch[Config](context.Background(), mamori.WithProvider(p))
// ... err check, defer w.Close()

p.Set("db/password", "rotated")     // one change since Watch started
mamoritest.WaitForSnapshot(t, w, 2)
```

### Wait for an error

```go
func CaptureErrors() (mamori.Option, *ErrorCapture)
func WaitForError(tb testing.TB, c *ErrorCapture, kind mamori.Kind) error
```

`CaptureErrors` returns an `OnError` sink option plus the `*ErrorCapture` it feeds; pass the option to `Watch` or `Load`. `WaitForError` blocks until the capture holds an error classified as `kind` (via `mamori.ErrorKind`) and returns it, else fails the test. See [Error kinds](/docs/concepts/error-kinds/) for the `Kind` values.

```go
onErr, errs := mamoritest.CaptureErrors()
w, err := mamori.Watch[Config](context.Background(), mamori.WithProvider(p), onErr)
// ... err check, defer w.Close()

p.Fail("cfg/level", mamori.ErrPermissionDenied)
got := mamoritest.WaitForError(t, errs, mamori.KindPermissionDenied)
```

## A complete test

Rotating a value and asserting both `Get()` and the `OnChange` handler, then failing a key and asserting `OnError` reaches it with the expected kind:

```go
package myapp_test

import (
	"context"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
)

type Config struct {
	DBPassword string `source:"mt://db/password"`
}

func TestRotationTriggersOnChange(t *testing.T) {
	p := mamoritest.NewProvider("mt")
	p.Set("db/password", "hunter2")

	// OnChange runs on its own goroutine; signal through a channel.
	changed := make(chan string, 1)
	w, err := mamori.Watch[Config](context.Background(),
		mamori.WithProvider(p),
		mamori.OnChange(func(ev mamori.Change[Config]) {
			changed <- ev.New.DBPassword
		}),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.Set("db/password", "rotated")
	mamoritest.WaitForSnapshot(t, w, 2) // 1 = initial load, 2 = first change

	// WaitForSnapshot guarantees Get() already reflects the rotated value.
	if w.Get().DBPassword != "rotated" {
		t.Fatalf("Get().DBPassword = %q, want rotated", w.Get().DBPassword)
	}

	// OnChange may land just after the snapshot; give it a bounded wait.
	select {
	case got := <-changed:
		if got != "rotated" {
			t.Fatalf("OnChange saw DBPassword = %q, want rotated", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnChange did not fire after Set")
	}
}

func TestBackendFailureReachesOnError(t *testing.T) {
	p := mamoritest.NewProvider("mt")
	p.Set("db/password", "hunter2")

	onErr, errs := mamoritest.CaptureErrors()
	w, err := mamori.Watch[Config](context.Background(),
		mamori.WithProvider(p), onErr)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.Fail("db/password", mamori.ErrPermissionDenied)

	got := mamoritest.WaitForError(t, errs, mamori.KindPermissionDenied)
	if mamori.ErrorKind(got) != mamori.KindPermissionDenied {
		t.Fatalf("WaitForError returned kind %q, want permission_denied", mamori.ErrorKind(got))
	}
	// The watcher keeps serving the last good value; a failure never blanks it.
	if w.Get().DBPassword != "hunter2" {
		t.Fatalf("Get().DBPassword after Fail = %q, want last-good hunter2", w.Get().DBPassword)
	}
}
```

## Fail the build when config cannot resolve

`Doctor` resolves every field of `T` once against your real provider wiring and reports every failure without starting a watcher. Run it as a build-tagged CI test that fails on any unhealthy field, so a rotated-away secret, a missing IAM permission, or a typo'd ref is caught before it ships:

```go
//go:build preflight

func TestConfigPreflight(t *testing.T) {
	rep, err := mamori.Doctor[Config](context.Background(), appProviders()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Fields {
		if f.LastKind != "" {
			t.Errorf("%s (%s): %s: %s", f.Path, f.Ref, f.LastKind, f.LastError)
		}
	}
}
```

Gate it behind the tag so it only runs where real credentials and network access exist:

```bash
go test -tags preflight ./...
```

Unlike `Load`, `Doctor` never aborts on the first failure, so one run reports every misconfigured ref. See [Doctor](/docs/observability/doctor/) for the full `Report` shape.

`Doctor` also runs your `WithDerive` hooks, so a hook that errors, or one typed for the wrong config, fails this preflight too. A hook that panics is not recovered: it crashes the test process rather than producing a row, so `go test` still catches it, just not through the loop above. See [Doctor: pre-deploy check](/docs/observability/doctor/#derived-fields-are-probed) for the four outcomes a derived row can report.

## Drive poll intervals with the fake clock

`mamoritest.Provider` delivers changes natively, not on a poll timer. To drive time itself (for example, testing `mamori.WithPollInterval` against a provider that only polls), inject `mamori.NewFakeClock` with `mamori.WithClock` and advance it manually:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

clk := mamori.NewFakeClock(time.Time{})

w, err := mamori.Watch[Config](ctx,
	mamori.WithProvider(p),
	mamori.WithClock(clk),
	mamori.WithPollInterval(30*time.Second),
)
// ... err check, defer w.Close()

// Wait for the poll goroutine to arm its timer before advancing.
if err := clk.BlockUntil(ctx, 1); err != nil {
	t.Fatal(err)
}
clk.Advance(30 * time.Second) // fires the next poll tick deterministically
```

That `BlockUntil` is not optional politeness - it is what makes the test deterministic. `Advance` fires only the timers already registered when it runs, and the watch goroutine arms its next timer *after* delivering the previous value, computing the deadline from whatever `Now()` says at that moment. Advance ahead of the registration and the timer is created with a deadline in the time you have already skipped past: it never fires, and the test blocks until some unrelated timeout with nothing to point at.

`BlockUntil(ctx, n)` returns once at least `n` timers are armed on the clock, or `ctx.Err()` if they never are - so an expectation that is simply wrong fails on your own deadline instead of hanging. Reach for it before **every** `Advance` meant to fire a timer belonging to another goroutine. Sleeping first only makes the race rarer.

## How it works

A `Watch` against the fake delivers changes over a native Go channel, not by polling. Each `Watch` replays the key's current state as an immediate baseline and delivers a delete as `ErrNotFound` (not a channel close), mirroring real native-watch backends (Kubernetes informers, Firestore, Redis keyspace notifications). The subscription is torn down when the context is done, leaving no goroutine behind (goleak-checked).

The snapshot versions `WaitForSnapshot` uses are the reconciler's own: `Watcher.Status().Snapshot` starts at 1 for the initial load and increments by one per applied change, so the first change lands on 2. `NewFakeClock` and `WithClock` live in the core `mamori` package, not `mamoritest`, since they are a general-purpose seam for any time-dependent test.

## See also

- [Write a provider](/docs/writing-a-provider/) covers `providertest`, the counterpart kit for provider authors.
- [Doctor](/docs/observability/doctor/) covers `Doctor` and the `Report` shape in full.
- [Error kinds](/docs/concepts/error-kinds/) lists the `Kind` values `WaitForError` matches against.
