//go:build integration

// Package flipt live integration test. Not run by the standard `go test ./...`
// pass; it requires a reachable Flipt server with a pre-created flag.
//
// Run it against a real Flipt instance, e.g.:
//
//	export FLIPT_URL=http://localhost:8080
//	export FLIPT_TOKEN=            # optional, if the server requires auth
//	export FLIPT_TEST_NAMESPACE=default
//	export FLIPT_TEST_FLAG=my-flag # must exist in that namespace
//	go test -tags=integration -run TestLive ./...
package flipt

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/xavidop/mamori"
)

func TestLive(t *testing.T) {
	url := os.Getenv("FLIPT_URL")
	namespace := os.Getenv("FLIPT_TEST_NAMESPACE")
	flag := os.Getenv("FLIPT_TEST_FLAG")
	if url == "" || namespace == "" || flag == "" {
		t.Skip("set FLIPT_URL, FLIPT_TEST_NAMESPACE, FLIPT_TEST_FLAG to run the live test")
	}

	// URL and token are read lazily from FLIPT_URL / FLIPT_TOKEN.
	p := New()
	ref, err := mamori.ParseRef("flipt://" + namespace + "/" + flag)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("live Resolve: %v", err)
	}
	if v.Version == "" {
		t.Error("live flag has empty Version")
	}
	if v.Sensitive {
		t.Error("live flag marked Sensitive; feature flags are not secrets")
	}
	t.Logf("resolved %s/%s (entity=%s): value=%q version=%s type=%s",
		namespace, flag, defaultEntity, v.Bytes, v.Version, v.Metadata["type"])

	// A non-existent flag must be ErrNotFound.
	miss, _ := mamori.ParseRef("flipt://" + namespace + "/___definitely_missing___")
	if _, err := p.Resolve(context.Background(), miss); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("missing flag error = %v, want ErrNotFound", err)
	}
}
