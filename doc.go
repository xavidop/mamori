// Package mamori loads application configuration and secrets from heterogeneous
// sources (environment, files, cloud secret managers, Vault, Kubernetes, ...)
// into typed, validated Go structs, and keeps them reconciled at runtime.
//
// When a source value changes, mamori detects it, re-validates the whole
// configuration, and - only if the new snapshot is valid - atomically swaps it
// in and notifies the application with a diff-aware callback so it can react
// (rotate a database pool, rebuild a client, ...) without restarting.
//
// # Loading
//
// Define a struct whose fields carry a `source` tag describing where each value
// comes from, then call [Load]:
//
//	type Config struct {
//	    DBPassword secret.String `source:"aws-sm://prod/db#password"`
//	    LogLevel   string        `source:"env:LOG_LEVEL" default:"info"`
//	    Workers    int           `source:"env:WORKERS" default:"4" validate:"gte=1,lte=256"`
//	}
//
//	cfg, err := mamori.Load[Config](ctx)
//
// # Watching
//
// [Watch] performs an initial fail-fast load and then keeps the configuration
// reconciled, delivering validated, diff-aware updates:
//
//	w, err := mamori.Watch[Config](ctx,
//	    mamori.OnChange(func(ev mamori.Change[Config]) {
//	        if ev.Changed("DBPassword") {
//	            pool.Rotate(ev.New.DBPassword.Reveal())
//	        }
//	    }),
//	)
//	defer w.Close()
//	cfg := w.Get() // lock-free snapshot; always the last valid config
//
// # Providers
//
// Sources are pluggable via the Provider SPI and registered with [Register]
// using the database/sql pattern. The core module has zero cloud-SDK
// dependencies; each cloud provider ships as its own module under providers/.
package mamori
