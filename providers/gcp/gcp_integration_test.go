//go:build integration

// Package gcp live integration test. Not run by the default `go test ./...`.
//
// Run against a real Google Cloud Secret Manager with Application Default
// Credentials configured (e.g. `gcloud auth application-default login` or a
// service-account key in GOOGLE_APPLICATION_CREDENTIALS), then:
//
//	GCP_TEST_PROJECT=my-project \
//	GCP_TEST_SECRET=my-secret \
//	GCP_TEST_EXPECT=expected-latest-payload \
//	go test -tags integration -run TestLive ./...
//
// The secret named by GCP_TEST_SECRET must already exist in the project with at
// least one enabled version whose payload equals GCP_TEST_EXPECT.
package gcp

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

func TestLiveResolve(t *testing.T) {
	project := os.Getenv("GCP_TEST_PROJECT")
	secret := os.Getenv("GCP_TEST_SECRET")
	if project == "" || secret == "" {
		t.Skip("set GCP_TEST_PROJECT and GCP_TEST_SECRET to run the live test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New()
	t.Cleanup(func() { _ = p.Close() })

	ref, err := mamori.ParseRef("gcp-sm://" + project + "/" + secret)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Error("Sensitive = false, want true")
	}
	if v.Version == "" {
		t.Error("Version is empty")
	}
	if want := os.Getenv("GCP_TEST_EXPECT"); want != "" && string(v.Bytes) != want {
		t.Fatalf("payload = %q, want %q", v.Bytes, want)
	}
	t.Logf("resolved version %s (%d bytes)", v.Version, len(v.Bytes))
}

func TestLiveNotFound(t *testing.T) {
	project := os.Getenv("GCP_TEST_PROJECT")
	if project == "" {
		t.Skip("set GCP_TEST_PROJECT to run the live test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New()
	t.Cleanup(func() { _ = p.Close() })

	ref, _ := mamori.ParseRef("gcp-sm://" + project + "/mamori-does-not-exist-xyz")
	_, err := p.Resolve(ctx, ref)
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
