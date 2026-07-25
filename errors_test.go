package mamori

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestProviderErrorIsNotFound(t *testing.T) {
	err := &ProviderError{Scheme: "aws-sm", Ref: "prod/db", Err: ErrNotFound}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("errors.Is(ProviderError{ErrNotFound}, ErrNotFound) = false, want true")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("errors.As did not match *ProviderError")
	}
	if pe.Scheme != "aws-sm" {
		t.Errorf("Scheme = %q, want aws-sm", pe.Scheme)
	}
}

func TestValidationErrorUnwrap(t *testing.T) {
	base := errors.New("field Workers must be <= 256")
	err := &ValidationError{Err: base}
	if !errors.Is(err, base) {
		t.Fatalf("ValidationError does not unwrap to base")
	}
}

func TestStaleErrorUnwrap(t *testing.T) {
	err := &StaleError{Ref: "vault://x", Err: ErrNotFound}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("StaleError does not unwrap to ErrNotFound")
	}
}

func TestErrorKindClassifiesSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Kind
	}{
		{"NotFound", ErrNotFound, KindNotFound},
		{"PermissionDenied", ErrPermissionDenied, KindPermissionDenied},
		{"Unauthenticated", ErrUnauthenticated, KindUnauthenticated},
		{"Unavailable", ErrUnavailable, KindUnavailable},
		{"RateLimited", ErrRateLimited, KindRateLimited},
		{"Invalid", ErrInvalid, KindInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorKind(tc.err); got != tc.want {
				t.Fatalf("ErrorKind(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestErrorKindNilIsEmpty(t *testing.T) {
	if got := ErrorKind(nil); got != "" {
		t.Fatalf("ErrorKind(nil) = %q, want empty string", got)
	}
}

func TestErrorKindUnrecognizedIsUnknown(t *testing.T) {
	if got := ErrorKind(errors.New("something odd")); got != KindUnknown {
		t.Fatalf("ErrorKind(unrecognized) = %q, want %q", got, KindUnknown)
	}
}

func TestErrorKindThroughWrapping(t *testing.T) {
	// The %w verb is how providers are told to classify. If a provider uses %v
	// instead, the chain breaks and this is the behavior that catches it.
	wrapped := fmt.Errorf("secretsmanager: %w: AccessDeniedException", ErrPermissionDenied)
	if got := ErrorKind(wrapped); got != KindPermissionDenied {
		t.Fatalf("ErrorKind(wrapped) = %q, want %q", got, KindPermissionDenied)
	}

	flattened := fmt.Errorf("secretsmanager: %v: AccessDeniedException", ErrPermissionDenied)
	if got := ErrorKind(flattened); got != KindUnknown {
		t.Fatalf("ErrorKind(flattened with %%v) = %q, want %q", got, KindUnknown)
	}
}

func TestErrorKindThroughProviderError(t *testing.T) {
	pe := &ProviderError{
		Scheme: "aws-sm",
		Ref:    "aws-sm://prod/db#password",
		Err:    fmt.Errorf("%w: denied by policy", ErrPermissionDenied),
	}
	if got := ErrorKind(pe); got != KindPermissionDenied {
		t.Fatalf("ErrorKind(ProviderError) = %q, want %q", got, KindPermissionDenied)
	}
}

func TestErrorKindThroughJoin(t *testing.T) {
	joined := errors.Join(errors.New("first"), ErrRateLimited)
	if got := ErrorKind(joined); got != KindRateLimited {
		t.Fatalf("ErrorKind(joined) = %q, want %q", got, KindRateLimited)
	}
}

func TestErrorKindNotFoundWinsOverOthers(t *testing.T) {
	// NotFound is the only kind that drives behavior (defaults and optional
	// handling), so when an error somehow carries two sentinels it must win.
	both := errors.Join(ErrUnavailable, ErrNotFound)
	if got := ErrorKind(both); got != KindNotFound {
		t.Fatalf("ErrorKind(NotFound+Unavailable) = %q, want %q", got, KindNotFound)
	}
}

func TestSentinelForRoundTrips(t *testing.T) {
	kinds := []Kind{
		KindNotFound, KindPermissionDenied, KindUnauthenticated,
		KindUnavailable, KindRateLimited, KindInvalid,
	}
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			sentinel := SentinelFor(k)
			if sentinel == nil {
				t.Fatalf("SentinelFor(%q) returned nil", k)
			}
			if got := ErrorKind(sentinel); got != k {
				t.Fatalf("ErrorKind(SentinelFor(%q)) = %q, want %q", k, got, k)
			}
		})
	}
}

