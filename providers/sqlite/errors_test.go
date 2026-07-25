package sqlite

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"unsafe"

	"github.com/xavidop/mamori"
	msqlite "modernc.org/sqlite"
)

// sqliteErrorWithCode constructs a genuine *msqlite.Error carrying code, via
// reflection into its unexported fields.
//
// modernc.org/sqlite does not export a constructor for its own Error type,
// and three of the six SQLite result codes classifySqlite maps cannot be
// provoked through the driver's public API within a fast, portable unit
// test:
//
//   - SQLITE_AUTH (23) requires an sqlite3_set_authorizer callback, which
//     modernc.org/sqlite does not expose anywhere in its public API (there is
//     no Authorizer / SetAuthorizer symbol in the module at all).
//   - SQLITE_PERM (3) is produced by this driver's POSIX errno translator
//     only for a *lock* operation that fails with EPERM specifically
//     (verified in modernc.org/sqlite/lib's generated
//     _sqliteErrorFromPosixError); an ordinary permission-denied file open
//     (EACCES) instead falls through to SQLITE_CANTOPEN in exploratory
//     testing, and provoking a lock syscall to fail with EPERM rather than
//     EACCES/EAGAIN is not something a portable test can force.
//   - SQLITE_LOCKED (6)'s classic same-connection trigger (a pending read
//     cursor blocking a write statement on the same connection handle) did
//     not reproduce against this driver/SQLite build in exploratory testing.
//
// This still exercises the real type and its real Code() method - the two
// things classifySqlite actually depends on via errors.As - it only bypasses
// the type's private constructor.
func sqliteErrorWithCode(t *testing.T, code int) *msqlite.Error {
	t.Helper()
	e := &msqlite.Error{}
	v := reflect.ValueOf(e).Elem()
	f := v.FieldByName("code")
	if !f.IsValid() {
		t.Fatal("modernc.org/sqlite.Error no longer has a field named \"code\"; update sqliteErrorWithCode")
	}
	reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().SetInt(int64(code))
	return e
}

// triggerCantopen provokes a real SQLITE_CANTOPEN (14) from modernc.org/sqlite
// by pointing the driver at a directory that does not exist.
func triggerCantopen(t *testing.T) error {
	t.Helper()
	db, err := sql.Open("sqlite", "file:/nonexistent-dir-mamori-test/x.db")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec("CREATE TABLE t(a)")
	if err == nil {
		t.Fatal("expected an error creating a table in a nonexistent directory")
	}
	return err
}

// triggerReadonly provokes a real SQLITE_READONLY (8) by opening an existing
// database read-only, then attempting a write.
func triggerReadonly(t *testing.T) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ro.db")
	setup, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("setup open: %v", err)
	}
	if _, err := setup.Exec("CREATE TABLE t(a)"); err != nil {
		t.Fatalf("setup create: %v", err)
	}
	if err := setup.Close(); err != nil {
		t.Fatalf("setup close: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("readonly open: %v", err)
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec("INSERT INTO t(a) VALUES(1)")
	if err == nil {
		t.Fatal("expected an error writing to a read-only-opened database")
	}
	return err
}

// triggerBusy provokes a real SQLITE_BUSY (5): one connection holds an open,
// uncommitted write transaction while a second connection, configured with a
// near-zero busy timeout, tries to write concurrently.
func triggerBusy(t *testing.T) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "busy.db")
	dbA, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("setup open: %v", err)
	}
	defer func() { _ = dbA.Close() }()
	if _, err := dbA.Exec("CREATE TABLE t(a)"); err != nil {
		t.Fatalf("setup create: %v", err)
	}
	tx, err := dbA.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("INSERT INTO t(a) VALUES(1)"); err != nil {
		t.Fatalf("tx insert: %v", err)
	}

	dbB, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatalf("setup open B: %v", err)
	}
	defer func() { _ = dbB.Close() }()
	_, err = dbB.Exec("INSERT INTO t(a) VALUES(2)")
	if err == nil {
		t.Fatal("expected a busy error from the concurrent writer")
	}
	return err
}

// TestClassifySqlite is the table test proving classifySqlite's mapping
// against real *msqlite.Error values: three (Cantopen, Readonly, Busy) are
// genuinely provoked through the real driver; the other three (Perm, Locked,
// Auth) use sqliteErrorWithCode because - as documented on that helper - the
// driver's public API cannot be made to emit those specific codes portably.
// Every case still exercises classifySqlite against the real *msqlite.Error
// type and its real Code() method.
func TestClassifySqlite(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"Perm", sqliteErrorWithCode(t, 3), mamori.KindPermissionDenied},
		{"Readonly", triggerReadonly(t), mamori.KindPermissionDenied},
		{"Busy", triggerBusy(t), mamori.KindUnavailable},
		{"Locked", sqliteErrorWithCode(t, 6), mamori.KindUnavailable},
		{"Cantopen", triggerCantopen(t), mamori.KindUnavailable},
		{"Auth", sqliteErrorWithCode(t, 23), mamori.KindUnauthenticated},
		{"UnmappedCode", sqliteErrorWithCode(t, 999), mamori.KindUnknown},
		{"PlainError", errors.New("boom"), mamori.KindUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySqlite(tc.err)
			if kind := mamori.ErrorKind(got); kind != tc.want {
				t.Fatalf("classifySqlite(%v) kind = %q, want %q", tc.err, kind, tc.want)
			}
		})
	}

	if classifySqlite(nil) != nil {
		t.Fatal("classifySqlite(nil) must return nil")
	}
}

// TestClassifySqlitePreservesUnderlyingError proves a classified error still
// satisfies errors.As back to the original *msqlite.Error with Code()
// recoverable, so a caller that wants the raw driver error (not just the
// mamori Kind) can still reach it.
func TestClassifySqlitePreservesUnderlyingError(t *testing.T) {
	orig := sqliteErrorWithCode(t, 5) // SQLITE_BUSY
	got := classifySqlite(orig)

	if !errors.Is(got, mamori.ErrUnavailable) {
		t.Fatalf("classifySqlite(orig) = %v, does not satisfy errors.Is(mamori.ErrUnavailable)", got)
	}
	var se *msqlite.Error
	if !errors.As(got, &se) {
		t.Fatal("classified error lost errors.As access to the underlying *msqlite.Error")
	}
	if se.Code() != 5 {
		t.Fatalf("recovered Code() = %d, want 5", se.Code())
	}
}

// TestClassifySqliteUnmappedPreservesError proves an unmapped code is
// returned completely unwrapped (not just unclassified) - classifySqlite's
// contract for a code it does not recognize.
func TestClassifySqliteUnmappedPreservesError(t *testing.T) {
	orig := sqliteErrorWithCode(t, 999)
	got := classifySqlite(orig)
	if got != error(orig) {
		t.Fatalf("classifySqlite(unmapped) = %v (%T), want the original error unchanged", got, got)
	}
}
