package postgres

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/xavidop/mamori"
)

func TestClassifyPostgres(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"InsufficientPrivilege", &pgconn.PgError{Code: "42501"}, mamori.KindPermissionDenied},
		{"InvalidPassword", &pgconn.PgError{Code: "28P01"}, mamori.KindUnauthenticated},
		{"InvalidAuthorizationSpecification", &pgconn.PgError{Code: "28000"}, mamori.KindUnauthenticated},
		{"TooManyConnections", &pgconn.PgError{Code: "53300"}, mamori.KindUnavailable},
		{"CannotConnectNow", &pgconn.PgError{Code: "57P03"}, mamori.KindUnavailable},
		{"ConnectionFailure", &pgconn.PgError{Code: "08006"}, mamori.KindUnavailable},
		{"SqlclientUnableToEstablishSqlconnection", &pgconn.PgError{Code: "08001"}, mamori.KindUnavailable},
		{"SqlserverRejectedEstablishmentOfSqlconnection", &pgconn.PgError{Code: "08004"}, mamori.KindUnavailable},
		{"UnmappedCode", &pgconn.PgError{Code: "XX000"}, mamori.KindUnknown},
		{"PlainError", errors.New("connection reset"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifyPostgres(tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyPostgres(%v)) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyPostgresNilIsNil(t *testing.T) {
	if err := classifyPostgres(nil); err != nil {
		t.Fatalf("classifyPostgres(nil) = %v, want nil", err)
	}
}

// TestClassifyPostgresPreservesPgError guards that classification does not
// discard the original SDK error: callers who already reach it with
// errors.As (to read pgErr.Code, Detail, Hint, ...) must keep working.
func TestClassifyPostgresPreservesPgError(t *testing.T) {
	orig := &pgconn.PgError{Code: "42501", Message: "permission denied for table settings"}
	wrapped := classifyPostgres(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var pgErr *pgconn.PgError
	if !errors.As(wrapped, &pgErr) {
		t.Fatalf("errors.As can no longer reach *pgconn.PgError: %v", wrapped)
	}
	if pgErr.Code != "42501" {
		t.Fatalf("recovered code = %q, want 42501", pgErr.Code)
	}
}
