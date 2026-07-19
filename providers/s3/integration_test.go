//go:build integration

// Package s3 live integration tests. These hit a real S3 (or S3-compatible)
// endpoint and are excluded from the default build. Run them explicitly against
// a bucket/object you control:
//
//	export AWS_REGION=us-east-1
//	export MAMORI_S3_BUCKET=my-bucket
//	export MAMORI_S3_KEY=config/app.json        # an existing object key
//	# optional, for MinIO / Cloudflare R2 / custom S3-compatible stores:
//	export MAMORI_S3_ENDPOINT=http://localhost:9000
//	GOWORK=off go test -tags integration -run Integration ./...
//
// Credentials come from the default AWS credential chain. Any test whose
// required environment variable is unset is skipped.
package s3

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

func liveOpts() []Option {
	var opts []Option
	if r := os.Getenv("AWS_REGION"); r != "" {
		opts = append(opts, WithRegion(r))
	}
	if e := os.Getenv("MAMORI_S3_ENDPOINT"); e != "" {
		opts = append(opts, WithEndpoint(e))
	}
	return opts
}

func liveRef(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

func TestIntegrationResolve(t *testing.T) {
	bucket := os.Getenv("MAMORI_S3_BUCKET")
	key := os.Getenv("MAMORI_S3_KEY")
	if bucket == "" || key == "" {
		t.Skip("set MAMORI_S3_BUCKET and MAMORI_S3_KEY to run the S3 integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(liveOpts()...)
	v, err := p.Resolve(ctx, liveRef(t, "s3://"+bucket+"/"+key))
	if err != nil {
		t.Fatalf("Resolve %q/%q: %v", bucket, key, err)
	}
	if len(v.Bytes) == 0 {
		t.Error("resolved object has empty payload")
	}
	if v.Version == "" {
		t.Error("S3 value must carry a Version (ETag or VersionId)")
	}
}

func TestIntegrationNotFound(t *testing.T) {
	bucket := os.Getenv("MAMORI_S3_BUCKET")
	if bucket == "" {
		t.Skip("set MAMORI_S3_BUCKET to run the S3 integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(liveOpts()...)
	_, err := p.Resolve(ctx, liveRef(t, "s3://"+bucket+"/mamori/does-not-exist/"+time.Now().Format("150405")))
	if err == nil || !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a missing object, got %v", err)
	}
}
