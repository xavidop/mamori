//go:build integration

// Package split live integration test. Not run by the standard `go test ./...`
// pass; it requires a real Split SDK key and a pre-created feature flag.
//
// Run it against a real Split workspace, e.g.:
//
//	export SPLIT_API_KEY=your-server-side-sdk-key
//	export SPLIT_TEST_FLAG=my-feature-flag          # must exist in the environment
//	export SPLIT_TEST_KEY=user-123                  # optional traffic key (default "mamori")
//	go test -tags=integration -run TestLive ./...
package split

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/xavidop/mamori"
)

func TestLive(t *testing.T) {
	apiKey := os.Getenv("SPLIT_API_KEY")
	flag := os.Getenv("SPLIT_TEST_FLAG")
	if apiKey == "" || flag == "" {
		t.Skip("set SPLIT_API_KEY and SPLIT_TEST_FLAG to run the live test")
	}

	p := New() // SDK key read from SPLIT_API_KEY lazily
	t.Cleanup(func() { _ = p.Close() })

	raw := "split://" + flag
	if key := os.Getenv("SPLIT_TEST_KEY"); key != "" {
		raw += "?key=" + key
	}
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("live Resolve: %v", err)
	}
	if len(v.Bytes) == 0 {
		t.Error("live flag resolved to empty treatment")
	}
	if v.Version == "" {
		t.Error("live flag has empty Version")
	}
	if v.Sensitive {
		t.Error("feature flag treatment must not be marked Sensitive")
	}
	t.Logf("resolved %s: treatment=%q version=%s", raw, v.Bytes, v.Version)

	// A non-existent flag must be ErrNotFound (Split returns "control").
	miss, _ := mamori.ParseRef("split://___definitely_missing_flag___")
	if _, err := p.Resolve(context.Background(), miss); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("missing flag error = %v, want ErrNotFound", err)
	}
}
