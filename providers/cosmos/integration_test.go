//go:build integration

// Live test against a real Azure Cosmos DB account (SQL / Core API). Not run by
// default.
//
// Run with an endpoint + the Azure default credential chain:
//
//	MAMORI_COSMOS_ENDPOINT=https://<account>.documents.azure.com:443/ \
//	MAMORI_COSMOS_DATABASE=<database> \
//	MAMORI_COSMOS_CONTAINER=<container> \
//	MAMORI_COSMOS_ID=<item-id> \
//	MAMORI_COSMOS_PK=<partition-key-value> \
//	go test -tags integration -run TestLive ./...
//
// Or with a connection string instead of an endpoint:
//
//	MAMORI_COSMOS_CONNECTION_STRING="AccountEndpoint=...;AccountKey=..." \
//	MAMORI_COSMOS_DATABASE=<database> \
//	MAMORI_COSMOS_CONTAINER=<container> \
//	MAMORI_COSMOS_ID=<item-id> \
//	go test -tags integration -run TestLive ./...
//
// When using an endpoint, authentication uses the Azure default credential chain
// (azidentity.NewDefaultAzureCredential): environment variables, workload
// identity, managed identity, or the Azure CLI login, in that order. The
// identity needs data-plane read access to the container (e.g. the Cosmos DB
// Built-in Data Reader role) on the target account.
package cosmos

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

func TestLive(t *testing.T) {
	endpoint := os.Getenv("MAMORI_COSMOS_ENDPOINT")
	connStr := os.Getenv("MAMORI_COSMOS_CONNECTION_STRING")
	database := os.Getenv("MAMORI_COSMOS_DATABASE")
	container := os.Getenv("MAMORI_COSMOS_CONTAINER")
	id := os.Getenv("MAMORI_COSMOS_ID")
	pk := os.Getenv("MAMORI_COSMOS_PK")
	if (endpoint == "" && connStr == "") || database == "" || container == "" || id == "" {
		t.Skip("set MAMORI_COSMOS_ENDPOINT or MAMORI_COSMOS_CONNECTION_STRING, plus MAMORI_COSMOS_DATABASE, MAMORI_COSMOS_CONTAINER and MAMORI_COSMOS_ID to run the live test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var p *Provider
	if connStr != "" {
		p = New(WithConnectionString(connStr))
	} else {
		p = New(WithEndpoint(endpoint)) // default credential chain, real azcosmos client
	}

	raw := "cosmos://" + database + "/" + container + "/" + id
	if pk != "" {
		raw += "?pk=" + pk
	}
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(v.Bytes) == 0 {
		t.Error("resolved item is empty")
	}
	if v.Version == "" {
		t.Error("Value.Version is empty")
	}
	t.Logf("resolved %s/%s/%s version=%s (%d bytes)", database, container, id, v.Version, len(v.Bytes))
}
