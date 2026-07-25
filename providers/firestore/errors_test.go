package firestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestClassifyFirestore(t *testing.T) {
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
		{codes.Aborted, mamori.KindUnknown},
		{codes.FailedPrecondition, mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.code.String(), func(t *testing.T) {
			err := status.Error(tc.code, "boom")
			if got := mamori.ErrorKind(classifyFirestore(err)); got != tc.want {
				t.Fatalf("ErrorKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyFirestorePreservesStatus(t *testing.T) {
	orig := status.Error(codes.PermissionDenied, "caller lacks datastore.documents.get")
	wrapped := classifyFirestore(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	if got := status.Code(wrapped); got != codes.PermissionDenied {
		t.Fatalf("status.Code(wrapped) = %v, want PermissionDenied; the %%w: %%w "+
			"pattern must keep the gRPC status reachable", got)
	}
}

func TestClassifyFirestoreNilAndPlain(t *testing.T) {
	if err := classifyFirestore(nil); err != nil {
		t.Fatalf("classifyFirestore(nil) = %v, want nil", err)
	}
	plain := errors.New("dial tcp: connection refused")
	if got := mamori.ErrorKind(classifyFirestore(plain)); got != mamori.KindUnknown {
		t.Fatalf("plain error kind = %q, want unknown", got)
	}
}

// TestResolveNotFoundPreservesStatus guards Resolve's status.Code(err) ==
// codes.NotFound pre-check specifically: it must double-wrap (sentinel AND the
// underlying gRPC status), not just the sentinel, so the gRPC status stays
// reachable through status.Code.
func TestResolveNotFoundPreservesStatus(t *testing.T) {
	fake := newFakeStore()
	fake.fail("config", "app", status.Error(codes.NotFound, "no such document"))
	p := New(withBackend(fake))

	_, err := p.Resolve(context.Background(), mustRef(t, "firestore://config/app"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("status.Code(err) = %v, want NotFound; the gRPC status is no longer reachable", got)
	}
}

// TestResolveClassifiesNonNotFoundError exercises classifyFirestore through
// Resolve itself, not just as a direct function call. The conformance
// ErrorClassification case cannot catch a regression here: it injects a
// mamori sentinel directly (not a gRPC status), so it passes even if the
// classifyFirestore call were deleted from Resolve's fallback branch. This
// test injects a real gRPC status through fakeStore, the same shape a live
// Firestore client would return, so it fails if the wiring is removed.
func TestResolveClassifiesNonNotFoundError(t *testing.T) {
	fake := newFakeStore()
	fake.set("config", "app", map[string]interface{}{"value": "v"})
	fake.fail("config", "app", status.Error(codes.PermissionDenied, "caller lacks datastore.documents.get"))
	p := New(withBackend(fake))

	_, err := p.Resolve(context.Background(), mustRef(t, "firestore://config/app"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyFirestore may not be wired into Resolve", got, mamori.KindPermissionDenied)
	}
}

// errBackend is a minimal backend whose snapshot listener immediately fails
// with a fixed error on its first Next() call. It exists purely to prove
// classifyFirestore is wired into Watch's own stream-error branch: fakeStore's
// stream (used by the rest of this package's tests) only ever fails with
// context cancellation, never a gRPC status, so it cannot exercise this path.
type errBackend struct{ err error }

func (b *errBackend) Get(_ context.Context, _, _ string) (snapshot, error) { return nil, b.err }
func (b *errBackend) Snapshots(_ context.Context, _, _ string) (snapshotStream, error) {
	return &errStream{err: b.err}, nil
}
func (b *errBackend) Close() error { return nil }

// errStream is the snapshotStream half of errBackend: every Next() call
// returns the same injected error, modeling a listener that failed outright
// (e.g. the caller's permissions were revoked mid-watch).
type errStream struct{ err error }

func (s *errStream) Next() (snapshot, error) { return nil, s.err }
func (s *errStream) Stop()                   {}

var (
	_ backend        = (*errBackend)(nil)
	_ snapshotStream = (*errStream)(nil)
)

// TestWatchClassifiesNonNotFoundError proves classifyFirestore reaches the
// Watch path, not just Resolve. valueFor's ErrNotFound branch is shared by
// Resolve and Watch, but the gRPC-status classification added to Watch's
// stream.Next() error branch is not: it is exercised only when the listener
// itself fails, which valueFor never sees (valueFor only ever receives a
// successfully-retrieved snapshot). Without this test, deleting the
// classifyFirestore call from Watch's error branch would go unnoticed by
// every other test in this package.
func TestWatchClassifiesNonNotFoundError(t *testing.T) {
	grpcErr := status.Error(codes.PermissionDenied, "caller lacks datastore.documents.get")
	p := New(withBackend(&errBackend{err: grpcErr}))

	ch, err := p.Watch(context.Background(), mustRef(t, "firestore://config/app"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	select {
	case u := <-ch:
		if u.Err == nil {
			t.Fatal("Watch emitted a nil error while the listener was failing")
		}
		if got := mamori.ErrorKind(u.Err); got != mamori.KindPermissionDenied {
			t.Fatalf("ErrorKind(u.Err) = %q, want %q; classifyFirestore may not be wired into Watch's stream error path", got, mamori.KindPermissionDenied)
		}
		if got := status.Code(u.Err); got != codes.PermissionDenied {
			t.Fatalf("status.Code(u.Err) = %v, want PermissionDenied; the gRPC status is no longer reachable", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no Update emitted")
	}
}
