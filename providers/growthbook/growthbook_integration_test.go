//go:build integration

// Package growthbook live integration test. Not run by the default
// `go test ./...`.
//
// Run against a real GrowthBook project. Provide an SDK client key (and, for a
// self-hosted GrowthBook, its API host), the key of a feature that exists in the
// project, and optionally its expected evaluated value:
//
//	GROWTHBOOK_CLIENT_KEY=sdk-abc123 \
//	GROWTHBOOK_API_HOST=https://cdn.growthbook.io \
//	GROWTHBOOK_FEATURE=my_feature \
//	GROWTHBOOK_EXPECT=true \
//	go test -tags integration -run TestLive ./...
//
// The feature named by GROWTHBOOK_FEATURE must exist in the project's feature
// set and, when GROWTHBOOK_EXPECT is set, evaluate (with no attributes) to that
// value.
package growthbook

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

func liveProvider(t *testing.T) *Provider {
	t.Helper()
	key := os.Getenv("GROWTHBOOK_CLIENT_KEY")
	if key == "" {
		t.Skip("set GROWTHBOOK_CLIENT_KEY to run the live test")
	}
	opts := []Option{WithClientKey(key)}
	if host := os.Getenv("GROWTHBOOK_API_HOST"); host != "" {
		opts = append(opts, WithAPIHost(host))
	}
	if dk := os.Getenv("GROWTHBOOK_DECRYPTION_KEY"); dk != "" {
		opts = append(opts, WithDecryptionKey(dk))
	}
	return New(opts...)
}

func TestLiveResolve(t *testing.T) {
	feature := os.Getenv("GROWTHBOOK_FEATURE")
	if feature == "" {
		t.Skip("set GROWTHBOOK_FEATURE to run the live test")
	}
	p := liveProvider(t)
	t.Cleanup(func() { _ = p.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("growthbook://" + feature)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Sensitive {
		t.Error("Sensitive = true, want false")
	}
	if v.Version == "" {
		t.Error("Version is empty")
	}
	if want := os.Getenv("GROWTHBOOK_EXPECT"); want != "" && string(v.Bytes) != want {
		t.Fatalf("value = %q, want %q", v.Bytes, want)
	}
	t.Logf("resolved feature %q = %q (version %s)", feature, v.Bytes, v.Version)
}

func TestLiveNotFound(t *testing.T) {
	p := liveProvider(t)
	t.Cleanup(func() { _ = p.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, _ := mamori.ParseRef("growthbook://mamori_does_not_exist_xyz")
	_, err := p.Resolve(ctx, ref)
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
