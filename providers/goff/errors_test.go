package goff

import (
	"errors"
	"testing"

	"github.com/thomaspoignant/go-feature-flag/modules/core/flag"
	"github.com/thomaspoignant/go-feature-flag/modules/core/model"
	"github.com/xavidop/mamori"
)

func TestClassifyGoff(t *testing.T) {
	cases := []struct {
		name string
		res  model.RawVarResult
		err  error
		want mamori.Kind
	}{
		{"ProviderNotReady", model.RawVarResult{ErrorCode: flag.ErrorCodeProviderNotReady, ErrorDetails: "cache not yet loaded"}, nil, mamori.KindUnavailable},
		{"ParseError", model.RawVarResult{ErrorCode: flag.ErrorCodeParseError, ErrorDetails: "invalid yaml"}, nil, mamori.KindInvalid},
		{"TypeMismatch", model.RawVarResult{ErrorCode: flag.ErrorCodeTypeMismatch, ErrorDetails: "flag is not a boolean"}, nil, mamori.KindInvalid},
		{"InvalidContext", model.RawVarResult{ErrorCode: flag.ErrorCodeInvalidContext, ErrorDetails: "evaluation context is invalid"}, nil, mamori.KindInvalid},
		{"TargetingKeyMissing", model.RawVarResult{ErrorCode: flag.ErrorCodeTargetingKeyMissing, ErrorDetails: "no targeting key"}, nil, mamori.KindInvalid},
		// GENERAL is go-feature-flag's catch-all for a failure that fits no other
		// code, so mapping it would be a guess. This case locks that choice in
		// against someone "helpfully" adding it later.
		{"GeneralDeliberatelyUnmapped", model.RawVarResult{ErrorCode: flag.ErrorCodeGeneral, ErrorDetails: "something went wrong"}, nil, mamori.KindUnknown},
		{"UnrecognizedFutureCode", model.RawVarResult{ErrorCode: "SOME_FUTURE_CODE", ErrorDetails: "boom"}, nil, mamori.KindUnknown},
		{"PlainErrorNoCode", model.RawVarResult{}, errors.New("connection reset"), mamori.KindUnknown},
		{"EmptyResultNoError", model.RawVarResult{}, nil, mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifyGoff(tc.res, tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyGoff(%+v, %v)) = %q, want %q", tc.res, tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyGoffPreservesUnderlyingError guards that a caller who already
// reaches the RawVariation error with errors.Is/errors.As keeps working after
// classification wraps it with a mamori sentinel.
func TestClassifyGoffPreservesUnderlyingError(t *testing.T) {
	orig := errors.New("provider not ready: cache empty")
	res := model.RawVarResult{ErrorCode: flag.ErrorCodeProviderNotReady}
	wrapped := classifyGoff(res, orig)

	if !errors.Is(wrapped, mamori.ErrUnavailable) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	if !errors.Is(wrapped, orig) {
		t.Fatalf("errors.Is can no longer reach the original error: %v", wrapped)
	}
}

// TestClassifyGoffNeverNil documents and locks in a deliberate deviation from
// classifyPostgres/classifyAWS/classifyGCP: those take a single error and pass
// nil straight through. classifyGoff's detection surface is a struct field
// (model.RawVarResult.ErrorCode), not the error itself, and every call site in
// Resolve only invokes it once a failure is already known (err != nil, or
// res.Failed). So there is no nil-passthrough case to preserve, and
// classifyGoff always returns a non-nil error.
func TestClassifyGoffNeverNil(t *testing.T) {
	if got := classifyGoff(model.RawVarResult{}, nil); got == nil {
		t.Fatal("classifyGoff(zero-value result, nil) = nil, want a non-nil error")
	}
}
