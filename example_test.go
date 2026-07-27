package mamori_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
	"github.com/xavidop/mamori/secret"
)

// Load reads every field of a struct from the source named in its `source`
// tag, applies `default` for anything the source does not provide, and
// validates the whole result before returning it. A field typed
// [secret.String] is redacted everywhere except an explicit Reveal.
func Example() {
	type Config struct {
		LogLevel string        `source:"env:EXAMPLE_LOG_LEVEL" default:"info" validate:"oneof=debug info warn error"`
		Workers  int           `source:"env:EXAMPLE_WORKERS"   default:"4"    validate:"gte=1,lte=256"`
		APIKey   secret.String `source:"env:EXAMPLE_API_KEY"`
	}

	_ = os.Setenv("EXAMPLE_LOG_LEVEL", "debug")
	_ = os.Setenv("EXAMPLE_API_KEY", "sk-live-abc123")
	defer func() { _ = os.Unsetenv("EXAMPLE_LOG_LEVEL") }()
	defer func() { _ = os.Unsetenv("EXAMPLE_API_KEY") }()

	cfg, err := mamori.Load[Config](context.Background())
	if err != nil {
		fmt.Println("load failed:", err)
		return
	}

	fmt.Println("log level:", cfg.LogLevel)
	fmt.Println("workers:  ", cfg.Workers) // EXAMPLE_WORKERS is unset, so the default applies
	fmt.Println("api key:  ", cfg.APIKey)  // redacted by default
	fmt.Println("revealed: ", cfg.APIKey.Reveal())

	// Output:
	// log level: debug
	// workers:   4
	// api key:   [REDACTED]
	// revealed:  sk-live-abc123
}

// A comma-separated `source` tag is a precedence chain: sources are tried in
// order and the first to yield a value wins, so an operator override can sit in
// front of a centrally managed value without the caller writing that fallback
// by hand. Whitespace around the separator is ignored.
func ExampleLoad_precedenceChain() {
	type Config struct {
		// Prefer an explicit override, fall back to the deploy-wide value,
		// fall back to the default.
		Port string `source:"env:EXAMPLE_PORT_OVERRIDE, env:EXAMPLE_PORT" default:"8080"`
	}

	// Nothing set: the chain falls through to the default.
	cfg, _ := mamori.Load[Config](context.Background())
	fmt.Println("neither set:  ", cfg.Port)

	// The lower-priority source answers.
	_ = os.Setenv("EXAMPLE_PORT", "9090")
	defer func() { _ = os.Unsetenv("EXAMPLE_PORT") }()
	cfg, _ = mamori.Load[Config](context.Background())
	fmt.Println("deploy value: ", cfg.Port)

	// The override wins wherever it is set.
	_ = os.Setenv("EXAMPLE_PORT_OVERRIDE", "3000")
	defer func() { _ = os.Unsetenv("EXAMPLE_PORT_OVERRIDE") }()
	cfg, _ = mamori.Load[Config](context.Background())
	fmt.Println("override wins:", cfg.Port)

	// Output:
	// neither set:   8080
	// deploy value:  9090
	// override wins: 3000
}

// A snapshot that fails validation is rejected whole: Load returns a
// *ValidationError rather than handing back a half-valid config.
func ExampleLoad_validationFailure() {
	type Config struct {
		Workers int `source:"env:EXAMPLE_BAD_WORKERS" default:"4" validate:"gte=1,lte=256"`
	}

	_ = os.Setenv("EXAMPLE_BAD_WORKERS", "9000")
	defer func() { _ = os.Unsetenv("EXAMPLE_BAD_WORKERS") }()

	_, err := mamori.Load[Config](context.Background())

	var verr *mamori.ValidationError
	fmt.Println("rejected as invalid:", errors.As(err, &verr))

	// Output:
	// rejected as invalid: true
}

// Watch keeps the configuration reconciled at runtime. When a source value
// changes, mamori re-resolves, re-validates, atomically swaps the snapshot, and
// calls OnChange with both the old and new value so the application can react
// without restarting.
func ExampleWatch() {
	type Config struct {
		DBPassword secret.String `source:"mem://db-password"`
	}

	// A scriptable in-memory provider stands in for a real secret store.
	prov := mamoritest.NewProvider("mem")
	prov.Set("db-password", "old-password")

	rotated := make(chan string, 1)

	w, err := mamori.Watch[Config](context.Background(),
		mamori.WithProvider(prov),
		mamori.WithDebounce(time.Millisecond),
		mamori.OnChange(func(ev mamori.Change[Config]) {
			if ev.Changed("DBPassword") {
				rotated <- ev.New.DBPassword.Reveal()
			}
		}),
	)
	if err != nil {
		fmt.Println("watch failed:", err)
		return
	}
	defer func() { _ = w.Close() }()

	fmt.Println("before rotation:", w.Get().DBPassword.Reveal())

	prov.Set("db-password", "new-password")

	fmt.Println("OnChange saw:  ", <-rotated)
	fmt.Println("after rotation: ", w.Get().DBPassword.Reveal())

	// Output:
	// before rotation: old-password
	// OnChange saw:   new-password
	// after rotation:  new-password
}

