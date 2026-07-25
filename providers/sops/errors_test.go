package sops

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/xavidop/mamori"
)

func TestClassifySops(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"NotExist", &fs.PathError{Op: "open", Path: "secret.enc.yaml", Err: fs.ErrNotExist}, mamori.KindNotFound},
		{"Permission", &fs.PathError{Op: "open", Path: "secret.enc.yaml", Err: fs.ErrPermission}, mamori.KindPermissionDenied},
		{"PlainError", errors.New("gpg: decryption failed: no secret key"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifySops(tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifySops(%v)) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifySopsNilIsNil(t *testing.T) {
	if err := classifySops(nil); err != nil {
		t.Fatalf("classifySops(nil) = %v, want nil", err)
	}
}

// TestClassifySopsNotFoundIsBare guards that the not-found branch stays
// exactly as it was before classifySops existed: a bare mamori.ErrNotFound,
// not additionally wrapped around the original *fs.PathError. Every existing
// sops not-found test (TestResolveNotFound, TestResolveMissingKey) asserts
// only errors.Is, which a bare or a wrapped sentinel would both satisfy, so
// this locks in the specific "unchanged" shape the task requires rather than
// relying on those tests to catch a regression here.
func TestClassifySopsNotFoundIsBare(t *testing.T) {
	got := classifySops(&fs.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist})
	if got != mamori.ErrNotFound {
		t.Fatalf("classifySops(NotExist) = %v, want the bare mamori.ErrNotFound sentinel", got)
	}
}

// TestClassifySopsPreservesOriginalError guards that the permission-denied
// branch does not discard the underlying *fs.PathError: callers who already
// reach it with errors.As must keep working.
func TestClassifySopsPreservesOriginalError(t *testing.T) {
	orig := &fs.PathError{Op: "open", Path: "secret.enc.yaml", Err: fs.ErrPermission}
	wrapped := classifySops(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var pathErr *fs.PathError
	if !errors.As(wrapped, &pathErr) {
		t.Fatalf("errors.As can no longer reach the original *fs.PathError: %v", wrapped)
	}
	if pathErr.Path != "secret.enc.yaml" {
		t.Fatalf("recovered Path = %q, want secret.enc.yaml", pathErr.Path)
	}
}

// TestResolveClassifiesPermissionDenied exercises classifySops through
// Resolve itself, not just as a direct function call. The conformance
// ErrorClassification case (wired via TestConformance's Fail/Clear) cannot
// catch a regression here: it injects the mamori.ErrPermissionDenied sentinel
// directly, which already satisfies errors.Is even if classifySops's
// os.IsPermission branch were deleted, because Resolve's decrypt-error path
// wraps every error with %w regardless of classification. This test instead
// injects a real os-shaped permission error (an *fs.PathError wrapping
// fs.ErrPermission) through the DecryptFunc seam, the same shape a real
// unreadable mounted secret would produce from decrypt.File, so it fails if
// the classifySops wiring is removed from Resolve.
func TestResolveClassifiesPermissionDenied(t *testing.T) {
	path := writeFile(t, "app.enc.yaml", "ENC[...]")
	osErr := &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
	p := New(WithDecrypt(func(_, _ string) ([]byte, error) { return nil, osErr }))

	ref, err := mamori.ParseRef("sops://" + path)
	if err != nil {
		t.Fatal(err)
	}
	_, resolveErr := p.Resolve(context.Background(), ref)
	if resolveErr == nil {
		t.Fatal("Resolve returned a nil error while decrypt was failing")
	}
	if got := mamori.ErrorKind(resolveErr); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifySops may not be wired into Resolve", got, mamori.KindPermissionDenied)
	}
	var pathErr *fs.PathError
	if !errors.As(resolveErr, &pathErr) {
		t.Fatalf("errors.As can no longer reach the underlying *fs.PathError: %v", resolveErr)
	}
}
