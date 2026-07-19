//go:build integration

// Package onepassword live integration test. Run against a real 1Password
// Connect server with:
//
//	OP_CONNECT_HOST=https://connect.example:8080 \
//	OP_CONNECT_TOKEN=eyJ... \
//	OP_TEST_REF='op://Production/postgres/password' \
//	go test -tags integration -run TestLiveResolve ./...
//
// It is excluded from the default build and is never run in CI without a live
// backend.
package onepassword

import (
	"context"
	"os"
	"testing"

	"github.com/xavidop/mamori"
)

func TestLiveResolve(t *testing.T) {
	if os.Getenv("OP_CONNECT_HOST") == "" || os.Getenv("OP_CONNECT_TOKEN") == "" {
		t.Skip("OP_CONNECT_HOST/OP_CONNECT_TOKEN not set; skipping live test")
	}
	raw := os.Getenv("OP_TEST_REF")
	if raw == "" {
		t.Skip("OP_TEST_REF not set; skipping live test")
	}

	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}

	p := New() // host + token read from the environment
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", raw, err)
	}
	if len(v.Bytes) == 0 {
		t.Error("resolved value is empty")
	}
	if !v.Sensitive {
		t.Error("expected Sensitive = true")
	}
	if v.Version == "" {
		t.Error("expected a non-empty Version")
	}
	// Never log v.Bytes - it is a secret.
	t.Logf("resolved %q: %d bytes, version %s", raw, len(v.Bytes), v.Version)
}
