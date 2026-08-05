//go:build integration

// Package postgres integration test. Runs against a real PostgreSQL server.
//
// Start a database and run the suite:
//
//	docker run --rm -e POSTGRES_PASSWORD=pass -p 5432:5432 postgres:16
//	export DATABASE_URL='postgres://postgres:pass@127.0.0.1:5432/postgres?sslmode=disable'
//	GOWORK=off go test -tags integration -run Integration ./...
//
// The test creates a scratch table with a NOTIFY trigger, seeds and mutates
// rows, and drops the table on cleanup.
package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

func livePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping live PostgreSQL integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	return pool
}

// setupTable creates a fresh config table keyed by "key" with a "value" column
// and installs a trigger that NOTIFYs the mamori_config channel on every
// insert/update, which is exactly what the native watch relies on.
func setupTable(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE %s (key text PRIMARY KEY, value text NOT NULL, updated_at timestamptz NOT NULL DEFAULT now())`, table),
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s_notify() RETURNS trigger AS $fn$
BEGIN
  PERFORM pg_notify('mamori_config', NEW.key);
  RETURN NEW;
END;
$fn$ LANGUAGE plpgsql`, table),
		fmt.Sprintf(`CREATE TRIGGER %s_notify_trg AFTER INSERT OR UPDATE ON %s
FOR EACH ROW EXECUTE FUNCTION %s_notify()`, table, table, table),
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, table))
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s_notify() CASCADE`, table))
		pool.Close()
	})
}

func upsert(pool *pgxpool.Pool, table string) func(context.Context, string, string) error {
	return func(ctx context.Context, key, val string) error {
		_, err := pool.Exec(ctx,
			fmt.Sprintf(`INSERT INTO %s (key, value, updated_at) VALUES ($1, $2, now())
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, table),
			key, val)
		return err
	}
}

func TestIntegrationConformance(t *testing.T) {
	pool := livePool(t)
	table := fmt.Sprintf("mamori_it_%d", time.Now().UnixNano())
	setupTable(t, pool, table)

	providertest.Run(t, providertest.Config{
		New:    func() mamori.Provider { return New(WithPool(pool)) },
		Ref:    func(key string) string { return "postgres://" + table + "/" + key },
		Seed:   upsert(pool, table),
		Mutate: upsert(pool, table),
		// See the unit-test conformance config: LISTEN precedes the baseline
		// query, and pgx buffers a NOTIFY arriving before the next wait.
		WatchDeliversBaseline: true,
		EventuallyTimeout:     15 * time.Second,
		// live-backend integration test; error injection not possible, unit test covers classification
		NoResolveErrors: true,
	})
}

func TestIntegrationResolveWatchAndVersionColumn(t *testing.T) {
	pool := livePool(t)
	table := fmt.Sprintf("mamori_it_%d", time.Now().UnixNano())
	setupTable(t, pool, table)
	put := upsert(pool, table)

	if err := put(context.Background(), "db", `{"level":"info"}`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p := New(WithPool(pool), WithVersionColumn("updated_at"))
	ref := mustRef(t, "postgres://"+table+"/db#level")

	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "info" {
		t.Fatalf("level = %q, want info", v.Bytes)
	}
	if v.Version == "" {
		t.Fatal("version column produced an empty Version")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, ref)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	<-ch // baseline

	if err := put(context.Background(), "db", `{"level":"debug"}`); err != nil {
		t.Fatalf("update: %v", err)
	}
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("watch update err: %v", u.Err)
		}
		if string(u.Value.Bytes) != "debug" {
			t.Fatalf("watch level = %q, want debug", u.Value.Bytes)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("LISTEN/NOTIFY watch did not deliver the update")
	}
}

// TestIntegrationCloseReleasesLazilyBuiltPool exercises the one line in Close
// that actually releases a live OS resource: p.ownPool.Close() on a pool
// backendFor built itself. Nothing else in this package's Close coverage
// reaches that line: the unit tests in postgres_test.go inject a fakeBackend
// with no real pool, and TestIntegrationConformance and
// TestIntegrationResolveWatchAndVersionColumn above both construct via
// WithPool, which hands the provider an already-built pool and bypasses
// backendFor's lazy-construction path entirely.
//
// This test constructs with WithDSN instead, so Resolve forces backendFor to
// build the pool itself, then reads the unexported p.ownPool field - valid
// here because this file, like postgres_test.go, is package postgres - to
// get a handle on the exact pool Close is responsible for releasing. It
// deliberately does not treat that field read as the proof of release: the
// assertion after Close is behavioral, against the pool's own public API
// (Acquire), not against Provider's internal state.
func TestIntegrationCloseReleasesLazilyBuiltPool(t *testing.T) {
	admin := livePool(t)
	table := fmt.Sprintf("mamori_it_close_%d", time.Now().UnixNano())
	setupTable(t, admin, table)
	if err := upsert(admin, table)(context.Background(), "k", "v"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	dsn := os.Getenv("DATABASE_URL")
	p := New(WithDSN(dsn))
	ref := mustRef(t, "postgres://"+table+"/k")

	if _, err := p.Resolve(context.Background(), ref); err != nil {
		t.Fatalf("Resolve to force the lazy pool build: %v", err)
	}

	pool := p.ownPool
	if pool == nil {
		t.Fatal("backendFor did not record an owned pool; nothing for this test to verify")
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A closed pgxpool.Pool refuses Acquire locally (puddle.ErrClosedPool)
	// rather than dialing, so this is a fast, live assertion that Close
	// actually released the pool object, not merely that Provider's own
	// bookkeeping (p.ownPool, p.be) was zeroed out.
	if _, err := pool.Acquire(context.Background()); err == nil {
		t.Fatal("pool.Acquire succeeded after provider Close; want the underlying pool released")
	}
}
