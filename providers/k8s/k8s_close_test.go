package k8s

import (
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"testing"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// countingTransport wraps a real RoundTripper and counts calls to
// CloseIdleConnections, so a test can prove closeIdleConnectionsSafely
// actually reached it rather than merely compiling a call that never fires.
type countingTransport struct {
	http.RoundTripper
	closes int
}

func (c *countingTransport) CloseIdleConnections() {
	c.closes++
	if cc, ok := c.RoundTripper.(interface{ CloseIdleConnections() }); ok {
		cc.CloseIdleConnections()
	}
}

// fakeWrapper is a minimal utilnet.RoundTripperWrapper, standing in for the
// client-go transport wrappers (user agent, bearer token, impersonation) that
// sit between rc.Client.Transport and whatever base RoundTripper is
// underneath. None of the real ones implement CloseIdleConnections
// themselves, which is exactly what makes them wrappers rather than leaves in
// this chain.
type fakeWrapper struct {
	inner http.RoundTripper
}

func (f *fakeWrapper) RoundTrip(r *http.Request) (*http.Response, error) { return f.inner.RoundTrip(r) }
func (f *fakeWrapper) WrappedRoundTripper() http.RoundTripper            { return f.inner }

var _ utilnet.RoundTripperWrapper = (*fakeWrapper)(nil)

// connReused issues one GET against target through http.DefaultClient and
// reports whether the round trip reused a pooled connection rather than
// dialing a new one.
func connReused(t *testing.T, target string) bool {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	var reused bool
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	return reused
}

// TestCloseIdleConnectionsSafelyReleasesARealTransport is the positive case:
// unwrapping a chain that bottoms out at a genuine, non-shared transport must
// still call CloseIdleConnections on it. Without this, a version of the
// nil/identity guard that refused too broadly (e.g. skipping every transport
// reached through any wrapper) would leave every real cluster connection
// leaking right alongside the DefaultTransport case this file exists to
// guard against.
func TestCloseIdleConnectionsSafelyReleasesARealTransport(t *testing.T) {
	ct := &countingTransport{RoundTripper: http.DefaultTransport}
	closeIdleConnectionsSafely(&fakeWrapper{inner: &fakeWrapper{inner: ct}})
	if ct.closes != 1 {
		t.Fatalf("CloseIdleConnections called %d times, want 1", ct.closes)
	}
}

// TestCloseIdleConnectionsSafelyRefusesDefaultTransport is the direct,
// no-networking proof that the identity check fires even after unwrapping
// several wrapper layers, not only when http.DefaultTransport is handed in
// directly. It uses the same connReused trick as the end-to-end tests below,
// which is what makes this a proof of "nothing was released" rather than a
// proof of "the call did not panic".
func TestCloseIdleConnectionsSafelyRefusesDefaultTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(http.DefaultClient.CloseIdleConnections)

	if connReused(t, srv.URL) {
		t.Fatal("first request unexpectedly reused a connection; test setup is broken")
	}

	closeIdleConnectionsSafely(&fakeWrapper{inner: &fakeWrapper{inner: http.DefaultTransport}})

	if !connReused(t, srv.URL) {
		t.Fatal("closeIdleConnectionsSafely evicted the process-global http.DefaultTransport's idle " +
			"connection pool after unwrapping wrapper layers; it must refuse at the bottom of the " +
			"chain, not only when handed http.DefaultTransport directly")
	}
}

// TestCloseDoesNotEvictTheSharedDefaultTransportViaFactory reproduces the
// direct case: a WithClientFactory that hands back
// kubernetes.NewForConfigAndClient(cfg, callerClient) with callerClient left
// at &http.Client{} (no Transport set). ownClient is true for this path (see
// Close's doc comment on why a factory-built clientset still counts as
// owned), so Close reaches closeIdleConnections; that function must still
// refuse, because rc.Client is exactly the caller's client, not one this
// provider is free to release from under it.
func TestCloseDoesNotEvictTheSharedDefaultTransportViaFactory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(http.DefaultClient.CloseIdleConnections)

	if connReused(t, srv.URL) {
		t.Fatal("first request unexpectedly reused a connection; test setup is broken")
	}

	p := New(WithClientFactory(func() (kubernetes.Interface, error) {
		return kubernetes.NewForConfigAndClient(&rest.Config{Host: srv.URL}, &http.Client{})
	}))
	if _, err := p.getClient(); err != nil {
		t.Fatalf("getClient: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !connReused(t, srv.URL) {
		t.Fatal("Close evicted the process-global http.DefaultTransport's idle connection pool " +
			"through a WithClientFactory client left with no Transport set")
	}
}

// TestCloseDoesNotEvictTheSharedDefaultTransportViaDefaultConfig reproduces
// the subtle case named in this fix: a clientset built from a plain
// rest.Config with NO TLS material at all - no CA, no client cert, not
// insecure, no ServerName or NextProtos, no custom dialer or proxy - exactly
// the shape client-go's own transport cache hands http.DefaultTransport back
// for, wrapped in a *transport.userAgentRoundTripper because NewForConfig
// always sets a UserAgent. This is NOT the ordinary in-cluster or kubeconfig
// shape: rest.InClusterConfig sets a CAFile and a typical kubeconfig sets
// CAData, either of which is enough on its own to make client-go build a
// real *http.Transport instead. What this config DOES match is a plain-http
// host - kubectl proxy, or (as here) an in-process test server - which is
// narrower than "the default path" but still a config nothing about this
// test's construction looks unusual: no caller ever touched WithHTTPClient,
// only rest.Config.Host. A naive fix that unwrapped the chain and called
// CloseIdleConnections on whatever it found at the bottom, with no identity
// check, would evict the shared pool here.
func TestCloseDoesNotEvictTheSharedDefaultTransportViaDefaultConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(http.DefaultClient.CloseIdleConnections)

	if connReused(t, srv.URL) {
		t.Fatal("first request unexpectedly reused a connection; test setup is broken")
	}

	p := New(WithClientFactory(func() (kubernetes.Interface, error) {
		return kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	}))
	if _, err := p.getClient(); err != nil {
		t.Fatalf("getClient: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !connReused(t, srv.URL) {
		t.Fatal("Close evicted the process-global http.DefaultTransport's idle connection pool " +
			"through the ordinary default-config path (no custom TLS material), which client-go's " +
			"own transport cache resolves to http.DefaultTransport underneath a userAgentRoundTripper")
	}
}

// TestCloseIdleConnectionsSafelyNilIsNoop is a smoke test, not a proof: it
// only asserts that passing a nil Transport does not panic. It does not pin
// the `rt == nil` check's behavior, because that check is not load-bearing
// here - removing it leaves this test passing regardless (confirmed in this
// change's mutation table), since a genuinely nil http.RoundTripper matches
// neither the closeIdler nor the RoundTripperWrapper case in the type switch
// below on its own. The check is kept for parity with
// k8s.io/apimachinery/pkg/util/net.CloseIdleConnectionsFor, which has the
// identical explicit nil check, and as documentation of the precondition,
// not because this test would catch its removal.
func TestCloseIdleConnectionsSafelyNilIsNoop(t *testing.T) {
	closeIdleConnectionsSafely(nil) // must not panic
}
