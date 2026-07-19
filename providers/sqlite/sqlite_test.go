package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// dsnFor builds the same busy-timeout DSN the provider uses, so test writers and
// the provider open the database identically (default rollback-journal mode, so
// commits modify the main file in place and the fsnotify watch fires).
func dsnFor(path string) string {
	return fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path)
}

// mustExec opens a short-lived connection, runs stmt, and closes it. Opening and
// closing per write keeps no background *sql.DB goroutine alive across the
// conformance kit's goleak check.
func mustExec(t *testing.T, path, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", dsnFor(path))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

// writeKV upserts a (key, value) row into a table with columns key/value.
func writeKV(path, table, key, val string) error {
	db, err := sql.Open("sqlite", dsnFor(path))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	stmt := fmt.Sprintf("INSERT INTO %s(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", table)
	_, err = db.Exec(stmt, key, val)
	return err
}

// newKVTable creates a fresh temp database file with a key/value table and
// returns its path.
func newKVTable(t *testing.T, table string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	mustExec(t, path, fmt.Sprintf("CREATE TABLE %s (key TEXT PRIMARY KEY, value TEXT)", table))
	return path
}

func mustRef(t *testing.T, s string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return ref
}

// TestConformance runs the full mamori provider conformance kit against a real
// temporary SQLite database file (pure-Go modernc.org/sqlite, no cgo). Seed and
// Mutate write rows; the fsnotify watch fires on those writes so the watch
// conformance checks exercise the native watch.
func TestConformance(t *testing.T) {
	path := newKVTable(t, "cfg")
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider {
			return New(WithPath(path), WithDebounce(40*time.Millisecond))
		},
		Ref:  func(key string) string { return "sqlite://cfg/" + key },
		Seed: func(_ context.Context, key, val string) error { return writeKV(path, "cfg", key, val) },
		Mutate: func(_ context.Context, key, val string) error {
			return writeKV(path, "cfg", key, val)
		},
		EventuallyTimeout: 5 * time.Second,
	})
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != scheme {
		t.Fatalf("Scheme() = %q, want %q", got, scheme)
	}
}

