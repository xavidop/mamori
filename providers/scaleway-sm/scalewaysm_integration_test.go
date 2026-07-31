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
// revision pinned via ?revision=<n>, and asserts the two agree - proving
// Version really does track the pinned revision rather than something
// incidental to the request (a static value, a hash, an unrelated counter).
//
// When the live secret has more than one revision, it additionally pins
// revision 1 (which the module doc comment and fake_test.go agree always
// exists once a secret has been written at all) and asserts THAT resolves to
// Version "1", distinct from the newer revision latest_enabled reported -
// the "returned versions differ appropriately" half of this check. A secret
// with only one revision cannot exercise that half, so it is skipped rather
// than faked.
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
		t.Logf("%q has only one revision; skipping the distinct-older-revision check", testSecret)
		return
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
