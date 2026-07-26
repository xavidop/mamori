package mamoriprov

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

func newTestServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func newTestProvider(t *testing.T, ts *httptest.Server) *Provider {
	t.Helper()
	return New(Config{Endpoint: ts.URL, InsecureNoTLS: true})
}

func TestResolveSuccessDecodesValue(t *testing.T) {
	body := `{"name":"db-password","bytes":"aHVudGVyMg==","version":"v1","sensitive":true,"not_after":"2026-07-24T12:00:00Z","metadata":{"env":"prod"},"kind":""}`
	ts := newTestServer(t, http.StatusOK, body)
	p := newTestProvider(t, ts)

	ref := mamori.Ref{Scheme: "mamori", Path: "db-password", Raw: "mamori://db-password"}
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve returned unexpected error: %v", err)
	}
	if got, want := string(v.Bytes), "hunter2"; got != want {
		t.Errorf("Bytes = %q, want %q", got, want)
	}
	if got, want := v.Version, "v1"; got != want {
		t.Errorf("Version = %q, want %q", got, want)
	}
	if !v.Sensitive {
		t.Error("Sensitive = false, want true")
	}
	wantNotAfter, err := time.Parse(time.RFC3339, "2026-07-24T12:00:00Z")
	if err != nil {
		t.Fatalf("test setup: parsing want time: %v", err)
	}
	if !v.NotAfter.Equal(wantNotAfter) {
		t.Errorf("NotAfter = %v, want %v", v.NotAfter, wantNotAfter)
	}
	if got, want := v.Metadata["env"], "prod"; got != want {
		t.Errorf("Metadata[env] = %q, want %q", got, want)
	}
}

func TestResolveStaleKindStillReturnsValueNilError(t *testing.T) {
	body := `{"name":"db-password","bytes":"aGVsbG8=","metadata":{},"kind":"unavailable"}`
	ts := newTestServer(t, http.StatusOK, body)
	p := newTestProvider(t, ts)

	ref := mamori.Ref{Scheme: "mamori", Path: "db-password", Raw: "mamori://db-password"}
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve returned unexpected error for a stale-but-serving value: %v", err)
	}
	if got, want := string(v.Bytes), "hello"; got != want {
		t.Errorf("Bytes = %q, want %q", got, want)
	}
}

func TestResolveClassificationPassthrough(t *testing.T) {
	cases := []struct {
		kind      string
		status    int
		wantErrIs error
	}{
		{"permission_denied", http.StatusForbidden, mamori.ErrPermissionDenied},
		{"unauthenticated", http.StatusUnauthorized, mamori.ErrUnauthenticated},
		{"unavailable", http.StatusServiceUnavailable, mamori.ErrUnavailable},
		{"rate_limited", http.StatusTooManyRequests, mamori.ErrRateLimited},
		{"invalid", http.StatusBadRequest, mamori.ErrInvalid},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			body := fmt.Sprintf(`{"error":{"kind":%q,"message":"m"}}`, tc.kind)
			ts := newTestServer(t, tc.status, body)
			p := newTestProvider(t, ts)

			ref := mamori.Ref{Scheme: "mamori", Path: "db-password", Raw: "mamori://db-password"}
			_, err := p.Resolve(context.Background(), ref)
			if !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("err = %v, want errors.Is(err, %v)", err, tc.wantErrIs)
			}
		})
	}
}

func TestResolveNotFoundIsErrNotFound(t *testing.T) {
	body := `{"error":{"kind":"not_found","message":"m"}}`
	ts := newTestServer(t, http.StatusNotFound, body)
	p := newTestProvider(t, ts)

	ref := mamori.Ref{Scheme: "mamori", Path: "db-password", Raw: "mamori://db-password"}
	_, err := p.Resolve(context.Background(), ref)
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want errors.Is(err, mamori.ErrNotFound)", err)
	}
}

func TestResolveUnknownKindReportsKindUnknown(t *testing.T) {
	body := `{"error":{"kind":"unknown","message":"m"}}`
	ts := newTestServer(t, http.StatusInternalServerError, body)
	p := newTestProvider(t, ts)

	ref := mamori.Ref{Scheme: "mamori", Path: "db-password", Raw: "mamori://db-password"}
	_, err := p.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("expected an error for an unknown-kind failure")
	}
	if got, want := mamori.ErrorKind(err), mamori.KindUnknown; got != want {
		t.Fatalf("ErrorKind(err) = %q, want %q", got, want)
	}
}

func TestResolveEmptyNameIsInvalid(t *testing.T) {
	p := New(Config{Endpoint: "http://unused", InsecureNoTLS: true})

	ref := mamori.Ref{Scheme: "mamori", Raw: "mamori://"}
	_, err := p.Resolve(context.Background(), ref)
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want errors.Is(err, mamori.ErrInvalid)", err)
	}
}
