//go:build integration

// Package scalewaysm live integration tests hit the real Scaleway Secret
// Manager REST API and are excluded from the default build. Run them
// explicitly against a project that already has the referenced secret
// written to it:
//
//	export SCW_SECRET_KEY=...
//	export SCW_DEFAULT_PROJECT_ID=...
//	export SCALEWAY_SM_TEST_SECRET=some-existing-secret-name
//	GOWORK=off go test -tags integration -run Integration ./...
//
// All three variables are required; the tests skip if any is unset.
// SCW_DEFAULT_REGION is honored if set (see settingsFor), but not required:
// Secret Manager falls back to "fr-par" the same way the non-integration
// path does.
//
// SCALEWAY_SM_TEST_SECRET should be provisioned with AT LEAST TWO revisions.
// TestIntegrationRevisionIsBackendRevision exists to verify the one property
// a fake can never check: that a pinned ?revision=<n> actually reaches the
// API and is honored, rather than being silently ignored in favor of the
// default latest_enabled selector. On a single-revision secret that check is
// a tautology - pinning revision=1 and resolving latest_enabled both name
// the same underlying revision, so a provider that ignored the parsed
// revision entirely and always sent latest_enabled would still "pass" by
// coincidence. The test detects that degenerate case and calls t.Skip naming
// exactly what it could not verify, rather than reporting a false PASS; give
// it a secret with two or more revisions so the meaningful half actually
// runs.
//
// Nothing here ever logs a resolved value, the API secret key, or the
// project id - only the secret name (an environment variable the operator
// chose, not a secret in itself) and a byte count.
package scalewaysm

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// liveCreds reads the three environment variables the integration tests
// require, skipping the calling test if any is unset.
func liveCreds(t *testing.T) (secretKey, projectID, testSecret string) {
	t.Helper()
	secretKey = os.Getenv("SCW_SECRET_KEY")
	projectID = os.Getenv("SCW_DEFAULT_PROJECT_ID")
	testSecret = os.Getenv("SCALEWAY_SM_TEST_SECRET")
	if secretKey == "" || projectID == "" || testSecret == "" {
		t.Skip("set SCW_SECRET_KEY, SCW_DEFAULT_PROJECT_ID, and SCALEWAY_SM_TEST_SECRET to run the Secret Manager integration tests")
	}
	return secretKey, projectID, testSecret
}

// TestIntegrationResolve resolves SCALEWAY_SM_TEST_SECRET at its default
// revision selector (latest_enabled) against the live API and asserts a
// non-empty, versioned, sensitive value comes back.
func TestIntegrationResolve(t *testing.T) {
	secretKey, projectID, testSecret := liveCreds(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(WithSecretKey(secretKey), WithProjectID(projectID))
	ref, err := mamori.ParseRef("scaleway-sm://" + testSecret)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", testSecret, err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", testSecret, err)
	}
	if len(v.Bytes) == 0 {
		t.Fatalf("Resolve(%q) returned an empty value; SCALEWAY_SM_TEST_SECRET must name a secret with a non-empty enabled revision", testSecret)
	}
	if v.Version == "" {
		t.Errorf("Resolve(%q) returned an empty Version", testSecret)
	} else if _, perr := strconv.ParseUint(v.Version, 10, 32); perr != nil {
		t.Errorf("Resolve(%q) returned Version %q that does not parse as a decimal revision number: %v", testSecret, v.Version, perr)
	}
	if !v.Sensitive {
		t.Errorf("Resolve(%q) returned Sensitive=false; this provider must always report Sensitive=true", testSecret)
	}
	t.Logf("Resolve(%q): %d byte(s)", testSecret, len(v.Bytes))
}

