// Package postgres implements a mamori provider that resolves configuration
// values from a PostgreSQL table, with native hot-reload driven by
// LISTEN/NOTIFY.
//
// The scheme is "postgres" and the ref grammar is:
//
//	postgres://<table>/<key>[#json-field][?key_col=<c>&val_col=<c>]
//
// A value is fetched with a parameterized query of the form
//
//	SELECT <val_col> FROM <table> WHERE <key_col> = $1
//
// where the key is bound as the $1 argument (never string-interpolated). The
// table, key column, and value column names cannot be parameterized by SQL, so
// they are validated against a strict identifier allowlist
// (^[A-Za-z_][A-Za-z0-9_]*$, optionally one schema-qualifying dot) and any ref
// that does not match is rejected before a query is built. This is the SQL
// injection boundary for identifiers.
//
//	DBPassword string `source:"postgres://app_config/db_password"`
//	LogLevel   string `source:"postgres://app_config/log_level"`
//	DBHost     string `source:"postgres://app_config/db#host"` // JSON field
//
// The column names default to key_col="key" and val_col="value" and can be
// overridden per-ref with the query options:
//
//	Feature string `source:"postgres://settings/feature?key_col=name&val_col=data"`
//
// By default Value.Version is a hash of the value bytes (mamori.VersionHash),
// giving cheap change detection. If your table has a monotonic revision or
// timestamp column, point the provider at it with WithVersionColumn and that
// column (cast to text) becomes the version instead.
//
// Resolved values are not marked Sensitive by default (a config table is not a
// managed secret store); construct the provider with WithSensitive(true) to
// have every resolved value drive redaction downstream, or wrap individual
// fields in secret.String.
//
// The provider implements mamori.WatchableProvider using PostgreSQL
// LISTEN/NOTIFY: it LISTENs on a channel (default "mamori_config") on a
// dedicated pooled connection and, on every NOTIFY, re-queries the ref and
// emits an Update. The database must issue NOTIFY on change; see the README for
// a sample trigger.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "postgres"

// defaultKeyCol and defaultValCol are the column names used when a ref does not
// override them with ?key_col= / ?val_col=.
const (
	defaultKeyCol  = "key"
	defaultValCol  = "value"
	defaultChannel = "mamori_config"
)

// watchErrBackoff is how long the watch loop pauses after a transient error
// before retrying, so a persistent failure does not spin.
const watchErrBackoff = 500 * time.Millisecond

// identRe matches a single, unqualified SQL identifier. It is intentionally
// strict: only ASCII letters, digits, and underscores, not starting with a
// digit. Everything else (whitespace, quotes, semicolons, parentheses, dots,
// dashes) is rejected. This is the allowlist that makes it safe to interpolate
// a validated table/column name into the SQL text - the key value itself is
// always bound as a $1 parameter and never interpolated.
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validIdent reports whether s is a single safe SQL identifier.
func validIdent(s string) bool { return identRe.MatchString(s) }

// validTable reports whether s is a safe table reference: a single identifier,
// optionally schema-qualified with exactly one dot (schema.table).
func validTable(s string) bool {
	schema, table, qualified := strings.Cut(s, ".")
	if !qualified {
		return validIdent(s)
	}
	// Reject a second dot: schema.table.extra is not allowed.
	if strings.Contains(table, ".") {
		return false
	}
	return validIdent(schema) && validIdent(table)
}

// row is the minimal single-row result the provider scans. *pgx.Row (returned
// by pool.QueryRow) satisfies it directly, and the test fake implements it.
type row interface {
	Scan(dest ...any) error
}

// notifier is a dedicated LISTENing connection that blocks until a NOTIFY
// arrives. It abstracts the pgx WaitForNotification loop so tests can drive
// watch deterministically without a live database.
type notifier interface {
	// Wait blocks until a notification is received on the subscribed channel,
	// the context is cancelled, or the connection fails.
	Wait(ctx context.Context) error
	// Close releases the underlying connection. It is safe to call once.
	Close()
}

// backend is the minimal database surface the provider depends on. The real
// implementation (poolBackend) wraps *pgxpool.Pool; tests inject an in-memory
// fake implementing the same shape, so the conformance and watch suites run
// with no live PostgreSQL.
type backend interface {
	// QueryRow runs a parameterized query returning at most one row. A missing
	// row is reported as pgx.ErrNoRows from the returned row's Scan.
	QueryRow(ctx context.Context, sql string, args ...any) row
	// Listen opens a dedicated connection subscribed to channel via LISTEN.
	Listen(ctx context.Context, channel string) (notifier, error)
}

// Provider resolves postgres:// refs against a PostgreSQL table. It is safe for
// concurrent use. The connection pool is built lazily on first use from the
// DATABASE_URL environment variable (or WithDSN) unless a pool/backend is
// injected via WithPool.
type Provider struct {
	dsn        string
	keyCol     string
	valCol     string
	versionCol string
	channel    string
	sensitive  bool

	mu sync.Mutex
	be backend // resolved backend (injected or lazily built)
}

// Option configures a Provider.
type Option func(*Provider)

