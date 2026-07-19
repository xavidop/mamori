//go:build integration

// Package goff live integration test. It exercises a REAL go-feature-flag client
// (the embedded ffclient) loaded from an on-disk flag-configuration file, so it
// validates that the provider maps real evaluated variations - and the library's
// FLAG_NOT_FOUND signalling and poll-interval hot-reload - exactly as the unit
// tests assert against the in-memory fake.
//
// It needs no external service (go-feature-flag is file-driven), only the build
// tag:
//
//	GOWORK=off go test -tags integration -run Integration ./...
package goff

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	ffclient "github.com/thomaspoignant/go-feature-flag"
	"github.com/thomaspoignant/go-feature-flag/retriever/fileretriever"
	"github.com/xavidop/mamori"
)

// flagsV1 is a go-feature-flag configuration exercising every variation type the
// provider renders: bool, string, number, and JSON object.
const flagsV1 = `
new-checkout:
  variations:
    "on": true
    "off": false
  defaultRule:
    variation: "on"
greeting:
  variations:
    hello: "hola"
  defaultRule:
    variation: hello
api-ratelimit:
  variations:
    high: 100
  defaultRule:
    variation: high
ui-config:
  variations:
    dark:
      theme: dark
      maxItems: 20
  defaultRule:
    variation: dark
`

// flagsV2 changes greeting's value; a poll cycle later the provider must observe
// it, proving file-driven hot-reload works end to end.
const flagsV2 = `
greeting:
  variations:
    hello: "bonjour"
  defaultRule:
    variation: hello
`

func writeFlags(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write flags: %v", err)
	}
}

func TestIntegrationResolveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flags.yaml")
	writeFlags(t, path, flagsV1)

	// Build the real client ourselves so we can Close it deterministically, then
	// inject it into the provider (bypassing the lazy builder for lifecycle
	// control only - resolution goes through the real SDK).
	client, err := ffclient.New(ffclient.Config{
		PollingInterval: 500 * time.Millisecond,
		Retriever:       &fileretriever.Retriever{Path: path},
	})
	if err != nil {
		t.Fatalf("ffclient.New: %v", err)
	}
	defer client.Close()

	p := New(withEvaluator(client))
	ctx := context.Background()

	got := func(raw string) string {
		t.Helper()
		v, err := p.Resolve(ctx, mustRef(t, raw))
		if err != nil {
			t.Fatalf("Resolve %s: %v", raw, err)
		}
		return string(v.Bytes)
	}

	if got("goff://new-checkout") != "true" {
		t.Errorf("new-checkout = %q, want true", got("goff://new-checkout"))
	}
	if got("goff://greeting") != "hola" {
		t.Errorf("greeting = %q, want hola", got("goff://greeting"))
	}
	if got("goff://api-ratelimit") != "100" {
		t.Errorf("api-ratelimit = %q, want 100", got("goff://api-ratelimit"))
	}
	if got("goff://ui-config#theme") != "dark" {
		t.Errorf("ui-config#theme = %q, want dark", got("goff://ui-config#theme"))
	}
	if got("goff://ui-config#maxItems") != "20" {
		t.Errorf("ui-config#maxItems = %q, want 20", got("goff://ui-config#maxItems"))
	}

	// A flag absent from the configuration must be a typed not-found.
	if _, err := p.Resolve(ctx, mustRef(t, "goff://does-not-exist")); !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing flag err = %v, want ErrNotFound", err)
	}

	// Hot-reload: rewrite the file and wait for a poll cycle to pick it up.
	writeFlags(t, path, flagsV2)
	deadline := time.After(10 * time.Second)
	for {
		if got("goff://greeting") == "bonjour" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("greeting did not hot-reload to bonjour within the timeout")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func TestIntegrationLazyClientFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flags.yaml")
	writeFlags(t, path, flagsV1)

	// Exercise the provider's own lazy client builder via WithConfigFile.
	p := New(WithConfigFile(path), WithPollingInterval(time.Second))
	v, err := p.Resolve(context.Background(), mustRef(t, "goff://greeting"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hola" {
		t.Fatalf("greeting = %q, want hola", v.Bytes)
	}
	if v.Version != mamori.VersionHash(v.Bytes) {
		t.Errorf("Version = %q, want VersionHash of bytes", v.Version)
	}
	if v.Sensitive {
		t.Error("goff values must not be marked Sensitive")
	}
}
