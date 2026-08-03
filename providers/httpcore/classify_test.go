package httpcore

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

func TestClassifyStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   mamori.Kind
	}{
		{"ok", http.StatusOK, ""},
		{"created", http.StatusCreated, ""},
		{"not modified", http.StatusNotModified, ""},
		{"bad request", http.StatusBadRequest, mamori.KindInvalid},
		// 422 must not fall through to the default. The default kind is
		// transient, so mamori would back off and retry a request that was well
		// formed and semantically wrong, which retrying can never fix.
		{"unprocessable entity", http.StatusUnprocessableEntity, mamori.KindInvalid},
		{"unauthorized", http.StatusUnauthorized, mamori.KindUnauthenticated},
		{"forbidden", http.StatusForbidden, mamori.KindPermissionDenied},
		{"not found", http.StatusNotFound, mamori.KindNotFound},
		{"request timeout", http.StatusRequestTimeout, mamori.KindRateLimited},
		{"too many requests", http.StatusTooManyRequests, mamori.KindRateLimited},
		{"internal error", http.StatusInternalServerError, mamori.KindUnavailable},
		{"bad gateway", http.StatusBadGateway, mamori.KindUnavailable},
		{"teapot", http.StatusTeapot, mamori.KindUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ClassifyStatus(tt.status, "")
			if tt.want == "" {
				if err != nil {
					t.Fatalf("ClassifyStatus(%d) = %v, want nil", tt.status, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ClassifyStatus(%d) = nil, want %s", tt.status, tt.want)
			}
			if got := mamori.ErrorKind(err); got != tt.want {
				t.Fatalf("ClassifyStatus(%d) kind = %s, want %s", tt.status, got, tt.want)
			}
		})
	}
}

// TestClassifyStatusIncludesDetail proves the caller-supplied detail reaches the
// message, since that is the only channel a provider has for a vendor's error
// text.
func TestClassifyStatusIncludesDetail(t *testing.T) {
	err := ClassifyStatus(http.StatusForbidden, "token lacks secrets:read")
	if err == nil {
		t.Fatal("ClassifyStatus returned nil")
	}
	if !strings.Contains(err.Error(), "token lacks secrets:read") {
		t.Fatalf("detail missing from %q", err.Error())
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("errors.Is(ErrPermissionDenied) = false for %v", err)
	}
}

// TestClassifyStatusNamesAnUnknownVendorStatus pins that a status http.StatusText
// does not know renders without an orphaned space. Cloudflare's 520 through 527
// are exactly the codes the backends this package targets emit, so
// "http 520 : mamori: unavailable" is reachable, not hypothetical.
func TestClassifyStatusNamesAnUnknownVendorStatus(t *testing.T) {
	err := ClassifyStatus(520, "")
	if err == nil {
		t.Fatal("ClassifyStatus(520) returned nil")
	}
	if strings.Contains(err.Error(), "520 :") {
		t.Fatalf("orphaned space before the colon in %q", err.Error())
	}
	if want := "httpcore: http 520: mamori: unavailable"; err.Error() != want {
		t.Fatalf("ClassifyStatus(520) = %q, want %q", err.Error(), want)
	}
	// A status http.StatusText DOES know must still be named.
	known := ClassifyStatus(http.StatusForbidden, "")
	if !strings.Contains(known.Error(), "403 Forbidden") {
		t.Fatalf("known status lost its text: %q", known.Error())
	}
}

// TestClassifyStatusIsAttributed pins the package prefix. Client.Do wraps its
// own errors, so this is invisible there, but a provider calling ClassifyStatus
// directly is the documented pattern and would otherwise surface an unsourced
// "http 403 Forbidden" in a mamori status page.
func TestClassifyStatusIsAttributed(t *testing.T) {
	err := ClassifyStatus(http.StatusForbidden, "")
	if err == nil {
		t.Fatal("ClassifyStatus returned nil")
	}
	if !strings.HasPrefix(err.Error(), "httpcore: ") {
		t.Fatalf("ClassifyStatus error %q is not attributed to this package", err.Error())
	}
}

// TestDoDoesNotDoublePrefix pins the other half of that decision: Do adds the
// package prefix itself, so it must not stack a second one. This is why the
// table lives in an unexported classify that ClassifyStatus wraps.
func TestDoDoesNotDoublePrefix(t *testing.T) {
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusForbidden, nil, nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Do(context.Background(), Request{Path: "x"})
	if err == nil {
		t.Fatal("Do with 403 returned nil error")
	}
	if n := strings.Count(err.Error(), "httpcore: "); n != 1 {
		t.Fatalf("%q carries the package prefix %d times, want 1", err.Error(), n)
	}
}

// TestStatusForKindRoundTrips is the reason StatusForKind is exported rather
// than hand-rolled per provider: it pins the two tables as exact inverses, so
// neither can drift without this failing. A drifted inverse makes a conformance
// Fail hook inject a status that maps to a different kind, which silently
// exercises one classification case five times instead of five cases once.
func TestStatusForKindRoundTrips(t *testing.T) {
	kinds := []mamori.Kind{
		mamori.KindInvalid,
		mamori.KindUnauthenticated,
		mamori.KindPermissionDenied,
		mamori.KindNotFound,
		mamori.KindRateLimited,
		mamori.KindUnavailable,
	}
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			status := StatusForKind(k)
			err := ClassifyStatus(status, "")
			if err == nil {
				t.Fatalf("StatusForKind(%s) = %d, which ClassifyStatus treats as success", k, status)
			}
			if got := mamori.ErrorKind(err); got != k {
				t.Fatalf("round trip %s -> %d -> %s, want %s", k, status, got, k)
			}
		})
	}
}

// TestStatusForKindUnknown pins the fallback. ClassifyStatus never produces
// KindUnknown, so the inverse has no exact answer and must pick a status that
// is at least an honest failure.
func TestStatusForKindUnknown(t *testing.T) {
	if got := StatusForKind(mamori.KindUnknown); got < 500 {
		t.Fatalf("StatusForKind(unknown) = %d, want a 5xx", got)
	}
}
