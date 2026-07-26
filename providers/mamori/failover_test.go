package mamoriprov

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// deadEndpointURL returns the URL of an httptest server that has already been
// shut down: an address nothing is listening on, so dialing it fails at the
// connection level rather than with any HTTP response at all. That is the
// "replica is gone" half of failover, which no status-code-based fixture can
// reproduce.
func deadEndpointURL(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close()
	return url
}

// countingServer serves status/body and counts how many requests it received,
// so a test can assert not just what came back but WHICH replicas were asked -
// the only way to prove that an authoritative answer was not replayed against
// the rest of the fleet.
func countingServer(t *testing.T, status int, body string) (url string, hits *atomic.Int32) {
	t.Helper()
	hits = &atomic.Int32{}
	ts := newTestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	return ts.URL, hits
}

// okValueBody is a well-formed GET /v1/values/{name} success body for the
// binding every test in this file resolves.
const okValueBody = `{"name":"db-password","bytes":"aGVhbHRoeQ==","version":"v1","metadata":{}}`

// errBody renders the wire failure envelope for a kind, the shape every
// non-200 in this file returns.
func errBody(kind, message string) string {
	return fmt.Sprintf(`{"error":{"kind":%q,"message":%q}}`, kind, message)
}

func testRef(t *testing.T) mamori.Ref {
	t.Helper()
	return mustParseRef(t, "mamori://db-password")
}

func TestBothEndpointFieldsSetIsConfigError(t *testing.T) {
	// A real, healthy server: the point is that a provider misconfigured this
	// way never reaches it, however reachable it happens to be.
	url, hits := countingServer(t, http.StatusOK, okValueBody)

	cases := []struct {
		name string
		call func(p *Provider) error
	}{
		{"Resolve", func(p *Provider) error {
			_, err := p.Resolve(context.Background(), testRef(t))
			return err
		}},
		{"ResolveBatch", func(p *Provider) error {
			_, err := p.ResolveBatch(context.Background(), []mamori.Ref{testRef(t)})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := New(Config{Endpoint: url, Endpoints: []string{url}, InsecureNoTLS: true})

			err := tc.call(p)
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("err = %v, want errors.Is(err, mamori.ErrInvalid)", err)
			}
			if !strings.Contains(err.Error(), "both set") {
				t.Errorf("err = %v, want a message naming both fields as the problem", err)
			}
		})
	}

	if got := hits.Load(); got != 0 {
		t.Fatalf("server hits = %d, want 0: a configuration error must be reported without sending anything", got)
	}
}

// TestBadEndpointInListIsConfigError pins the fail-fast rule: one unusable
// entry poisons the whole provider rather than being quietly dropped, so an
// operator cannot end up believing they have N replicas when one of them is a
// typo. The healthy entry's hit counter proves the provider did not just skip
// the bad one and carry on.
func TestBadEndpointInListIsConfigError(t *testing.T) {
	healthy, hits := countingServer(t, http.StatusOK, okValueBody)

	cases := []struct {
		name string
		bad  string
	}{
		{"unsupported scheme", "ftp://replica-2:9000"},
		{"plaintext without InsecureNoTLS", "http://replica-2:8080"},
		{"empty entry", ""},
		{"unix with no socket path", "unix://"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// InsecureNoTLS is deliberately left off so the plaintext case is
			// refused; the healthy entry is an https URL for the same reason,
			// since an httptest http:// URL would be refused too.
			p := New(Config{Endpoints: []string{strings.Replace(healthy, "http://", "https://", 1), tc.bad}})

			_, err := p.Resolve(context.Background(), testRef(t))
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("err = %v, want errors.Is(err, mamori.ErrInvalid)", err)
			}
		})
	}

	if got := hits.Load(); got != 0 {
		t.Fatalf("healthy endpoint hits = %d, want 0: a bad entry must fail the provider, not be skipped past", got)
	}
}

