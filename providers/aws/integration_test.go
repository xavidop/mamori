//go:build integration

// Package aws live integration tests. These hit real AWS APIs and are excluded
// from the default build. Run them explicitly against an account with the
// referenced secret/parameter provisioned:
//
//	export AWS_REGION=us-east-1
//	export MAMORI_AWS_SM_SECRET_ID=mamori/integration/test   # SecretString
//	export MAMORI_AWS_PS_PARAM_NAME=/mamori/integration/test  # String or SecureString
//	GOWORK=off go test -tags integration -run Integration ./...
//
// Credentials come from the default AWS credential chain. Any test whose
// environment variable is unset is skipped.
package aws

import (
	"context"
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

func TestIntegrationSecretsManager(t *testing.T) {
	id := os.Getenv("MAMORI_AWS_SM_SECRET_ID")
	if id == "" {
		t.Skip("set MAMORI_AWS_SM_SECRET_ID to run the Secrets Manager integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := NewSecretsManager(liveRegionOpts()...)
	v, err := p.Resolve(ctx, mustParseLive(t, "aws-sm://"+id))
	if err != nil {
		t.Fatalf("Resolve %q: %v", id, err)
	}
	if len(v.Bytes) == 0 {
		t.Error("resolved secret has empty payload")
	}
	if !v.Sensitive {
		t.Error("Secrets Manager value must be Sensitive")
	}
	if v.Version == "" {
		t.Error("Secrets Manager value must carry a VersionId")
	}
}

func TestIntegrationSecretsManagerNotFound(t *testing.T) {
	if os.Getenv("MAMORI_AWS_SM_SECRET_ID") == "" {
		t.Skip("set MAMORI_AWS_SM_SECRET_ID to run the Secrets Manager integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := NewSecretsManager(liveRegionOpts()...)
	_, err := p.Resolve(ctx, mustParseLive(t, "aws-sm://mamori/does-not-exist/"+time.Now().Format("150405")))
	if err == nil || !isNotFound(err) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestIntegrationParameterStore(t *testing.T) {
	name := os.Getenv("MAMORI_AWS_PS_PARAM_NAME")
	if name == "" {
		t.Skip("set MAMORI_AWS_PS_PARAM_NAME to run the Parameter Store integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := NewParameterStore(liveRegionOpts()...)
	v, err := p.Resolve(ctx, mustParseLive(t, "aws-ps://"+name))
	if err != nil {
		t.Fatalf("Resolve %q: %v", name, err)
	}
	if len(v.Bytes) == 0 {
		t.Error("resolved parameter has empty payload")
	}
	if v.Version == "" {
		t.Error("Parameter Store value must carry a numeric Version")
	}
}

func TestIntegrationParameterStoreNotFound(t *testing.T) {
	if os.Getenv("MAMORI_AWS_PS_PARAM_NAME") == "" {
		t.Skip("set MAMORI_AWS_PS_PARAM_NAME to run the Parameter Store integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := NewParameterStore(liveRegionOpts()...)
	_, err := p.Resolve(ctx, mustParseLive(t, "aws-ps:///mamori/does-not-exist/"+time.Now().Format("150405")))
	if err == nil || !isNotFound(err) {
		t.Fatalf("expected ErrNotFound, got %v", err)
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

func isNotFound(err error) bool {
	return err != nil && errorsIsNotFound(err)
}
