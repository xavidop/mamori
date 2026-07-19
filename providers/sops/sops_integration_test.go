//go:build integration

// Package sops integration test. It exercises the REAL
// github.com/getsops/sops/v3/decrypt.File path end to end using an age key
// generated inside the test, so no ambient credentials or checked-in keys are
// needed. It is compiled and run only under the `integration` build tag:
//
//	GOWORK=off go test -tags integration ./...
//
// It requires the `age-keygen` and `sops` binaries on PATH; when either is
// missing the test skips rather than fails, so CI can gate it on tool
// availability.
package sops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

func lookRequired(t *testing.T, bin string) {
	t.Helper()
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s not on PATH; skipping real-sops integration test", bin)
	}
}

// generateAgeKey runs age-keygen, writes the identity to keyFile, and returns
// the public recipient string.
func generateAgeKey(t *testing.T, keyFile string) string {
	t.Helper()
	out, err := exec.Command("age-keygen", "-o", keyFile).CombinedOutput()
	if err != nil {
		t.Fatalf("age-keygen: %v\n%s", err, out)
	}
	// age-keygen prints "Public key: age1..." to stderr/stdout.
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, "age1"); i >= 0 {
			return strings.TrimSpace(line[i:])
		}
	}
	// Fall back to deriving the recipient from the key file.
	pub, err := exec.Command("age-keygen", "-y", keyFile).Output()
	if err != nil {
		t.Fatalf("age-keygen -y: %v", err)
	}
	return strings.TrimSpace(string(pub))
}

func TestIntegrationRealSOPS(t *testing.T) {
	lookRequired(t, "age-keygen")
	lookRequired(t, "sops")

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "keys.txt")
	recipient := generateAgeKey(t, keyFile)

	// Point both age and sops at the generated identity.
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)

	plainPath := filepath.Join(dir, "plain.yaml")
	const plaintext = "db_password: super-secret\napi_key: k-42\n"
	if err := os.WriteFile(plainPath, []byte(plaintext), 0o600); err != nil {
		t.Fatal(err)
	}

	encPath := filepath.Join(dir, "secrets.enc.yaml")
	enc := exec.Command("sops", "--encrypt", "--age", recipient, plainPath)
	encBytes, err := enc.Output()
	if err != nil {
		t.Fatalf("sops --encrypt: %v", err)
	}
	if err := os.WriteFile(encPath, encBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// Real provider (default decrypt.File), no fake.
	p := New()

	// Whole-file decrypt.
	ref, _ := mamori.ParseRef("sops://" + encPath)
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve whole file: %v", err)
	}
	if !v.Sensitive {
		t.Error("expected Sensitive=true")
	}
	if !strings.Contains(string(v.Bytes), "super-secret") {
		t.Errorf("decrypted whole file missing expected content: %q", v.Bytes)
	}

	// #key selection through the real YAML->JSON path.
	refKey, _ := mamori.ParseRef("sops://" + encPath + "#db_password")
	vk, err := p.Resolve(context.Background(), refKey)
	if err != nil {
		t.Fatalf("Resolve #db_password: %v", err)
	}
	if string(vk.Bytes) != "super-secret" {
		t.Errorf("#db_password = %q, want super-secret", vk.Bytes)
	}
}
