package mamori_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
)

// jsonKeyProvider serves one JSON document and applies ref.Key through
// SelectKey, the way a real structured-payload provider does.
type jsonKeyProvider struct {
	scheme string
	doc    string
}

func (p jsonKeyProvider) Scheme() string { return p.scheme }
func (p jsonKeyProvider) Resolve(_ context.Context, ref mamori.Ref) (mamori.Value, error) {
	if ref.Path != "cfg" {
		return mamori.Value{}, mamori.ErrNotFound
	}
	b, err := mamori.SelectKey([]byte(p.doc), ref.Key)
	if err != nil {
		return mamori.Value{}, err
	}
	return mamori.Value{Bytes: b, Version: mamori.VersionHash(b)}, nil
}

func TestResolveSimple(t *testing.T) {
	f := mamoritest.NewProvider("rr-simple")
	f.Set("a", "hello")
	v, err := mamori.Resolve(context.Background(), "rr-simple://a", mamori.WithProvider(f))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hello" {
		t.Errorf("Bytes = %q, want hello", v.Bytes)
	}
}

func TestResolveKey(t *testing.T) {
	p := jsonKeyProvider{scheme: "rr-key", doc: `{"db.password":"s3cret","n":1}`}
	v, err := mamori.Resolve(context.Background(), "rr-key://cfg#db.password", mamori.WithProvider(p))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "s3cret" {
		t.Errorf("Bytes = %q, want s3cret", v.Bytes)
	}
	_, err = mamori.Resolve(context.Background(), "rr-key://cfg#missing", mamori.WithProvider(p))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("missing key err = %v, want ErrNotFound", err)
	}
}

func TestResolveChain(t *testing.T) {
	a := mamoritest.NewProvider("rr-chain-a")
	b := mamoritest.NewProvider("rr-chain-b")
	b.Set("y", "from-b")
	v, err := mamori.Resolve(context.Background(), "rr-chain-a://x, rr-chain-b://y",
		mamori.WithProvider(a), mamori.WithProvider(b))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "from-b" {
		t.Errorf("Bytes = %q, want from-b", v.Bytes)
	}
}

func TestResolveChainFirstWins(t *testing.T) {
	a := mamoritest.NewProvider("rr-first-a")
	b := mamoritest.NewProvider("rr-first-b")
	a.Set("x", "from-a")
	b.Set("y", "from-b")
	v, err := mamori.Resolve(context.Background(), "rr-first-a://x,rr-first-b://y",
		mamori.WithProvider(a), mamori.WithProvider(b))
	if err != nil || string(v.Bytes) != "from-a" {
		t.Fatalf("got %q, %v; want from-a", v.Bytes, err)
	}
}

func TestResolveChainStopsOnNonNotFound(t *testing.T) {
	a := mamoritest.NewProvider("rr-stop-a")
	b := mamoritest.NewProvider("rr-stop-b")
	a.Fail("x", mamori.ErrUnavailable)
	b.Set("y", "from-b")
	_, err := mamori.Resolve(context.Background(), "rr-stop-a://x,rr-stop-b://y",
		mamori.WithProvider(a), mamori.WithProvider(b))
	if mamori.ErrorKind(err) != mamori.KindUnavailable {
		t.Errorf("kind = %q (err %v), want unavailable", mamori.ErrorKind(err), err)
	}
}

