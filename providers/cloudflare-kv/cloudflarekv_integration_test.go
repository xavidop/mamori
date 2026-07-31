//go:build integration

// Package cloudflarekv live integration tests hit the real Cloudflare Workers
// KV REST API and are excluded from the default build. Run them explicitly
// against a namespace that already has the referenced key written to it:
//
//	export CLOUDFLARE_API_TOKEN=...
//	export CLOUDFLARE_ACCOUNT_ID=...
//	export CLOUDFLARE_KV_NAMESPACE_ID=...
//	export CLOUDFLARE_KV_TEST_KEY=some-existing-key
//	GOWORK=off go test -tags integration -run Integration ./...
//
// All four variables are required; the tests skip if any is unset. Nothing
// here ever logs a token, an account id, or a resolved value - only the key
// name (an environment variable the operator chose, not a secret) and a byte
// count.
package cloudflarekv

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// liveCreds reads the four environment variables the integration tests
// require, skipping the calling test if any is unset.
func liveCreds(t *testing.T) (token, account, namespace, key string) {
	t.Helper()
	token = os.Getenv("CLOUDFLARE_API_TOKEN")
	account = os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	namespace = os.Getenv("CLOUDFLARE_KV_NAMESPACE_ID")
	key = os.Getenv("CLOUDFLARE_KV_TEST_KEY")
	if token == "" || account == "" || namespace == "" || key == "" {
		t.Skip("set CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID, CLOUDFLARE_KV_NAMESPACE_ID, and " +
			"CLOUDFLARE_KV_TEST_KEY to run the Workers KV integration tests")
	}
	return token, account, namespace, key
}

// liveRef builds the ref for CLOUDFLARE_KV_TEST_KEY against the live provider
// p addresses.
func liveRef(t *testing.T, key string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef("cloudflare-kv://" + key)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", key, err)
	}
	return ref
}

// TestIntegrationResolve resolves CLOUDFLARE_KV_TEST_KEY against the live API
// and asserts a non-empty value comes back, on the single-key GET endpoint's
// raw-bytes response shape (see get's doc comment in resolve.go).
func TestIntegrationResolve(t *testing.T) {
	token, account, namespace, key := liveCreds(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(WithAPIToken(token), WithAccountID(account), WithNamespaceID(namespace))
	v, err := p.Resolve(ctx, liveRef(t, key))
	if err != nil {
		t.Fatalf("Resolve(%q): %v", key, err)
	}
	if len(v.Bytes) == 0 {
		t.Fatalf("Resolve(%q) returned an empty value; CLOUDFLARE_KV_TEST_KEY must name a key with a non-empty value", key)
	}
	if v.Version == "" {
		t.Errorf("Resolve(%q) returned an empty Version", key)
	}
	t.Logf("Resolve(%q): %d byte(s)", key, len(v.Bytes))
}

// TestIntegrationResolveBatchAgreesWithResolve is the check a fake can never
// perform. The single-key GET endpoint responds with a value's raw stored
// bytes; the bulk/get endpoint wraps the very same value inside a JSON
// envelope ({"result":{"values":{...}}}). fake_test.go models both shapes,
// but a fake can only confirm this provider's bulk parsing agrees with the
// fake's own encoding of that envelope, not with what Cloudflare actually
// sends - and the bulk fake was written in Task 2 and went unexercised by any
// test until Task 3, so it was briefly an unvalidated oracle. Resolving the
// same live key through both Resolve and ResolveBatch and asserting they
// agree is the only test that exercises the real bulk response shape end to
// end.
func TestIntegrationResolveBatchAgreesWithResolve(t *testing.T) {
	token, account, namespace, key := liveCreds(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(WithAPIToken(token), WithAccountID(account), WithNamespaceID(namespace))
	ref := liveRef(t, key)

	single, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", key, err)
	}
	if len(single.Bytes) == 0 {
		t.Fatalf("Resolve(%q) returned an empty value; CLOUDFLARE_KV_TEST_KEY must name a key with a non-empty value", key)
	}

	batch, err := p.ResolveBatch(ctx, []mamori.Ref{ref})
	if err != nil {
		t.Fatalf("ResolveBatch(%q): %v", key, err)
	}
	batched, ok := batch[ref.Raw]
	if !ok {
		t.Fatalf("ResolveBatch(%q) omitted the key entirely, but Resolve found it", key)
	}

	t.Logf("Resolve(%q): %d byte(s); ResolveBatch(%q): %d byte(s)", key, len(single.Bytes), key, len(batched.Bytes))

	if string(single.Bytes) != string(batched.Bytes) {
		t.Fatalf("Resolve and ResolveBatch disagree on %q: single-key GET returned %d byte(s), bulk/get returned %d byte(s); "+
			"the bulk JSON-envelope parsing does not match the single-key raw-bytes path", key, len(single.Bytes), len(batched.Bytes))
	}
	if single.Version != batched.Version {
		t.Errorf("Resolve and ResolveBatch disagree on Version for %q", key)
	}
}
