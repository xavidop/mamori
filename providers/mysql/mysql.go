// Package mysql implements a mamori provider that resolves configuration values
// from a MySQL (or MariaDB) key/value table using the standard library
// database/sql package and the github.com/go-sql-driver/mysql driver.
//
// # Scheme
//
//	mysql://<table>/<key>[#json-field]
//
// The path names the table and the lookup key. Each resolve runs a single
// parameterized query:
//
//	SELECT `<val_col>` FROM `<table>` WHERE `<key_col>` = ?
//
// with the ref key bound as the placeholder argument. By default the key column
// is "key" and the value column is "value"; both are overridable per-ref with
// the query options key_col and val_col:
//
//	Feature string `source:"mysql://settings/feature_x?key_col=name&val_col=data"`
//
// An optional #json-field fragment selects a single field from a JSON object
// stored in the value column, via mamori.SelectKey (identical behavior across
// every mamori provider).
//
//	type Config struct {
//	    LogLevel string `source:"mysql://config/app"`          // whole value
//	    DBHost   string `source:"mysql://config/db#host"`      // field of a JSON value
//	}
//
// # Identifier safety
//
// A table or column name cannot be supplied as a bound parameter, so those
// identifiers are interpolated into the SQL text. To make that safe every
// identifier (table, key_col, val_col, and any version column) is validated
// against a strict allowlist (^[A-Za-z_][A-Za-z0-9_]*$) and rejected otherwise,
// then backtick-quoted. Anything containing whitespace, punctuation, or SQL
// metacharacters is refused before a query is ever built, which prevents SQL
// injection through the ref. The key itself is always bound as a "?" placeholder
// and is never interpolated.
//
// # Value semantics
//
// Value.Version is a content hash of the row's value column (mamori.VersionHash)
// by default, giving cheap change detection without a byte comparison. If the
// table carries a native revision column (a monotonically increasing version or
// an updated_at timestamp), point the provider at it with WithVersionColumn and
// that column's value is used as Value.Version instead.
//
// Value.Sensitive is false by default (a config table is not a secret manager).
// Set it with WithSensitive(true) to have resolved values redacted downstream,
// or wrap the destination field in secret.String.
//
// # Authentication
//
// The database is reached through a go-sql-driver/mysql DSN supplied via
// WithDSN, or, when unset, read lazily at first resolve from DATABASE_URL and
// then MYSQL_DSN. The connection (and its *sql.DB) is opened lazily on the first
// resolve, so registering the provider from init never contacts a database.
// Tests inject a fake with WithDB(*sql.DB).
//
// # Watch
//
// MySQL has no native change-notification mechanism, so this provider is not
// watchable; mamori wraps it in its polling adapter automatically.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "mysql"

// Default column names, used when a ref does not override them.
const (
	defaultKeyCol = "key"
	defaultValCol = "value"
)

// identRe is the strict allowlist every interpolated SQL identifier (table and
// column names) must match. Identifiers are the only parts of the query that
// cannot be parameterized, so anything outside this set is rejected to prevent
// SQL injection.
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// rowScanner is the single-row result of a query. Both *sql.Row and the test
// fake satisfy it, which keeps the provider decoupled from database/sql for
// unit testing.
type rowScanner interface {
	Scan(dest ...any) error
}

// queryer is the minimal surface the provider needs from a database: a single
// parameterized row query. A real *sql.DB is adapted by dbQueryer; tests inject
// an in-memory fake.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) rowScanner
}

// dbQueryer adapts a *sql.DB (whose QueryRowContext returns the concrete
// *sql.Row) to the queryer interface.
type dbQueryer struct{ db *sql.DB }

func (d dbQueryer) QueryRowContext(ctx context.Context, query string, args ...any) rowScanner {
	return d.db.QueryRowContext(ctx, query, args...)
}

// Provider resolves mysql:// refs against a MySQL/MariaDB table. It is safe for
// concurrent use.
type Provider struct {
	dsn        string
	versionCol string
	sensitive  bool

	mu      sync.Mutex
	q       queryer // injected (WithDB/withQueryer) or lazily opened from the DSN
	db      *sql.DB // the lazily opened pool, released by Close
	ownDB   bool    // true only when this provider opened db itself
	opened  bool    // a lazy open has been attempted
	openErr error   // remembered failure of the lazy open
}

// Option configures a Provider.
type Option func(*Provider)

// WithDSN sets the go-sql-driver/mysql DSN explicitly, e.g.
// "user:pass@tcp(127.0.0.1:3306)/appdb". When unset, the provider reads
// DATABASE_URL and then MYSQL_DSN from the environment at first resolve.
func WithDSN(dsn string) Option {
	return func(p *Provider) { p.dsn = dsn }
}

// WithDB injects a pre-configured *sql.DB, bypassing DSN handling. It is the
// intended way to point the provider at an existing pool, and is used by tests
// (including against an in-memory database). A nil db is ignored.
func WithDB(db *sql.DB) Option {
	return func(p *Provider) {
		if db != nil {
			p.db = db
			p.q = dbQueryer{db}
		}
	}
}

// WithVersionColumn makes the provider read Value.Version from the named column
// (a revision counter or an updated_at timestamp) instead of hashing the value.
// The column name is validated against the identifier allowlist like any other.
func WithVersionColumn(col string) Option {
	return func(p *Provider) { p.versionCol = col }
}

// WithSensitive marks resolved values as secret (driving redaction downstream).
// It is false by default, since a MySQL config table is not a secret manager.
func WithSensitive(sensitive bool) Option {
	return func(p *Provider) { p.sensitive = sensitive }
}

