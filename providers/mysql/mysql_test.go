package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeDB is an in-memory stand-in for the query surface the provider needs. It
// backs both the unit tests and the providertest conformance suite, so the
// exact same Seed/Mutate semantics are exercised everywhere without a real
// database (and without the background goroutine sql.Open would start, which the
// conformance goleak check forbids).
type fakeDB struct {
	mu    sync.Mutex
	rows  map[string]fakeRowData
	rev   map[string]int
	calls []fakeCall
}

type fakeRowData struct {
	value   string
	version string // a native revision, used only when WithVersionColumn is set
}

type fakeCall struct {
	query string
	args  []any
}

func newFakeDB() *fakeDB {
	return &fakeDB{rows: map[string]fakeRowData{}, rev: map[string]int{}}
}

// set upserts a row, bumping a synthetic native revision so the version column
// changes on every write (mirroring an updated_at / rev column).
func (f *fakeDB) set(key, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rev[key]++
	f.rows[key] = fakeRowData{value: val, version: "rev-" + strconv.Itoa(f.rev[key])}
}

func (f *fakeDB) lastCall() (fakeCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return fakeCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// QueryRowContext implements the provider's queryer interface.
func (f *fakeDB) QueryRowContext(ctx context.Context, query string, args ...any) rowScanner {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{query: query, args: args})
	var key string
	if len(args) > 0 {
		key, _ = args[0].(string)
	}
	data, ok := f.rows[key]
	f.mu.Unlock()
	return &fakeRow{ctxErr: ctx.Err(), data: data, found: ok}
}

// fakeRow implements rowScanner. It honors context cancellation the way a real
// *sql.Row does and returns sql.ErrNoRows for a missing key.
type fakeRow struct {
	ctxErr error
	data   fakeRowData
	found  bool
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.ctxErr != nil {
		return r.ctxErr
	}
	if !r.found {
		return sql.ErrNoRows
	}
	if len(dest) >= 1 {
		if err := assignScan(dest[0], r.data.value); err != nil {
			return err
		}
	}
	if len(dest) >= 2 {
		if err := assignScan(dest[1], r.data.version); err != nil {
			return err
		}
	}
	return nil
}

func assignScan(dest any, s string) error {
	switch d := dest.(type) {
	case *[]byte:
		*d = []byte(s)
	case *string:
		*d = s
	default:
		return fmt.Errorf("fakeRow: unsupported scan dest %T", dest)
	}
	return nil
}

func mustRef(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

// --- Unit tests (in-memory fake) ---

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != scheme {
		t.Fatalf("Scheme() = %q, want %q", got, scheme)
	}
}

