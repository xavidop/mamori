// Package sqlite implements a mamori provider that resolves configuration
// values from a SQLite database table, using the pure-Go modernc.org/sqlite
// driver (no cgo).
//
// The scheme is "sqlite" and the ref grammar is:
//
//	sqlite://<table>/<key>[#json-field][?key_col=<c>&val_col=<c>]
//
// A ref names a table and a key. The provider runs
//
//	SELECT <val_col> FROM <table> WHERE <key_col> = ?
//
// with the key value bound as a query parameter, so the key can never be an
// injection vector. The <table>, <key_col>, and <val_col> identifiers are NOT
// parameterizable in SQL, so each is validated against a strict allowlist
// (^[A-Za-z_][A-Za-z0-9_]*$) and rejected otherwise - this is the injection
// guard for the parts of the query that must be string-interpolated.
//
//	DBPassword string `source:"sqlite://config/db_password"`
//	LogLevel   string `source:"sqlite://config/log_level"`
//	Host       string `source:"sqlite://config/db#host"` // db value is a JSON object
//
// The key/value column names default to "key" and "value" and can be overridden
// per-ref with the key_col / val_col query options. A #json-field fragment
// selects a single field from a JSON object value via mamori.SelectKey,
// identically to every other mamori provider.
//
// The database FILE path is provider configuration, not part of the ref: supply
// it with WithPath (or a full driver DSN with WithDSN), defaulting to the
// SQLITE_PATH environment variable. This keeps refs portable across
// environments that store the same table in different files.
//
// # Value semantics
//
//   - Value.Bytes is the raw column value (after optional #json-field selection).
//   - Value.Version is mamori.VersionHash of the raw column value, giving cheap
//     change detection without a native revision. If the table carries its own
//     revision column, point the provider at it with WithVersionColumn and that
//     column's value is used verbatim instead.
//   - Value.Sensitive is false by default (SQLite commonly holds configuration,
//     not managed secrets). Set WithSensitive(true) to mark every resolved value
//     sensitive so it is redacted downstream.
//
// # Watching
//
// The provider implements mamori.WatchableProvider by watching the database
// file on disk with fsnotify (the same mechanism as the built-in file://
// provider). On a write to the database file it re-queries the ref and emits an
// Update. Rapid bursts of filesystem events (SQLite touches the main file plus
// its rollback journal on each commit) are coalesced with a short debounce.
//
// The provider opens the database in the default rollback-journal mode so that
// committed writes modify the main database file in place, which is what
// fsnotify observes. WAL mode would route writes to a side file and defer main
// file changes until a checkpoint, defeating the file watch; avoid WAL if you
// rely on the native watch.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/xavidop/mamori"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

// scheme is the URL scheme this provider handles.
const scheme = "sqlite"

const (
	// defaultKeyCol / defaultValCol are the column names used when a ref does not
	// override them with ?key_col= / ?val_col=.
	defaultKeyCol = "key"
	defaultValCol = "value"

	// defaultDebounce coalesces the burst of filesystem events SQLite emits per
	// commit (main file + rollback journal) into a single re-query.
	defaultDebounce = 150 * time.Millisecond

	// busyTimeout is applied to every connection so a read that races an external
	// writer holding the file lock waits briefly instead of failing immediately.
	busyTimeoutMS = 5000
)

// identRe is the SQL identifier allowlist. Table and column names must match it;
// anything else (spaces, quotes, semicolons, comment markers, ...) is rejected
// before being interpolated into a query, closing the injection surface that the
// non-parameterizable parts of the statement would otherwise open.
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Provider resolves sqlite:// refs against a single SQLite database file. It is
// safe for concurrent use. A short-lived *sql.DB is opened per resolve and
// closed again, so the provider holds no background goroutines or open file
// handles between calls - important for goroutine hygiene under the conformance
// kit and cheap enough for configuration workloads.
type Provider struct {
	path       string        // database file path (from WithPath / SQLITE_PATH)
	dataSource string        // full driver DSN (from WithDSN); overrides path
	versionCol string        // optional revision column (from WithVersionColumn)
	sensitive  bool          // mark resolved values Sensitive (from WithSensitive)
	debounce   time.Duration // watch debounce window
}

// Option configures a Provider.
type Option func(*Provider)

// WithPath sets the SQLite database file path. When unset the provider falls
// back to the SQLITE_PATH environment variable.
func WithPath(path string) Option {
	return func(p *Provider) { p.path = path }
}

