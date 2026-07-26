package main

import (
	"encoding/base64"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBearerFileReadsToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("s3cr3t-tok\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	f := &liveFlags{bearerFile: path}
	header, certs, err := f.resolveCredential()
	if err != nil {
		t.Fatalf("resolveCredential() error = %v", err)
	}
	if want := "Bearer s3cr3t-tok"; header != want {
		t.Errorf("header = %q, want %q", header, want)
	}
	if certs != nil {
		t.Errorf("certs = %v, want nil", certs)
	}
}

func TestBearerStdin(t *testing.T) {
	f := &liveFlags{bearerFile: "-", stdin: strings.NewReader("tok\n")}
	header, _, err := f.resolveCredential()
	if err != nil {
		t.Fatalf("resolveCredential() error = %v", err)
	}
	if want := "Bearer tok"; header != want {
		t.Errorf("header = %q, want %q", header, want)
	}
}

func TestBasicFileReadsUserPass(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds")
	if err := os.WriteFile(path, []byte("user:pass\n"), 0o600); err != nil {
		t.Fatalf("write creds file: %v", err)
	}

	f := &liveFlags{basicFile: path}
	header, _, err := f.resolveCredential()
	if err != nil {
		t.Fatalf("resolveCredential() error = %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))
	if header != want {
		t.Errorf("header = %q, want %q", header, want)
	}
}

func TestBearerAndBasicMutuallyExclusive(t *testing.T) {
	f := &liveFlags{bearer: "tok", basic: "user:pass"}
	if _, _, err := f.resolveCredential(); err == nil {
		t.Fatal("resolveCredential() error = nil, want error for mutually exclusive bearer+basic")
	}
}

func TestClientCertRequiresBothCertAndKey(t *testing.T) {
	f := &liveFlags{clientCert: "/tmp/does-not-matter.crt"}
	if _, _, err := f.resolveCredential(); err == nil {
		t.Fatal("resolveCredential() error = nil, want error for cert without key")
	}

	f2 := &liveFlags{clientKey: "/tmp/does-not-matter.key"}
	if _, _, err := f2.resolveCredential(); err == nil {
		t.Fatal("resolveCredential() error = nil, want error for key without cert")
	}
}

func TestNoCredentialIsValid(t *testing.T) {
	f := &liveFlags{}
	header, certs, err := f.resolveCredential()
	if err != nil {
		t.Fatalf("resolveCredential() error = %v", err)
	}
	if header != "" {
		t.Errorf("header = %q, want empty", header)
	}
	if certs != nil {
		t.Errorf("certs = %v, want nil", certs)
	}
}

func TestCredentialNotInArgs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	token := "super-secret-token-value"
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	args := []string{"--bearer-file", path}

	f := &liveFlags{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	f.register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	for _, a := range args {
		if strings.Contains(a, token) {
			t.Errorf("token %q leaked into parsed args: %q", token, a)
		}
	}

	header, _, err := f.resolveCredential()
	if err != nil {
		t.Fatalf("resolveCredential() error = %v", err)
	}
	if want := "Bearer " + token; header != want {
		t.Errorf("header = %q, want %q", header, want)
	}
}

func TestRegisterFlags(t *testing.T) {
	f := &liveFlags{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	f.register(fs)

	args := []string{
		"--endpoint", "https://example.com",
		"--insecure",
		"--bearer", "tok",
		"--client-cert", "/tmp/c.crt",
		"--client-key", "/tmp/c.key",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if f.endpoint != "https://example.com" {
		t.Errorf("endpoint = %q", f.endpoint)
	}
	if !f.insecure {
		t.Errorf("insecure = false, want true")
	}
	if f.bearer != "tok" {
		t.Errorf("bearer = %q", f.bearer)
	}
	if f.clientCert != "/tmp/c.crt" {
		t.Errorf("clientCert = %q", f.clientCert)
	}
	if f.clientKey != "/tmp/c.key" {
		t.Errorf("clientKey = %q", f.clientKey)
	}
}
