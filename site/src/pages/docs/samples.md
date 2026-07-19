---
layout: ../../layouts/DocsLayout.astro
title: Samples
---

# Samples

A complete, runnable example lives in [`examples/basic`](https://github.com/xavidop/mamori/tree/main/examples/basic): it loads from `env:` and `file://`, watches, and reacts to a live file rotation.

```bash
LOG_LEVEL=debug WORKERS=8 go run ./examples/basic
```

## Rotate a database pool

React only to the field you care about, and keep the value redacted everywhere except the one call that needs it:

```go
w, _ := mamori.Watch[Config](ctx,
	mamori.OnChange(func(ev mamori.Change[Config]) {
		if ev.Changed("DBPassword") {
			pool.Rotate(ev.New.DBPassword.Reveal())
		}
	}),
)
defer w.Close()
```

## Tolerate transient outages, page on real staleness

Fail fast at startup, ride out short backend blips at runtime, and escalate only when a value has been un-refreshable for too long:

```go
w, err := mamori.Watch[Config](ctx,
	mamori.WithStale(10*time.Minute),
	mamori.OnError(func(err error) {
		var stale *mamori.StaleError
		if errors.As(err, &stale) {
			alert.Page("config stale", stale)
			return
		}
		metrics.Inc("config_transient_error")
	}),
)
if err != nil {
	log.Fatal(err) // startup failure: nothing resolved
}
defer w.Close()
```

## Compose providers with middleware

Cache to cut API cost, fail over to a replica, and audit every access:

```go
cfg, err := mamori.Load[Config](ctx,
	mamori.WithProvider(
		middleware.Cache(5*time.Minute,
			middleware.Audit(logger,
				middleware.Failover(primarySM, replicaSM),
			),
		),
	),
)
```

## A file-backed secret, hot-reloaded

Mounted Kubernetes secrets and TLS certs update in place; the built-in `file://` provider watches them with fsnotify, so no restart is needed:

```go
type TLS struct {
	Cert []byte `source:"file:///etc/tls/tls.crt?debounce=0"`
	Key  secret.Bytes `source:"file:///etc/tls/tls.key?debounce=0"`
}
// ?debounce=0 applies certificate updates immediately, with no coalescing delay.
```
