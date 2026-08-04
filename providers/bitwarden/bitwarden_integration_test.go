//go:build integration

package bitwarden

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// TestIntegrationResolve resolves a real secret from a real Bitwarden Secrets
// Manager organization.
//
// It skips cleanly unless BOTH variables are set, so the default `go test
// -tags integration ./...` on a machine with no Bitwarden credentials is a
// skip and not a failure:
//
//	BWS_ACCESS_TOKEN     a machine account access token
//	MAMORI_BWS_SECRET_ID the UUID of a secret that account can read
//
// Optionally, MAMORI_BWS_SERVER_URL points at a self-hosted install (the base
// URL, e.g. https://vault.example.com) and MAMORI_BWS_EXPECT asserts the exact
// plaintext.
//
// This is the ONE test that can establish that the decryption chain works
// against ciphertext Bitwarden actually produced. Everything else in this
// module is either a vendor test vector or a self-consistent fixture; see the
// README's "What is verified".
func TestIntegrationResolve(t *testing.T) {
	token := os.Getenv("BWS_ACCESS_TOKEN")
	id := os.Getenv("MAMORI_BWS_SECRET_ID")
	if token == "" || id == "" {
		t.Skip("set BWS_ACCESS_TOKEN and MAMORI_BWS_SECRET_ID to run the Bitwarden integration test")
	}

	opts := []Option{WithAccessToken(token)}
	if base := os.Getenv("MAMORI_BWS_SERVER_URL"); base != "" {
		opts = append(opts, WithServerURL(base))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("bitwarden-sm://" + id)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

	v, err := New(opts...).Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// The value itself is never logged, only its shape.
	if len(v.Bytes) == 0 {
		t.Error("resolved an empty value")
	}
	if !v.Sensitive {
		t.Error("Value.Sensitive is false")
	}
	if v.Version == "" {
		t.Error("Value.Version is empty")
	}
	if want := os.Getenv("MAMORI_BWS_EXPECT"); want != "" && string(v.Bytes) != want {
		// Neither side is printed: both are the secret.
		t.Error("resolved value does not match MAMORI_BWS_EXPECT")
	}
	t.Logf("resolved %d bytes, version %s", len(v.Bytes), v.Version)
}

// TestIntegrationRejectsBadToken asserts a live identity endpoint rejects a
// syntactically valid but wrong access token, and that the failure classifies
// rather than hanging or panicking. It needs no secret of its own.
func TestIntegrationRejectsBadToken(t *testing.T) {
	if os.Getenv("BWS_ACCESS_TOKEN") == "" {
		t.Skip("set BWS_ACCESS_TOKEN to run the Bitwarden integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("bitwarden-sm://ec2c1d46-6a4b-4751-a310-af9601317f2d")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

	const bogus = "0.ec2c1d46-6a4b-4751-a310-af9601317f2d.notarealsecret:X8vbvA0bduihIDe/qrzIQQ=="
	if _, err := New(WithAccessToken(bogus)).Resolve(ctx, ref); err == nil {
		t.Fatal("a bogus access token resolved successfully")
	} else {
		t.Logf("rejected as expected: %v", mamori.ErrorKind(err))
	}
}
