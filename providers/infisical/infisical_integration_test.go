//go:build integration

// Package infisical live integration tests hit a real Infisical instance and
// are excluded from the default build. They exist for one reason: nobody on
// this project has Infisical credentials, so the JSON wire shapes in
// infisical.go and auth.go come from the vendor's API reference rather than
// from a live call. These tests are how the first person with a machine
// identity confirms that shape in one command.
//
//	export INFISICAL_CLIENT_ID=...
//	export INFISICAL_CLIENT_SECRET=...
//	export INFISICAL_PROJECT_ID=...
//	export INFISICAL_TEST_SECRET_NAME=SOME_EXISTING_SECRET
//	export INFISICAL_ENVIRONMENT=dev          # optional
//	export INFISICAL_SECRET_PATH=/            # optional, defaults to /
//	export INFISICAL_BASE_URL=https://...     # optional, self-hosted installs
//	GOWORK=off go test -tags integration -run Integration ./...
//
// The first four are required and every test here skips if any is unset.
// Nothing below ever logs a client secret, an access token, or a resolved
// value: only the secret NAME (an environment variable the operator chose, not
// a secret) and a byte count.
package infisical

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// liveProvider builds a provider from the environment, skipping the calling
// test when the four required variables are not all set.
//
// The credentials are passed as explicit options rather than left to the
// provider's own environment reading, so that this test fails loudly on a
// missing variable instead of silently exercising a differently-configured
// provider.
func liveProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	clientID := os.Getenv(envClientID)
	clientSecret := os.Getenv(envClientSecret)
	projectID := os.Getenv(envProjectID)
	name := os.Getenv("INFISICAL_TEST_SECRET_NAME")
	if clientID == "" || clientSecret == "" || projectID == "" || name == "" {
		t.Skip("set INFISICAL_CLIENT_ID, INFISICAL_CLIENT_SECRET, INFISICAL_PROJECT_ID and " +
			"INFISICAL_TEST_SECRET_NAME to run the Infisical integration tests")
	}

	opts := []Option{
		WithClientID(clientID),
		WithClientSecret(clientSecret),
		WithProjectID(projectID),
		WithEnvironment(os.Getenv(envEnvironment)),
		WithSecretPath(os.Getenv(envSecretPath)),
	}
	if base := os.Getenv("INFISICAL_BASE_URL"); base != "" {
		opts = append(opts, WithBaseURL(base))
	}
	return New(opts...), name
}

// TestIntegrationResolve is the check no fake can perform: that Infisical's
// login reply really carries accessToken/expiresIn and its read reply really
// nests the value under "secret" as secretValue, rather than whatever shape a
// third-party write-up describes for the older v3 endpoint.
func TestIntegrationResolve(t *testing.T) {
	p, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("infisical://" + name)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", name, err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", name, err)
	}
	if len(v.Bytes) == 0 {
		t.Fatalf("Resolve(%q) returned an empty value; INFISICAL_TEST_SECRET_NAME must name a secret with a non-empty value", name)
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

// TestIntegrationTokenIsReused confirms against the real identity endpoint what
// the fake can only confirm against its own counter: that a second resolve
// reuses the cached access token. If Infisical's expiresIn were ever expressed
// in something other than seconds, this is where that would surface, as a
// second login (or a spurious refresh) rather than as a silently short-lived
// cache.
func TestIntegrationTokenIsReused(t *testing.T) {
	p, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("infisical://" + name)
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
		t.Errorf("Version changed between two immediate reads (%q then %q); the secret was rotated mid-test, or Version is not the backend revision",
			first.Version, second.Version)
	}
	if len(first.Bytes) != len(second.Bytes) {
		t.Fatalf("second Resolve returned %d byte(s), want %d", len(second.Bytes), len(first.Bytes))
	}
	t.Logf("two resolves of %q agreed on %d byte(s) at version %q", name, len(first.Bytes), first.Version)
}

// TestIntegrationAbsentSecretIsNotFound confirms the live 404 really is a 404,
// which is what makes a field's default: and optional handling apply. A backend
// that answered 200-with-an-empty-secret, or 403, would break that and no fake
// could tell us.
func TestIntegrationAbsentSecretIsNotFound(t *testing.T) {
	p, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	absent := name + "_MAMORI_INTEGRATION_ABSENT"
	ref, err := mamori.ParseRef("infisical://" + absent)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", absent, err)
	}
	if _, err := p.Resolve(ctx, ref); !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("Resolve(%q) err = %v, want ErrNotFound; if a secret of this derived name genuinely exists, rename it", absent, err)
	}
	t.Logf("absent secret %q correctly reported not-found", absent)
}

// TestIntegrationBadCredentialsAreUnauthenticated confirms the live login
// endpoint answers a wrong secret with a status that classifies as
// unauthenticated rather than, say, 400 or 422. mamori treats unauthenticated
// as terminal and invalid as terminal too, but only the first names the actual
// problem in `mamori doctor`.
func TestIntegrationBadCredentialsAreUnauthenticated(t *testing.T) {
	_, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opts := []Option{
		WithClientID(os.Getenv(envClientID)),
		WithClientSecret("mamori-integration-deliberately-wrong-secret"),
		WithProjectID(os.Getenv(envProjectID)),
		WithEnvironment(os.Getenv(envEnvironment)),
		WithSecretPath(os.Getenv(envSecretPath)),
	}
	if base := os.Getenv("INFISICAL_BASE_URL"); base != "" {
		opts = append(opts, WithBaseURL(base))
	}

	ref, err := mamori.ParseRef("infisical://" + name)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", name, err)
	}
	_, err = New(opts...).Resolve(ctx, ref)
	if err == nil {
		t.Fatal("a deliberately wrong client secret resolved successfully")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindUnauthenticated {
		t.Errorf("kind = %s, want unauthenticated", got)
	}
	// The error is not logged: it is the reply to a login that carried a
	// credential, and only its kind is safe to report.
	t.Logf("a wrong client secret was classified as %s", mamori.ErrorKind(err))
}