// withQueryer injects a custom queryer. It is unexported and used only by tests
// to supply an in-memory fake.
func withQueryer(q queryer) Option {
	return func(p *Provider) { p.q = q }
}

// New constructs a MySQL provider. Without options it targets the DSN found in
// DATABASE_URL or MYSQL_DSN, read lazily at resolve time, so it is safe to
// register from init even when no database is reachable at process start.
//
// Users who need explicit configuration call
// mamori.WithProvider(mysql.New(mysql.WithDSN("user:pass@tcp(host:3306)/db"))).
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, o := range opts {
		o(p)
	}
	return p
}

func init() { mamori.Register(New()) }

// Scheme returns "mysql".
func (p *Provider) Scheme() string { return scheme }

// Resolve fetches the value for the key encoded in ref.Path from the ref's
// table. A key with no matching row is reported as ErrNotFound.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	table, key, err := splitPath(ref.Path)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("mamori/mysql: ref %q: %w", ref.Raw, err)
	}

	keyCol := ref.Opt("key_col")
	if keyCol == "" {
		keyCol = defaultKeyCol
	}
	valCol := ref.Opt("val_col")
	if valCol == "" {
		valCol = defaultValCol
	}

	// Validate and quote every interpolated identifier. This is the injection
	// barrier: the key value below is always a bound "?" placeholder, but table
	// and column names must be part of the SQL text, so they are allowlisted.
	tableQ, err := quoteIdent("table name", table)
	if err != nil {
		return mamori.Value{}, err
	}
	keyColQ, err := quoteIdent("key_col", keyCol)
	if err != nil {
		return mamori.Value{}, err
	}
	valColQ, err := quoteIdent("val_col", valCol)
	if err != nil {
		return mamori.Value{}, err
	}
	var verColQ string
	if p.versionCol != "" {
		verColQ, err = quoteIdent("version column", p.versionCol)
		if err != nil {
			return mamori.Value{}, err
		}
	}

	cols := valColQ
	if verColQ != "" {
		cols += ", " + verColQ
	}
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s = ?", cols, tableQ, keyColQ)

	q, err := p.getQueryer()
	if err != nil {
		return mamori.Value{}, err
	}

	var (
		value   []byte
		version []byte
	)
	row := q.QueryRowContext(ctx, query, key)
	if verColQ != "" {
		err = row.Scan(&value, &version)
	} else {
		err = row.Scan(&value)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return mamori.Value{}, fmt.Errorf("mamori/mysql: key %q not found in table %q: %w", key, table, mamori.ErrNotFound)
	case err != nil:
		return mamori.Value{}, fmt.Errorf("mamori/mysql: querying table %q: %w", table, err)
	}

	// Select a JSON sub-field if requested. The version below is still derived
	// from the whole row value, so it reflects the row's revision regardless of
	// which field was selected.
	out := value
	if ref.Key != "" {
		out, err = mamori.SelectKey(value, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}

	ver := string(version)
	if ver == "" {
		ver = mamori.VersionHash(value)
	}

	return mamori.Value{
		Bytes:     out,
		Version:   ver,
		Sensitive: p.sensitive,
		Metadata: map[string]string{
			"table": table,
			"key":   key,
		},
	}, nil
}

// getQueryer returns the injected queryer, or lazily opens one from the DSN on
// first use. Failures are remembered so a bad configuration fails fast on every
// resolve rather than repeatedly reopening.
func (p *Provider) getQueryer() (queryer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.q != nil {
		return p.q, nil
	}
	if p.opened {
		return nil, p.openErr
	}
	p.opened = true

	dsn := p.resolveDSN()
	if dsn == "" {
		p.openErr = errors.New("mamori/mysql: no DSN configured; set mysql.WithDSN, DATABASE_URL, or MYSQL_DSN")
		return nil, p.openErr
	}
	if _, err := mysqldriver.ParseDSN(dsn); err != nil {
		p.openErr = fmt.Errorf("mamori/mysql: invalid DSN: %w", err)
		return nil, p.openErr
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		p.openErr = fmt.Errorf("mamori/mysql: opening database: %w", err)
		return nil, p.openErr
	}
	p.db = db
	p.ownDB = true
	p.q = dbQueryer{db}
	return p.q, nil
}

// resolveDSN returns the configured DSN, or DATABASE_URL / MYSQL_DSN read lazily
// from the environment (in that order).
func (p *Provider) resolveDSN() string {
	if p.dsn != "" {
		return p.dsn
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	return os.Getenv("MYSQL_DSN")
}

// Close releases the *sql.DB the provider opened lazily. It is a no-op when a
// pool was injected with WithDB (the caller owns that pool) or when nothing was
// opened.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ownDB && p.db != nil {
		return p.db.Close()
	}
	return nil
}

// splitPath splits a ref path "<table>/<key>" into its table and key parts. The
// key may itself contain slashes; only the first segment is the table.
func splitPath(path string) (table, key string, err error) {
	path = strings.TrimPrefix(path, "/")
	table, key, ok := strings.Cut(path, "/")
	if !ok || table == "" || key == "" {
		return "", "", fmt.Errorf("path %q must be <table>/<key>", path)
	}
	return table, key, nil
}

// quoteIdent validates an SQL identifier against the strict allowlist and
// returns it backtick-quoted. Backtick-quoting lets reserved words such as the
// default "key" column be used safely; the allowlist guarantees the identifier
// contains no backtick or other metacharacter to escape.
func quoteIdent(kind, ident string) (string, error) {
	if !identRe.MatchString(ident) {
		return "", fmt.Errorf("mamori/mysql: invalid %s %q: must match %s", kind, ident, identRe.String())
	}
	return "`" + ident + "`", nil
}
