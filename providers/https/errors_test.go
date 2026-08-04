package https

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// TestStatusToKind maps every status this provider can receive onto the kind it
// must report. This is the requirement CONTRIBUTING calls out separately from
// the conformance kit: the kit proves a classified error survives transit, this
// proves the classification exists at all.
func TestStatusToKind(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   mamori.Kind
	}{
		{"bad request", http.StatusBadRequest, mamori.KindInvalid},
		{"unauthorized", http.StatusUnauthorized, mamori.KindUnauthenticated},
		{"forbidden", http.StatusForbidden, mamori.KindPermissionDenied},
		{"not found", http.StatusNotFound, mamori.KindNotFound},
		{"request timeout", http.StatusRequestTimeout, mamori.KindRateLimited},
		{"too many requests", http.StatusTooManyRequests, mamori.KindRateLimited},
		{"internal error", http.StatusInternalServerError, mamori.KindUnavailable},
		{"bad gateway", http.StatusBadGateway, mamori.KindUnavailable},
		{"gateway timeout", http.StatusGatewayTimeout, mamori.KindUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.set("/v1/cfg", []byte("payload"))
			f.fail(tt.status)
			p := newTestProvider(t, f, nil)

			_, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
			if err == nil {
				t.Fatalf("status %d produced no error", tt.status)
			}
			if got := mamori.ErrorKind(err); got != tt.want {
				t.Fatalf("status %d kind = %s, want %s", tt.status, got, tt.want)
			}
		})
	}
}

// TestResolveErrorCarriesNoPayload proves a failing resolve does not put the
// response body into the error, since a body can be the secret itself.
func TestResolveErrorCarriesNoPayload(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte("s3cr3t-value"))
	f.fail(http.StatusForbidden)
	p := newTestProvider(t, f, nil)

	_, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	if got := err.Error(); strings.Contains(got, "s3cr3t-value") {
		t.Fatalf("payload leaked into error %q", got)
	}
}