func TestResolveAllMissing(t *testing.T) {
	a := mamoritest.NewProvider("rr-miss-a")
	b := mamoritest.NewProvider("rr-miss-b")
	_, err := mamori.Resolve(context.Background(), "rr-miss-a://x,rr-miss-b://y",
		mamori.WithProvider(a), mamori.WithProvider(b))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveUnknownScheme(t *testing.T) {
	_, err := mamori.Resolve(context.Background(), "nope-scheme://x")
	if err == nil {
		t.Fatal("want error")
	}
	if mamori.ErrorKind(err) != mamori.KindInvalid {
		t.Errorf("kind = %q, want invalid", mamori.ErrorKind(err))
	}
	if want := `mamori: no provider registered for scheme "nope-scheme"`; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
}

func TestResolveParseError(t *testing.T) {
	if _, err := mamori.Resolve(context.Background(), "  "); err == nil {
		t.Error("empty ref: want error")
	}
	if _, err := mamori.Resolve(context.Background(), "no-scheme"); err == nil {
		t.Error("schemeless ref: want error")
	}
}

func TestResolveEnvBuiltin(t *testing.T) {
	t.Setenv("MAMORI_RESOLVE_FOO", "bar")
	v, err := mamori.Resolve(context.Background(), "env:MAMORI_RESOLVE_FOO")
	if err != nil || string(v.Bytes) != "bar" {
		t.Fatalf("got %q, %v; want bar", v.Bytes, err)
	}
}

func TestResolveFileBuiltin(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pg")
	if err := os.WriteFile(p, []byte("pw"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := mamori.Resolve(context.Background(), "file://"+p)
	if err != nil || string(v.Bytes) != "pw" {
		t.Fatalf("got %q, %v; want pw", v.Bytes, err)
	}
}

func TestResolveRefVars(t *testing.T) {
	f := mamoritest.NewProvider("rr-vars")
	f.Set("prod/db", "ok")
	v, err := mamori.Resolve(context.Background(), "rr-vars://${ENV}/db",
		mamori.WithProvider(f), mamori.WithRefVars(map[string]string{"ENV": "prod"}))
	if err != nil || string(v.Bytes) != "ok" {
		t.Fatalf("got %q, %v; want ok", v.Bytes, err)
	}
	_, err = mamori.Resolve(context.Background(), "rr-vars://${UNSET}/db", mamori.WithProvider(f))
	if mamori.ErrorKind(err) != mamori.KindInvalid {
		t.Errorf("unset var kind = %q (err %v), want invalid", mamori.ErrorKind(err), err)
	}
}

func TestResolveProviderOverridesRegistry(t *testing.T) {
	t.Setenv("MAMORI_RESOLVE_OVR", "from-env")
	over := mamoritest.NewProvider("env")
	over.Set("MAMORI_RESOLVE_OVR", "from-override")
	v, err := mamori.Resolve(context.Background(), "env:MAMORI_RESOLVE_OVR", mamori.WithProvider(over))
	if err != nil || string(v.Bytes) != "from-override" {
		t.Fatalf("got %q, %v; want from-override", v.Bytes, err)
	}
}

func TestResolveCtxCanceled(t *testing.T) {
	f := mamoritest.NewProvider("rr-ctx")
	f.Set("a", "v")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := mamori.Resolve(ctx, "rr-ctx://a", mamori.WithProvider(f))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	calls := &countingProvider{scheme: "rr-ctx2"}
	_, _ = mamori.Resolve(ctx, "rr-ctx2://a", mamori.WithProvider(calls))
	if calls.n != 0 {
		t.Errorf("provider called %d times with cancelled ctx, want 0", calls.n)
	}
}

type countingProvider struct {
	scheme string
	n      int
}

func (p *countingProvider) Scheme() string { return p.scheme }
func (p *countingProvider) Resolve(context.Context, mamori.Ref) (mamori.Value, error) {
	p.n++
	return mamori.Value{}, mamori.ErrNotFound
}

func TestResolveErrorOmitsSecretBytes(t *testing.T) {
	f := mamoritest.NewProvider("rr-leak")
	f.Set("a", "TOP-SECRET-VALUE")
	_, err := mamori.Resolve(context.Background(), "rr-leak://a?decode=base64", mamori.WithProvider(f))
	if err == nil {
		t.Fatal("want decode error")
	}
	if strings.Contains(err.Error(), "TOP-SECRET-VALUE") {
		t.Errorf("error leaks secret bytes: %v", err)
	}
}
