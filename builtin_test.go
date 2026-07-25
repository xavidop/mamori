package mamori

import (
	"context"
	"errors"
	"io/fs"
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

func TestFileProviderClassifiesPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits are not enforced")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(path, []byte("hunter2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	ref, err := ParseRef("file://" + path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fileProvider{}.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("Resolve of an unreadable file returned nil error")
	}
	if got := ErrorKind(err); got != KindPermissionDenied {
		t.Fatalf("ErrorKind = %q, want %q (err: %v)", got, KindPermissionDenied, err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("wrapped error no longer satisfies errors.Is(err, fs.ErrPermission); "+
			"the %%w: %%w pattern must preserve the original: %v", err)
	}
}

func TestFileProviderStillReportsNotFound(t *testing.T) {
	ref, err := ParseRef("file:///nonexistent/path/to/nothing")
	if err != nil {
		t.Fatal(err)
	}
	_, err = fileProvider{}.Resolve(context.Background(), ref)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file must still report ErrNotFound, got %v", err)
	}
	if got := ErrorKind(err); got != KindNotFound {
		t.Fatalf("ErrorKind = %q, want %q", got, KindNotFound)
	}
}

func TestExecProviderMissingBinaryStaysUnknown(t *testing.T) {
	// A binary missing from PATH means mamori could not even attempt to fetch
	// the value; it is not evidence the value itself is absent. It must NOT be
	// classified as ErrNotFound (that would trigger default:/optional
	// handling), so it stays unclassified and reports unknown.
	ref, err := ParseRef("exec:mamori-no-such-binary-exists-anywhere")
	if err != nil {
		t.Fatal(err)
	}
	_, err = execProvider{}.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("Resolve of a nonexistent binary returned nil error")
	}
	if got := ErrorKind(err); got != KindUnknown {
		t.Fatalf("ErrorKind = %q, want %q (err: %v)", got, KindUnknown, err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("missing binary must not satisfy errors.Is(err, ErrNotFound): %v", err)
	}
}

// TestExecProviderMissingBinaryDoesNotTriggerDefault is the regression test for
// the semantic bug this fix restores: a missing binary must fail Load loudly,
// never fall back to a field's default:. If exec.ErrNotFound is ever remapped
// to mamori.ErrNotFound again, this test must fail.
func TestExecProviderMissingBinaryDoesNotTriggerDefault(t *testing.T) {
	type cfg struct {
		Out string `source:"exec:mamori-no-such-binary-exists-anywhere" default:"FALLBACK"`
	}
	_, err := Load[cfg](context.Background(), WithExecProvider())
	if err == nil {
		t.Fatal("Load with a missing exec binary returned nil error; " +
			"it must fail instead of silently applying default:")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("Load error satisfies errors.Is(err, ErrNotFound); "+
			"a missing binary must not be classified as not-found: %v", err)
	}
}

func TestExecProviderClassifiesEmptyCommand(t *testing.T) {
	ref, err := ParseRef("exec:   ")
	if err != nil {
		t.Fatal(err)
	}
	_, err = execProvider{}.Resolve(context.Background(), ref)
	if got := ErrorKind(err); got != KindInvalid {
		t.Fatalf("ErrorKind = %q, want %q (err: %v)", got, KindInvalid, err)
	}
}

func TestExecProviderNonZeroExitStaysUnknown(t *testing.T) {
	// A command that runs and fails is a real failure, but mamori has no way to
	// know whether it was a permission problem, a missing value, or a bug in the
	// script. Reporting unknown is the honest answer.
	ref, err := ParseRef("exec:false")
	if err != nil {
		t.Fatal(err)
	}
	_, err = execProvider{}.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("Resolve of a failing command returned nil error")
	}
	if got := ErrorKind(err); got != KindUnknown {
		t.Fatalf("ErrorKind = %q, want %q (err: %v)", got, KindUnknown, err)
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
