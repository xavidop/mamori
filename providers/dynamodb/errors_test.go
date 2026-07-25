package dynamodb

import (
	"context"
	"errors"
	"testing"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
	"github.com/xavidop/mamori"
)

func TestClassifyDynamoDB(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"ResourceNotFoundException", &smithy.GenericAPIError{Code: "ResourceNotFoundException"}, mamori.KindNotFound},
		{"AccessDeniedException", &smithy.GenericAPIError{Code: "AccessDeniedException"}, mamori.KindPermissionDenied},
		{"UnrecognizedClientException", &smithy.GenericAPIError{Code: "UnrecognizedClientException"}, mamori.KindUnauthenticated},
		{"ExpiredTokenException", &smithy.GenericAPIError{Code: "ExpiredTokenException"}, mamori.KindUnauthenticated},
		{"InvalidSignatureException", &smithy.GenericAPIError{Code: "InvalidSignatureException"}, mamori.KindUnauthenticated},
		// The five cases below are AWS-wide codes (not DynamoDB-specific) that
		// providers/aws's classifyAWS already maps; they are mirrored here so a
		// shared root cause classifies identically whether it surfaces from
		// DynamoDB or another AWS service.
		{"MissingAuthenticationToken", &smithy.GenericAPIError{Code: "MissingAuthenticationToken"}, mamori.KindUnauthenticated},
		{"IncompleteSignature", &smithy.GenericAPIError{Code: "IncompleteSignature"}, mamori.KindUnauthenticated},
		{"ProvisionedThroughputExceededException", &smithy.GenericAPIError{Code: "ProvisionedThroughputExceededException"}, mamori.KindRateLimited},
		{"ThrottlingException", &smithy.GenericAPIError{Code: "ThrottlingException"}, mamori.KindRateLimited},
		{"RequestLimitExceeded", &smithy.GenericAPIError{Code: "RequestLimitExceeded"}, mamori.KindRateLimited},
		{"Throttling", &smithy.GenericAPIError{Code: "Throttling"}, mamori.KindRateLimited},
		{"TooManyRequestsException", &smithy.GenericAPIError{Code: "TooManyRequestsException"}, mamori.KindRateLimited},
		{"InternalServerError", &smithy.GenericAPIError{Code: "InternalServerError"}, mamori.KindUnavailable},
		{"ServiceUnavailable", &smithy.GenericAPIError{Code: "ServiceUnavailable"}, mamori.KindUnavailable},
		{"InternalFailure", &smithy.GenericAPIError{Code: "InternalFailure"}, mamori.KindUnavailable},
		{"ValidationException", &smithy.GenericAPIError{Code: "ValidationException"}, mamori.KindInvalid},
		{"UnmappedCode", &smithy.GenericAPIError{Code: "SomeFutureCode"}, mamori.KindUnknown},
		{"PlainError", errors.New("connection reset"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifyDynamoDB(tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyDynamoDB(%v)) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyDynamoDBNil(t *testing.T) {
	if err := classifyDynamoDB(nil); err != nil {
		t.Fatalf("classifyDynamoDB(nil) = %v, want nil", err)
	}
}

func TestClassifyDynamoDBPreservesSdkError(t *testing.T) {
	// Callers who already reach the SDK error with errors.As must keep working.
	orig := &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"}
	wrapped := classifyDynamoDB(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var api smithy.APIError
	if !errors.As(wrapped, &api) {
		t.Fatalf("errors.As can no longer reach smithy.APIError: %v", wrapped)
	}
	if api.ErrorCode() != "AccessDeniedException" {
		t.Fatalf("recovered code = %q, want AccessDeniedException", api.ErrorCode())
	}
}

// TestResolveClassifiesNonNotFoundError exercises classifyDynamoDB through
// Resolve itself, not just as a direct function call. Neither existing kind of
// test catches a regression here: TestClassifyDynamoDB calls classifyDynamoDB
// directly, so it passes whether or not mapError actually uses it, and the
// providertest ErrorClassification case injects a mamori sentinel (not a
// smithy.APIError), which classifyDynamoDB returns unchanged either way - it
// would pass even with mapError's classifyDynamoDB call deleted. This test
// injects a real DynamoDB SDK error through fakeDDB.fail, the same shape a live
// DynamoDB call would return, so it fails if mapError's fallback stops routing
// through classifyDynamoDB.
func TestResolveClassifiesNonNotFoundError(t *testing.T) {
	fake := newFakeDDB()
	fake.put("t", map[string]ddbtypes.AttributeValue{"pk": s("k"), "value": s("s3cr3t")}, "pk")
	fake.fail("t", "k", &smithy.GenericAPIError{Code: "AccessDeniedException"})
	p := newWithClient(fake)

	_, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://t/k#value"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyDynamoDB may not be wired into mapError", got, mamori.KindPermissionDenied)
	}
}
