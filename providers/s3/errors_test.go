package s3

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/xavidop/mamori"
)

func TestClassifyS3(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"NoSuchKey", &smithy.GenericAPIError{Code: "NoSuchKey"}, mamori.KindNotFound},
		{"NoSuchBucket", &smithy.GenericAPIError{Code: "NoSuchBucket"}, mamori.KindNotFound},
		{"NoSuchVersion", &smithy.GenericAPIError{Code: "NoSuchVersion"}, mamori.KindNotFound},
		{"GenericNotFound", &smithy.GenericAPIError{Code: "NotFound"}, mamori.KindNotFound},
		{"AccessDenied", &smithy.GenericAPIError{Code: "AccessDenied"}, mamori.KindPermissionDenied},
		{"AllAccessDisabled", &smithy.GenericAPIError{Code: "AllAccessDisabled"}, mamori.KindPermissionDenied},
		{"InvalidAccessKeyId", &smithy.GenericAPIError{Code: "InvalidAccessKeyId"}, mamori.KindUnauthenticated},
		{"SignatureDoesNotMatch", &smithy.GenericAPIError{Code: "SignatureDoesNotMatch"}, mamori.KindUnauthenticated},
		{"ExpiredToken", &smithy.GenericAPIError{Code: "ExpiredToken"}, mamori.KindUnauthenticated},
		{"InvalidToken", &smithy.GenericAPIError{Code: "InvalidToken"}, mamori.KindUnauthenticated},
		{"TokenRefreshRequired", &smithy.GenericAPIError{Code: "TokenRefreshRequired"}, mamori.KindUnauthenticated},
		{"SlowDown", &smithy.GenericAPIError{Code: "SlowDown"}, mamori.KindRateLimited},
		{"ServiceUnavailable", &smithy.GenericAPIError{Code: "ServiceUnavailable"}, mamori.KindUnavailable},
		{"InternalError", &smithy.GenericAPIError{Code: "InternalError"}, mamori.KindUnavailable},
		{"InvalidRequest", &smithy.GenericAPIError{Code: "InvalidRequest"}, mamori.KindInvalid},
		{"InvalidArgument", &smithy.GenericAPIError{Code: "InvalidArgument"}, mamori.KindInvalid},
		{"MalformedXML", &smithy.GenericAPIError{Code: "MalformedXML"}, mamori.KindInvalid},
		{"UnmappedCode", &smithy.GenericAPIError{Code: "SomeFutureCode"}, mamori.KindUnknown},
		{"PlainError", errors.New("connection reset"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifyS3(tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyS3(%v)) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyS3Nil(t *testing.T) {
	if err := classifyS3(nil); err != nil {
		t.Fatalf("classifyS3(nil) = %v, want nil", err)
	}
}

func TestClassifyS3PreservesSdkError(t *testing.T) {
	// Callers who already reach the SDK error with errors.As must keep working.
	orig := &smithy.GenericAPIError{Code: "AccessDenied", Message: "denied"}
	wrapped := classifyS3(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var api smithy.APIError
	if !errors.As(wrapped, &api) {
		t.Fatalf("errors.As can no longer reach smithy.APIError: %v", wrapped)
	}
	if api.ErrorCode() != "AccessDenied" {
		t.Fatalf("recovered code = %q, want AccessDenied", api.ErrorCode())
	}
}

// TestResolveClassifiesNonNotFoundError exercises classifyS3 through Resolve
// itself, not just as a direct function call. Neither existing kind of test
// catches a regression here: TestClassifyS3 calls classifyS3 directly, so it
// passes whether or not mapError actually uses it, and the providertest
// ErrorClassification case injects a mamori sentinel (not a smithy.APIError),
// which classifyS3 returns unchanged either way - it would pass even with
// mapError's classifyS3 call deleted. This test injects a real S3 SDK error
// through fakeS3.fail, the same shape a live S3 (or MinIO/R2) call would
// return, so it fails if mapError's fallback stops routing through
// classifyS3.
func TestResolveClassifiesNonNotFoundError(t *testing.T) {
	fake := newFakeS3()
	fake.put(testBucket, "prod/db", "s3cr3t")
	fake.fail(testBucket, "prod/db", &smithy.GenericAPIError{Code: "AccessDenied"})
	p := newWithClient(fake)

	_, err := p.Resolve(context.Background(), mustParse(t, "s3://"+testBucket+"/prod/db"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyS3 may not be wired into mapError", got, mamori.KindPermissionDenied)
	}
}
