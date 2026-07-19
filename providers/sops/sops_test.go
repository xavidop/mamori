package sops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// writeFile writes content to a fresh file with the given name under a temp dir
// and returns its path. The content stands in for the ciphertext-on-disk; the
// injected fake decrypt returns the plaintext, so the bytes here only drive
// os.Stat (existence + size + mtime → Version).
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// fakeReturning builds an Option whose decrypt ignores the file and returns a
// fixed plaintext, recording the format it was asked for.
func fakeReturning(plaintext string, gotFormat *string) Option {
	return WithDecrypt(func(_ string, format string) ([]byte, error) {
		if gotFormat != nil {
			*gotFormat = format
		}
		return []byte(plaintext), nil
	})
}

func mustResolve(t *testing.T, p *Provider, raw string) mamori.Value {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", raw, err)
	}
	return v
}

func TestResolveWholeFile(t *testing.T) {
	const plain = "top-secret-token\n"
	path := writeFile(t, "app.enc.yaml", "ENC[...]")
	var format string
	p := New(fakeReturning(plain, &format))

	v := mustResolve(t, p, "sops://"+path)
	if string(v.Bytes) != plain {
		t.Fatalf("Bytes = %q, want %q", v.Bytes, plain)
	}
	if !v.Sensitive {
		t.Error("Value.Sensitive = false, want true for sops secrets")
	}
	if v.Version == "" {
		t.Error("Value.Version is empty; expected size+mtime version")
	}
	if format != "yaml" {
		t.Errorf("decrypt asked for format %q, want yaml (from .yaml extension)", format)
	}
}

func TestResolveYAMLKey(t *testing.T) {
	const doc = `database:
  password: s3cr3t!
api_key: abc-123
port: 5432
`
	path := writeFile(t, "secrets.enc.yaml", "ENC[...]")
	p := New(fakeReturning(doc, nil))

	// Scalar string field, unquoted.
	if got := string(mustResolve(t, p, "sops://"+path+"#api_key").Bytes); got != "abc-123" {
		t.Errorf("#api_key = %q, want abc-123", got)
	}
	// Numeric field, returned as its JSON encoding.
	if got := string(mustResolve(t, p, "sops://"+path+"#port").Bytes); got != "5432" {
		t.Errorf("#port = %q, want 5432", got)
	}
	// Nested object field, returned as JSON.
	if got := string(mustResolve(t, p, "sops://"+path+"#database").Bytes); got != `{"password":"s3cr3t!"}` {
		t.Errorf("#database = %q, want {\"password\":\"s3cr3t!\"}", got)
	}
}

func TestResolveJSONKey(t *testing.T) {
	const doc = `{"db_password":"p@ss","port":5432}`
	path := writeFile(t, "secrets.enc.json", "ENC[...]")
	var format string
	p := New(fakeReturning(doc, &format))

	if got := string(mustResolve(t, p, "sops://"+path+"#db_password").Bytes); got != "p@ss" {
		t.Errorf("#db_password = %q, want p@ss", got)
	}
	if got := string(mustResolve(t, p, "sops://"+path+"#port").Bytes); got != "5432" {
		t.Errorf("#port = %q, want 5432", got)
	}
	if format != "json" {
		t.Errorf("decrypt asked for format %q, want json (from .json extension)", format)
	}
}

func TestResolveMissingKey(t *testing.T) {
	path := writeFile(t, "secrets.enc.json", "ENC[...]")
	p := New(fakeReturning(`{"present":"x"}`, nil))
	ref, _ := mamori.ParseRef("sops://" + path + "#absent")
	_, err := p.Resolve(context.Background(), ref)
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing key error = %v, want errors.Is ErrNotFound", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.enc.yaml")
	p := New(fakeReturning("unused", nil))
	ref, _ := mamori.ParseRef("sops://" + missing)
	_, err := p.Resolve(context.Background(), ref)
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing file error = %v, want errors.Is ErrNotFound", err)
	}
}

func TestResolveContextCancelled(t *testing.T) {
	path := writeFile(t, "app.enc.yaml", "ENC[...]")
	p := New(fakeReturning("x", nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ref, _ := mamori.ParseRef("sops://" + path)
	if _, err := p.Resolve(ctx, ref); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestResolveDecryptError(t *testing.T) {
	path := writeFile(t, "app.enc.yaml", "ENC[...]")
	sentinel := errors.New("no key material")
	p := New(WithDecrypt(func(_, _ string) ([]byte, error) { return nil, sentinel }))
	ref, _ := mamori.ParseRef("sops://" + path)
	_, err := p.Resolve(context.Background(), ref)
	if !errors.Is(err, sentinel) {
		t.Fatalf("decrypt error = %v, want it to wrap the sentinel", err)
	}
	// A decrypt failure is NOT a not-found.
	if errors.Is(err, mamori.ErrNotFound) {
		t.Error("decrypt failure must not satisfy ErrNotFound")
	}
}

func TestFormatForPath(t *testing.T) {
	cases := map[string]string{
		"a.yaml":         "yaml",
		"a.yml":          "yaml",
		"a.enc.YAML":     "yaml",
		"a.json":         "json",
		"secrets.env":    "dotenv",
		"a.dotenv":       "dotenv",
		"a.txt":          "binary",
		"noext":          "binary",
		"/abs/path.yaml": "yaml",
	}
	for in, want := range cases {
		if got := formatForPath(in); got != want {
			t.Errorf("formatForPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestConformance runs the mamori provider conformance kit against the provider
// with an injected fake decrypt. Seed/Mutate write plaintext to a temp file that
// the fake reads back, so the full suite (resolve, not-found, versioning, native
// fsnotify watch, concurrency, goroutine hygiene) runs without any SOPS keys.
func TestConformance(t *testing.T) {
	dir := t.TempDir()
	pathFor := func(key string) string { return filepath.Join(dir, key+".enc.yaml") }

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider {
			return New(WithDecrypt(func(path, _ string) ([]byte, error) {
				return os.ReadFile(path)
			}))
		},
		Ref: func(key string) string { return "sops://" + pathFor(key) },
		Seed: func(_ context.Context, key, val string) error {
			return os.WriteFile(pathFor(key), []byte(val), 0o600)
		},
		Mutate: func(_ context.Context, key, val string) error {
			return os.WriteFile(pathFor(key), []byte(val), 0o600)
		},
	})
}