func TestRegistered(t *testing.T) {
	found := false
	for _, s := range mamori.RegisteredSchemes() {
		if s == scheme {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("scheme %q not registered by init()", scheme)
	}
}

func TestResolveValueAndVersion(t *testing.T) {
	path := newKVTable(t, "cfg")
	if err := writeKV(path, "cfg", "log_level", "debug"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := New(WithPath(path))

	v, err := p.Resolve(context.Background(), mustRef(t, "sqlite://cfg/log_level"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("Bytes = %q, want debug", v.Bytes)
	}
	if v.Version == "" {
		t.Fatal("Version is empty; want a VersionHash")
	}
	if v.Sensitive {
		t.Fatal("value must not be Sensitive by default")
	}

	// A change must change the version (VersionHash of the bytes).
	if err := writeKV(path, "cfg", "log_level", "info"); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	v2, err := p.Resolve(context.Background(), mustRef(t, "sqlite://cfg/log_level"))
	if err != nil {
		t.Fatalf("Resolve after mutate: %v", err)
	}
	if v2.Version == v.Version {
		t.Fatalf("Version did not change after write (both %q)", v.Version)
	}
	if string(v2.Bytes) != "info" {
		t.Fatalf("Bytes = %q, want info", v2.Bytes)
	}
}

func TestResolveJSONKey(t *testing.T) {
	path := newKVTable(t, "cfg")
	if err := writeKV(path, "cfg", "db", `{"host":"db.internal","port":5432,"password":"s3cr3t"}`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := New(WithPath(path))

	get := func(field string) string {
		t.Helper()
		v, err := p.Resolve(context.Background(), mustRef(t, "sqlite://cfg/db#"+field))
		if err != nil {
			t.Fatalf("Resolve #%s: %v", field, err)
		}
		return string(v.Bytes)
	}
	if get("host") != "db.internal" {
		t.Fatalf("host = %q", get("host"))
	}
	if get("port") != "5432" {
		t.Fatalf("port = %q, want 5432", get("port"))
	}
	if get("password") != "s3cr3t" {
		t.Fatalf("password = %q", get("password"))
	}

	_, err := p.Resolve(context.Background(), mustRef(t, "sqlite://cfg/db#absent"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing json field err = %v, want ErrNotFound", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	path := newKVTable(t, "cfg")
	p := New(WithPath(path))
	_, err := p.Resolve(context.Background(), mustRef(t, "sqlite://cfg/nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveContextCancelled(t *testing.T) {
	path := newKVTable(t, "cfg")
	_ = writeKV(path, "cfg", "k", "v")
	p := New(WithPath(path))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustRef(t, "sqlite://cfg/k")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestCustomColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	mustExec(t, path, "CREATE TABLE settings (name TEXT PRIMARY KEY, val TEXT)")
	mustExec(t, path, "INSERT INTO settings(name, val) VALUES(?, ?)", "region", "eu-west-1")

	p := New(WithPath(path))
	v, err := p.Resolve(context.Background(), mustRef(t, "sqlite://settings/region?key_col=name&val_col=val"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "eu-west-1" {
		t.Fatalf("Bytes = %q, want eu-west-1", v.Bytes)
	}
}

func TestVersionColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	mustExec(t, path, "CREATE TABLE cfg (key TEXT PRIMARY KEY, value TEXT, rev INTEGER)")
	mustExec(t, path, "INSERT INTO cfg(key, value, rev) VALUES(?, ?, ?)", "k", "one", 7)

	p := New(WithPath(path), WithVersionColumn("rev"))
	v, err := p.Resolve(context.Background(), mustRef(t, "sqlite://cfg/k"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Version != "7" {
		t.Fatalf("Version = %q, want 7 (from version column)", v.Version)
	}

	mustExec(t, path, "UPDATE cfg SET value=?, rev=? WHERE key=?", "two", 8, "k")
	v2, err := p.Resolve(context.Background(), mustRef(t, "sqlite://cfg/k"))
	if err != nil {
		t.Fatalf("Resolve after update: %v", err)
	}
	if v2.Version != "8" {
		t.Fatalf("Version = %q, want 8", v2.Version)
	}
}

func TestSensitive(t *testing.T) {
	path := newKVTable(t, "cfg")
	_ = writeKV(path, "cfg", "token", "abc123")
	p := New(WithPath(path), WithSensitive(true))
	v, err := p.Resolve(context.Background(), mustRef(t, "sqlite://cfg/token"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Fatal("WithSensitive(true) did not mark the value Sensitive")
	}
}

func TestBadRefPath(t *testing.T) {
	path := newKVTable(t, "cfg")
	p := New(WithPath(path))
	// No key segment.
	if _, err := p.Resolve(context.Background(), mustRef(t, "sqlite://cfg")); err == nil {
		t.Fatal("ref without <key> segment returned nil error")
	}
}

func TestUnconfigured(t *testing.T) {
	t.Setenv("SQLITE_PATH", "")
	p := New()
	_, err := p.Resolve(context.Background(), mustRef(t, "sqlite://cfg/k"))
	if err == nil {
		t.Fatal("unconfigured provider returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("unconfigured error should not be ErrNotFound: %v", err)
	}
}

func TestSQLitePathEnv(t *testing.T) {
	path := newKVTable(t, "cfg")
	_ = writeKV(path, "cfg", "k", "from-env")
	t.Setenv("SQLITE_PATH", path)
	p := New() // no WithPath: must fall back to SQLITE_PATH
	v, err := p.Resolve(context.Background(), mustRef(t, "sqlite://cfg/k"))
	if err != nil {
		t.Fatalf("Resolve via SQLITE_PATH: %v", err)
	}
	if string(v.Bytes) != "from-env" {
		t.Fatalf("Bytes = %q, want from-env", v.Bytes)
	}
}

// TestIdentifierAllowlistRejectsInjection is the security-critical test: a
// malicious table or column name must be rejected by the allowlist and must
// never reach the database, so an injected statement cannot execute.
func TestIdentifierAllowlistRejectsInjection(t *testing.T) {
	path := newKVTable(t, "cfg")
	if err := writeKV(path, "cfg", "canary", "alive"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := New(WithPath(path))

	// Malicious table name (via the ref path).
	badTable := mustRef(t, "sqlite://users;DROP TABLE cfg/canary")
	if _, err := p.Resolve(context.Background(), badTable); err == nil {
		t.Fatal("malicious table name was accepted; want rejection")
	} else if errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("injection should be an invalid-identifier error, got ErrNotFound: %v", err)
	}

	// Malicious column name (via the val_col option). Build the ref directly so
	// the payload survives verbatim.
	badCol := mamori.Ref{
		Scheme: scheme,
		Path:   "cfg/canary",
		Opts:   url.Values{"val_col": {"value; DROP TABLE cfg"}},
		Raw:    "sqlite://cfg/canary?val_col=value; DROP TABLE cfg",
	}
	if _, err := p.Resolve(context.Background(), badCol); err == nil {
		t.Fatal("malicious val_col was accepted; want rejection")
	}

	// Malicious version column (WithVersionColumn).
	pv := New(WithPath(path), WithVersionColumn("rev); DROP TABLE cfg;--"))
	if _, err := pv.Resolve(context.Background(), mustRef(t, "sqlite://cfg/canary")); err == nil {
		t.Fatal("malicious version column was accepted; want rejection")
	}

	// The table must still be intact and the canary row still present - proof no
	// injected DROP executed.
	v, err := p.Resolve(context.Background(), mustRef(t, "sqlite://cfg/canary"))
	if err != nil {
		t.Fatalf("canary row lost - injection may have executed: %v", err)
	}
	if string(v.Bytes) != "alive" {
		t.Fatalf("canary = %q, want alive", v.Bytes)
	}
}

func TestValidIdent(t *testing.T) {
	good := []string{"key", "value", "my_table", "_x", "Col123", "V"}
	bad := []string{"", "1abc", "has space", "a;b", "a-b", `a"b`, "a.b", "a)b", "table--", "a b; DROP"}
	for _, s := range good {
		if !validIdent(s) {
			t.Errorf("validIdent(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if validIdent(s) {
			t.Errorf("validIdent(%q) = true, want false", s)
		}
	}
}

// TestWatchEmitsBaselineAndChange is a dedicated watch unit test (belt and
// braces alongside the conformance watch checks): it verifies the fsnotify-based
// watch delivers a baseline and then re-queries on a write to the database file.
func TestWatchEmitsBaselineAndChange(t *testing.T) {
	path := newKVTable(t, "cfg")
	if err := writeKV(path, "cfg", "watched", "v1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := New(WithPath(path), WithDebounce(40*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "sqlite://cfg/watched"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Baseline.
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("baseline err: %v", u.Err)
		}
		if string(u.Value.Bytes) != "v1" {
			t.Fatalf("baseline = %q, want v1", u.Value.Bytes)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no baseline emitted")
	}

	// Write, then expect a re-queried Update.
	if err := writeKV(path, "cfg", "watched", "v2"); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case u, open := <-ch:
			if !open {
				t.Fatal("watch channel closed before delivering the change")
			}
			if u.Err != nil {
				continue
			}
			if string(u.Value.Bytes) == "v2" {
				return
			}
		case <-deadline:
			t.Fatal("watch did not deliver the change within the timeout")
		}
	}
}

func TestWatchClosesOnCancel(t *testing.T) {
	path := newKVTable(t, "cfg")
	_ = writeKV(path, "cfg", "k", "v")
	p := New(WithPath(path), WithDebounce(40*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Watch(ctx, mustRef(t, "sqlite://cfg/k"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("watch channel not closed after context cancellation")
		}
	}
}

func TestWatchRequiresPath(t *testing.T) {
	t.Setenv("SQLITE_PATH", "")
	p := New(WithDSN("file::memory:")) // DSN only, no file path to watch
	if _, err := p.Watch(context.Background(), mustRef(t, "sqlite://cfg/k")); err == nil {
		t.Fatal("Watch without a file path returned nil error")
	}
}
