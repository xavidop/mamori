// Command basic demonstrates mamori: loading typed config from the environment
// and a file, then watching for changes and reacting without a restart.
//
// Run it:
//
//	LOG_LEVEL=debug WORKERS=8 go run ./examples/basic
//
// While it runs it rotates the token file itself so you can see mamori
// reconcile the change live. You can also edit the printed file yourself.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/secret"
)

// tokenPath is a fixed location so the `source` tag can reference it as a
// compile-time constant. mamori never interpolates values into refs (no
// injection chains), so refs must be static strings. It matches the file://
// path in Config.APIToken exactly.
const tokenPath = "/tmp/mamori-example-token"

// Config is loaded from multiple sources by tag.
type Config struct {
	// From the environment, with a default and validation applied on every update.
	LogLevel string `source:"env:LOG_LEVEL" default:"info" validate:"oneof=debug info warn error"`
	Workers  int    `source:"env:WORKERS" default:"4" validate:"gte=1,lte=256"`

	// A secret loaded from a file and hot-reloaded via fsnotify. secret.String
	// redacts itself in logs, fmt, and JSON - only Reveal() exposes the value.
	APIToken secret.String `source:"file:///tmp/mamori-example-token"`
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := os.WriteFile(tokenPath, []byte("initial-token"), 0o600); err != nil {
		logger.Error("write token", "err", err)
		os.Exit(1)
	}
	defer func() { _ = os.Remove(tokenPath) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := mamori.Watch[Config](ctx,
		mamori.WithPollInterval(2*time.Second),
		mamori.OnChange(func(ev mamori.Change[Config]) {
			for _, f := range ev.Fields {
				logger.Info("config changed", "field", f.Path,
					"from", f.OldVersion, "to", f.NewVersion)
			}
			if ev.Changed("APIToken") {
				// The value stays redacted in logs; Reveal() would expose it.
				logger.Info("rotating clients with the new API token", "token", ev.New.APIToken)
			}
		}),
		mamori.OnError(func(err error) { logger.Warn("reconcile error", "err", err) }),
	)
	if err != nil {
		logger.Error("watch failed", "err", err)
		os.Exit(1)
	}
	defer func() { _ = w.Close() }()

	cfg := w.Get()
	logger.Info("loaded config",
		"logLevel", cfg.LogLevel, "workers", cfg.Workers, "apiToken", cfg.APIToken)
	fmt.Printf("\nEdit this file and save to see live reconciliation:\n  %s\n\n", tokenPath)

	// Simulate an external rotation after a moment.
	go func() {
		time.Sleep(3 * time.Second)
		_ = os.WriteFile(tokenPath, []byte("rotated-token"), 0o600)
	}()

	time.Sleep(8 * time.Second)
	logger.Info("final config", "apiToken", w.Get().APIToken)
}
