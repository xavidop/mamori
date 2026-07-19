//go:build integration

// Package dynamodb live integration tests. These hit a real DynamoDB endpoint
// and are excluded from the default build. Run them explicitly against a table
// that has the referenced item provisioned:
//
//	export AWS_REGION=us-east-1
//	export MAMORI_DDB_TABLE=mamori-integration          # table name
//	export MAMORI_DDB_PK=mamori/integration/test        # partition key value
//	# optional, if the table has a partition key attribute other than "pk":
//	export MAMORI_DDB_PK_NAME=id
//	# optional, if the table uses a composite primary key:
//	export MAMORI_DDB_SK=2024
//	export MAMORI_DDB_SK_NAME=year
//	GOWORK=off go test -tags integration -run Integration ./...
//
// Credentials come from the default AWS credential chain. The test is skipped
// unless MAMORI_DDB_TABLE and MAMORI_DDB_PK are set.
package dynamodb

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

func liveRegionOpts() []Option {
	if r := os.Getenv("AWS_REGION"); r != "" {
		return []Option{WithRegion(r)}
	}
	return nil
}

// liveRef assembles a dynamodb:// ref from the environment.
func liveRef(pk string) string {
	ref := "dynamodb://" + os.Getenv("MAMORI_DDB_TABLE") + "/" + pk
	q := url.Values{}
	if v := os.Getenv("MAMORI_DDB_PK_NAME"); v != "" {
		q.Set("pk_name", v)
	}
	if v := os.Getenv("MAMORI_DDB_SK"); v != "" {
		q.Set("sk", v)
	}
	if v := os.Getenv("MAMORI_DDB_SK_NAME"); v != "" {
		q.Set("sk_name", v)
	}
	if len(q) > 0 {
		ref += "?" + q.Encode()
	}
	return ref
}

func TestIntegrationResolve(t *testing.T) {
	if os.Getenv("MAMORI_DDB_TABLE") == "" || os.Getenv("MAMORI_DDB_PK") == "" {
		t.Skip("set MAMORI_DDB_TABLE and MAMORI_DDB_PK to run the DynamoDB integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(liveRegionOpts()...)
	ref := mustParseLive(t, liveRef(os.Getenv("MAMORI_DDB_PK")))
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve %q: %v", ref.String(), err)
	}
	if len(v.Bytes) == 0 {
		t.Error("resolved item has empty payload")
	}
	if v.Version == "" {
		t.Error("resolved value must carry a Version")
	}
}

func TestIntegrationNotFound(t *testing.T) {
	if os.Getenv("MAMORI_DDB_TABLE") == "" {
		t.Skip("set MAMORI_DDB_TABLE to run the DynamoDB integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(liveRegionOpts()...)
	ref := mustParseLive(t, liveRef("mamori-does-not-exist-"+time.Now().Format("150405")))
	_, err := p.Resolve(ctx, ref)
	if err == nil || !isNotFound(err) {
		t.Fatalf("expected ErrNotFound for a missing item, got %v", err)
	}
}

func mustParseLive(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

func isNotFound(err error) bool { return errors.Is(err, mamori.ErrNotFound) }