// TestResolveFailsOverToHealthyEndpoint covers the three shapes of "this
// replica cannot answer": it is not listening at all, it answered 500 with an
// unclassifiable body, and it answered a classified unavailable.
func TestResolveFailsOverToHealthyEndpoint(t *testing.T) {
	cases := []struct {
		name  string
		first func(t *testing.T) string
	}{
		{"connection refused", deadEndpointURL},
		{"500 unknown kind", func(t *testing.T) string {
			url, _ := countingServer(t, http.StatusInternalServerError, errBody("unknown", "boom"))
			return url
		}},
		{"503 unavailable", func(t *testing.T) string {
			url, _ := countingServer(t, http.StatusServiceUnavailable, errBody("unavailable", "upstream down"))
			return url
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			healthy, hits := countingServer(t, http.StatusOK, okValueBody)
			p := New(Config{Endpoints: []string{tc.first(t), healthy}, InsecureNoTLS: true})

			v, err := p.Resolve(context.Background(), testRef(t))
			if err != nil {
				t.Fatalf("Resolve returned unexpected error: %v", err)
			}
			if got, want := string(v.Bytes), "healthy"; got != want {
				t.Errorf("Bytes = %q, want %q", got, want)
			}
			if got := hits.Load(); got != 1 {
				t.Errorf("healthy endpoint hits = %d, want 1", got)
			}
		})
	}
}

// TestResolveDoesNotFailOverOnAuthoritativeError is the counterweight to
// TestResolveFailsOverToHealthyEndpoint: for an answer every replica would
// give alike, the second endpoint must never be contacted. Its hit counter
// staying at zero is the whole assertion - a 403 must cost one request, not
// one per replica.
func TestResolveDoesNotFailOverOnAuthoritativeError(t *testing.T) {
	cases := []struct {
		kind      string
		status    int
		wantErrIs error
	}{
		{"not_found", http.StatusNotFound, mamori.ErrNotFound},
		{"permission_denied", http.StatusForbidden, mamori.ErrPermissionDenied},
		{"unauthenticated", http.StatusUnauthorized, mamori.ErrUnauthenticated},
		{"invalid", http.StatusBadRequest, mamori.ErrInvalid},
		{"rate_limited", http.StatusTooManyRequests, mamori.ErrRateLimited},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			first, firstHits := countingServer(t, tc.status, errBody(tc.kind, "m"))
			second, secondHits := countingServer(t, http.StatusOK, okValueBody)
			p := New(Config{Endpoints: []string{first, second}, InsecureNoTLS: true})

			_, err := p.Resolve(context.Background(), testRef(t))
			if !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("err = %v, want errors.Is(err, %v)", err, tc.wantErrIs)
			}
			if got := firstHits.Load(); got != 1 {
				t.Errorf("first endpoint hits = %d, want 1", got)
			}
			if got := secondHits.Load(); got != 0 {
				t.Errorf("second endpoint hits = %d, want 0: an authoritative answer must not be replayed against the fleet", got)
			}
		})
	}
}

// TestResolveAllEndpointsDownReturnsLastError pins both halves of the
// exhausted-fleet contract: the LAST endpoint's error is the one returned (not
// the first one's), and it is returned verbatim so its classification still
// survives errors.Is for the caller.
func TestResolveAllEndpointsDownReturnsLastError(t *testing.T) {
	first, firstHits := countingServer(t, http.StatusServiceUnavailable, errBody("unavailable", "first replica down"))
	second, secondHits := countingServer(t, http.StatusServiceUnavailable, errBody("unavailable", "last replica down"))
	p := New(Config{Endpoints: []string{first, second}, InsecureNoTLS: true})

	_, err := p.Resolve(context.Background(), testRef(t))
	if !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("err = %v, want errors.Is(err, mamori.ErrUnavailable)", err)
	}
	if got, want := mamori.ErrorKind(err), mamori.KindUnavailable; got != want {
		t.Errorf("ErrorKind(err) = %q, want %q", got, want)
	}
	if !strings.Contains(err.Error(), "last replica down") {
		t.Errorf("err = %v, want the LAST endpoint's error, not an earlier one", err)
	}
	if got := firstHits.Load(); got != 1 {
		t.Errorf("first endpoint hits = %d, want 1", got)
	}
	if got := secondHits.Load(); got != 1 {
		t.Errorf("second endpoint hits = %d, want 1", got)
	}
}

