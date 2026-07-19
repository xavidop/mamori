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

const sampleEnv = `# a comment
export DB_PASSWORD=s3cr3t
API_KEY="ab\ncd"
QUOTED='raw $value'
PLAIN=hello # trailing comment
EMPTY=
`

func TestDotenvResolveKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(sampleEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	p := dotenvProvider{}

	cases := map[string]string{
		"DB_PASSWORD": "s3cr3t",
		"API_KEY":     "ab\ncd",   // double-quote escapes
		"QUOTED":      "raw $value", // single-quote literal
		"PLAIN":       "hello",    // trailing comment stripped
		"EMPTY":       "",
	}
	for key, want := range cases {
		ref := Ref{Scheme: "dotenv", Path: path, Key: key}
		v, err := p.Resolve(context.Background(), ref)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if string(v.Bytes) != want {
			t.Errorf("%s = %q, want %q", key, v.Bytes, want)
		}
	}
}

func TestDotenvKeyNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	_ = os.WriteFile(path, []byte("FOO=bar\n"), 0o600)
	p := dotenvProvider{}

	_, err := p.Resolve(context.Background(), Ref{Scheme: "dotenv", Path: path, Key: "MISSING"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key err = %v, want ErrNotFound", err)
	}
}

func TestDotenvFileNotFound(t *testing.T) {
	p := dotenvProvider{}
	_, err := p.Resolve(context.Background(), Ref{Scheme: "dotenv", Path: "/no/such/.env", Key: "X"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file err = %v, want ErrNotFound", err)
	}
}

func TestDotenvWholeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	_ = os.WriteFile(path, []byte("A=1\nB=2\n"), 0o600)
	p := dotenvProvider{}

	v, err := p.Resolve(context.Background(), Ref{Scheme: "dotenv", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Bytes) != "A=1\nB=2\n" {
		t.Errorf("whole file = %q", v.Bytes)
	}
}

func TestDotenvAutoRegistered(t *testing.T) {
	if _, ok := providerFor("dotenv"); !ok {
		t.Fatal("dotenv provider not auto-registered")
	}
}

func TestDotenvWatch(t *testing.T) {
	defer goleak.VerifyNone(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("TOKEN=v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := dotenvProvider{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := p.Watch(ctx, Ref{Scheme: "dotenv", Path: path, Key: "TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case u := <-ch:
		if string(u.Value.Bytes) != "v1" {
			t.Fatalf("baseline = %q, want v1", u.Value.Bytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no baseline")
	}

	if err := os.WriteFile(path, []byte("TOKEN=v2\n"), 0o600); err != nil {
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
		t.Fatal("no update after rewrite")
	}

	cancel()
	for range ch {
	}
}
