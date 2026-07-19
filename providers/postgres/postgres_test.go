package postgres

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeBackend is an in-memory implementation of the backend interface. It models
// just enough of PostgreSQL for the conformance and watch suites:
//
//   - a keyed row store with a per-write monotonic version (so a WithVersionColumn
//     provider sees a changing version), and
//   - a LISTEN/NOTIFY analogue: every write closes-and-replaces a broadcast
//     channel so any blocked Wait (the provider's watch loop) wakes, exactly as a
//     NOTIFY wakes a real WaitForNotification.
//
// The fake does not parse SQL; it looks the row up by the bound $1 key argument
// and records the last SQL string so tests can assert on the generated query.
type fakeBackend struct {
	mu       sync.Mutex
	rows     map[string]fakeRowData
	verSeq   uint64
	lastSQL  string
	notifyCh chan struct{} // closed+replaced on each write to wake blocked Waits
}

type fakeRowData struct {
	value   []byte
	version string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		rows:     map[string]fakeRowData{},
		notifyCh: make(chan struct{}),
	}
}

// set writes val for key, bumping the version, then fires the NOTIFY analogue.
func (f *fakeBackend) set(key, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verSeq++
	f.rows[key] = fakeRowData{value: []byte(val), version: strconv.FormatUint(f.verSeq, 10)}
	close(f.notifyCh)
	f.notifyCh = make(chan struct{})
}

func (f *fakeBackend) QueryRow(ctx context.Context, sql string, args ...any) row {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSQL = sql
	if err := ctx.Err(); err != nil {
		return errRow{err: err}
	}
	var key string
	if len(args) > 0 {
		key, _ = args[0].(string)
	}
	data, ok := f.rows[key]
	if !ok {
		return &fakeRow{found: false}
	}
	return &fakeRow{found: true, value: append([]byte(nil), data.value...), version: data.version}
}

func (f *fakeBackend) Listen(_ context.Context, _ string) (notifier, error) {
	return &fakeNotifier{be: f}, nil
}

// fakeRow implements the row (pgx.Row-shaped) contract. A one-column select
// scans the value; a two-column select (WithVersionColumn) scans value+version.
type fakeRow struct {
	found   bool
	value   []byte
	version string
}

func (r *fakeRow) Scan(dest ...any) error {
	if !r.found {
		return pgx.ErrNoRows
	}
	if len(dest) >= 1 {
		if bp, ok := dest[0].(*[]byte); ok {
			*bp = append([]byte(nil), r.value...)
		}
	}
	if len(dest) >= 2 {
		if sp, ok := dest[1].(*string); ok {
			*sp = r.version
		}
	}
	return nil
}

// errRow defers a fixed error to Scan, matching pgx's deferred-error behavior.
type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

// fakeNotifier blocks until the next write to the backend (the NOTIFY analogue)
// or ctx cancellation.
type fakeNotifier struct {
	be     *fakeBackend
	closed bool
}

func (n *fakeNotifier) Wait(ctx context.Context) error {
	n.be.mu.Lock()
	wake := n.be.notifyCh
	n.be.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wake:
		return nil
	}
}

func (n *fakeNotifier) Close() { n.closed = true }

// compile-time checks that the fakes satisfy the provider interfaces.
var (
	_ backend  = (*fakeBackend)(nil)
	_ notifier = (*fakeNotifier)(nil)
	_ row      = (*fakeRow)(nil)
)

func mustRef(t *testing.T, s string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return ref
}

// TestConformance runs the full mamori provider conformance kit against the
// in-memory fake, including the native LISTEN/NOTIFY watch path.
func TestConformance(t *testing.T) {
	fake := newFakeBackend()
	providertest.Run(t, providertest.Config{
		New:  func() mamori.Provider { return New(withBackend(fake)) },
		Ref:  func(key string) string { return "postgres://app_config/" + key },
		Seed: func(_ context.Context, key, val string) error { fake.set(key, val); return nil },
		Mutate: func(_ context.Context, key, val string) error {
			fake.set(key, val)
			return nil
		},
		EventuallyTimeout: 3 * time.Second,
	})
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != scheme {
		t.Fatalf("Scheme() = %q, want %q", got, scheme)
	}
}

