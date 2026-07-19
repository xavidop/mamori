//go:build integration

// Package vault integration tests run against a live Vault. They are excluded
// from the default build and require a dev-mode Vault:
//
//	vault server -dev -dev-root-token-id=root
//	export VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root
//	go test -tags=integration ./...
//
// The default KV v2 mount in dev mode is "secret".
package vault

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

func liveClient(t *testing.T) *vaultapi.Client {
	t.Helper()
	if os.Getenv("VAULT_ADDR") == "" || os.Getenv("VAULT_TOKEN") == "" {
		t.Skip("set VAULT_ADDR and VAULT_TOKEN to run integration tests")
	}
	cfg := vaultapi.DefaultConfig()
	if cfg.Error != nil {
		t.Fatalf("vault config: %v", cfg.Error)
	}
	c, err := vaultapi.NewClient(cfg)
	if err != nil {
		t.Fatalf("vault client: %v", err)
	}
	c.SetToken(os.Getenv("VAULT_TOKEN"))
	return c
}

func TestLiveResolve(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()
	const mount, path = "secret", "mamori-int/config"

	if _, err := c.KVv2(mount).Put(ctx, path, map[string]interface{}{
		"username": "admin",
		"password": "s3cr3t",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _ = c.KVv2(mount).Delete(context.Background(), path) })

	p := New(WithClient(c))

	// #key selection.
	v, err := p.Resolve(ctx, mustRef(t, "vault://"+mount+"/"+path+"#password"))
	if err != nil {
		t.Fatalf("Resolve #password: %v", err)
	}
	if string(v.Bytes) != "s3cr3t" {
		t.Errorf("Bytes = %q, want s3cr3t", v.Bytes)
	}
	if !v.Sensitive || v.Version == "" {
		t.Errorf("expected Sensitive with a Version, got Sensitive=%v Version=%q", v.Sensitive, v.Version)
	}

	// Whole data map as JSON.
	v, err = p.Resolve(ctx, mustRef(t, "vault://"+mount+"/"+path))
	if err != nil {
		t.Fatalf("Resolve whole: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(v.Bytes, &got); err != nil {
		t.Fatalf("payload not JSON object: %v", err)
	}
	if got["username"] != "admin" {
		t.Errorf("username = %q, want admin", got["username"])
	}

	// Missing secret.
	if _, err := p.Resolve(ctx, mustRef(t, "vault://"+mount+"/mamori-int/absent")); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("absent secret err = %v, want ErrNotFound", err)
	}
}

func TestLiveConformance(t *testing.T) {
	c := liveClient(t)
	const mount = "secret"
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return New(WithClient(c)) },
		Ref: func(key string) string { return "vault://" + mount + "/" + key + "#value" },
		Seed: func(ctx context.Context, key, val string) error {
			_, err := c.KVv2(mount).Put(ctx, key, map[string]interface{}{"value": val})
			return err
		},
		Mutate: func(ctx context.Context, key, val string) error {
			_, err := c.KVv2(mount).Put(ctx, key, map[string]interface{}{"value": val})
			return err
		},
	})
}
