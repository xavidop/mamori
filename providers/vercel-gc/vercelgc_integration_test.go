//go:build integration

package vercelgc_test

import (
	"context"
	"os"
	"testing"

	"github.com/xavidop/mamori"
	vercelgc "github.com/xavidop/mamori/providers/vercel-gc"
)

// TestIntegrationResolve exercises a real Vercel Global Config store. It is
// guarded by a build tag and skips unless GLOBAL_CONFIG (or EDGE_CONFIG) and
// VERCEL_GC_TEST_KEY name an existing store and key.
//
// It is also the check on the one part of the read API whose response shape
// Vercel documents only as "JSON": the digest endpoint. parseDigest accepts
// both a bare string and an object with a digest field, and this test is what
// proves which one production actually returns.
//
//	export GLOBAL_CONFIG='https://global-config.vercel.com/ecfg_xxx?token=yyy'
//	export VERCEL_GC_TEST_KEY=my-existing-key
//	GOWORK=off go test -tags integration -run Integration ./...
func TestIntegrationResolve(t *testing.T) {
	if os.Getenv("GLOBAL_CONFIG") == "" && os.Getenv("EDGE_CONFIG") == "" {
		t.Skip("set GLOBAL_CONFIG or EDGE_CONFIG to run the integration test")
	}
	key := os.Getenv("VERCEL_GC_TEST_KEY")
	if key == "" {
		t.Skip("set VERCEL_GC_TEST_KEY to an existing Global Config key")
	}

	p := vercelgc.New()
	ref, err := mamori.ParseRef("vercel-gc://" + key)
	if err != nil {
		t.Fatalf("parsing ref: %v", err)
	}

	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("resolving %q: %v", key, err)
	}
	if len(v.Bytes) == 0 {
		t.Error("resolved an empty value")
	}
	if v.Metadata["digest"] == "" {
		t.Error("no digest in metadata: the digest endpoint returned a shape parseDigest did not recognize")
	}
	t.Logf("resolved %q (%d bytes), store digest %s", key, len(v.Bytes), v.Metadata["digest"])
}
