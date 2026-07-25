package mongodb

import (
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/mongo"

	"github.com/xavidop/mamori"
)

func TestClassifyMongo(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"AuthenticationFailed", mongo.CommandError{Code: 18, Name: "AuthenticationFailed"}, mamori.KindUnauthenticated},
		{"Unauthorized", mongo.CommandError{Code: 13, Name: "Unauthorized"}, mamori.KindPermissionDenied},
		{"UnmappedCode", mongo.CommandError{Code: 189, Name: "PrimarySteppedDown"}, mamori.KindUnknown},
		{"PlainError", errors.New("connection reset"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifyMongo(tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyMongo(%v)) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyMongoNilIsNil(t *testing.T) {
	if err := classifyMongo(nil); err != nil {
		t.Fatalf("classifyMongo(nil) = %v, want nil", err)
	}
}

// TestClassifyMongoPreservesCommandError guards that classification does not
// discard the original driver error: callers who already reach it with
// errors.As (to read ce.Code, ce.Name, ...) must keep working.
func TestClassifyMongoPreservesCommandError(t *testing.T) {
	orig := mongo.CommandError{Code: 13, Name: "Unauthorized", Message: "not authorized on app_config"}
	wrapped := classifyMongo(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var ce mongo.CommandError
	if !errors.As(wrapped, &ce) {
		t.Fatalf("errors.As can no longer reach mongo.CommandError: %v", wrapped)
	}
	if ce.Code != 13 {
		t.Fatalf("recovered code = %d, want 13", ce.Code)
	}
}
