package mamori

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestEnvProvider(t *testing.T) {
	p := envProvider{}
	t.Setenv("MAMORI_TEST_VAR", "hello")

	v, err := p.Resolve(context.Background(), Ref{Scheme: "env", Path: "MAMORI_TEST_VAR"})
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Bytes) != "hello" {
		t.Errorf("value = %q, want hello", v.Bytes)
	}
	if v.Version == "" {
		t.Error("expected non-empty version")
	}

	_, err = p.Resolve(context.Background(), Ref{Scheme: "env", Path: "MAMORI_DEFINITELY_UNSET_XYZ"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unset var error = %v, want ErrNotFound", err)
	}
}

func TestEnvProviderAutoRegistered(t *testing.T) {
	if _, ok := providerFor("env"); !ok {
		t.Fatal("env provider not auto-registered")
	}
	if _, ok := providerFor("file"); !ok {
		t.Fatal("file provider not auto-registered")
	}
}

func TestFileProviderResolve(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(path, []byte("filedata"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := fileProvider{}
	v, err := p.Resolve(context.Background(), Ref{Scheme: "file", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Bytes) != "filedata" {
		t.Errorf("value = %q, want filedata", v.Bytes)
	}

	_, err = p.Resolve(context.Background(), Ref{Scheme: "file", Path: filepath.Join(dir, "nope")})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file error = %v, want ErrNotFound", err)
	}
}

func TestFileProviderWatch(t *testing.T) {
	defer goleak.VerifyNone(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "watched.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := fileProvider{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := p.Watch(ctx, Ref{Scheme: "file", Path: path})
	if err != nil {
		t.Fatal(err)
	}

	// baseline
	select {
	case u := <-ch:
		if string(u.Value.Bytes) != "v1" {
			t.Fatalf("baseline = %q, want v1", u.Value.Bytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no baseline update")
	}

	// rewrite -> expect an update
	if err := os.WriteFile(path, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("watch error: %v", u.Err)
		}
		if string(u.Value.Bytes) != "v2" {
			t.Fatalf("update = %q, want v2", u.Value.Bytes)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no update after file rewrite")
	}

	cancel()
	for range ch { // drain to closure
	}
}

func TestExecProviderOptIn(t *testing.T) {
	// Not registered by default.
	if _, ok := providerFor("exec"); ok {
		t.Fatal("exec provider should NOT be auto-registered")
	}

	type cfg struct {
		Out string `source:"exec:echo hello"`
	}
	// Without WithExecProvider -> no provider for scheme.
	if _, err := Load[cfg](context.Background()); err == nil {
		t.Fatal("exec ref resolved without WithExecProvider")
	}
	// With opt-in -> works.
	c, err := Load[cfg](context.Background(), WithExecProvider())
	if err != nil {
		t.Fatalf("Load with exec: %v", err)
	}
	if c.Out != "hello\n" && c.Out != "hello" {
		t.Fatalf("exec out = %q, want hello", c.Out)
	}
}
