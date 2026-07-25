package launchdarkly

import (
	"errors"
	"testing"

	"github.com/launchdarkly/go-sdk-common/v3/ldreason"
	"github.com/xavidop/mamori"
)

func TestClassifyReason(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name   string
		reason ldreason.EvaluationReason
		err    error
		want   mamori.Kind
	}{
		{"ClientNotReady", ldreason.NewEvalReasonError(ldreason.EvalErrorClientNotReady), boom, mamori.KindUnavailable},
		// MalformedFlag, UserNotSpecified, and Exception are internal/data
		// conditions with no confirmed, single real-world cause (see
		// classifyReason's doc comment), so they are deliberately left
		// unclassified. These cases lock that choice in against someone
		// "helpfully" adding a guess later.
		{"MalformedFlagDeliberatelyUnmapped", ldreason.NewEvalReasonError(ldreason.EvalErrorMalformedFlag), boom, mamori.KindUnknown},
		{"UserNotSpecifiedDeliberatelyUnmapped", ldreason.NewEvalReasonError(ldreason.EvalErrorUserNotSpecified), boom, mamori.KindUnknown},
		{"ExceptionDeliberatelyUnmapped", ldreason.NewEvalReasonError(ldreason.EvalErrorException), boom, mamori.KindUnknown},
		{"WrongTypeDeliberatelyUnmapped", ldreason.NewEvalReasonError(ldreason.EvalErrorWrongType), boom, mamori.KindUnknown},
		{"NonErrorReason", ldreason.NewEvalReasonFallthrough(), boom, mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mamori.ErrorKind(classifyReason(tc.reason, tc.err))
			if got != tc.want {
				t.Fatalf("ErrorKind(classifyReason(%s, err)) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestClassifyReasonNilErrIsNil(t *testing.T) {
	if err := classifyReason(ldreason.NewEvalReasonError(ldreason.EvalErrorClientNotReady), nil); err != nil {
		t.Fatalf("classifyReason(_, nil) = %v, want nil", err)
	}
}

// TestClassifyReasonPreservesSdkError guards the double-%w wrap: a caller that
// already reaches the original SDK error with errors.Is must keep working
// after classification, not just see the mamori sentinel.
func TestClassifyReasonPreservesSdkError(t *testing.T) {
	orig := errors.New("client not initialized")
	got := classifyReason(ldreason.NewEvalReasonError(ldreason.EvalErrorClientNotReady), orig)
	if !errors.Is(got, mamori.ErrUnavailable) {
		t.Fatalf("classification lost: %v", got)
	}
	if !errors.Is(got, orig) {
		t.Fatalf("errors.Is can no longer reach the original SDK error: %v", got)
	}
}
