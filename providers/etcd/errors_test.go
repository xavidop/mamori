package etcd

import (
	"errors"
	"testing"

	"github.com/xavidop/mamori"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestClassifyEtcd(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"PermissionDenied", status.Error(codes.PermissionDenied, "etcdserver: permission denied"), mamori.KindPermissionDenied},
		{"Unauthenticated", status.Error(codes.Unauthenticated, "etcdserver: invalid auth token"), mamori.KindUnauthenticated},
		{"Unavailable", status.Error(codes.Unavailable, "etcdserver: no leader"), mamori.KindUnavailable},
		{"DeadlineExceeded", status.Error(codes.DeadlineExceeded, "context deadline exceeded"), mamori.KindUnavailable},
		{"ResourceExhausted", status.Error(codes.ResourceExhausted, "etcdserver: mvcc: database space exceeded"), mamori.KindRateLimited},
		{"Internal", status.Error(codes.Internal, "boom"), mamori.KindUnknown},
		{"Unimplemented", status.Error(codes.Unimplemented, "boom"), mamori.KindUnknown},
		{"NotFound", status.Error(codes.NotFound, "boom"), mamori.KindUnknown},
		// THE TRAP: etcd reports a bad username/password as InvalidArgument
		// (rpctypes.ErrGRPCAuthFailed), not Unauthenticated, and that code is
		// genuinely ambiguous for etcd (it also covers ordinary malformed
		// requests). This case locks in that InvalidArgument stays unknown
		// rather than being guessed at as either Unauthenticated or Invalid.
		{"InvalidArgumentAuthFailureStaysUnknown", status.Error(codes.InvalidArgument, "auth: invalid user ID or password"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifyEtcd(tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyEtcd(%v)) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyEtcdRpctypesEtcdError exercises the fallback path: for a fixed
// set of well-known server error messages, the etcd v3 client's internal
// ContextError helper rewrites the gRPC status into an rpctypes.EtcdError
// before Get/Put/Delete return it, and that type does not implement
// GRPCStatus(), so status.Code alone would report Unknown for it. classifyEtcd
// must still classify it correctly via errors.As, or a live server's ordinary
// permission/auth/availability/rate-limit errors would silently report
// unknown despite matching this classifier's table.
func TestClassifyEtcdRpctypesEtcdError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"PermissionDenied", rpctypes.Error(status.Error(codes.PermissionDenied, "etcdserver: permission denied")), mamori.KindPermissionDenied},
		{"Unauthenticated", rpctypes.Error(status.Error(codes.Unauthenticated, "etcdserver: invalid auth token")), mamori.KindUnauthenticated},
		{"Unavailable", rpctypes.Error(status.Error(codes.Unavailable, "etcdserver: no leader")), mamori.KindUnavailable},
		{"ResourceExhausted", rpctypes.Error(status.Error(codes.ResourceExhausted, "etcdserver: too many requests")), mamori.KindRateLimited},
		// The trap survives the EtcdError rewrite too: the real
		// rpctypes.ErrGRPCAuthFailed message goes through this exact
		// conversion on a live server, and must still stay unknown.
		{"InvalidArgumentAuthFailureStaysUnknown", rpctypes.Error(rpctypes.ErrGRPCAuthFailed), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var etcdErr rpctypes.EtcdError
			if !errors.As(tc.err, &etcdErr) {
				t.Fatalf("test setup: %v did not convert to rpctypes.EtcdError; the message must match a known etcd server error text exactly", tc.err)
			}
			got := mamori.ErrorKind(classifyEtcd(tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyEtcd(%v)) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyEtcdPreservesStatus(t *testing.T) {
	orig := status.Error(codes.PermissionDenied, "etcdserver: permission denied")
	wrapped := classifyEtcd(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	if got := status.Code(wrapped); got != codes.PermissionDenied {
		t.Fatalf("status.Code(wrapped) = %v, want PermissionDenied; the %%w: %%w "+
			"pattern must keep the gRPC status reachable", got)
	}
}

// TestClassifyEtcdPreservesEtcdError guards the EtcdError fallback path
// specifically: it must double-wrap (sentinel AND the underlying
// rpctypes.EtcdError), not just the sentinel, so callers who already reach
// the etcd-typed error with errors.As keep working, and its Code() stays
// recoverable.
func TestClassifyEtcdPreservesEtcdError(t *testing.T) {
	orig := rpctypes.Error(status.Error(codes.PermissionDenied, "etcdserver: permission denied"))
	wrapped := classifyEtcd(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var etcdErr rpctypes.EtcdError
	if !errors.As(wrapped, &etcdErr) {
		t.Fatalf("errors.As can no longer reach rpctypes.EtcdError: %v", wrapped)
	}
	if etcdErr.Code() != codes.PermissionDenied {
		t.Fatalf("recovered code = %v, want PermissionDenied", etcdErr.Code())
	}
}

func TestClassifyEtcdNilAndPlain(t *testing.T) {
	if err := classifyEtcd(nil); err != nil {
		t.Fatalf("classifyEtcd(nil) = %v, want nil", err)
	}
	plain := errors.New("dial tcp: connection refused")
	if got := mamori.ErrorKind(classifyEtcd(plain)); got != mamori.KindUnknown {
		t.Fatalf("plain error kind = %q, want unknown", got)
	}
}