// WithDSN sets a full modernc.org/sqlite DSN (e.g.
// "file:/var/lib/app.db?_pragma=busy_timeout(5000)"), bypassing the path->DSN
// construction. Use it for read-only opens, custom pragmas, or shared-cache
// setups. Note: with a DSN the provider cannot infer a file path to watch, so
// the native watch is unavailable unless WithPath is also supplied.
func WithDSN(dsn string) Option {
	return func(p *Provider) { p.dataSource = dsn }
}

// WithVersionColumn names a column whose value is used verbatim as Value.Version
// instead of hashing the resolved bytes. Point it at a monotonic revision /
// updated_at / rowversion column when the table maintains one. The column name
// is validated against the identifier allowlist like any other.
func WithVersionColumn(col string) Option {
	return func(p *Provider) { p.versionCol = col }
}

// WithSensitive marks every resolved value as sensitive (redacted downstream).
// Default is false.
func WithSensitive(sensitive bool) Option {
	return func(p *Provider) { p.sensitive = sensitive }
}

// WithDebounce overrides how long the watch coalesces filesystem events before
// re-querying (default 150ms).
func WithDebounce(d time.Duration) Option {
	return func(p *Provider) { p.debounce = d }
}

// New constructs a SQLite provider. The database is opened lazily on first
// Resolve/Watch, so New never fails and never touches the filesystem.
func New(opts ...Option) *Provider {
	p := &Provider{debounce: defaultDebounce}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// init registers a lazily-configured provider so `import _` wiring picks up the
// database path from SQLITE_PATH. Users who need explicit configuration call
// mamori.WithProvider(sqlite.New(sqlite.WithPath("..."))).
func init() { mamori.Register(New()) }

// Scheme returns "sqlite".
func (p *Provider) Scheme() string { return scheme }

// filePath returns the database file path, from WithPath or the SQLITE_PATH
// environment variable. It is empty when only a raw DSN was supplied.
func (p *Provider) filePath() string {
	if p.path != "" {
		return p.path
	}
	return os.Getenv("SQLITE_PATH")
}

// dsn returns the driver DSN used to open the database. A raw DSN (WithDSN) is
// used as-is; otherwise one is built from the file path with a busy timeout so
// reads tolerate a concurrent writer.
func (p *Provider) dsn() (string, error) {
	if p.dataSource != "" {
		return p.dataSource, nil
	}
	path := p.filePath()
	if path == "" {
		return "", errors.New("sqlite: no database configured; set SQLITE_PATH or use sqlite.WithPath/WithDSN")
	}
	return fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)", path, busyTimeoutMS), nil
}

// open returns a fresh *sql.DB. Callers must Close it. sql.Open itself performs
// no I/O; the first query opens the underlying connection.
func (p *Provider) open() (*sql.DB, error) {
	dsn, err := p.dsn()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(scheme, dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	return db, nil
}

// Resolve fetches the current value for ref. A missing row (sql.ErrNoRows) or a
// missing #json-field yields an error satisfying errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	db, err := p.open()
	if err != nil {
		return mamori.Value{}, err
	}
	defer func() { _ = db.Close() }()
	return p.query(ctx, db, ref)
}