func TestResolveValueAndVersion(t *testing.T) {
	fake := newFakeBackend()
	fake.set("log_level", "info")
	p := New(withBackend(fake))

	v, err := p.Resolve(context.Background(), mustRef(t, "postgres://app_config/log_level"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "info" {
		t.Fatalf("Bytes = %q, want info", v.Bytes)
	}
	if v.Version == "" {
		t.Fatal("Version must not be empty (VersionHash by default)")
	}
	if v.Sensitive {
		t.Fatal("values must not be Sensitive by default")
	}

	// A rewrite with a different value must change the (hash) version.
	fake.set("log_level", "debug")
	v2, err := p.Resolve(context.Background(), mustRef(t, "postgres://app_config/log_level"))
	if err != nil {
		t.Fatalf("Resolve after mutate: %v", err)
	}
	if v2.Version == v.Version {
		t.Fatalf("Version did not change after value change (both %q)", v.Version)
	}
	if string(v2.Bytes) != "debug" {
		t.Fatalf("Bytes = %q, want debug", v2.Bytes)
	}
}

func TestResolveJSONKey(t *testing.T) {
	fake := newFakeBackend()
	fake.set("db", `{"host":"db.internal","port":5432,"password":"s3cr3t"}`)
	p := New(withBackend(fake))

	get := func(key string) string {
		t.Helper()
		v, err := p.Resolve(context.Background(), mustRef(t, "postgres://app_config/db#"+key))
		if err != nil {
			t.Fatalf("Resolve #%s: %v", key, err)
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

	_, err := p.Resolve(context.Background(), mustRef(t, "postgres://app_config/db#nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing json key err = %v, want ErrNotFound", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(withBackend(newFakeBackend()))
	_, err := p.Resolve(context.Background(), mustRef(t, "postgres://app_config/absent"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveContextCancelled(t *testing.T) {
	fake := newFakeBackend()
	fake.set("k", "v")
	p := New(withBackend(fake))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustRef(t, "postgres://app_config/k")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestCustomColumnsViaOpts(t *testing.T) {
	fake := newFakeBackend()
	fake.set("feature_x", "on")
	p := New(withBackend(fake))

	v, err := p.Resolve(context.Background(),
		mustRef(t, "postgres://settings/feature_x?key_col=name&val_col=data"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "on" {
		t.Fatalf("Bytes = %q, want on", v.Bytes)
	}
	// The generated SQL must reflect the overridden identifiers and bind the key
	// positionally, never interpolate it.
	want := "SELECT data FROM settings WHERE name = $1"
	if fake.lastSQL != want {
		t.Fatalf("lastSQL = %q, want %q", fake.lastSQL, want)
	}
}

func TestSchemaQualifiedTable(t *testing.T) {
	fake := newFakeBackend()
	fake.set("k", "v")
	p := New(withBackend(fake))
	if _, err := p.Resolve(context.Background(), mustRef(t, "postgres://cfg.app_config/k")); err != nil {
		t.Fatalf("Resolve schema-qualified table: %v", err)
	}
	want := "SELECT value FROM cfg.app_config WHERE key = $1"
	if fake.lastSQL != want {
		t.Fatalf("lastSQL = %q, want %q", fake.lastSQL, want)
	}
}

func TestVersionColumn(t *testing.T) {
	fake := newFakeBackend()
	fake.set("k", "v")
	p := New(withBackend(fake), WithVersionColumn("updated_at"))

	v, err := p.Resolve(context.Background(), mustRef(t, "postgres://app_config/k"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// With a version column the SQL selects it, cast to text.
	want := "SELECT value, updated_at::text FROM app_config WHERE key = $1"
	if fake.lastSQL != want {
		t.Fatalf("lastSQL = %q, want %q", fake.lastSQL, want)
	}
	if v.Version != "1" {
		t.Fatalf("Version = %q, want 1 (from version column)", v.Version)
	}
	// A rewrite bumps the fake's version column.
	fake.set("k", "v") // same value, but version column advances
	v2, err := p.Resolve(context.Background(), mustRef(t, "postgres://app_config/k"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v2.Version == v.Version {
		t.Fatalf("version column did not advance (both %q)", v.Version)
	}
}

func TestSensitiveOption(t *testing.T) {
	fake := newFakeBackend()
	fake.set("api_key", "sk-123")
	p := New(withBackend(fake), WithSensitive(true))
	v, err := p.Resolve(context.Background(), mustRef(t, "postgres://secrets/api_key"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Fatal("WithSensitive(true) must mark the value Sensitive")
	}
}

// TestRejectsMaliciousIdentifiers is the SQL-injection guardrail test: any
// table, key, or value column name that is not a plain identifier must be
// rejected before a query runs, and the rejection must be a validation error,
// never ErrNotFound (which would let an attacker probe by masquerading as a
// missing key).
func TestRejectsMaliciousIdentifiers(t *testing.T) {
	fake := newFakeBackend()
	p := New(withBackend(fake))

	cases := []struct {
		name string
		ref  string
	}{
		{"table with statement", "postgres://users;DROP TABLE users/k"},
		{"table with comment", "postgres://config'--/k"},
		{"table with space", "postgres://config table/k"},
		{"table with paren", "postgres://config)/k"},
		{"double schema dot", "postgres://a.b.c/k"},
		{"val_col injection", "postgres://config/k?val_col=value)--"},
		{"key_col injection", "postgres://config/k?key_col=id OR 1=1"},
		{"val_col quote", `postgres://config/k?val_col=v"x`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := mustRef(t, tc.ref)
			_, err := p.Resolve(context.Background(), ref)
			if err == nil {
				t.Fatalf("Resolve(%q) succeeded; a malicious identifier must be rejected", tc.ref)
			}
			if !errors.Is(err, errUnsafeIdentifier) {
				t.Fatalf("Resolve(%q) err = %v, want errUnsafeIdentifier", tc.ref, err)
			}
			if errors.Is(err, mamori.ErrNotFound) {
				t.Fatalf("Resolve(%q) returned ErrNotFound; injection must not look like a missing key", tc.ref)
			}
		})
	}
	// The malicious refs must never have reached the backend.
	if fake.lastSQL != "" {
		t.Fatalf("a rejected ref produced SQL %q; validation must run before any query", fake.lastSQL)
	}
}

func TestValidIdentAndTable(t *testing.T) {
	okIdent := []string{"key", "value", "_x", "a1", "Camel_Case", "col123"}
	for _, s := range okIdent {
		if !validIdent(s) {
			t.Errorf("validIdent(%q) = false, want true", s)
		}
	}
	badIdent := []string{"", "1abc", "a b", "a-b", "a.b", "a;b", `a"b`, "a)", "*", "a\n"}
	for _, s := range badIdent {
		if validIdent(s) {
			t.Errorf("validIdent(%q) = true, want false", s)
		}
	}
	okTable := []string{"t", "schema.table", "public.app_config", "_s._t"}
	for _, s := range okTable {
		if !validTable(s) {
			t.Errorf("validTable(%q) = false, want true", s)
		}
	}
	badTable := []string{"", "a.b.c", ".t", "t.", "a.b.", "sch ema.t", "t;DROP", "a.b;c"}
	for _, s := range badTable {
		if validTable(s) {
			t.Errorf("validTable(%q) = true, want false", s)
		}
	}
}

func TestMalformedPath(t *testing.T) {
	p := New(withBackend(newFakeBackend()))
	// Missing key part: postgres://table with no /key.
	_, err := p.Resolve(context.Background(), mustRef(t, "postgres://app_config"))
	if err == nil {
		t.Fatal("ref without a /<key> must be rejected")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("malformed ref must not be reported as ErrNotFound")
	}
}

// --- Watch unit tests (dedicated coverage of the LISTEN/NOTIFY path) ---

func TestWatchEmitsBaselineAndChange(t *testing.T) {
	fake := newFakeBackend()
	fake.set("watched", "v1")
	p := New(withBackend(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "postgres://app_config/watched"))
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
	case <-time.After(2 * time.Second):
		t.Fatal("no baseline emitted")
	}

	// A write fires the NOTIFY analogue; the watch must re-query and emit.
	fake.set("watched", "v2")
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("change err: %v", u.Err)
		}
		if string(u.Value.Bytes) != "v2" {
			t.Fatalf("change = %q, want v2", u.Value.Bytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("NOTIFY-driven change not delivered")
	}
}

func TestWatchClosesOnCancel(t *testing.T) {
	fake := newFakeBackend()
	fake.set("k", "v")
	p := New(withBackend(fake))

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Watch(ctx, mustRef(t, "postgres://app_config/k"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return // closed as required
			}
		case <-deadline:
			t.Fatal("channel not closed after cancel")
		}
	}
}

func TestWatchRejectsMaliciousChannel(t *testing.T) {
	fake := newFakeBackend()
	p := New(withBackend(fake), WithChannel("evil; DROP TABLE users"))
	_, err := p.Watch(context.Background(), mustRef(t, "postgres://app_config/k"))
	if err == nil {
		t.Fatal("Watch with an unsafe channel must be rejected")
	}
	if !errors.Is(err, errUnsafeIdentifier) {
		t.Fatalf("err = %v, want errUnsafeIdentifier", err)
	}
}
