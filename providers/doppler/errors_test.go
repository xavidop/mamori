package doppler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// statusServerProvider returns a provider pointed at a server that answers
// every request with status and body.
func statusServerProvider(t *testing.T, status int, body string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(WithBaseURL(srv.URL), WithToken(testToken), WithHTTPClient(srv.Client()))
}

// TestResolveClassifiesStatus pins the status-to-kind mapping this provider
// inherits from httpcore.ClassifyStatus, exercised end to end through Resolve
// against a real response. There is no longer a hand-rolled
// classifyDopplerStatus to table-test directly: the mapping lives in one place
// for every httpcore-backed provider, and what this provider still owes is
// that a real response reaches it.
//
// Three rows differ from the mapping this provider carried before it moved
// onto httpcore, and each is deliberate:
//
//   - 422 was previously unclassified, so mamori treated it as an unknown
//     failure and kept retrying a request that was well formed and
//     semantically wrong. It is now KindInvalid.
//   - 408 was previously unclassified. It is now KindRateLimited, alongside
//     429.
//   - 409, and every other status neither this provider nor httpcore names,
//     was previously KindUnknown and is now KindUnavailable: httpcore has one
//     default for an unrecognized status rather than per-provider silence.
func TestResolveClassifiesStatus(t *testing.T) {
	cases := []struct {
		status int
		want   mamori.Kind
	}{
		{http.StatusForbidden, mamori.KindPermissionDenied},
		{http.StatusUnauthorized, mamori.KindUnauthenticated},
		{http.StatusTooManyRequests, mamori.KindRateLimited},
		{http.StatusRequestTimeout, mamori.KindRateLimited},
		{http.StatusInternalServerError, mamori.KindUnavailable},
		{http.StatusBadGateway, mamori.KindUnavailable},
		{http.StatusServiceUnavailable, mamori.KindUnavailable},
		{http.StatusBadRequest, mamori.KindInvalid},
		{http.StatusUnprocessableEntity, mamori.KindInvalid},
		{http.StatusConflict, mamori.KindUnavailable},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			p := statusServerProvider(t, tc.status, `{"messages":["nope"]}`)
			_, err := p.Resolve(context.Background(), mustRef(t, "doppler://"+testProject+"/"+testConfig+"#K"))
			if err == nil {
				t.Fatalf("status %d: Resolve returned a nil error", tc.status)
			}
			if got := mamori.ErrorKind(err); got != tc.want {
				t.Fatalf("status %d: ErrorKind = %q, want %q (err: %v)", tc.status, got, tc.want, err)
			}
		})
	}
}

// TestResolveErrorMessagePreservesContext guards what the migration onto
// httpcore had to keep: the secret name, the HTTP status, and the verbatim
// (bounded) upstream body all still reach the message, and the classification
// is still reachable with errors.Is rather than flattened into text.
func TestResolveErrorMessagePreservesContext(t *testing.T) {
	p := statusServerProvider(t, http.StatusForbidden, `{"messages":["token lacks read access"]}`)
	_, err := p.Resolve(context.Background(), mustRef(t, "doppler://"+testProject+"/"+testConfig+"#STRIPE_API_KEY"))
	if err == nil {
		t.Fatal("Resolve returned a nil error for a 403 response")
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"STRIPE_API_KEY", "403", "token lacks read access"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message lost %q: %v", want, err)
		}
	}
}

// TestResolveNotFoundNamesProjectAndConfig pins the one status that keeps a
// message of its own rather than httpcore's: a 404 says which project and
// config the secret was looked for in, which the status alone does not.
func TestResolveNotFoundNamesProjectAndConfig(t *testing.T) {
	p := statusServerProvider(t, http.StatusNotFound, `{"messages":["Could not find requested secret"]}`)
	_, err := p.Resolve(context.Background(), mustRef(t, "doppler://"+testProject+"/"+testConfig+"#MISSING"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	msg := err.Error()
	for _, want := range []string{"MISSING", testProject, testConfig} {
		if !strings.Contains(msg, want) {
			t.Fatalf("not-found message lost %q: %v", want, err)
		}
	}
}
