package vault

import (
	"context"
	"errors"
	"net/http"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/xavidop/mamori"
)

func respErr(status int) error {
	return &vaultapi.ResponseError{StatusCode: status, Errors: []string{"test"}}
}

func TestClassifyVault(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"NotFoundStatus", respErr(http.StatusNotFound), mamori.KindNotFound},
		{"NotFoundSentinel", vaultapi.ErrSecretNotFound, mamori.KindNotFound},
		{"Forbidden", respErr(http.StatusForbidden), mamori.KindPermissionDenied},
		{"BadRequest", respErr(http.StatusBadRequest), mamori.KindInvalid},
		{"TooManyRequests", respErr(http.StatusTooManyRequests), mamori.KindRateLimited},
		{"Sealed", respErr(http.StatusServiceUnavailable), mamori.KindUnavailable},
		{"InternalError", respErr(http.StatusInternalServerError), mamori.KindUnavailable},
		{"Teapot", respErr(http.StatusTeapot), mamori.KindUnknown},
		{"PlainError", errors.New("connection refused"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mamori.ErrorKind(classifyVault(tc.err)); got != tc.want {
				t.Fatalf("ErrorKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyVaultPreservesResponseError(t *testing.T) {
	orig := &vaultapi.ResponseError{StatusCode: http.StatusForbidden, Errors: []string{"permission denied"}}
	wrapped := classifyVault(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	var re *vaultapi.ResponseError
	if !errors.As(wrapped, &re) {
		t.Fatalf("errors.As can no longer reach *vaultapi.ResponseError: %v", wrapped)
	}
	if re.StatusCode != http.StatusForbidden {
		t.Fatalf("recovered StatusCode = %d, want 403", re.StatusCode)
	}
}

// TestResolveNotFoundPreservesResponseError guards Resolve's isNotFound
// pre-check branch specifically (not classifyVault directly): a 404
// *vaultapi.ResponseError must be double-wrapped (sentinel AND the underlying
// SDK error), not just the sentinel, so errors.As can still reach it.
func TestResolveNotFoundPreservesResponseError(t *testing.T) {
	fake := newFakeVault()
	fake.fail("secret", "myapp/config", &vaultapi.ResponseError{StatusCode: http.StatusNotFound, Errors: []string{"not found"}})
	p := newWithReader(fake)

	_, err := p.Resolve(context.Background(), mustRef(t, "vault://secret/myapp/config"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	var re *vaultapi.ResponseError
	if !errors.As(err, &re) {
		t.Fatalf("errors.As can no longer reach *vaultapi.ResponseError: %v", err)
	}
}
