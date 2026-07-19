//go:build integration

// Package gcs live integration test. Not run by the default `go test ./...`.
//
// Run against a real Google Cloud Storage bucket with Application Default
// Credentials configured (e.g. `gcloud auth application-default login` or a
// service-account key in GOOGLE_APPLICATION_CREDENTIALS), then:
//
//	GCS_TEST_BUCKET=my-bucket \
//	GCS_TEST_OBJECT=path/to/object.json \
//	GCS_TEST_EXPECT=expected-object-contents \
//	go test -tags integration -run TestLive ./...
//
// The object named by GCS_TEST_OBJECT must already exist in the bucket; if
// GCS_TEST_EXPECT is set, its contents must equal that value.
package gcs

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

func TestLiveResolve(t *testing.T) {
	bucket := os.Getenv("GCS_TEST_BUCKET")
	object := os.Getenv("GCS_TEST_OBJECT")
	if bucket == "" || object == "" {
		t.Skip("set GCS_TEST_BUCKET and GCS_TEST_OBJECT to run the live test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New()
	t.Cleanup(func() { _ = p.Close() })

	ref, err := mamori.ParseRef("gcs://" + bucket + "/" + object)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Version == "" {
		t.Error("Version is empty")
	}
	if want := os.Getenv("GCS_TEST_EXPECT"); want != "" && string(v.Bytes) != want {
		t.Fatalf("payload = %q, want %q", v.Bytes, want)
	}
	t.Logf("resolved generation %s (%d bytes)", v.Version, len(v.Bytes))
}

func TestLiveNotFound(t *testing.T) {
	bucket := os.Getenv("GCS_TEST_BUCKET")
	if bucket == "" {
		t.Skip("set GCS_TEST_BUCKET to run the live test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New()
	t.Cleanup(func() { _ = p.Close() })

	ref, _ := mamori.ParseRef("gcs://" + bucket + "/mamori-does-not-exist-xyz.json")
	_, err := p.Resolve(ctx, ref)
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
