//go:build integration

// Package mysql live integration test. It is NOT part of the standard
// `go test ./...` pass; it requires a reachable MySQL/MariaDB instance and is
// guarded by the `integration` build tag.
//
// Run it against a real database, e.g.:
//
//	# a throwaway MySQL:
//	docker run --rm -d --name mamori-mysql -e MYSQL_ROOT_PASSWORD=secret \
//	    -e MYSQL_DATABASE=appdb -p 3306:3306 mysql:8
//
//	export MYSQL_DSN='root:secret@tcp(127.0.0.1:3306)/appdb'
//	go test -tags=integration -run TestLive ./...
//
// The test creates its own table, seeds a row, resolves it, verifies not-found
// and JSON #key selection, then drops the table.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/xavidop/mamori"
)

func TestLive(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("set MYSQL_DSN (or DATABASE_URL) to run the live MySQL test")
	}

	ctx := context.Background()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	const table = "mamori_live_kv"
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"CREATE TABLE "+table+" (`key` VARCHAR(191) PRIMARY KEY, `value` TEXT NOT NULL)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS "+table) }()

	if _, err := db.ExecContext(ctx,
		"INSERT INTO "+table+" (`key`,`value`) VALUES (?,?)", "log_level", "debug"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO "+table+" (`key`,`value`) VALUES (?,?)", "db", `{"host":"db.internal"}`); err != nil {
		t.Fatalf("insert json: %v", err)
	}

	p := New(WithDB(db))

	v, err := p.Resolve(ctx, mustRef(t, "mysql://"+table+"/log_level"))
	if err != nil {
		t.Fatalf("live Resolve: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("Bytes = %q, want debug", v.Bytes)
	}
	if v.Version == "" {
		t.Error("live value has empty Version")
	}

	jv, err := p.Resolve(ctx, mustRef(t, "mysql://"+table+"/db#host"))
	if err != nil {
		t.Fatalf("live Resolve #host: %v", err)
	}
	if string(jv.Bytes) != "db.internal" {
		t.Fatalf("Bytes = %q, want db.internal", jv.Bytes)
	}

	if _, err := p.Resolve(ctx, mustRef(t, "mysql://"+table+"/___missing___")); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("missing key error = %v, want ErrNotFound", err)
	}
}