// TestIntegrationRevisionIsBackendRevision is the check a fake can never
// perform. fake_test.go's fakeSM only ever confirms that this provider's
// Resolve agrees with the fake's own bookkeeping of revision numbers; it
// cannot confirm that the real Secret Manager API actually returns a
// "revision" field that means what valueFor assumes it means. This test
// resolves the test secret once at its default selector (latest_enabled) to
// learn its current revision number, then resolves it again with that exact
// revision pinned via ?revision=<n>, and asserts the two agree.
//
// That round-trip alone is NOT sufficient, and must not be mistaken for
// proof that ?revision=<n> is honored: on a secret with only one revision,
// pinning revision=1 and resolving latest_enabled both name the same
// underlying revision, so a provider that silently ignored the parsed
// revision and always sent latest_enabled regardless of what the ref asked
// for would still make pinned.Version equal latest.Version, purely by
// coincidence. It is a necessary check, not a sufficient one.
//
// The property that actually rules out that bug requires a SECOND, DIFFERENT
// revision to compare against: when the live secret has more than one
// revision, this test additionally pins revision 1 (which the module doc
// comment and fake_test.go agree always exists once a secret has been
// written at all) and asserts THAT resolves to Version "1", distinct from
// the newer revision latest_enabled reported. Only this second comparison
// can catch a provider that never threads the parsed revision through to the
// request at all. A secret with only one revision cannot exercise that half,
// so the test calls t.Skip naming exactly what went unverified, rather than
// reporting a false PASS on the tautological check alone.
func TestIntegrationRevisionIsBackendRevision(t *testing.T) {
	secretKey, projectID, testSecret := liveCreds(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(WithSecretKey(secretKey), WithProjectID(projectID))

	latestRef, err := mamori.ParseRef("scaleway-sm://" + testSecret + "?revision=latest_enabled")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	latest, err := p.Resolve(ctx, latestRef)
	if err != nil {
		t.Fatalf("Resolve(%q, revision=latest_enabled): %v", testSecret, err)
	}
	if len(latest.Bytes) == 0 {
		t.Fatalf("Resolve(%q, revision=latest_enabled) returned an empty value", testSecret)
	}
	latestRevision, err := strconv.ParseUint(latest.Version, 10, 32)
	if err != nil {
		t.Fatalf("Version %q returned for revision=latest_enabled does not parse as a decimal revision number: %v", latest.Version, err)
	}
	t.Logf("Resolve(%q, revision=latest_enabled): %d byte(s), revision %d", testSecret, len(latest.Bytes), latestRevision)

	pinnedRef, err := mamori.ParseRef(fmt.Sprintf("scaleway-sm://%s?revision=%d", testSecret, latestRevision))
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	pinned, err := p.Resolve(ctx, pinnedRef)
	if err != nil {
		t.Fatalf("Resolve(%q, revision=%d): %v", testSecret, latestRevision, err)
	}
	if pinned.Version != latest.Version {
		t.Fatalf("pinning the exact revision latest_enabled resolved to (%d) returned Version %q, want %q: "+
			"Version must equal the specific revision requested, not something incidental to the request",
			latestRevision, pinned.Version, latest.Version)
	}
	if len(pinned.Bytes) != len(latest.Bytes) {
		t.Fatalf("pinned revision %d returned a different byte count (%d) than revision=latest_enabled did (%d) for what should be the same revision",
			latestRevision, len(pinned.Bytes), len(latest.Bytes))
	}
	t.Logf("Resolve(%q, revision=%d): %d byte(s), matches revision=latest_enabled", testSecret, latestRevision, len(pinned.Bytes))

	if latestRevision <= 1 {
		t.Skipf("%q has only one revision (revision %d). The pinned round-trip check above cannot "+
			"distinguish a provider that honors ?revision=<n> from one that silently ignores it and "+
			"always resolves latest_enabled instead, because both requests would reach the same "+
			"revision on a single-revision secret; that property is UNVERIFIED by this run, not "+
			"confirmed. Provision SCALEWAY_SM_TEST_SECRET with at least two revisions so the "+
			"distinct-older-revision check can run and actually rule that bug out.",
			testSecret, latestRevision)
	}

	firstRef, err := mamori.ParseRef("scaleway-sm://" + testSecret + "?revision=1")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	first, err := p.Resolve(ctx, firstRef)
	if err != nil {
		t.Fatalf("Resolve(%q, revision=1): %v", testSecret, err)
	}
	if first.Version != "1" {
		t.Fatalf("pinning revision=1 returned Version %q, want %q: Version must equal the specific revision requested", first.Version, "1")
	}
	if first.Version == latest.Version {
		t.Fatalf("revision 1 and revision %d reported the same Version %q, but revision=latest_enabled resolved to a higher revision number: "+
			"Version is not tracking the backend revision", latestRevision, first.Version)
	}
	t.Logf("Resolve(%q, revision=1): %d byte(s), Version %q distinct from revision %d", testSecret, len(first.Bytes), first.Version, latestRevision)
}