// WithDSN sets the PostgreSQL connection string (libpq/pgx URL or keyword form).
// It overrides the DATABASE_URL environment variable.
func WithDSN(dsn string) Option {
	return func(p *Provider) { p.dsn = dsn }
}

// WithPool injects a pre-configured *pgxpool.Pool, bypassing lazy construction.
// Use it when you build the pool yourself (custom TLS, pool sizing, tracing).
func WithPool(pool *pgxpool.Pool) Option {
	return func(p *Provider) {
		if pool != nil {
			p.be = &poolBackend{pool: pool}
		}
	}
}

// WithKeyColumn overrides the default key column name ("key"). A per-ref
// ?key_col= option takes precedence over this.
func WithKeyColumn(col string) Option {
	return func(p *Provider) { p.keyCol = col }
}

// WithValueColumn overrides the default value column name ("value"). A per-ref
// ?val_col= option takes precedence over this.
func WithValueColumn(col string) Option {
	return func(p *Provider) { p.valCol = col }
}

// WithVersionColumn selects a column whose value (cast to text) is used as
// Value.Version instead of a hash of the value bytes. Point it at a monotonic
// revision counter or an updated_at timestamp for exact, native change
// detection. The column name is validated by the same identifier allowlist.
func WithVersionColumn(col string) Option {
	return func(p *Provider) { p.versionCol = col }
}

// WithChannel sets the LISTEN/NOTIFY channel used by Watch (default
// "mamori_config"). The database's NOTIFY must target the same channel.
func WithChannel(channel string) Option {
	return func(p *Provider) { p.channel = channel }
}

// WithSensitive marks every resolved value as secret, driving redaction
// downstream. Off by default: a config table is not a managed secret store.
func WithSensitive(sensitive bool) Option {
	return func(p *Provider) { p.sensitive = sensitive }
}

// withBackend injects a bare backend. Unexported: used by tests to supply an
// in-memory fake.
func withBackend(be backend) Option {
	return func(p *Provider) { p.be = be }
}

// New constructs a PostgreSQL provider. The pool is created lazily on first
// Resolve/Watch, so New never fails and never contacts the database.
func New(opts ...Option) *Provider {
	p := &Provider{
		keyCol:  defaultKeyCol,
		valCol:  defaultValCol,
		channel: defaultChannel,
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.keyCol == "" {
		p.keyCol = defaultKeyCol
	}
	if p.valCol == "" {
		p.valCol = defaultValCol
	}
	if p.channel == "" {
		p.channel = defaultChannel
	}
	return p
}

// init registers a lazily-initialized provider so `import _` wiring works from
// the ambient DATABASE_URL. Users who need explicit config call
// mamori.WithProvider(postgres.New(postgres.WithDSN("..."))).
func init() { mamori.Register(New()) }

// Scheme returns "postgres".
func (p *Provider) Scheme() string { return scheme }

// backendFor returns the backend, building a pool lazily from the DSN
// (WithDSN or DATABASE_URL) on first use. Concurrent callers share one pool.
func (p *Provider) backendFor(ctx context.Context) (backend, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.be != nil {
		return p.be, nil
	}
	dsn := p.dsn
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return nil, errors.New("postgres: no DSN configured; set DATABASE_URL or use postgres.WithDSN")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	p.be = &poolBackend{pool: pool}
	return p.be, nil
}

// plan validates a ref and builds the parameterized SELECT plus the key value
// to bind as $1. It is the single choke point where table/column identifiers
// are validated against the allowlist, so every code path (Resolve and Watch)
// shares the exact same SQL-injection defense.
func (p *Provider) plan(ref mamori.Ref) (sql, keyval string, err error) {
	table, keyval, ok := strings.Cut(ref.Path, "/")
	if !ok || table == "" || keyval == "" {
		return "", "", fmt.Errorf("postgres: ref %q must be postgres://<table>/<key>", ref.String())
	}

	keyCol := p.keyCol
	if v := ref.Opt("key_col"); v != "" {
		keyCol = v
	}
	valCol := p.valCol
	if v := ref.Opt("val_col"); v != "" {
		valCol = v
	}

	if !validTable(table) {
		return "", "", fmt.Errorf("postgres: %w: table %q", errUnsafeIdentifier, table)
	}
	if !validIdent(keyCol) {
		return "", "", fmt.Errorf("postgres: %w: key_col %q", errUnsafeIdentifier, keyCol)
	}
	if !validIdent(valCol) {
		return "", "", fmt.Errorf("postgres: %w: val_col %q", errUnsafeIdentifier, valCol)
	}
	if p.versionCol != "" && !validIdent(p.versionCol) {
		return "", "", fmt.Errorf("postgres: %w: version column %q", errUnsafeIdentifier, p.versionCol)
	}

	if p.versionCol != "" {
		// Cast the version column to text so any column type (int, bigint,
		// timestamptz, uuid, ...) scans into a string uniformly.
		sql = fmt.Sprintf("SELECT %s, %s::text FROM %s WHERE %s = $1",
			valCol, p.versionCol, table, keyCol)
	} else {
		sql = fmt.Sprintf("SELECT %s FROM %s WHERE %s = $1", valCol, table, keyCol)
	}
	return sql, keyval, nil
}

// errUnsafeIdentifier is the sentinel wrapped when a table/column name fails the
// identifier allowlist. It is distinct from ErrNotFound so a rejected injection
// attempt is never mistaken for a missing key.
var errUnsafeIdentifier = errors.New("unsafe SQL identifier rejected")

// Resolve fetches the current value for ref from PostgreSQL. A missing row
// yields an error satisfying errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	sql, keyval, err := p.plan(ref)
	if err != nil {
		return mamori.Value{}, err
	}
	be, err := p.backendFor(ctx)
	if err != nil {
		return mamori.Value{}, err
	}
	return p.resolveWith(ctx, be, sql, keyval, ref)
}

