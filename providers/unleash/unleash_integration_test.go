//go:build integration

// Package unleash live integration test. Not run by the standard `go test ./...`
// pass; it requires a reachable Unleash server, an API token, and a
// pre-created feature toggle.
//
// Run it against a real Unleash instance, e.g.:
//
//	export UNLEASH_URL=https://unleash.example.com/api
//	export UNLEASH_API_TOKEN=*:development.xxxxxxxx   # client token
//	export UNLEASH_APP_NAME=mamori-itest
//	export UNLEASH_TEST_TOGGLE=my-existing-toggle     # must exist on the server
//	go test -tags=integration -run TestLive ./...
package unleash

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/xavidop/mamori"
)

func TestLive(t *testing.T) {
	url := os.Getenv("UNLEASH_URL")
	token := os.Getenv("UNLEASH_API_TOKEN")
	name := os.Getenv("UNLEASH_TEST_TOGGLE")
	if url == "" || token == "" || name == "" {
		t.Skip("set UNLEASH_URL, UNLEASH_API_TOKEN, UNLEASH_TEST_TOGGLE to run the live test")
	}

	// URL/token/app name are read lazily from the environment; the client is
	// created and synchronized (WaitForReady) on the first Resolve below.
	p := New()
	t.Cleanup(func() { _ = p.Close() })

	// Enabled state of an existing toggle: "true" or "false", never an error.
	ref, err := mamori.ParseRef("unleash://" + name)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("live Resolve of %q: %v", name, err)
	}
	if s := string(v.Bytes); s != "true" && s != "false" {
		t.Errorf("enabled state = %q, want true or false", s)
	}
	if v.Version == "" {
		t.Error("live value has empty Version")
	}
	if v.Sensitive {
		t.Error("live value marked Sensitive; feature toggles are not secrets")
	}
	t.Logf("resolved %s: enabled=%s version=%s", name, v.Bytes, v.Version)

	// A non-existent toggle must be ErrNotFound.
	miss, _ := mamori.ParseRef("unleash://___definitely_missing_toggle___")
	if _, err := p.Resolve(context.Background(), miss); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("missing toggle error = %v, want ErrNotFound", err)
	}
}
