package nacos

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// TestStatusToKind is the provider's own status-to-kind table, separate from the
// conformance kit.
//
// The kit's ErrorClassification case injects a mamori sentinel and checks it
// survives the trip back out, which proves the errors.Is chain is not flattened
// but says nothing about which HTTP status produces which kind. This table is
// the other half: every status Nacos's v1 configuration and listener endpoints
// are documented to return, driven through a real Resolve against the fake, and
// asserted against the kind a mamori user will actually see.
//
// The documented set is 200, 400, 403, 404 and 500 for both endpoints ("Open API
// Guide", Nacos 1.X / 2.X). 409 is included because the server also writes
// SC_CONFLICT with "requested file is being modified, please try later" from
// ConfigServletInner.doGetConfig, which the docs' error table omits. 401 and 429
// are included as the general HTTP mappings a fronting gateway or an
// auth-enabled deployment can produce; they are inherited from
// httpcore.ClassifyStatus rather than claimed to be emitted by Nacos itself.
func TestStatusToKind(t *testing.T) {
	clearEnv(t)

	tests := []struct {
		name   string
		status int
		want   mamori.Kind
		// documented reports whether the Nacos Open API error table names this
		// status for these endpoints, as opposed to it being a general HTTP
		// mapping inherited from httpcore.
		documented bool
	}{
		{name: "bad request", status: http.StatusBadRequest, want: mamori.KindInvalid, documented: true},
		{name: "unauthorized", status: http.StatusUnauthorized, want: mamori.KindUnauthenticated},
		{name: "forbidden", status: http.StatusForbidden, want: mamori.KindPermissionDenied, documented: true},
		{name: "not found", status: http.StatusNotFound, want: mamori.KindNotFound, documented: true},
		{name: "request timeout", status: http.StatusRequestTimeout, want: mamori.KindRateLimited},
		{name: "conflict while the config is being modified", status: http.StatusConflict, want: mamori.KindUnavailable},
		{name: "unprocessable entity", status: http.StatusUnprocessableEntity, want: mamori.KindInvalid},
		{name: "too many requests", status: http.StatusTooManyRequests, want: mamori.KindRateLimited},
		{name: "internal server error", status: http.StatusInternalServerError, want: mamori.KindUnavailable, documented: true},
		{name: "bad gateway", status: http.StatusBadGateway, want: mamori.KindUnavailable},
		{name: "service unavailable", status: http.StatusServiceUnavailable, want: mamori.KindUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeNacos()
			fake.set("", defaultGroup, "app.yaml", "value")
			fake.fail(tt.status)
			p := fake.provider("")

			_, err := p.Resolve(context.Background(), mustRef(t, "nacos://app.yaml"))
			if err == nil {
				t.Fatalf("status %d resolved without an error", tt.status)
			}
			if got := mamori.ErrorKind(err); got != tt.want {
				source := "inherited from httpcore.ClassifyStatus"
				if tt.documented {
					source = "named in the Nacos Open API error table"
				}
				t.Fatalf("status %d -> kind %v, want %v (%s)", tt.status, got, tt.want, source)
			}
			// Whatever the status, the response body must not reach the error:
			// on a 200 that same body IS the configuration, so this provider
			// sets no httpcore ErrorDetail hook at all.
			if got := err.Error(); strings.Contains(got, "injected failure") {
				t.Fatalf("error text carries the response body: %q", got)
			}
		})
	}
}

// TestNotFoundIsTheOnlyKindThatFallsBackToADefault guards the one mapping whose
// consequence is silent. mamori applies a field's default: only for
// KindNotFound, so a status that wrongly mapped to not-found would replace a
// real backend failure with a default value and report success.
func TestNotFoundIsTheOnlyKindThatFallsBackToADefault(t *testing.T) {
	clearEnv(t)
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusConflict,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		fake := newFakeNacos()
		fake.set("", defaultGroup, "app.yaml", "value")
		fake.fail(status)
		p := fake.provider("")

		_, err := p.Resolve(context.Background(), mustRef(t, "nacos://app.yaml"))
		if mamori.ErrorKind(err) == mamori.KindNotFound {
			t.Fatalf("status %d classified as not-found; mamori would apply the field's default and report success", status)
		}
	}
}
