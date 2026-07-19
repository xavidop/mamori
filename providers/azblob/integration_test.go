//go:build integration

// Live test against a real Azure Blob Storage account. Not run by default.
//
// Run with:
//
//	MAMORI_AZBLOB_ACCOUNT=<account-name-or-service-url> \
//	MAMORI_AZBLOB_CONTAINER=<container> \
//	MAMORI_AZBLOB_BLOB=<blob-name> \
//	go test -tags integration -run TestLive ./...
//
// Authentication uses the Azure default credential chain
// (azidentity.NewDefaultAzureCredential): environment variables, workload
// identity, managed identity, or the Azure CLI login, in that order. The
// identity needs data-plane read access to the blob (e.g. the "Storage Blob
// Data Reader" role) on the target account.
package azblob

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

func TestLive(t *testing.T) {
	account := os.Getenv("MAMORI_AZBLOB_ACCOUNT")
	container := os.Getenv("MAMORI_AZBLOB_CONTAINER")
	blob := os.Getenv("MAMORI_AZBLOB_BLOB")
	if account == "" || container == "" || blob == "" {
		t.Skip("set MAMORI_AZBLOB_ACCOUNT, MAMORI_AZBLOB_CONTAINER and MAMORI_AZBLOB_BLOB to run the live test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(WithAccountURL(account)) // default credential chain, real azblob client
	ref, err := mamori.ParseRef("azblob://" + container + "/" + blob)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(v.Bytes) == 0 {
		t.Error("resolved blob is empty")
	}
	if v.Version == "" {
		t.Error("Value.Version is empty")
	}
	t.Logf("resolved %s/%s version=%s (%d bytes)", container, blob, v.Version, len(v.Bytes))
}
