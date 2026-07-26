package mamoriprov

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/xavidop/mamori"
)

func newTestServerFunc(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func mustParseRef(t *testing.T, tag string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(tag)
	if err != nil {
		t.Fatalf("ParseRef(%q) returned unexpected error: %v", tag, err)
	}
	return ref
}

func TestResolveBatchReturnsValuesKeyedByRawRef(t *testing.T) {
	body := `{"values":[` +
		`{"name":"a","bytes":"aGVsbG8=","version":"v1","metadata":{}},` +
		`{"name":"b","bytes":"d29ybGQ=","version":"v2","metadata":{}}` +
		`]}`
	ts := newTestServer(t, http.StatusOK, body)
	p := newTestProvider(t, ts)

	refA := mustParseRef(t, "mamori://a")
	refB := mustParseRef(t, "mamori://b")

	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{refA, refB})
	if err != nil {
		t.Fatalf("ResolveBatch returned unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2: %+v", len(got), got)
	}
	va, ok := got[refA.String()]
	if !ok {
		t.Fatalf("missing entry for %q in %+v", refA.String(), got)
	}
	if string(va.Bytes) != "hello" {
		t.Errorf("a.Bytes = %q, want %q", va.Bytes, "hello")
	}
	vb, ok := got[refB.String()]
	if !ok {
		t.Fatalf("missing entry for %q in %+v", refB.String(), got)
	}
	if string(vb.Bytes) != "world" {
		t.Errorf("b.Bytes = %q, want %q", vb.Bytes, "world")
	}
}

func TestResolveBatchOmitsNotFound(t *testing.T) {
	body := `{"values":[` +
		`{"name":"a","bytes":"aGVsbG8=","metadata":{}},` +
		`{"name":"b","error":{"kind":"not_found","message":"m"}}` +
		`]}`
	ts := newTestServer(t, http.StatusOK, body)
	p := newTestProvider(t, ts)

	refA := mustParseRef(t, "mamori://a")
	refB := mustParseRef(t, "mamori://b")

	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{refA, refB})
	if err != nil {
		t.Fatalf("ResolveBatch returned unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1: %+v", len(got), got)
	}
	if _, ok := got[refA.String()]; !ok {
		t.Errorf("missing entry for %q in %+v", refA.String(), got)
	}
	if _, ok := got[refB.String()]; ok {
		t.Errorf("expected %q to be omitted from %+v", refB.String(), got)
	}
}

func TestResolveBatchHardErrorFailsWholeBatch(t *testing.T) {
	body := `{"values":[` +
		`{"name":"a","bytes":"aGVsbG8=","metadata":{}},` +
		`{"name":"b","error":{"kind":"permission_denied","message":"m"}}` +
		`]}`
	ts := newTestServer(t, http.StatusOK, body)
	p := newTestProvider(t, ts)

	refA := mustParseRef(t, "mamori://a")
	refB := mustParseRef(t, "mamori://b")

	_, err := p.ResolveBatch(context.Background(), []mamori.Ref{refA, refB})
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("err = %v, want errors.Is(err, mamori.ErrPermissionDenied)", err)
	}
}

func TestResolveBatchWholeRequestFailure(t *testing.T) {
	body := `{"error":{"kind":"unauthenticated","message":"m"}}`
	ts := newTestServer(t, http.StatusUnauthorized, body)
	p := newTestProvider(t, ts)

	refA := mustParseRef(t, "mamori://a")

	_, err := p.ResolveBatch(context.Background(), []mamori.Ref{refA})
	if !errors.Is(err, mamori.ErrUnauthenticated) {
		t.Fatalf("err = %v, want errors.Is(err, mamori.ErrUnauthenticated)", err)
	}
}

func TestResolveBatchSingleRequest(t *testing.T) {
	var count int32
	body := `{"values":[` +
		`{"name":"a","bytes":"aGVsbG8=","metadata":{}},` +
		`{"name":"b","bytes":"aGVsbG8=","metadata":{}},` +
		`{"name":"c","bytes":"aGVsbG8=","metadata":{}}` +
		`]}`
	ts := newTestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
	p := newTestProvider(t, ts)

	refA := mustParseRef(t, "mamori://a")
	refB := mustParseRef(t, "mamori://b")
	refC := mustParseRef(t, "mamori://c")

	_, err := p.ResolveBatch(context.Background(), []mamori.Ref{refA, refB, refC})
	if err != nil {
		t.Fatalf("ResolveBatch returned unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("request count = %d, want 1", got)
	}
}

func TestResolveBatchEmptyPathIsInvalid(t *testing.T) {
	p := New(Config{Endpoint: "http://unused", InsecureNoTLS: true})

	ref := mamori.Ref{Scheme: "mamori", Raw: "mamori://"}
	_, err := p.ResolveBatch(context.Background(), []mamori.Ref{ref})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want errors.Is(err, mamori.ErrInvalid)", err)
	}
}