func TestResolveBatchFailsOverToHealthyEndpoint(t *testing.T) {
	dead := deadEndpointURL(t)
	body := `{"values":[{"name":"a","bytes":"aGVsbG8=","metadata":{}}]}`
	healthy, hits := countingServer(t, http.StatusOK, body)
	p := New(Config{Endpoints: []string{dead, healthy}, InsecureNoTLS: true})

	refA := mustParseRef(t, "mamori://a")
	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{refA})
	if err != nil {
		t.Fatalf("ResolveBatch returned unexpected error: %v", err)
	}
	v, ok := got[refA.String()]
	if !ok {
		t.Fatalf("missing entry for %q in %+v", refA.String(), got)
	}
	if string(v.Bytes) != "hello" {
		t.Errorf("a.Bytes = %q, want %q", v.Bytes, "hello")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("healthy endpoint hits = %d, want 1", got)
	}
}

// TestResolveBatchFailoverResendsBody guards the retry's request body: the
// first attempt drains the reader it was given, so a retry that reused it
// would POST an empty body and the second replica would answer about no names
// at all. The second server asserts on what it actually received.
func TestResolveBatchFailoverResendsBody(t *testing.T) {
	first, _ := countingServer(t, http.StatusServiceUnavailable, errBody("unavailable", "down"))

	var gotBody atomic.Value
	second := newTestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody.Store(string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"values":[{"name":"a","bytes":"aGVsbG8=","metadata":{}}]}`))
	})

	p := New(Config{Endpoints: []string{first, second.URL}, InsecureNoTLS: true})
	if _, err := p.ResolveBatch(context.Background(), []mamori.Ref{mustParseRef(t, "mamori://a")}); err != nil {
		t.Fatalf("ResolveBatch returned unexpected error: %v", err)
	}

	body, _ := gotBody.Load().(string)
	if !strings.Contains(body, `"a"`) {
		t.Fatalf("retried request body = %q, want the original names to be resent", body)
	}
}

// TestWatchRotatesToNextEndpoint proves the watch does not black-hole itself
// on a dead replica: the first endpoint refuses every connection, so the only
// way the update from the second one ever arrives is if the reconnect rotated
// instead of redialing the same address.
func TestWatchRotatesToNextEndpoint(t *testing.T) {
	dead := deadEndpointURL(t)

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "event: update\ndata: %s\n\n", `{"name":"db-password","bytes":"c2Vjb25k","metadata":{}}`)
		flusher.Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(live.Close)

	p := New(Config{Endpoints: []string{dead, live.URL}, InsecureNoTLS: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := p.Watch(ctx, testRef(t))
	if err != nil {
		t.Fatalf("Watch returned unexpected error: %v", err)
	}

	// The dead endpoint reports itself as a transient error Update first; the
	// value from the live one must follow it without any further prompting.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case u := <-ch:
			if u.Err != nil {
				if !errors.Is(u.Err, mamori.ErrUnavailable) {
					t.Fatalf("Update.Err = %v, want errors.Is(..., mamori.ErrUnavailable) for an unreachable replica", u.Err)
				}
				continue
			}
			if got, want := string(u.Value.Bytes), "second"; got != want {
				t.Fatalf("Bytes = %q, want %q", got, want)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for the watch to rotate onto the second endpoint")
		}
	}
}

// TestWatchSingleEndpointStillRetriesSameEndpoint pins the one-replica case
// against the rotation change: with a single endpoint the round-robin wraps on
// every iteration, so the loop must keep redialing that endpoint (with the
// backoff sleep it always had) rather than running out of endpoints to try.
func TestWatchSingleEndpointStillRetriesSameEndpoint(t *testing.T) {
	var reqs atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqs.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		if n == 1 {
			return // drop immediately; the client must come back to this same endpoint
		}
		_, _ = fmt.Fprintf(w, "event: update\ndata: %s\n\n", `{"name":"db-password","bytes":"c2Vjb25k","metadata":{}}`)
		flusher.Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)

	p := New(Config{Endpoint: ts.URL, InsecureNoTLS: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := p.Watch(ctx, testRef(t))
	if err != nil {
		t.Fatalf("Watch returned unexpected error: %v", err)
	}

	u := recvUpdate(t, ch)
	if u.Err != nil {
		t.Fatalf("Update.Err = %v, want nil", u.Err)
	}
	if got, want := string(u.Value.Bytes), "second"; got != want {
		t.Errorf("Bytes = %q, want %q", got, want)
	}
	if got := reqs.Load(); got < 2 {
		t.Errorf("request count = %d, want >= 2 (the single endpoint must be redialed)", got)
	}
}

// TestSingleEndpointResolveMakesExactlyOneRequest is the no-regression guard
// for the one-replica case: adding failover must not have added a retry where
// there was none, not even against the same endpoint.
func TestSingleEndpointResolveMakesExactlyOneRequest(t *testing.T) {
	url, hits := countingServer(t, http.StatusServiceUnavailable, errBody("unavailable", "down"))
	p := New(Config{Endpoint: url, InsecureNoTLS: true})

	_, err := p.Resolve(context.Background(), testRef(t))
	if !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("err = %v, want errors.Is(err, mamori.ErrUnavailable)", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("request count = %d, want exactly 1: a single endpoint must not be retried", got)
	}
}

// TestEndpointsSingleEntryMatchesEndpoint pins that the two config fields are
// the same feature, so a caller migrating from Endpoint to a one-element
// Endpoints sees no change at all.
func TestEndpointsSingleEntryMatchesEndpoint(t *testing.T) {
	url, hits := countingServer(t, http.StatusOK, okValueBody)
	p := New(Config{Endpoints: []string{url}, InsecureNoTLS: true})

	v, err := p.Resolve(context.Background(), testRef(t))
	if err != nil {
		t.Fatalf("Resolve returned unexpected error: %v", err)
	}
	if got, want := string(v.Bytes), "healthy"; got != want {
		t.Errorf("Bytes = %q, want %q", got, want)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("request count = %d, want 1", got)
	}
}

// TestEndpointsGetIndependentTransports covers the reason each endpoint owns
// its own client: a unix:// replica and an https:// replica cannot share a
// transport, since the unix one dials a fixed socket and ignores the URL host.
func TestEndpointsGetIndependentTransports(t *testing.T) {
	p := New(Config{Endpoints: []string{"unix:///run/a.sock", "https://replica-b:8443"}})
	if p.endpointErr != nil {
		t.Fatalf("unexpected endpointErr: %v", p.endpointErr)
	}
	if len(p.endpoints) != 2 {
		t.Fatalf("len(endpoints) = %d, want 2", len(p.endpoints))
	}
	if p.endpoints[0].client == p.endpoints[1].client {
		t.Fatal("both endpoints share one *http.Client; a unix endpoint and an https endpoint need different transports")
	}
	if got, want := p.endpoints[1].baseURL, "https://replica-b:8443"; got != want {
		t.Errorf("endpoints[1].baseURL = %q, want %q", got, want)
	}
}

// TestWithHTTPClientAppliesToEveryEndpoint pins New's documented precedence in
// the multi-endpoint case: a caller-supplied client replaces every
// per-endpoint client, not just the first one's.
func TestWithHTTPClientAppliesToEveryEndpoint(t *testing.T) {
	custom := &http.Client{}
	p := New(Config{Endpoints: []string{"https://a:8443", "https://b:8443"}}, WithHTTPClient(custom))

	for i, ep := range p.endpoints {
		if ep.client != custom {
			t.Errorf("endpoints[%d].client is not the injected client", i)
		}
	}
}

func TestShouldFailover(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"not found", fmt.Errorf("%w: x", mamori.ErrNotFound), false},
		{"permission denied", fmt.Errorf("%w: x", mamori.ErrPermissionDenied), false},
		{"unauthenticated", fmt.Errorf("%w: x", mamori.ErrUnauthenticated), false},
		{"invalid", fmt.Errorf("%w: x", mamori.ErrInvalid), false},
		{"rate limited", fmt.Errorf("%w: x", mamori.ErrRateLimited), false},
		{"unavailable", fmt.Errorf("%w: x", mamori.ErrUnavailable), true},
		{"unclassified", errors.New("dial tcp: connection refused"), true},
		{"deadline exceeded", fmt.Errorf("get: %w", context.DeadlineExceeded), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldFailover(tc.err); got != tc.want {
				t.Fatalf("shouldFailover(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}
