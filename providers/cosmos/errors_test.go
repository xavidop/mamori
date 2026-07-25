package cosmos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/xavidop/mamori"
)

func TestClassifyCosmos(t *testing.T) {
	cases := []struct {
		status int
		want   mamori.Kind
	}{
		{http.StatusNotFound, mamori.KindNotFound},
		{http.StatusForbidden, mamori.KindPermissionDenied},
		{http.StatusUnauthorized, mamori.KindUnauthenticated},
		{http.StatusTooManyRequests, mamori.KindRateLimited}, // Cosmos RU/s throttling
		{http.StatusInternalServerError, mamori.KindUnavailable},
		{http.StatusBadGateway, mamori.KindUnavailable},
		{http.StatusServiceUnavailable, mamori.KindUnavailable},
		{http.StatusBadRequest, mamori.KindInvalid},
		{http.StatusConflict, mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			err := &azcore.ResponseError{StatusCode: tc.status, ErrorCode: "Test"}
			if got := mamori.ErrorKind(classifyCosmos(err)); got != tc.want {
				t.Fatalf("status %d: ErrorKind = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestClassifyCosmosPreservesResponseError(t *testing.T) {
	orig := &azcore.ResponseError{StatusCode: http.StatusTooManyRequests, ErrorCode: "TooManyRequests"}
	wrapped := classifyCosmos(orig)

	if !errors.Is(wrapped, mamori.ErrRateLimited) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var respErr *azcore.ResponseError
	if !errors.As(wrapped, &respErr) {
		t.Fatalf("errors.As can no longer reach *azcore.ResponseError: %v", wrapped)
	}
	if respErr.ErrorCode != "TooManyRequests" {
		t.Fatalf("recovered ErrorCode = %q, want TooManyRequests", respErr.ErrorCode)
	}
}

func TestClassifyCosmosNilIsNil(t *testing.T) {
	if err := classifyCosmos(nil); err != nil {
		t.Fatalf("classifyCosmos(nil) = %v, want nil", err)
	}
}

// TestClassifyCosmosNonResponseErrorIsUnknown asserts that a plain transport
// error (never wrapped into an *azcore.ResponseError, e.g. a DNS failure or a
// dropped connection) reports KindUnknown, not KindUnavailable. Such an error
// could be a provider bug as easily as a genuine backend outage, so mamori
// does not guess; only a real HTTP response classifies.
func TestClassifyCosmosNonResponseErrorIsUnknown(t *testing.T) {
	plain := errors.New("dial tcp 10.0.0.1:443: i/o timeout")
	if got := mamori.ErrorKind(classifyCosmos(plain)); got != mamori.KindUnknown {
		t.Fatalf("plain transport error kind = %q, want unknown", got)
	}
}

// TestResolveClassifiesNonNotFoundError exercises classifyCosmos through
// Resolve itself, not just as a direct function call. The conformance
// ErrorClassification case cannot catch a regression here: it injects a
// mamori sentinel directly (not an *azcore.ResponseError), so it passes even
// if the classifyCosmos call were deleted from Resolve's fallback branch. This
// test injects a real *azcore.ResponseError through the fake, the same shape a
// live backend would return (including the 429 RU/s-throttling case), so it
// fails if the wiring is removed.
func TestResolveClassifiesNonNotFoundError(t *testing.T) {
	fake := newFakeStore()
	fake.put("appdb", "conn", "prod", "s3cr3t")
	fake.fail("appdb", "conn", "prod", &azcore.ResponseError{StatusCode: http.StatusForbidden, ErrorCode: "Forbidden"})
	p := New(WithClient(fake))

	_, err := p.Resolve(context.Background(), mustParse(t, "cosmos://appdb/conn/prod"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyCosmos may not be wired into Resolve", got, mamori.KindPermissionDenied)
	}
}

// azResponseErrReader is an itemReader whose not-found error is shaped exactly
// like the production sdkReader's post-fix output: mamori.ErrNotFound
// double-wrapped around an *azcore.ResponseError, the same chain a live
// ReadItem 404 produces after going through isNotFound. It exists to drive
// TestResolveNotFoundPreservesResponseError without needing to fake the real
// Cosmos SDK transport.
type azResponseErrReader struct{}

func (azResponseErrReader) ReadItem(context.Context, string, string, string, string) ([]byte, string, error) {
	orig := &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: "NotFound"}
	return nil, "", fmt.Errorf("%w: %w", mamori.ErrNotFound, orig)
}

// TestResolveNotFoundPreservesResponseError guards Resolve's not-found branch
// specifically: it must preserve err (which already double-wraps the sentinel
// AND the underlying *azcore.ResponseError), not replace it with a bare
// sentinel, so errors.As can still reach the SDK error, matching
// providers/azblob's equivalent guarantee.
func TestResolveNotFoundPreservesResponseError(t *testing.T) {
	p := New(WithClient(azResponseErrReader{}))
	_, err := p.Resolve(context.Background(), mustParse(t, "cosmos://appdb/settings/nope"))

	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing item error = %v, want ErrNotFound", err)
	}
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) {
		t.Fatalf("errors.As can no longer reach *azcore.ResponseError: %v", err)
	}
}

// TestResolveNotFoundMessageIsClean guards against a message-hygiene
// regression: sdkReader already double-wraps mamori.ErrNotFound around the SDK
// error, so Resolve's not-found branch must wrap err alone (a single %w), not
// wrap mamori.ErrNotFound a second time on top of it. A second wrap would
// repeat "mamori: not found" in the rendered message.
func TestResolveNotFoundMessageIsClean(t *testing.T) {
	p := New(WithClient(azResponseErrReader{}))
	_, err := p.Resolve(context.Background(), mustParse(t, "cosmos://appdb/settings/nope"))

	msg := err.Error()
	if n := strings.Count(msg, "mamori: not found"); n != 1 {
		t.Fatalf("message contains %q %d times, want exactly 1: %s", "mamori: not found", n, msg)
	}
}

// TestResolveClassifiesRateLimited specifically exercises the RU/s-throttling
// shape a live Cosmos backend returns on a 429: an *azcore.ResponseError with
// a Retry-After style header. It is the operationally important case for this
// provider (see the README), so it gets its own test rather than relying on
// the generic case above.
func TestResolveClassifiesRateLimited(t *testing.T) {
	fake := newFakeStore()
	fake.put("appdb", "conn", "prod", "s3cr3t")
	fake.fail("appdb", "conn", "prod", &azcore.ResponseError{StatusCode: http.StatusTooManyRequests, ErrorCode: "TooManyRequests"})
	p := New(WithClient(fake))

	_, err := p.Resolve(context.Background(), mustParse(t, "cosmos://appdb/conn/prod"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindRateLimited {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyCosmos may not be wired into Resolve", got, mamori.KindRateLimited)
	}
}
