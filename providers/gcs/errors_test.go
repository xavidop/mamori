package gcs

import (
	"context"
	"errors"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/xavidop/mamori"
	"google.golang.org/api/googleapi"
)

func TestClassifyGCS(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"ObjectNotExist", storage.ErrObjectNotExist, mamori.KindNotFound},
		{"BucketNotExist", storage.ErrBucketNotExist, mamori.KindNotFound},
		{"NotFound404", &googleapi.Error{Code: 404}, mamori.KindNotFound},
		{"Forbidden403", &googleapi.Error{Code: 403}, mamori.KindPermissionDenied},
		{"Unauthorized401", &googleapi.Error{Code: 401}, mamori.KindUnauthenticated},
		{"TooManyRequests429", &googleapi.Error{Code: 429}, mamori.KindRateLimited},
		{"InternalServerError500", &googleapi.Error{Code: 500}, mamori.KindUnavailable},
		{"ServiceUnavailable503", &googleapi.Error{Code: 503}, mamori.KindUnavailable},
		{"BadRequest400", &googleapi.Error{Code: 400}, mamori.KindInvalid},
		{"UnmappedCode", &googleapi.Error{Code: 418}, mamori.KindUnknown},
		{"PlainError", errors.New("connection reset"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifyGCS(tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyGCS(%v)) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyGCSNil(t *testing.T) {
	if err := classifyGCS(nil); err != nil {
		t.Fatalf("classifyGCS(nil) = %v, want nil", err)
	}
}

func TestClassifyGCSPreservesSdkError(t *testing.T) {
	// Callers who already reach the SDK error with errors.As must keep working.
	orig := &googleapi.Error{Code: 403, Message: "denied"}
	wrapped := classifyGCS(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var gerr *googleapi.Error
	if !errors.As(wrapped, &gerr) {
		t.Fatalf("errors.As can no longer reach *googleapi.Error: %v", wrapped)
	}
	if gerr.Code != 403 {
		t.Fatalf("recovered code = %d, want 403", gerr.Code)
	}
}

// TestResolveClassifiesNonNotFoundError exercises classifyGCS through Resolve
// itself, not just as a direct function call. Neither existing kind of test
// catches a regression here: TestClassifyGCS calls classifyGCS directly, so it
// passes whether or not Resolve actually uses it, and the providertest
// ErrorClassification case injects a mamori sentinel (not a *googleapi.Error),
// which classifyGCS returns unchanged either way - it would pass even with
// Resolve's classifyGCS call deleted. This test injects a real GCS SDK error
// through fakeGCS.fail, the same shape a live GCS bucket read would return, so
// it fails if Resolve's fallback stops routing through classifyGCS.
func TestResolveClassifiesNonNotFoundError(t *testing.T) {
	fake := newFakeGCS()
	fake.put("bkt", "prod/db", "s3cr3t")
	fake.fail("bkt", "prod/db", &googleapi.Error{Code: 403})
	p := New(WithClient(fake))

	_, err := p.Resolve(context.Background(), parse(t, "gcs://bkt/prod/db"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyGCS may not be wired into Resolve", got, mamori.KindPermissionDenied)
	}
}
