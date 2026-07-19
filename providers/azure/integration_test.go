//go:build integration

// Live test against a real Azure Key Vault. Not run by default.
//
// Run with:
//
//	MAMORI_AZURE_VAULT=<vault-name> \
//	MAMORI_AZURE_SECRET=<secret-name> \
//	go test -tags integration -run TestLive ./...
//
// Authentication uses the Azure default credential chain
// (azidentity.NewDefaultAzureCredential): environment variables, workload
// identity, managed identity, or the Azure CLI login, in that order.
package azure

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

func TestLive(t *testing.T) {
	vault := os.Getenv("MAMORI_AZURE_VAULT")
	secret := os.Getenv("MAMORI_AZURE_SECRET")
	if vault == "" || secret == "" {
		t.Skip("set MAMORI_AZURE_VAULT and MAMORI_AZURE_SECRET to run the live test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New() // default credential chain, real azsecrets client
	ref, err := mamori.ParseRef("azure-kv://" + vault + "/" + secret)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(v.Bytes) == 0 {
		t.Error("resolved secret is empty")
	}
	if !v.Sensitive {
		t.Error("Value.Sensitive = false, want true")
	}
	if v.Version == "" {
		t.Error("Value.Version is empty")
	}
	t.Logf("resolved %q version=%s (%d bytes)", secret, v.Version, len(v.Bytes))
}
