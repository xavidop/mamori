package azure

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/xavidop/mamori"
)

func TestClassifyAzure(t *testing.T) {
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
			if got := mamori.ErrorKind(classifyAzure(err)); got != tc.want {
				t.Fatalf("status %d: ErrorKind = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestClassifyAzurePreservesResponseError(t *testing.T) {
	orig := &azcore.ResponseError{StatusCode: http.StatusForbidden, ErrorCode: "Forbidden"}
	wrapped := classifyAzure(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var respErr *azcore.ResponseError
	if !errors.As(wrapped, &respErr) {
		t.Fatalf("errors.As can no longer reach *azcore.ResponseError: %v", wrapped)
	}
	if respErr.ErrorCode != "Forbidden" {
		t.Fatalf("recovered ErrorCode = %q, want Forbidden", respErr.ErrorCode)
	}
}

func TestClassifyAzureNilIsNil(t *testing.T) {
	if err := classifyAzure(nil); err != nil {
		t.Fatalf("classifyAzure(nil) = %v, want nil", err)
	}
}

// TestClassifyAzureNonResponseErrorIsUnknown asserts that a plain transport
// error (never wrapped into an *azcore.ResponseError, e.g. a DNS failure or a
// dropped connection) reports KindUnknown, not KindUnavailable. Such an error
// could be a provider bug as easily as a genuine backend outage, so mamori
// does not guess; only a real HTTP response classifies.
func TestClassifyAzureNonResponseErrorIsUnknown(t *testing.T) {
	plain := errors.New("dial tcp 10.0.0.1:443: i/o timeout")
	if got := mamori.ErrorKind(classifyAzure(plain)); got != mamori.KindUnknown {
		t.Fatalf("plain transport error kind = %q, want unknown", got)
	}
}

// TestResolveNotFoundPreservesResponseError guards Resolve's isNotFound
// pre-check branch specifically: it must double-wrap (sentinel AND the
// underlying *azcore.ResponseError), not just the sentinel, so errors.As can
// still reach the SDK error as the README's error table promises.
func TestResolveNotFoundPreservesResponseError(t *testing.T) {
	p := New(WithClient(newFakeVault()))
	_, err := p.Resolve(context.Background(), mustParse(t, "azure-kv://myvault/nope"))

	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing secret error = %v, want ErrNotFound", err)
	}
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) {
		t.Fatalf("errors.As can no longer reach *azcore.ResponseError: %v", err)
	}
}
