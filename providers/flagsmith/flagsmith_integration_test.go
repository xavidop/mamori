//go:build integration

// Package flagsmith live integration test. Not run by the standard
// `go test ./...` pass; it requires a real Flagsmith environment key and a
// pre-created feature.
//
// Run it against a real Flagsmith environment, e.g.:
//
//	export FLAGSMITH_ENVIRONMENT_KEY=ser.xxxxxxxx     # or a client-side key
//	export FLAGSMITH_TEST_FEATURE=my_feature          # must exist in that env
//	# optional, for self-hosted:
//	export FLAGSMITH_BASE_URL=https://flagsmith.example.com/api/v1/
//	go test -tags=integration -run TestLive ./...
package flagsmith

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/xavidop/mamori"
)

func TestLive(t *testing.T) {
	key := os.Getenv("FLAGSMITH_ENVIRONMENT_KEY")
	feature := os.Getenv("FLAGSMITH_TEST_FEATURE")
	if key == "" || feature == "" {
		t.Skip("set FLAGSMITH_ENVIRONMENT_KEY and FLAGSMITH_TEST_FEATURE to run the live test")
	}

	var opts []Option
	if base := os.Getenv("FLAGSMITH_BASE_URL"); base != "" {
		opts = append(opts, WithBaseURL(base))
	}
	p := New(opts...) // environment key read from FLAGSMITH_ENVIRONMENT_KEY lazily

	// The feature's value.
	ref, err := mamori.ParseRef("flagsmith://" + feature)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("live Resolve: %v", err)
	}
	if v.Version == "" {
		t.Error("live feature has empty Version")
	}
	if v.Sensitive {
		t.Error("feature flag should not be marked Sensitive")
	}
	t.Logf("resolved %s: %d value bytes, version=%s", feature, len(v.Bytes), v.Version)

	// The feature's enabled state.
	enabledRef, _ := mamori.ParseRef("flagsmith://" + feature + "#enabled")
	ev, err := p.Resolve(context.Background(), enabledRef)
	if err != nil {
		t.Fatalf("live Resolve(#enabled): %v", err)
	}
	if s := string(ev.Bytes); s != "true" && s != "false" {
		t.Errorf("#enabled = %q, want true or false", s)
	}
	t.Logf("resolved %s#enabled: %s", feature, ev.Bytes)

	// A non-existent feature must be ErrNotFound.
	miss, _ := mamori.ParseRef("flagsmith://___definitely_missing_feature___")
	if _, err := p.Resolve(context.Background(), miss); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("missing feature error = %v, want ErrNotFound", err)
	}
}
