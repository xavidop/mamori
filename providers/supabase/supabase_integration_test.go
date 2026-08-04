//go:build integration

// Package supabase live integration tests hit a real Supabase project and are
// excluded from the default build. They exist for one reason: nobody on this
// project has a Supabase project, so the PostgREST wire shapes in resolve.go
// come from the vendor's documentation rather than from a live call. These
// tests are how the first person with a project confirms that shape in one
// command.
//
// They require the setup SQL from this provider's README to have been applied,
// because vault.decrypted_secrets cannot be read over PostgREST at all without
// it: the vault schema is not exposed and cannot be added to the exposed-schema
// list. TestIntegrationResolve failing with not_found is the expected symptom
// of a project where that SQL was never run.
//
//	export SUPABASE_URL=https://<project-ref>.supabase.co
//	export SUPABASE_SERVICE_ROLE_KEY=...
//	export SUPABASE_TEST_SECRET_NAME=some-existing-vault-secret
//	export SUPABASE_VAULT_SCHEMA=public          # optional, defaults to public
//	export SUPABASE_VAULT_VIEW=decrypted_secrets # optional
//	GOWORK=off go test -tags integration -run Integration ./...
//
// The first three are required and every test here skips if any is unset.
// Nothing below ever logs the service-role key or a resolved value: only the
// secret NAME (an environment variable the operator chose, not a secret), a
// byte count, and a version.
package supabase

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// liveProvider builds a provider from the environment, skipping the calling
// test when the three required variables are not all set.
//
// The credentials are passed as explicit options rather than left to the
// provider's own environment reading, so that this test fails loudly on a
// missing variable instead of silently exercising a differently-configured
// provider.
func liveProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	projectURL := os.Getenv(envURL)
	serviceKey := os.Getenv(envServiceKey)
	name := os.Getenv("SUPABASE_TEST_SECRET_NAME")
	if projectURL == "" || serviceKey == "" || name == "" {
		t.Skip("set SUPABASE_URL, SUPABASE_SERVICE_ROLE_KEY and SUPABASE_TEST_SECRET_NAME " +
			"to run the Supabase integration tests")
	}

	return New(
		WithProjectURL(projectURL),
		WithServiceKey(serviceKey),
		WithSchema(os.Getenv(envSchema)),
		WithView(os.Getenv(envView)),
	), name
}

// TestIntegrationResolve is the check no fake can perform: that a real
// PostgREST really answers this filtered select with a JSON array of rows
// carrying a decrypted_secret column, reached through the operator's re-exposed
// relation rather than through vault.decrypted_secrets directly.
func TestIntegrationResolve(t *testing.T) {
	p, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("supabase://" + name)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", name, err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve(%q): %v; if this is not_found, the README's setup SQL has probably not been applied to this project", name, err)
	}
	if len(v.Bytes) == 0 {
		t.Fatalf("Resolve(%q) returned an empty value; SUPABASE_TEST_SECRET_NAME must name a secret with a non-empty value", name)
	}
	if v.Version == "" {
		t.Errorf("Resolve(%q) returned an empty Version", name)
	}
	if !v.Sensitive {
		t.Errorf("Resolve(%q) returned Sensitive=false; every value from a secret manager must be marked sensitive", name)
	}
	// The byte count and the version, never the value.
	t.Logf("Resolve(%q): %d byte(s), version %q", name, len(v.Bytes), v.Version)
}

// TestIntegrationAbsentSecretIsNotFound is the most valuable of these tests,
// because it confirms the one behaviour that is not a status code: PostgREST
// answering an unmatched filter with 200 and an empty array, which this
// provider must turn into ErrNotFound so a field's default: applies. A backend
// that answered 404, or 200 with a null, would break that and no fake could
// tell us.
func TestIntegrationAbsentSecretIsNotFound(t *testing.T) {
	p, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	absent := name + "-mamori-integration-absent"
	ref, err := mamori.ParseRef("supabase://" + absent)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", absent, err)
	}
	if _, err := p.Resolve(ctx, ref); !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("Resolve(%q) err = %v, want ErrNotFound; if a secret of this derived name genuinely exists, rename it", absent, err)
	}
	t.Logf("absent secret %q correctly reported not-found", absent)
}

// TestIntegrationVersionIsStableAcrossReads confirms against a real row that
// updated_at is what this provider reads and that it does not move on its own.
// If Supabase ever rendered that timestamp non-deterministically, this is where
// it would surface, as two different versions rather than as a permanently
// "changed" secret in production.
func TestIntegrationVersionIsStableAcrossReads(t *testing.T) {
	p, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("supabase://" + name)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", name, err)
	}
	first, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("first Resolve(%q): %v", name, err)
	}
	second, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("second Resolve(%q): %v", name, err)
	}
	if first.Version != second.Version {
		t.Errorf("Version changed between two immediate reads (%q then %q); the secret was rotated mid-test, or Version is not the row's updated_at",
			first.Version, second.Version)
	}
	if len(first.Bytes) != len(second.Bytes) {
		t.Fatalf("second Resolve returned %d byte(s), want %d", len(second.Bytes), len(first.Bytes))
	}
	t.Logf("two resolves of %q agreed on %d byte(s) at version %q", name, len(first.Bytes), first.Version)
}

// TestIntegrationVaultSchemaIsNotReachable is the test that documents the
// premise of this whole provider. Reading vault.decrypted_secrets directly must
// fail, because Supabase restricts the vault schema from the exposed-schema
// list; if this ever starts passing, the README's setup section can be deleted
// and this provider gets much simpler.
func TestIntegrationVaultSchemaIsNotReachable(t *testing.T) {
	p, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("supabase://" + name + "?schema=vault&view=decrypted_secrets")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if _, err := p.Resolve(ctx, ref); err == nil {
		t.Error("reading vault.decrypted_secrets directly SUCCEEDED; Supabase now exposes the vault schema and this provider's setup requirement can be relaxed")
	} else {
		// The kind, never the error text: it is a reply to a request that
		// carried the service-role key.
		t.Logf("vault.decrypted_secrets is not reachable over PostgREST, as documented: %s", mamori.ErrorKind(err))
	}
}

// TestIntegrationBadKeyIsNotSilentlyAccepted confirms the live gateway refuses
// a wrong key rather than falling back to the anonymous role and answering an
// empty array, which would be indistinguishable from an absent secret and would
// make mamori apply a field's default over an authentication failure.
func TestIntegrationBadKeyIsNotSilentlyAccepted(t *testing.T) {
	_, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(
		WithProjectURL(os.Getenv(envURL)),
		WithServiceKey("mamori-integration-deliberately-wrong-key"),
		WithSchema(os.Getenv(envSchema)),
		WithView(os.Getenv(envView)),
	)
	ref, err := mamori.ParseRef("supabase://" + name)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", name, err)
	}
	_, err = p.Resolve(ctx, ref)
	if err == nil {
		t.Fatal("a deliberately wrong service-role key resolved successfully")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Error("a wrong key was reported as not_found; mamori would apply a field's default over an authentication failure")
	}
	// The error is not logged: it is a reply to a request that carried a
	// credential, and only its kind is safe to report.
	t.Logf("a wrong service-role key was classified as %s", mamori.ErrorKind(err))
}