func TestResolveSuccess(t *testing.T) {
	f := newFakeDB()
	f.set("log_level", "debug")

	p := New(withQueryer(f))
	v, err := p.Resolve(context.Background(), mustRef(t, "mysql://config/log_level"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("Bytes = %q, want debug", v.Bytes)
	}
	if v.Version != mamori.VersionHash([]byte("debug")) {
		t.Errorf("Version = %q, want VersionHash of value", v.Version)
	}
	if v.Sensitive {
		t.Error("Sensitive = true, want false by default")
	}
	if v.Metadata["table"] != "config" || v.Metadata["key"] != "log_level" {
		t.Errorf("Metadata = %v, want table=config key=log_level", v.Metadata)
	}
}

// TestResolveParameterizedKey proves the key is bound as a placeholder argument
// and never interpolated into the SQL text.
func TestResolveParameterizedKey(t *testing.T) {
	f := newFakeDB()
	f.set("api", "value")

	p := New(withQueryer(f))
	if _, err := p.Resolve(context.Background(), mustRef(t, "mysql://config/api")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	call, ok := f.lastCall()
	if !ok {
		t.Fatal("provider issued no query")
	}
	if want := "SELECT `value` FROM `config` WHERE `key` = ?"; call.query != want {
		t.Fatalf("query = %q, want %q", call.query, want)
	}
	if len(call.args) != 1 || call.args[0] != "api" {
		t.Fatalf("args = %v, want [\"api\"] bound as a placeholder", call.args)
	}
}

func TestResolveCustomColumns(t *testing.T) {
	f := newFakeDB()
	f.set("feature_x", "on")

	p := New(withQueryer(f))
	_, err := p.Resolve(context.Background(), mustRef(t, "mysql://settings/feature_x?key_col=name&val_col=data"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	call, _ := f.lastCall()
	if want := "SELECT `data` FROM `settings` WHERE `name` = ?"; call.query != want {
		t.Fatalf("query = %q, want %q", call.query, want)
	}
}

func TestResolveNotFound(t *testing.T) {
	f := newFakeDB()
	p := New(withQueryer(f))
	_, err := p.Resolve(context.Background(), mustRef(t, "mysql://config/missing"))
	if err == nil {
		t.Fatal("Resolve of missing key returned nil error")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

func TestResolveJSONKey(t *testing.T) {
	f := newFakeDB()
	f.set("db", `{"host":"db.internal","port":5432}`)

	p := New(withQueryer(f))
	v, err := p.Resolve(context.Background(), mustRef(t, "mysql://config/db#host"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "db.internal" {
		t.Fatalf("Bytes = %q, want db.internal", v.Bytes)
	}
	// Version reflects the whole row, not the selected field.
	if v.Version != mamori.VersionHash([]byte(`{"host":"db.internal","port":5432}`)) {
		t.Errorf("Version = %q, want hash of the whole row value", v.Version)
	}
}

func TestResolveJSONKeyMissingField(t *testing.T) {
	f := newFakeDB()
	f.set("db", `{"host":"db.internal"}`)

	p := New(withQueryer(f))
	_, err := p.Resolve(context.Background(), mustRef(t, "mysql://config/db#password"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing JSON field error = %v, want ErrNotFound", err)
	}
}

func TestVersionColumn(t *testing.T) {
	f := newFakeDB()
	f.set("token", "one") // rev-1

	p := New(withQueryer(f), WithVersionColumn("updated_at"))
	v1, err := p.Resolve(context.Background(), mustRef(t, "mysql://config/token"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	call, _ := f.lastCall()
	if want := "SELECT `value`, `updated_at` FROM `config` WHERE `key` = ?"; call.query != want {
		t.Fatalf("query = %q, want %q", call.query, want)
	}
	if v1.Version != "rev-1" {
		t.Fatalf("Version = %q, want rev-1 from the version column", v1.Version)
	}

	f.set("token", "two") // rev-2
	v2, err := p.Resolve(context.Background(), mustRef(t, "mysql://config/token"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v2.Version != "rev-2" {
		t.Fatalf("Version = %q, want rev-2 after mutate", v2.Version)
	}
	if v1.Version == v2.Version {
		t.Fatal("version column did not change after mutate")
	}
}

func TestSensitive(t *testing.T) {
	f := newFakeDB()
	f.set("secret", "s3cr3t")

	p := New(withQueryer(f), WithSensitive(true))
	v, err := p.Resolve(context.Background(), mustRef(t, "mysql://config/secret"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Error("Sensitive = false, want true with WithSensitive(true)")
	}
}

func TestBadPath(t *testing.T) {
	f := newFakeDB()
	p := New(withQueryer(f))
	// Only a table, no key segment.
	_, err := p.Resolve(context.Background(), mustRef(t, "mysql://onlytable"))
	if err == nil {
		t.Fatal("Resolve with no key segment returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("malformed-path error should not be ErrNotFound")
	}
	if _, ok := f.lastCall(); ok {
		t.Fatal("provider issued a query for a malformed ref")
	}
}

// TestIdentifierAllowlistRejects is the SQL-injection guard: a malicious table
// or column name must be refused before any query is built, with an error that
// is NOT ErrNotFound (so it surfaces to the caller rather than being masked by a
// default).
func TestIdentifierAllowlistRejects(t *testing.T) {
	cases := []struct {
		name string
		ref  mamori.Ref
	}{
		{
			name: "malicious table",
			ref:  mustRef(t, "mysql://users; DROP TABLE users/k"),
		},
		{
			name: "malicious val_col",
			ref: mamori.Ref{
				Scheme: scheme,
				Path:   "config/k",
				Opts:   url.Values{"val_col": {"value FROM users UNION SELECT password"}},
				Raw:    "mysql://config/k?val_col=injection",
			},
		},
		{
			name: "malicious key_col",
			ref: mamori.Ref{
				Scheme: scheme,
				Path:   "config/k",
				Opts:   url.Values{"key_col": {"1=1 OR `key`"}},
				Raw:    "mysql://config/k?key_col=injection",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDB()
			f.set("k", "v")
			p := New(withQueryer(f))
			_, err := p.Resolve(context.Background(), tc.ref)
			if err == nil {
				t.Fatal("malicious identifier was accepted")
			}
			if errors.Is(err, mamori.ErrNotFound) {
				t.Fatalf("injection error should not be ErrNotFound, got %v", err)
			}
			if !strings.Contains(err.Error(), "invalid") {
				t.Fatalf("error %v does not report an invalid identifier", err)
			}
			if _, ok := f.lastCall(); ok {
				t.Fatal("a query was issued for a rejected identifier")
			}
		})
	}
}

func TestQuoteIdentAllowsReservedWord(t *testing.T) {
	// "key" is a MySQL reserved word but a valid identifier; backtick-quoting
	// makes it usable, which is why the default key column works.
	got, err := quoteIdent("key_col", "key")
	if err != nil {
		t.Fatalf("quoteIdent(key): %v", err)
	}
	if got != "`key`" {
		t.Fatalf("quoteIdent(key) = %q, want `key`", got)
	}
}

func TestNoDSNConfigured(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("MYSQL_DSN", "")
	p := New() // nothing injected, no DSN available
	_, err := p.Resolve(context.Background(), mustRef(t, "mysql://config/k"))
	if err == nil {
		t.Fatal("Resolve with no DSN returned nil error")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("no-DSN error should not be ErrNotFound")
	}
}

func TestInvalidDSN(t *testing.T) {
	// A malformed DSN is rejected by ParseDSN before any pool/goroutine is
	// created (so this test also leaves no background sql.DB opener running).
	p := New(WithDSN("this is not a valid dsn"))
	_, err := p.Resolve(context.Background(), mustRef(t, "mysql://config/k"))
	if err == nil {
		t.Fatal("Resolve with an invalid DSN returned nil error")
	}
	if !strings.Contains(err.Error(), "invalid DSN") {
		t.Fatalf("error %v does not report an invalid DSN", err)
	}
}

func TestDSNFromEnv(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("MYSQL_DSN", "u:p@tcp(127.0.0.1:3306)/db")
	p := New()
	if got := p.resolveDSN(); got != "u:p@tcp(127.0.0.1:3306)/db" {
		t.Fatalf("resolveDSN() = %q, want the MYSQL_DSN value", got)
	}
	// DATABASE_URL takes precedence over MYSQL_DSN.
	t.Setenv("DATABASE_URL", "u:p@tcp(db:3306)/primary")
	if got := p.resolveDSN(); got != "u:p@tcp(db:3306)/primary" {
		t.Fatalf("resolveDSN() = %q, want DATABASE_URL to win", got)
	}
}

// The MySQL provider intentionally does NOT implement WatchableProvider (MySQL
// has no native change notification); mamori polls it instead.
func TestNotWatchable(t *testing.T) {
	var p mamori.Provider = New()
	if _, ok := p.(mamori.WatchableProvider); ok {
		t.Fatal("mysql provider must not implement WatchableProvider (no native watch)")
	}
}

// --- Conformance ---

func TestConformance(t *testing.T) {
	f := newFakeDB()

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return New(withQueryer(f)) },
		Ref: func(key string) string { return "mysql://kv/" + key },
		Seed: func(_ context.Context, key, val string) error {
			f.set(key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			f.set(key, val)
			return nil
		},
		SkipWatch: true, // MySQL has no native change notification.
	})
}
