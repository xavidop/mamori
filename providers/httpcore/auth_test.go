package httpcore

import (
	"context"
	"net/http"
	"testing"
)

func newTestRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://example.test/v1/cfg", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

func TestBearer(t *testing.T) {
	req := newTestRequest(t)
	if err := Bearer("tok123").Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok123" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer tok123")
	}
}

func TestHeaderAuth(t *testing.T) {
	req := newTestRequest(t)
	if err := HeaderAuth("X-Api-Key", "k9").Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("X-Api-Key"); got != "k9" {
		t.Fatalf("X-Api-Key = %q, want %q", got, "k9")
	}
}

func TestBasicAuth(t *testing.T) {
	req := newTestRequest(t)
	if err := BasicAuth("u", "p").Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	user, pass, ok := req.BasicAuth()
	if !ok || user != "u" || pass != "p" {
		t.Fatalf("BasicAuth = (%q, %q, %v), want (u, p, true)", user, pass, ok)
	}
}

func TestQueryAuth(t *testing.T) {
	req := newTestRequest(t)
	if err := QueryAuth("access_token", "t7").Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.URL.Query().Get("access_token"); got != "t7" {
		t.Fatalf("access_token = %q, want %q", got, "t7")
	}
}

// TestQueryAuthPreservesExistingQuery proves the authenticator adds to the query
// rather than replacing it, since Endpoint.Query is merged in before auth runs.
func TestQueryAuthPreservesExistingQuery(t *testing.T) {
	req := newTestRequest(t)
	q := req.URL.Query()
	q.Set("env", "prod")
	req.URL.RawQuery = q.Encode()

	if err := QueryAuth("access_token", "t7").Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.URL.Query().Get("env"); got != "prod" {
		t.Fatalf("env = %q, want prod", got)
	}
	if got := req.URL.Query().Get("access_token"); got != "t7" {
		t.Fatalf("access_token = %q, want t7", got)
	}
}
