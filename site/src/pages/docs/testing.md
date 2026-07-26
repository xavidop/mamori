---
layout: ../../layouts/DocsLayout.astro
title: Testing
---

# Testing

To test application code that consumes `mamori` (an `OnChange` handler that rotates a DB pool, an `OnError` sink that increments a metric) without a real AWS account, Vault, or Kubernetes cluster, use `mamoritest`: an in-memory, scriptable `mamori.Provider` you drive by hand, plus helpers that block until a change has actually been applied instead of sleeping and hoping. (Where `providertest`, covered on [Write a provider](../writing-a-provider), proves a provider you are writing behaves correctly, `mamoritest` tests the other side, an application that consumes `mamori`.)

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

`Provider` implements both `mamori.Provider` and `mamori.WatchableProvider`, so it works with `Load`, `Watch`, and `Doctor` exactly like a real provider. Register it with `mamori.WithProvider` and reference it with `source:"<scheme>://<key>"`, the same as any other provider.

## Drive the fake provider

Five methods script what the provider does for a given key:

```go
func (p *Provider) Set(key, val string)
func (p *Provider) SetBytes(key string, b []byte)
func (p *Provider) Del(key string)
func (p *Provider) Fail(key string, err error)
func (p *Provider) Clear(key string)
```

- **`Set`** stores a string value for `key` and, if anything is watching that key, pushes the update to it. `key` must match the ref's path exactly: `source:"mt://cfg/level"` is looked up by `Set("cfg/level", ...)`, not by the full `mt://...` URL.
- **`SetBytes`** is the byte-oriented counterpart, for binary payloads.
- **`Del`** removes the value so a subsequent resolve reports `mamori.ErrNotFound`, and pushes that same not-found to any watcher. A field with `default:` or `optional` tolerates this and falls back to its default; a required field with no default surfaces it as an unhealthy, erroring field.
- **`Fail`** makes every future resolve of `key` return `err`, and pushes that error to any watcher, until `Clear` is called. Use it to exercise how your code (or mamori's own reconciler) reacts to a provider outage, a revoked permission, or a rate limit, deterministically, without needing the real backend to actually be down.
- **`Clear`** removes an injected failure, restoring normal resolution and, if a watcher is attached, pushing the restored state to it.

A key with neither `Set` nor `Fail` called on it behaves like an absent key: resolving it returns `mamori.ErrNotFound`.

## Wait for async propagation

A push from `Set`, `Del`, or `Fail` reaches a real `mamori.Watcher` asynchronously, on the reconciler's own goroutine. These helpers block until the effect is observable, so a test never needs a fixed `time.Sleep` that is either too short (flaky) or too long (slow).

### Wait for a change to land

```go
func WaitForSnapshot[T any](tb testing.TB, w *mamori.Watcher[T], v uint64)
```

`WaitForSnapshot` blocks the test until `w.Status().Snapshot` has reached version `v`, then returns, and fails the test via `tb.Fatalf` if `v` never arrives within its deadline. The initial load is snapshot version **1**, the first applied change after that lands on version **2**, a second change on **3**, and so on. Ask for a version that never arrives and the helper correctly times out.

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

`CaptureErrors` returns a `mamori.Option` (an `OnError` sink) plus the `*ErrorCapture` it feeds, so pass the option straight to `Watch` or `Load`. `WaitForError` then blocks until the capture holds an error classified as `kind` (via `mamori.ErrorKind`), returning that error, or fails the test if none arrives in time. See [Concepts](../concepts#error-kinds) for the full list of `Kind` values.

```go
onErr, errs := mamoritest.CaptureErrors()
w, err := mamori.Watch[Config](context.Background(), mamori.WithProvider(p), onErr)
// ... err check, defer w.Close()

p.Fail("cfg/level", mamori.ErrPermissionDenied)
got := mamoritest.WaitForError(t, errs, mamori.KindPermissionDenied)
```

## A complete test

A test that rotates a value and asserts both that `Get()` reflects it and that the `OnChange` handler ran, plus a second that fails a key and asserts `OnError` reaches it with the expected kind:

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

## Fail the build when config cannot resolve

`Doctor` resolves every field of `T` exactly once against your real provider wiring and reports every failure, without starting a watcher. Run it as a build-tagged CI test that fails the build on any field that did not come back healthy, so a rotated-away secret, a missing IAM permission, or a typo'd ref is caught before it ships instead of at container startup:

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

Gate it behind a tag so it only runs where real credentials and network access are available:

```bash
go test -tags preflight ./...
```

Unlike `Load`, `Doctor` never aborts on the first failure, so one run tells you about every misconfigured ref at once. [Observability](../observability#check-reachability-before-deploying-with-doctor) documents the full `Report` shape it returns.

## Drive poll intervals with the fake clock

The helpers above cover a `mamoritest.Provider`, which delivers changes natively rather than on a poll timer. If your test instead needs to drive time itself (for example, asserting behavior around `mamori.WithPollInterval` against a provider that only polls), inject `mamori.NewFakeClock` with `mamori.WithClock` and advance it manually instead of sleeping:

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

## How it works

A `Watch` against the fake delivers changes over a native Go channel, not by polling, so `mamoritest` never trades one kind of flakiness (a real backend) for another (an inline poll loop in every test). Each `Watch` registers a buffered subscription and replays the key's current state as an immediate baseline, mirroring how a real native-watch backend (Kubernetes informers, Firestore, Redis keyspace notifications) replays current state to a new subscriber and delivers a delete as `ErrNotFound` on the channel rather than a channel close. The subscription is deregistered and its channel closed when the context is done, leaving no goroutine behind; the kit's own teardown tests are goleak-checked to prove it.

The snapshot version numbering `WaitForSnapshot` relies on is the reconciler's own, not `mamoritest`'s: `Watcher.Status().Snapshot` starts at 1 for the initial load and increments by one per applied change. That is why the first change after `Watch` lands on version 2.

`NewFakeClock` and `WithClock` live in the core `mamori` package, not `mamoritest`, because they are a general-purpose seam for any time-dependent test, not specific to the scriptable provider.

## See also

- [Write a provider](../writing-a-provider) covers `providertest`, the counterpart kit for authors testing a provider they are writing.
- [Observability](../observability#check-reachability-before-deploying-with-doctor) covers `Doctor` and the `Report` shape in full.
- [Concepts](../concepts#error-kinds) lists the `Kind` values `WaitForError` matches against.
