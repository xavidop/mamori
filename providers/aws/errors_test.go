package aws

import (
	"context"
	"errors"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"
	"github.com/xavidop/mamori"
)

func TestClassifyAWS(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"ModeledNotFound", &smtypes.ResourceNotFoundException{}, mamori.KindNotFound},
		{"AccessDenied", &smithy.GenericAPIError{Code: "AccessDeniedException"}, mamori.KindPermissionDenied},
		{"UnrecognizedClient", &smithy.GenericAPIError{Code: "UnrecognizedClientException"}, mamori.KindUnauthenticated},
		{"ExpiredToken", &smithy.GenericAPIError{Code: "ExpiredTokenException"}, mamori.KindUnauthenticated},
		{"Throttling", &smithy.GenericAPIError{Code: "ThrottlingException"}, mamori.KindRateLimited},
		{"TooManyRequests", &smithy.GenericAPIError{Code: "TooManyRequestsException"}, mamori.KindRateLimited},
		{"InternalService", &smithy.GenericAPIError{Code: "InternalServiceError"}, mamori.KindUnavailable},
		{"ServiceUnavailable", &smithy.GenericAPIError{Code: "ServiceUnavailable"}, mamori.KindUnavailable},
		{"InvalidParameter", &smithy.GenericAPIError{Code: "InvalidParameterException"}, mamori.KindInvalid},
		{"InvalidRequest", &smithy.GenericAPIError{Code: "InvalidRequestException"}, mamori.KindInvalid},
		{"ValidationException", &smithy.GenericAPIError{Code: "ValidationException"}, mamori.KindInvalid},
		{"InvalidKeyId", &smithy.GenericAPIError{Code: "InvalidKeyId"}, mamori.KindInvalid},
		{"ParameterVersionNotFound", &smithy.GenericAPIError{Code: "ParameterVersionNotFound"}, mamori.KindNotFound},
		{"BadRequest", &smithy.GenericAPIError{Code: "BadRequestException"}, mamori.KindInvalid},
		{"InternalServerException", &smithy.GenericAPIError{Code: "InternalServerException"}, mamori.KindUnavailable},
		{"UnmappedCode", &smithy.GenericAPIError{Code: "SomeFutureException"}, mamori.KindUnknown},
		{"PlainError", errors.New("connection reset"), mamori.KindUnknown},
		// DecryptionFailure is a real Secrets Manager error, but it can mean a
		// key policy problem, a disabled key, or a KMS outage. It does not map
		// cleanly to one kind, so it is deliberately left unclassified. This
		// case locks that choice in against someone "helpfully" adding it later.
		{"DecryptionFailureDeliberatelyUnmapped", &smithy.GenericAPIError{Code: "DecryptionFailure"}, mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifyAWS(tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyAWS(%v)) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyAWSPreservesSdkError(t *testing.T) {
	// Callers who already reach the SDK error with errors.As must keep working.
	orig := &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"}
	wrapped := classifyAWS(orig)

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

func TestClassifyAWSNilIsNil(t *testing.T) {
	if err := classifyAWS(nil); err != nil {
		t.Fatalf("classifyAWS(nil) = %v, want nil", err)
	}
}

func TestMapSMErrorStillReportsNotFound(t *testing.T) {
	ref, err := mamori.ParseRef("aws-sm://prod/db")
	if err != nil {
		t.Fatal(err)
	}
	got := mapSMError(ref, &smtypes.ResourceNotFoundException{})
	if !errors.Is(got, mamori.ErrNotFound) {
		t.Fatalf("mapSMError lost ErrNotFound: %v", got)
	}
	if !strings.Contains(got.Error(), "prod/db") {
		t.Errorf("error message no longer names the ref: %v", got)
	}
}

// TestMapSMErrorNotFoundPreservesSdkError guards the not-found branch
// specifically: it must double-wrap (sentinel AND underlying SDK error), not
// just the sentinel, so errors.As can still reach *smtypes.ResourceNotFoundException
// as the README's error table promises.
func TestMapSMErrorNotFoundPreservesSdkError(t *testing.T) {
	ref, err := mamori.ParseRef("aws-sm://prod/db")
	if err != nil {
		t.Fatal(err)
	}
	orig := &smtypes.ResourceNotFoundException{Message: awssdk.String("Secrets Manager can't find the specified secret.")}
	got := mapSMError(ref, orig)

	if !errors.Is(got, mamori.ErrNotFound) {
		t.Fatalf("mapSMError lost ErrNotFound: %v", got)
	}
	var nf *smtypes.ResourceNotFoundException
	if !errors.As(got, &nf) {
		t.Fatalf("errors.As can no longer reach *smtypes.ResourceNotFoundException: %v", got)
	}
}

// TestSMResolveBatchClassifiesError locks in that an IAM denial surfacing from
// BatchGetSecretValue is routed through classifyAWS, not just wrapped raw.
func TestSMResolveBatchClassifiesError(t *testing.T) {
	fake := newFakeSM()
	fake.set("prod/db", "s3cr3t")
	fake.fail("prod/db", &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"})
	p := newSMWithClient(fake)

	_, err := p.ResolveBatch(context.Background(), []mamori.Ref{mustParse(t, "aws-sm://prod/db")})
	if err == nil {
		t.Fatal("expected error from ResolveBatch")
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("ResolveBatch error not classified as permission_denied: %v", err)
	}
}

// TestPSResolveBatchClassifiesError locks in that an IAM denial surfacing from
// GetParameters is routed through classifyAWS, not just wrapped raw.
func TestPSResolveBatchClassifiesError(t *testing.T) {
	fake := newFakeSSM()
	fake.set("/app/log-level", "debug")
	fake.fail("/app/log-level", &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"})
	p := newPSWithClient(fake)

	_, err := p.ResolveBatch(context.Background(), []mamori.Ref{mustParse(t, "aws-ps:///app/log-level")})
	if err == nil {
		t.Fatal("expected error from ResolveBatch")
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("ResolveBatch error not classified as permission_denied: %v", err)
	}
}

// TestSMResolveClassifiesNonNotFoundError exercises classifyAWS through
// Resolve itself, not just as a direct function call. Neither existing kind
// of test catches a regression here: TestClassifyAWS calls classifyAWS
// directly, so it passes whether or not Resolve/mapSMError actually use it,
// and the providertest ErrorClassification case injects a mamori sentinel
// (not a smithy.APIError), which classifyAWS returns unchanged either way -
// it would pass even with mapSMError's classifyAWS call deleted. This test
// injects a real AWS SDK error through fakeSM.fail, the same shape a live
// Secrets Manager call would return, so it fails if mapSMError's fallback
// stops routing through classifyAWS.
func TestSMResolveClassifiesNonNotFoundError(t *testing.T) {
	fake := newFakeSM()
	fake.set("prod/db", "s3cr3t")
	fake.fail("prod/db", &smithy.GenericAPIError{Code: "AccessDeniedException"})
	p := newSMWithClient(fake)

	_, err := p.Resolve(context.Background(), mustParse(t, "aws-sm://prod/db"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyAWS may not be wired into mapSMError", got, mamori.KindPermissionDenied)
	}
}

// TestPSResolveClassifiesNonNotFoundError is the Parameter Store equivalent of
// TestSMResolveClassifiesNonNotFoundError: it injects a real AWS SDK error
// through fakeSSM.fail and asserts on the kind Resolve returns, so it fails
// if mapPSError's fallback stops routing through classifyAWS.
func TestPSResolveClassifiesNonNotFoundError(t *testing.T) {
	fake := newFakeSSM()
	fake.set("/app/log-level", "debug")
	fake.fail("/app/log-level", &smithy.GenericAPIError{Code: "AccessDeniedException"})
	p := newPSWithClient(fake)

	_, err := p.Resolve(context.Background(), mustParse(t, "aws-ps:///app/log-level"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyAWS may not be wired into mapPSError", got, mamori.KindPermissionDenied)
	}
}

// TestAppConfigResolvePreservesClassification proves the classification
// actually reaches a caller through this provider's own error path, the same
// way TestSMResolveClassifiesNonNotFoundError and
// TestPSResolveClassifiesNonNotFoundError do for their providers. Getting the
// classification table right and getting the wrapping right are independent
// mistakes, and this locks in the second one.
func TestAppConfigResolvePreservesClassification(t *testing.T) {
	fake := newFakeAppConfig()
	fake.set("myapp/prod/flags", `{"a":1}`)
	fake.fail("myapp/prod/flags", &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"})
	p := newAppConfigWithClient(fake)

	ref, err := mamori.ParseRef("aws-appconfig://myapp/prod/flags")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	_, err = p.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("Resolve returned nil error")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyAWS may not be wired into the AppConfig resolve path", got, mamori.KindPermissionDenied)
	}
}