// query runs the parameterized SELECT for ref against db and builds the Value.
func (p *Provider) query(ctx context.Context, db *sql.DB, ref mamori.Ref) (mamori.Value, error) {
	table, key, ok := strings.Cut(ref.Path, "/")
	if !ok || table == "" || key == "" {
		return mamori.Value{}, fmt.Errorf("sqlite: ref %q: path must be <table>/<key>", ref.Raw)
	}

	keyCol := ref.Opt("key_col")
	if keyCol == "" {
		keyCol = defaultKeyCol
	}
	valCol := ref.Opt("val_col")
	if valCol == "" {
		valCol = defaultValCol
	}

	// Guard every string-interpolated identifier. Key/column values in the WHERE
	// clause are parameterized (?), but the table and column *names* cannot be, so
	// they must pass the allowlist or the query is refused.
	for what, id := range map[string]string{"table": table, "key_col": keyCol, "val_col": valCol} {
		if !validIdent(id) {
			return mamori.Value{}, fmt.Errorf("sqlite: invalid %s identifier %q (must match %s)", what, id, identRe.String())
		}
	}
	if p.versionCol != "" && !validIdent(p.versionCol) {
		return mamori.Value{}, fmt.Errorf("sqlite: invalid version column identifier %q (must match %s)", p.versionCol, identRe.String())
	}

	cols := valCol
	if p.versionCol != "" {
		cols = valCol + ", " + p.versionCol
	}
	stmt := fmt.Sprintf("SELECT %s FROM %s WHERE %s = ? LIMIT 1", cols, table, keyCol)

	row := db.QueryRowContext(ctx, stmt, key)
	var rawVal any
	var rawVer any
	if p.versionCol != "" {
		err := row.Scan(&rawVal, &rawVer)
		if err != nil {
			return mamori.Value{}, p.scanErr(table, key, err)
		}
	} else {
		if err := row.Scan(&rawVal); err != nil {
			return mamori.Value{}, p.scanErr(table, key, err)
		}
	}

	valBytes := toBytes(rawVal)

	version := mamori.VersionHash(valBytes)
	if p.versionCol != "" {
		version = string(toBytes(rawVer))
	}

	out := valBytes
	if ref.Key != "" {
		sel, err := mamori.SelectKey(valBytes, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		out = sel
	}

	return mamori.Value{
		Bytes:     out,
		Version:   version,
		Sensitive: p.sensitive,
	}, nil
}

// scanErr maps a row-scan error to a typed not-found (for a missing row) or
// wraps it with context otherwise.
func (p *Provider) scanErr(table, key string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("sqlite: %s/%s: %w", table, key, mamori.ErrNotFound)
	}
	return fmt.Errorf("sqlite: query %s: %w", table, err)
}

// Watch implements mamori.WatchableProvider by watching the database file with
// fsnotify. It emits the current value as a baseline, then re-queries and emits
// on each (debounced) write to the file. The channel is closed and the watcher
// released when ctx is cancelled; the goroutine never leaks.
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	path := p.filePath()
	if path == "" {
		return nil, errors.New("sqlite: cannot watch without a file path; use sqlite.WithPath or set SQLITE_PATH")
	}
	target := filepath.Clean(path)
	dir := filepath.Dir(target)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	// Watch the parent directory (catches atomic replace via rename), and also the
	// file itself (catches in-place writes, which is how SQLite commits in the
	// default journal mode). The directory always exists; the file may not yet.
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return nil, err
	}
	_ = w.Add(target) // best effort; ignored if the file does not exist yet

	debounce := p.debounce
	if debounce <= 0 {
		debounce = defaultDebounce
	}

	ch := make(chan mamori.Update, 1)
	go func() {
		defer close(ch)
		defer func() { _ = w.Close() }()

		emit := func() {
			v, err := p.Resolve(ctx, ref)
			select {
			case ch <- mamori.Update{Value: v, Err: err}:
			case <-ctx.Done():
			}
		}
		emit() // baseline

		timer := time.NewTimer(debounce)
		if !timer.Stop() {
			<-timer.C
		}
		defer timer.Stop()
		armed := false

		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if !relevant(ev.Name, target) {
					continue
				}
				// (Re)arm the debounce so a burst of events collapses into one query.
				if armed && !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(debounce)
				armed = true
			case <-timer.C:
				armed = false
				emit()
			case werr, ok := <-w.Errors:
				if !ok {
					return
				}
				select {
				case ch <- mamori.Update{Err: werr}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}

// relevant reports whether a filesystem event names the database file or one of
// its SQLite sidecar files (-journal, -wal, -shm), any of which indicates the
// database changed.
func relevant(name, target string) bool {
	n := filepath.Clean(name)
	return n == target || strings.HasPrefix(n, target+"-")
}

// validIdent reports whether s is a safe SQL identifier per the allowlist.
func validIdent(s string) bool { return identRe.MatchString(s) }

// toBytes converts a scanned column value of any SQLite storage class into
// bytes. TEXT/BLOB pass through; INTEGER/REAL/BOOL/time are rendered to their
// canonical textual form; NULL becomes nil.
func toBytes(v any) []byte {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return append([]byte(nil), t...)
	case string:
		return []byte(t)
	case int64:
		return []byte(strconv.FormatInt(t, 10))
	case float64:
		return []byte(strconv.FormatFloat(t, 'g', -1, 64))
	case bool:
		if t {
			return []byte("1")
		}
		return []byte("0")
	case time.Time:
		return []byte(t.Format(time.RFC3339Nano))
	default:
		return []byte(fmt.Sprintf("%v", t))
	}
}
