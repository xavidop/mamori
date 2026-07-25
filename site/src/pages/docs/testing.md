---
layout: ../../layouts/DocsLayout.astro
title: Testing
---

# Testing

Application code built on `mamori` usually reacts to change: an `OnChange` handler that rotates a DB pool, an `OnError` sink that increments a metric. Testing that reaction against a real provider means standing up a real AWS account, a real Vault, or a real Kubernetes cluster, then waiting on real network latency to see the change land. `mamoritest` replaces that with an in-memory, scriptable `mamori.Provider` you drive by hand, plus two helpers that wait for a change to actually be applied rather than sleeping and hoping.

Where `providertest` (covered on [Write a provider](../writing-a-provider)) proves a provider you're writing behaves correctly, `mamoritest` is for the other side: testing an application that consumes `mamori`, without depending on any of that provider's real backend.

```bash
go get github.com/xavidop/mamori/mamoritest
```

## The scriptable provider

```go
func NewProvider(scheme string) *Provider
```

`Provider` implements both `mamori.Provider` and `mamori.WatchableProvider`, so it works with `Load`, `Watch`, and `Doctor` exactly like a real provider - and a `Watch` against it delivers changes over a native channel, not by polling, so `mamoritest` never trades one kind of flakiness (a real backend) for another (an inline poll loop in every test).

Register it with `mamori.WithProvider` and reference it with `source:"<scheme>://<key>"`, the same as any other provider:

```go
p := mamoritest.NewProvider("mt")

type Config struct {
	Level string `source:"mt://cfg/level"`
}

cfg, err := mamori.Load[Config](context.Background(), mamori.WithProvider(p))
```

Five methods script what the provider does for a given key:

```go
func (p *Provider) Set(key, val string)
func (p *Provider) SetBytes(key string, b []byte)
func (p *Provider) Del(key string)
func (p *Provider) Fail(key string, err error)
func (p *Provider) Clear(key string)
```

- **`Set`** stores a string value for `key` and, if anything is watching that key, pushes the update to it. `key` must match the ref's path exactly - `source:"mt://cfg/level"` is looked up by `Set("cfg/level", ...)`, not by the full `mt://...` URL.
- **`SetBytes`** is the byte-oriented counterpart, for binary payloads.
- **`Del`** removes the value so a subsequent resolve reports `mamori.ErrNotFound`, and pushes that same not-found to any watcher - the same way a real native-watch backend (Kubernetes informers, Firestore, Redis keyspace notifications) delivers a delete. A field with `default:` or `optional` tolerates this and falls back to its default; a required field with no default surfaces it as an unhealthy, erroring field.
- **`Fail`** makes every future resolve of `key` return `err`, and pushes that error to any watcher, until `Clear` is called. Use it to exercise how your code (or mamori's own reconciler) reacts to a provider outage, a revoked permission, or a rate limit - deterministically, without needing the real backend to actually be down.
- **`Clear`** removes an injected failure, restoring normal resolution and, if a watcher is attached, pushing the restored state to it.

A key with neither `Set` nor `Fail` called on it behaves like an absent key: resolving it returns `mamori.ErrNotFound`.

## `WaitForSnapshot`: deterministic change assertions

```go
func WaitForSnapshot[T any](tb testing.TB, w *mamori.Watcher[T], v uint64)
```

A push from `Set`, `Del`, or `Fail` reaches a real `mamori.Watcher` asynchronously, on the reconciler's own goroutine. `WaitForSnapshot` blocks the test until `w.Status().Snapshot` has reached version `v`, then returns - no fixed `time.Sleep` that's either too short (flaky) or too long (slow). It fails the test via `tb.Fatalf` if `v` never arrives within its deadline.

The version numbering is the reconciler's, not `mamoritest`'s: the initial load is snapshot version **1**, and the first applied change after that lands on version **2**. A second change lands on **3**, and so on. Get this wrong and `WaitForSnapshot` will correctly time out and fail your test, since the version you asked for genuinely never arrives.

```go
p.Set("db/password", "hunter2")

w, err := mamori.Watch[Config](context.Background(), mamori.WithProvider(p))
// ... err check, defer w.Close()

p.Set("db/password", "rotated") // one change since Watch started
mamoritest.WaitForSnapshot(t, w, 2)
```

## `CaptureErrors` and `WaitForError`: error-path assertions

```go
func CaptureErrors() (mamori.Option, *ErrorCapture)
func WaitForError(tb testing.TB, c *ErrorCapture, kind mamori.Kind) error
```

`CaptureErrors` returns a `mamori.Option` (an `OnError` sink) plus the `*ErrorCapture` it feeds - pass the option straight to `Watch` or `Load`. `WaitForError` then blocks until the capture holds an error classified as `kind` (via `mamori.ErrorKind`), returning that error, or fails the test if none arrives in time. See [Concepts](../concepts#error-kinds) for the full list of `Kind` values.

```go
onErr, errs := mamoritest.CaptureErrors()
w, err := mamori.Watch[Config](context.Background(), mamori.WithProvider(p), onErr)
// ... err check, defer w.Close()

p.Fail("cfg/level", mamori.ErrPermissionDenied)
got := mamoritest.WaitForError(t, errs, mamori.KindPermissionDenied)
```

## Worked example

A complete test that rotates a value and asserts both that `Get()` reflects it and that the `OnChange` handler ran, and a second that fails a key and asserts `OnError` reaches it with the expected kind:

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

	// OnChange runs on its own serialized dispatch goroutine, so signal
	// through a channel rather than writing a bare variable from it.
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

	// The OnChange dispatch itself may land a moment after the snapshot
	// version does, so give it its own short, bounded wait rather than
	// assuming it already ran by the time WaitForSnapshot returned.
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

## Driving poll intervals with `NewFakeClock`

`WaitForSnapshot` and `WaitForError` cover a `mamoritest.Provider`, which delivers changes natively rather than on a poll timer. If your test instead needs to drive time itself - for example, asserting behavior around `mamori.WithPollInterval` against a provider that only polls - inject `mamori.NewFakeClock` with `mamori.WithClock` and advance it manually instead of sleeping:

```go
clk := mamori.NewFakeClock(time.Time{})

w, err := mamori.Watch[Config](context.Background(),
	mamori.WithProvider(p),
	mamori.WithClock(clk),
	mamori.WithPollInterval(30*time.Second),
)
// ... err check, defer w.Close()

clk.Advance(30 * time.Second) // fires the next poll tick deterministically
```

`NewFakeClock` and `WithClock` live in the core `mamori` package, not `mamoritest`, since they're a general-purpose seam for any time-dependent test, not specific to the scriptable provider.

## See also

[Observability](../observability) covers `Doctor`, a pre-deploy check that resolves every field once and reports every failure - useful as the build-tagged CI preflight test that complements the unit-level tests on this page.
