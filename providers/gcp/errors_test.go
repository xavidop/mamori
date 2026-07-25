package gcp

import (
	"context"
	"errors"
	"testing"

	"github.com/xavidop/mamori"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestClassifyGCP(t *testing.T) {
	cases := []struct {
		code codes.Code
		want mamori.Kind
	}{
		{codes.NotFound, mamori.KindNotFound},
		{codes.PermissionDenied, mamori.KindPermissionDenied},
		{codes.Unauthenticated, mamori.KindUnauthenticated},
		{codes.Unavailable, mamori.KindUnavailable},
		{codes.DeadlineExceeded, mamori.KindUnavailable},
		{codes.ResourceExhausted, mamori.KindRateLimited},
		{codes.InvalidArgument, mamori.KindInvalid},
		{codes.Internal, mamori.KindUnknown},
		{codes.Unimplemented, mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.code.String(), func(t *testing.T) {
			err := status.Error(tc.code, "boom")
			if got := mamori.ErrorKind(classifyGCP(err)); got != tc.want {
				t.Fatalf("ErrorKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyGCPPreservesStatus(t *testing.T) {
	orig := status.Error(codes.PermissionDenied, "caller lacks secretmanager.versions.access")
	wrapped := classifyGCP(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	if got := status.Code(wrapped); got != codes.PermissionDenied {
		t.Fatalf("status.Code(wrapped) = %v, want PermissionDenied; the %%w: %%w "+
			"pattern must keep the gRPC status reachable", got)
	}
}

func TestClassifyGCPNilAndPlain(t *testing.T) {
	if err := classifyGCP(nil); err != nil {
		t.Fatalf("classifyGCP(nil) = %v, want nil", err)
	}
	plain := errors.New("dial tcp: connection refused")
	if got := mamori.ErrorKind(classifyGCP(plain)); got != mamori.KindUnknown {
		t.Fatalf("plain error kind = %q, want unknown", got)
	}
}

// TestResolveNotFoundPreservesStatus guards Resolve's status.Code(err) ==
// codes.NotFound pre-check specifically: it must double-wrap (sentinel AND
// the underlying gRPC status), not just the sentinel, so the gRPC status
// stays reachable through status.Code as the README's error table promises.
func TestResolveNotFoundPreservesStatus(t *testing.T) {
	p := New(WithClient(newFakeSM()))
	_, err := p.Resolve(context.Background(), parse(t, "gcp-sm://proj/nope"))

	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("status.Code(err) = %v, want NotFound; the gRPC status is no longer reachable", got)
	}
}
