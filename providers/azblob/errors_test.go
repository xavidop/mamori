package azblob

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

func TestClassifyAzblob(t *testing.T) {
	cases := []struct {
		status int
		want   mamori.Kind
	}{
		{http.StatusNotFound, mamori.KindNotFound},
		{http.StatusForbidden, mamori.KindPermissionDenied},
		{http.StatusUnauthorized, mamori.KindUnauthenticated},
		{http.StatusTooManyRequests, mamori.KindRateLimited},
		{http.StatusInternalServerError, mamori.KindUnavailable},
		{http.StatusBadGateway, mamori.KindUnavailable},
		{http.StatusServiceUnavailable, mamori.KindUnavailable},
		{http.StatusBadRequest, mamori.KindInvalid},
		{http.StatusConflict, mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			err := &azcore.ResponseError{StatusCode: tc.status, ErrorCode: "Test"}
			if got := mamori.ErrorKind(classifyAzblob(err)); got != tc.want {
				t.Fatalf("status %d: ErrorKind = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestClassifyAzblobPreservesResponseError(t *testing.T) {
	orig := &azcore.ResponseError{StatusCode: http.StatusForbidden, ErrorCode: "AuthorizationPermissionMismatch"}
	wrapped := classifyAzblob(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var respErr *azcore.ResponseError
	if !errors.As(wrapped, &respErr) {
		t.Fatalf("errors.As can no longer reach *azcore.ResponseError: %v", wrapped)
	}
	if respErr.ErrorCode != "AuthorizationPermissionMismatch" {
		t.Fatalf("recovered ErrorCode = %q, want AuthorizationPermissionMismatch", respErr.ErrorCode)
	}
}

func TestClassifyAzblobNilIsNil(t *testing.T) {
	if err := classifyAzblob(nil); err != nil {
		t.Fatalf("classifyAzblob(nil) = %v, want nil", err)
	}
}

// TestClassifyAzblobNonResponseErrorIsUnknown asserts that a plain transport
// error (never wrapped into an *azcore.ResponseError, e.g. a DNS failure or a
// dropped connection) reports KindUnknown, not KindUnavailable. Such an error
// could be a provider bug as easily as a genuine backend outage, so mamori
// does not guess; only a real HTTP response classifies.
func TestClassifyAzblobNonResponseErrorIsUnknown(t *testing.T) {
	plain := errors.New("dial tcp 10.0.0.1:443: i/o timeout")
	if got := mamori.ErrorKind(classifyAzblob(plain)); got != mamori.KindUnknown {
		t.Fatalf("plain transport error kind = %q, want unknown", got)
	}
}

// azResponseErrDownloader is a blobDownloader whose not-found error is shaped
// exactly like the production sdkDownloader's post-fix output: mamori.ErrNotFound
// double-wrapped around an *azcore.ResponseError, the same chain a live
// DownloadStream 404 produces after going through isNotFound. It exists to
// drive TestResolveNotFoundPreservesResponseError without needing to fake the
// real Azure SDK transport.
type azResponseErrDownloader struct{}

func (azResponseErrDownloader) Download(context.Context, string, string) ([]byte, string, error) {
	orig := &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: "BlobNotFound"}
	return nil, "", fmt.Errorf("%w: %w", mamori.ErrNotFound, orig)
}

// TestResolveNotFoundPreservesResponseError guards Resolve's not-found branch
// specifically: it must double-wrap (sentinel AND the underlying
// *azcore.ResponseError), not just the sentinel, so errors.As can still reach
// the SDK error as the README's error table promises.
func TestResolveNotFoundPreservesResponseError(t *testing.T) {
	p := New(WithClient(azResponseErrDownloader{}))
	_, err := p.Resolve(context.Background(), mustParse(t, "azblob://config/nope.json"))

	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing blob error = %v, want ErrNotFound", err)
	}
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) {
		t.Fatalf("errors.As can no longer reach *azcore.ResponseError: %v", err)
	}
}

// TestResolveNotFoundMessageIsClean guards against a message-hygiene
// regression: the downloader already double-wraps mamori.ErrNotFound around
// the SDK error, so Resolve's not-found branch must wrap err alone (a single
// %w), not wrap mamori.ErrNotFound a second time on top of it. A second wrap
// would repeat "mamori: not found" in the rendered message.
func TestResolveNotFoundMessageIsClean(t *testing.T) {
	p := New(WithClient(azResponseErrDownloader{}))
	_, err := p.Resolve(context.Background(), mustParse(t, "azblob://config/nope.json"))

	msg := err.Error()
	if n := strings.Count(msg, "mamori: not found"); n != 1 {
		t.Fatalf("message contains %q %d times, want exactly 1: %s", "mamori: not found", n, msg)
	}
}

// TestResolveClassifiesNonNotFoundError exercises classifyAzblob through
// Resolve itself, not just as a direct function call. The conformance
// ErrorClassification case cannot catch a regression here: it injects a
// mamori sentinel directly (not an *azcore.ResponseError), so it passes even
// if the classifyAzblob call were deleted from Resolve's fallback branch. This
// test injects a real *azcore.ResponseError through the fake, the same shape a
// live backend would return, so it fails if the wiring is removed.
func TestResolveClassifiesNonNotFoundError(t *testing.T) {
	fake := newFakeStore()
	fake.put("config", "app.json", "s3cr3t")
	fake.fail("config", "app.json", &azcore.ResponseError{StatusCode: http.StatusForbidden, ErrorCode: "AuthorizationPermissionMismatch"})
	p := New(WithClient(fake))

	_, err := p.Resolve(context.Background(), mustParse(t, "azblob://config/app.json"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyAzblob may not be wired into Resolve", got, mamori.KindPermissionDenied)
	}
}