// An update that fails validation is rejected atomically: OnError is notified
// and Get keeps returning the last valid configuration rather than entering a
// broken state mid-flight.
func ExampleWatch_rejectsInvalidUpdate() {
	type Config struct {
		Workers int `source:"mem://workers" validate:"gte=1,lte=256"`
	}

	prov := mamoritest.NewProvider("mem")
	prov.Set("workers", "8")

	rejected := make(chan error, 1)

	w, err := mamori.Watch[Config](context.Background(),
		mamori.WithProvider(prov),
		mamori.WithDebounce(time.Millisecond),
		mamori.OnError(func(err error) {
			var verr *mamori.ValidationError
			if errors.As(err, &verr) {
				select {
				case rejected <- err:
				default:
				}
			}
		}),
	)
	if err != nil {
		fmt.Println("watch failed:", err)
		return
	}
	defer func() { _ = w.Close() }()

	fmt.Println("valid config:", w.Get().Workers)

	prov.Set("workers", "9000") // out of range

	<-rejected
	fmt.Println("after rejected update:", w.Get().Workers)

	// Output:
	// valid config: 8
	// after rejected update: 8
}

// Status reports live per-field health without ever exposing a value, which
// makes it safe to log or serve. Health reduces the same report to a single
// readiness answer suitable for a Kubernetes probe.
func ExampleWatcher_Status() {
	type Config struct {
		Token secret.String `source:"mem://token"`
	}

	prov := mamoritest.NewProvider("mem")
	prov.Set("token", "s3cret")

	w, err := mamori.Watch[Config](context.Background(), mamori.WithProvider(prov))
	if err != nil {
		fmt.Println("watch failed:", err)
		return
	}
	defer func() { _ = w.Close() }()

	report := w.Status()
	for _, f := range report.Fields {
		fmt.Printf("%s ref=%s sensitive=%t error=%q\n", f.Path, f.Ref, f.Sensitive, f.LastError)
	}
	fmt.Println("healthy:", report.Healthy)
	fmt.Println("ready:  ", w.Health() == nil)

	// Output:
	// Token ref=mem://token sensitive=true error=""
	// healthy: true
	// ready:   true
}

// Doctor resolves every field once, without starting a watcher, and reports
// every failure rather than stopping at the first. Run it as a pre-deploy check
// to catch a typo'd ref or a rotated-away secret before it ships.
func ExampleDoctor() {
	type Config struct {
		Present string `source:"mem://present"`
		Missing string `source:"mem://missing"`
	}

	prov := mamoritest.NewProvider("mem")
	prov.Set("present", "value")
	// "missing" is deliberately never Set.

	// err is non-nil only when Config itself cannot be walked as a config
	// struct. A field that fails to resolve is reported, not returned, so one
	// call surfaces every problem instead of just the first.
	report, err := mamori.Doctor[Config](context.Background(), mamori.WithProvider(prov))
	if err != nil {
		fmt.Println("not a usable config struct:", err)
		return
	}

	for _, f := range report.Fields {
		if f.LastKind == "" {
			fmt.Printf("%s: ok\n", f.Path)
			continue
		}
		fmt.Printf("%s: %s\n", f.Path, f.LastKind)
	}
	fmt.Println("all refs reachable:", report.Healthy)

	// Output:
	// Present: ok
	// Missing: not_found
	// all refs reachable: false
}

// ParseRefs turns a `source` tag into the chain of refs it denotes. A comma is
// a separator only when what follows it looks like a scheme, so a comma inside
// an opaque path or a query option is preserved.
func ExampleParseRefs() {
	for _, tag := range []string{
		"env:PORT,aws-ps://svc/port",  // compact chain
		"env:PORT, aws-ps://svc/port", // same chain, written with a space
		"exec:echo a, b",              // one opaque command, not a chain
		"vault://kv?tags=a,b",         // comma inside a query option
	} {
		refs, err := mamori.ParseRefs(tag)
		if err != nil {
			fmt.Println(err)
			continue
		}
		fmt.Printf("%d ref(s) from %q:", len(refs), tag)
		for _, r := range refs {
			fmt.Printf(" [%s %s]", r.Scheme, r.Path)
		}
		fmt.Println()
	}

	// Output:
	// 2 ref(s) from "env:PORT,aws-ps://svc/port": [env PORT] [aws-ps svc/port]
	// 2 ref(s) from "env:PORT, aws-ps://svc/port": [env PORT] [aws-ps svc/port]
	// 1 ref(s) from "exec:echo a, b": [exec echo a, b]
	// 1 ref(s) from "vault://kv?tags=a,b": [vault kv]
}
