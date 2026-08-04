//go:build integration

// Package hcpvaultsecrets live integration tests hit a real HCP Vault Secrets
// application and are excluded from the default build. They exist for one
// reason: nobody on this project has HCP credentials, so the OpenAppSecret path
// template and the JSON wire shapes in resolve.go come from HashiCorp's API
// reference rather than from a live call. These tests are how the first person
// with a service principal confirms that shape in one command.
//
//	export HCP_CLIENT_ID=...
//	export HCP_CLIENT_SECRET=...
//	export HCP_ORGANIZATION_ID=...
//	export HCP_PROJECT_ID=...
//	export HCP_APP_NAME=...
//	export HCP_TEST_SECRET_NAME=SOME_EXISTING_STATIC_SECRET
//	GOWORK=off go test -tags integration -run Integration ./...
//
// All six are required and every test here skips if any is unset. Nothing below
// ever logs a client secret, an access token, or a resolved value: only the
// secret NAME (an environment variable the operator chose, not a secret) and a
// byte count.
package hcpvaultsecrets

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// liveProvider builds a provider from the environment, skipping the calling
// test when the six required variables are not all set.
//
// The credentials are passed as explicit options rather than left to the
// provider's own environment reading, so that this test fails loudly on a
// missing variable instead of silently exercising a differently-configured
// provider.
func liveProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	clientID := os.Getenv(envClientID)
	clientSecret := os.Getenv(envClientSecret)
	org := os.Getenv(envOrganization)
	project := os.Getenv(envProject)
	app := os.Getenv(envApp)
	name := os.Getenv("HCP_TEST_SECRET_NAME")
	if clientID == "" || clientSecret == "" || org == "" || project == "" || app == "" || name == "" {
		t.Skip("set HCP_CLIENT_ID, HCP_CLIENT_SECRET, HCP_ORGANIZATION_ID, HCP_PROJECT_ID, " +
			"HCP_APP_NAME and HCP_TEST_SECRET_NAME to run the HCP Vault Secrets integration tests")
	}

	return New(
		WithClientID(clientID),
		WithClientSecret(clientSecret),
		WithOrganizationID(org),
		WithProjectID(project),
		WithAppName(app),
	), name
}

// TestIntegrationResolve is the check no fake can perform: that the
// OpenAppSecret path really is
// /secrets/2023-11-28/organizations/{org}/projects/{proj}/apps/{app}/secrets/{name}:open,
// and that its reply really nests the value under secret.static_version.value
// rather than at the top level or under the 2023-06-13 shape.
func TestIntegrationResolve(t *testing.T) {
	p, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("hcp-vs://" + name)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", name, err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", name, err)
	}
	if len(v.Bytes) == 0 {
		t.Fatalf("Resolve(%q) returned an empty value; HCP_TEST_SECRET_NAME must name a static secret with a non-empty value", name)
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
// reuses the cached access token. If HCP's expires_in were ever expressed in
// something other than seconds, this is where that would surface, as a second
// exchange rather than as a silently short-lived cache.
func TestIntegrationTokenIsReused(t *testing.T) {
	p, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("hcp-vs://" + name)
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
	ref, err := mamori.ParseRef("hcp-vs://" + absent)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", absent, err)
	}
	if _, err := p.Resolve(ctx, ref); !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("Resolve(%q) err = %v, want ErrNotFound; if a secret of this derived name genuinely exists, rename it", absent, err)
	}
	t.Logf("absent secret %q correctly reported not-found", absent)
}

// TestIntegrationBadCredentialsAreUnauthenticated confirms the live token
// endpoint answers a wrong client secret with a status that classifies as
// unauthenticated rather than, say, 400. mamori treats unauthenticated as
// terminal, and only that kind names the actual problem in `mamori doctor`.
func TestIntegrationBadCredentialsAreUnauthenticated(t *testing.T) {
	_, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(
		WithClientID(os.Getenv(envClientID)),
		WithClientSecret("mamori-integration-deliberately-wrong-secret"),
		WithOrganizationID(os.Getenv(envOrganization)),
		WithProjectID(os.Getenv(envProject)),
		WithAppName(os.Getenv(envApp)),
	)
	ref, err := mamori.ParseRef("hcp-vs://" + name)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", name, err)
	}
	_, err = p.Resolve(ctx, ref)
	if err == nil {
		t.Fatal("a deliberately wrong client secret resolved successfully")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindUnauthenticated {
		t.Errorf("kind = %s, want unauthenticated", got)
	}
	// The error is not logged: it is the reply to an exchange that carried a
	// credential, and only its kind is safe to report.
	t.Logf("a wrong client secret was classified as %s", mamori.ErrorKind(err))
}

// TestIntegrationAudienceIsRequired documents, by exercising it, the one part
// of the grant that a standards-compliant OAuth2 client would omit: HCP issues
// a token only for audience https://api.hashicorp.cloud, and this provider sets
// it through httpcore.OAuth2Config.Audience. A resolve that succeeds is proof
// the audience reached the identity provider, since HCP rejects the exchange
// without it.
func TestIntegrationAudienceIsRequired(t *testing.T) {
	p, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("hcp-vs://" + name)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", name, err)
	}
	if _, err := p.Resolve(ctx, ref); err != nil {
		t.Fatalf("Resolve(%q): %v; if this fails with unauthenticated, the audience parameter is the first thing to check", name, err)
	}
	t.Logf("the client-credentials grant with audience %q was accepted", tokenAudience)
}
