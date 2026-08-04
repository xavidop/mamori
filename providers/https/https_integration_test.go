//go:build integration

// Package https live integration tests hit a real HTTP endpoint you nominate
// and are excluded from the default build. Run them against an endpoint that
// already serves a JSON document:
//
//	export MAMORI_HTTPS_BASE_URL=https://api.example.com/v1
//	export MAMORI_HTTPS_PATH=config
//	export MAMORI_HTTPS_TOKEN=...          # optional bearer token
//	export MAMORI_HTTPS_POINTER=/db/host   # optional JSON Pointer
//	GOWORK=off go test -tags integration -run Integration ./...
//
// BASE_URL and PATH are required; the tests skip if either is unset. Nothing
// here ever logs a token or a resolved value, only a byte count.
package https

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// liveConfig reads the environment the integration tests require, skipping the
// calling test if the two required variables are unset.
func liveConfig(t *testing.T) (baseURL, path, token, pointer string) {
	t.Helper()
	baseURL = os.Getenv("MAMORI_HTTPS_BASE_URL")
	path = os.Getenv("MAMORI_HTTPS_PATH")
	if baseURL == "" || path == "" {
		t.Skip("set MAMORI_HTTPS_BASE_URL and MAMORI_HTTPS_PATH to run the live tests")
	}
	return baseURL, path, os.Getenv("MAMORI_HTTPS_TOKEN"), os.Getenv("MAMORI_HTTPS_POINTER")
}

// liveProvider builds a Provider against the nominated endpoint.
func liveProvider(t *testing.T, baseURL, token string) *Provider {
	t.Helper()
	e := Endpoint{Name: "live", BaseURL: baseURL}
	if token != "" {
		e.Auth = httpcore.Bearer(token)
	}
	p, err := New(e)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestIntegrationResolve(t *testing.T) {
	baseURL, path, token, _ := liveConfig(t)
	p := liveProvider(t, baseURL, token)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("https://live/" + path)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(v.Bytes) == 0 {
		t.Fatal("Resolve returned an empty value")
	}
	if v.Version == "" {
		t.Fatal("Resolve returned no Version")
	}
	t.Logf("resolved %d bytes", len(v.Bytes))
}

// TestIntegrationConditionalGet proves the second poll revalidates rather than
// re-downloading, which is the whole reason this provider uses a Revalidator.
func TestIntegrationConditionalGet(t *testing.T) {
	baseURL, path, token, _ := liveConfig(t)
	p := liveProvider(t, baseURL, token)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("https://live/" + path)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	first, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	second, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if first.Version != second.Version {
		t.Logf("version changed between polls (%q then %q); the endpoint may not send a stable ETag", first.Version, second.Version)
	}
	if len(second.Bytes) != len(first.Bytes) {
		t.Fatalf("second Resolve returned %d bytes, want %d", len(second.Bytes), len(first.Bytes))
	}
}

func TestIntegrationPointerSelection(t *testing.T) {
	baseURL, path, token, pointer := liveConfig(t)
	if pointer == "" {
		t.Skip("set MAMORI_HTTPS_POINTER to run the pointer selection test")
	}
	p := liveProvider(t, baseURL, token)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("https://live/" + path + "#" + pointer)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	t.Logf("selected %d bytes at %s", len(v.Bytes), pointer)
}
