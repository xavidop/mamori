package redis

import (
	"context"
	"errors"
	"net"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"github.com/xavidop/mamori"
)

// redisError is a minimal type satisfying goredis.Error - the same interface
// go-redis's own protocol errors implement - so test errors are constructed
// the way a live server response actually arrives, and classifyRedis is
// exercised through the exported predicates rather than a hand-rolled shape.
type redisError string

func (e redisError) Error() string { return string(e) }
func (e redisError) RedisError()   {}

var _ goredis.Error = redisError("")

func TestClassifyRedis(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"AuthErrorNOAUTH", redisError("NOAUTH Authentication required."), mamori.KindUnauthenticated},
		{"AuthErrorWRONGPASS", redisError("WRONGPASS invalid username-password pair or user is disabled."), mamori.KindUnauthenticated},
		{"PermissionErrorNOPERM", redisError("NOPERM User app has no permissions to run the 'get' command"), mamori.KindPermissionDenied},
		{"LoadingError", redisError("LOADING Redis is loading the dataset in memory"), mamori.KindUnavailable},
		{"ClusterDownError", redisError("CLUSTERDOWN The cluster is down"), mamori.KindUnavailable},
		{"MasterDownError", redisError("MASTERDOWN Link with MASTER is down and replica-serve-stale-data is set to 'no'."), mamori.KindUnavailable},
		{"DialFailure", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}, mamori.KindUnavailable},
		{"UnmappedRedisError", redisError("ERR wrong number of arguments for 'get' command"), mamori.KindUnknown},
		{"PlainError", errors.New("connection reset"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifyRedis(tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyRedis(%v)) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyRedisNilIsNil(t *testing.T) {
	if err := classifyRedis(nil); err != nil {
		t.Fatalf("classifyRedis(nil) = %v, want nil", err)
	}
}

// TestClassifyRedisPreservesOriginalError guards that classification does not
// discard the underlying error: callers who already reach it (for example
// with errors.Is against the exact value, or by reading its message) must
// keep working.
func TestClassifyRedisPreservesOriginalError(t *testing.T) {
	orig := redisError("NOPERM User app has no permissions to run the 'get' command")
	wrapped := classifyRedis(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	if !errors.Is(wrapped, orig) {
		t.Fatalf("errors.Is can no longer reach the original redis error: %v", wrapped)
	}
}

// TestResolveClassifiesNonNotFoundError exercises classifyRedis through
// Resolve itself, not just as a direct function call. The conformance
// ErrorClassification case cannot catch a regression here: it injects a
// mamori sentinel directly (not a go-redis protocol error), so it passes even
// if the classifyRedis call were deleted from resolveWith's error branch.
// This test injects a real go-redis-shaped error through fakeRedis.fail, the
// same shape a live server would return, so it fails if the wiring is
// removed.
func TestResolveClassifiesNonNotFoundError(t *testing.T) {
	f := newFakeRedis()
	f.set("app/db", "s3cr3t")
	f.fail("app/db", redisError("NOPERM User app has no permissions to run the 'get' command"))
	p := New(withRedisAPI(f))

	ref, err := mamori.ParseRef("redis://app/db")
	if err != nil {
		t.Fatal(err)
	}
	_, resolveErr := p.Resolve(context.Background(), ref)
	if resolveErr == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(resolveErr); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyRedis may not be wired into resolveWith", got, mamori.KindPermissionDenied)
	}
}
