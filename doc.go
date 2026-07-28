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
// A source tag's optional #key fragment selects one field from a structured
// payload: a fragment beginning with '/' is an RFC 6901 JSON Pointer
// addressing a value at any depth ("#/credentials/password"), and any other
// fragment is a literal top-level key ("#ca.crt"), exactly as before. See
// [ParseRef] and [SelectKey].
//
// An optional ?decode= query option declares that the resolved value is
// encoded, so core decodes it (base64, base64url, hex, gzip, or trim,
// stacked left to right) before it reaches the field:
// "aws-sm://prod/tls#key?decode=base64". A bad payload is a loud ErrInvalid,
// never a silent passthrough, and a field's default: is used as-is,
// undecoded. See [WithDecodeHook] for arbitrary per-type conversion beyond
// this closed set.
//
// A tag may also reference a variable with ${VAR}, expanded once, before
// parsing, from the map supplied via [WithRefVars]
// ("aws-sm://${ENV}/db#password"). Expansion never reads the ambient
// environment - only WithRefVars, or its explicit opt-in helper [EnvVars],
// supply values - because a ref decides which secret gets read, and that
// must not be steerable by anything able to set an environment variable. An
// undefined variable, an unterminated "${", or an empty "${}" are errors
// rather than a silently empty ref.
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