func TestSentinelForUnknownIsNil(t *testing.T) {
	if got := SentinelFor(KindUnknown); got != nil {
		t.Fatalf("SentinelFor(KindUnknown) = %v, want nil", got)
	}
	if got := SentinelFor(""); got != nil {
		t.Fatalf("SentinelFor(\"\") = %v, want nil", got)
	}
}

// localSdkErr stands in for a provider SDK's own error type, the way
// smithy.APIError or a Vault client error would show up in real code.
type localSdkErr struct{ code string }

func (e *localSdkErr) Error() string { return "sdk error: " + e.code }

// TestErrorKindPreservesSdkErrorForErrorsAs proves the documented wrapping
// pattern, fmt.Errorf("%w: %w", sentinel, err), lets a caller recover BOTH the
// mamori classification via ErrorKind AND the original SDK error type via
// errors.As. A single %w with the SDK error formatted as %v would satisfy
// ErrorKind but break errors.As, which is the mistake this test guards
// against.
func TestErrorKindPreservesSdkErrorForErrorsAs(t *testing.T) {
	sdkErr := &localSdkErr{code: "AccessDeniedException"}
	wrapped := fmt.Errorf("%w: %w", ErrPermissionDenied, sdkErr)

	if got := ErrorKind(wrapped); got != KindPermissionDenied {
		t.Fatalf("ErrorKind(wrapped) = %q, want %q", got, KindPermissionDenied)
	}

	var recovered *localSdkErr
	if !errors.As(wrapped, &recovered) {
		t.Fatalf("errors.As did not recover *localSdkErr from the doubly-wrapped error")
	}
	if recovered.code != "AccessDeniedException" {
		t.Errorf("recovered.code = %q, want AccessDeniedException", recovered.code)
	}
}

func TestErrorKindContextDeadlineExceededIsUnavailable(t *testing.T) {
	if got := ErrorKind(context.DeadlineExceeded); got != KindUnavailable {
		t.Fatalf("ErrorKind(context.DeadlineExceeded) = %q, want %q", got, KindUnavailable)
	}

	wrapped := fmt.Errorf("dial tcp: %w", context.DeadlineExceeded)
	if got := ErrorKind(wrapped); got != KindUnavailable {
		t.Fatalf("ErrorKind(wrapped DeadlineExceeded) = %q, want %q", got, KindUnavailable)
	}
}

func TestErrorKindContextCanceledIsUnknown(t *testing.T) {
	if got := ErrorKind(context.Canceled); got != KindUnknown {
		t.Fatalf("ErrorKind(context.Canceled) = %q, want %q", got, KindUnknown)
	}

	wrapped := fmt.Errorf("dial tcp: %w", context.Canceled)
	if got := ErrorKind(wrapped); got != KindUnknown {
		t.Fatalf("ErrorKind(wrapped Canceled) = %q, want %q", got, KindUnknown)
	}
}

func TestErrorKindSentinelWinsOverWrappedDeadlineExceeded(t *testing.T) {
	// An explicit mamori sentinel must take precedence over the automatic
	// context.DeadlineExceeded classification, the same way NotFound wins
	// over other sentinels in TestErrorKindNotFoundWinsOverOthers.
	both := fmt.Errorf("%w: %w", ErrRateLimited, context.DeadlineExceeded)
	if got := ErrorKind(both); got != KindRateLimited {
		t.Fatalf("ErrorKind(RateLimited+DeadlineExceeded) = %q, want %q", got, KindRateLimited)
	}
}
