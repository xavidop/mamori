package mysql

import (
	"errors"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/xavidop/mamori"
)

func TestClassifyMySQL(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"DBAccessDenied", &mysqldriver.MySQLError{Number: 1044}, mamori.KindPermissionDenied},
		{"TableAccessDenied", &mysqldriver.MySQLError{Number: 1142}, mamori.KindPermissionDenied},
		{"AccessDenied", &mysqldriver.MySQLError{Number: 1045}, mamori.KindUnauthenticated},
		{"ConCountError", &mysqldriver.MySQLError{Number: 1040}, mamori.KindUnavailable},
		{"TooManyUserConnections", &mysqldriver.MySQLError{Number: 1203}, mamori.KindUnavailable},
		{"UnmappedNumber", &mysqldriver.MySQLError{Number: 1064}, mamori.KindUnknown},
		{"PlainError", errors.New("connection reset"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifyMySQL(tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyMySQL(%v)) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyMySQLNilIsNil(t *testing.T) {
	if err := classifyMySQL(nil); err != nil {
		t.Fatalf("classifyMySQL(nil) = %v, want nil", err)
	}
}

// TestClassifyMySQLPreservesMySQLError guards that classification does not
// discard the original driver error: callers who already reach it with
// errors.As (to read me.Number, me.Message, ...) must keep working.
func TestClassifyMySQLPreservesMySQLError(t *testing.T) {
	orig := &mysqldriver.MySQLError{Number: 1044, Message: "access denied for db appdb"}
	wrapped := classifyMySQL(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var me *mysqldriver.MySQLError
	if !errors.As(wrapped, &me) {
		t.Fatalf("errors.As can no longer reach *mysqldriver.MySQLError: %v", wrapped)
	}
	if me.Number != 1044 {
		t.Fatalf("recovered number = %d, want 1044", me.Number)
	}
}