// resolveWith runs the planned query against be and maps the row into a Value.
func (p *Provider) resolveWith(ctx context.Context, be backend, sql, keyval string, ref mamori.Ref) (mamori.Value, error) {
	r := be.QueryRow(ctx, sql, keyval)

	var raw []byte
	var ver string
	if p.versionCol != "" {
		err := r.Scan(&raw, &ver)
		if err != nil {
			return mamori.Value{}, mapScanErr(keyval, err)
		}
	} else {
		if err := r.Scan(&raw); err != nil {
			return mamori.Value{}, mapScanErr(keyval, err)
		}
		ver = mamori.VersionHash(raw)
	}

	b := raw
	if ref.Key != "" {
		sel, err := mamori.SelectKey(b, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		b = sel
	}
	return mamori.Value{Bytes: b, Version: ver, Sensitive: p.sensitive}, nil
}

// mapScanErr converts a row Scan error into the mamori-typed error, mapping the
// no-rows case to ErrNotFound.
func mapScanErr(keyval string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("postgres: key %q not found: %w", keyval, mamori.ErrNotFound)
	}
	return fmt.Errorf("postgres: query key %q: %w", keyval, err)
}

// Watch implements mamori.WatchableProvider using PostgreSQL LISTEN/NOTIFY. It
// emits the current value as a baseline, then blocks on a dedicated LISTENing
// connection; on every NOTIFY it re-queries the ref and emits a fresh Update.
// The channel is closed when ctx is cancelled and the LISTEN connection is
// released, so the goroutine never leaks.
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	sql, keyval, err := p.plan(ref)
	if err != nil {
		return nil, err
	}
	if !validIdent(p.channel) {
		return nil, fmt.Errorf("postgres: %w: channel %q", errUnsafeIdentifier, p.channel)
	}
	be, err := p.backendFor(ctx)
	if err != nil {
		return nil, err
	}
	sub, err := be.Listen(ctx, p.channel)
	if err != nil {
		return nil, fmt.Errorf("postgres: listen %q: %w", p.channel, err)
	}

	ch := make(chan mamori.Update, 1)
	go func() {
		defer close(ch)
		defer sub.Close()

		emit := func(u mamori.Update) bool {
			select {
			case ch <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}

		// Baseline: the current value.
		v, rerr := p.resolveWith(ctx, be, sql, keyval, ref)
		if !emit(mamori.Update{Value: v, Err: rerr}) {
			return
		}

		for {
			if ctx.Err() != nil {
				return
			}
			werr := sub.Wait(ctx)
			if ctx.Err() != nil {
				return
			}
			if werr != nil {
				if !emit(mamori.Update{Err: fmt.Errorf("postgres: watch %q: %w", ref.Path, werr)}) {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(watchErrBackoff):
				}
				continue
			}
			v, rerr := p.resolveWith(ctx, be, sql, keyval, ref)
			if !emit(mamori.Update{Value: v, Err: rerr}) {
				return
			}
		}
	}()
	return ch, nil
}

// poolBackend adapts *pgxpool.Pool to the backend interface.
type poolBackend struct {
	pool *pgxpool.Pool
}

var _ backend = (*poolBackend)(nil)

func (b *poolBackend) QueryRow(ctx context.Context, sql string, args ...any) row {
	return b.pool.QueryRow(ctx, sql, args...)
}

func (b *poolBackend) Listen(ctx context.Context, channel string) (notifier, error) {
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	// channel has already been validated by the caller (Watch); Sanitize gives
	// belt-and-suspenders quoting.
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		conn.Release()
		return nil, err
	}
	return &poolNotifier{conn: conn}, nil
}

// poolNotifier is a dedicated LISTENing pooled connection.
type poolNotifier struct {
	conn *pgxpool.Conn
	once sync.Once
}

var _ notifier = (*poolNotifier)(nil)

func (n *poolNotifier) Wait(ctx context.Context) error {
	_, err := n.conn.Conn().WaitForNotification(ctx)
	return err
}

func (n *poolNotifier) Close() {
	n.once.Do(func() {
		// The connection has an active LISTEN and must not be reused, so take it
		// out of the pool and close it outright rather than releasing it back.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.conn.Hijack().Close(ctx)
	})
}
