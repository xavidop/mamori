package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
	"go.uber.org/goleak"
)

// transportTestWait bounds how long the polling helpers below wait for a
// listener to be bound (Addrs) or a binding to resolve (waitForLookup, from
// resolve_test.go, reused here unchanged). Mirrors resolveTestWait's own
// reasoning: nothing this file exercises is bound by a real network's
// latency, so anything that does not land within this window is a bug, not
// a slow environment.
const transportTestWait = 2 * time.Second

// waitForAddrs polls s.Addrs() until it reports exactly want bound
// listeners, returning them. Serve binds every listener before it starts any
// serving goroutine, but it runs as a whole in a background goroutine in
// these tests (Serve blocks), so a test must not read Addrs() before that
// goroutine has actually reached the point of binding.
func waitForAddrs(t *testing.T, s *Server, want int) []net.Addr {
	t.Helper()
	deadline := time.Now().Add(transportTestWait)
	for time.Now().Before(deadline) {
		if addrs := s.Addrs(); len(addrs) == want {
			return addrs
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("mamori/server: Addrs() did not reach %d bound listener(s) within %s (last observed: %v)", want, transportTestWait, s.Addrs())
	return nil
}

// shortSocketPath returns a path for a Unix-domain socket in a fresh
// temporary directory that is NOT t.TempDir(): t.TempDir() embeds the full
// (sub)test name in its path, which for some of this file's more
// verbosely-named tests is long enough to overflow sockaddr_un's sun_path
// (108 bytes on Linux, a tighter 104 on Darwin - see unix(7)/unix(4)),
// making net.Listen("unix", ...) fail with "bind: invalid argument" for a
// reason that has nothing to do with the transport code under test. The
// directory this creates is removed via t.Cleanup, the same lifetime
// t.TempDir() would otherwise have given it.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mamori-sock")
	if err != nil {
		t.Fatalf("create temp dir for unix socket: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// unixHTTPClient returns an http.Client that dials sockPath over a
// Unix-domain socket for every request, regardless of the URL's host - so
// tests can write plain "http://unix/..." URLs and have them actually reach
// the Unix listener under test.
func unixHTTPClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 2 * time.Second,
	}
}

// getJSON performs a GET against url with client, requires a 200 response,
// and decodes its body as a valueBody (wire.go) - the shape every successful
// GET /v1/values/{name} in this file's tests expects back.
func getJSON(t *testing.T, client *http.Client, url string) valueBody {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, body = %s", url, resp.StatusCode, body)
	}
	var vb valueBody
	if err := json.Unmarshal(body, &vb); err != nil {
		t.Fatalf("GET %s: decode %q: %v", url, body, err)
	}
	return vb
}

// generateSelfSignedCert builds a throwaway self-signed certificate valid
// for 127.0.0.1, entirely with the standard library - the server module's
// own copy of the same fixture core's adminhttp_test.go uses (it cannot be
// imported: that one is unexported, in a different module), so
// TestTCPTLSServesHTTPSRejectsPlaintext has no external test-fixture
// dependency either.
func generateSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	return cert
}

// TestUnixServesBinding is the UDS happy path: a real client dialing the
// Unix socket Serve bound gets the resolved binding value back over
// GET /v1/values/{name}.
func TestUnixServesBinding(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("udsserve")
	p.Set("k", "v1")

	sockPath := shortSocketPath(t)

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "udsserve://k"),
		WithProvider(p),
		Unix(sockPath, 0o600),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	defer func() { _ = s.Close() }()

	waitForAddrs(t, s, 1)
	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	client := unixHTTPClient(sockPath)
	defer client.CloseIdleConnections()

	vb := getJSON(t, client, "http://unix/v1/values/b")
	if vb.Name != "b" || string(vb.Bytes) != "v1" {
		t.Fatalf("valueBody = %+v, want Name=b Bytes=v1", vb)
	}
}

// TestUnixSocketHasRequestedMode confirms Unix(path, mode) actually applies
// mode to the socket file, not merely whatever the process umask would have
// produced from a plain net.Listen("unix", ...) - see bindUnix's doc comment
// in transport.go for why an explicit os.Chmod after Listen is required.
func TestUnixSocketHasRequestedMode(t *testing.T) {
	defer goleak.VerifyNone(t)

	sockPath := shortSocketPath(t)

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Unix(sockPath, 0o600),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	defer func() { _ = s.Close() }()

	waitForAddrs(t, s, 1)

	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket permissions = %o, want %o", got, 0o600)
	}
}

// TestTCPTLSServesHTTPSRejectsPlaintext confirms TCP(addr, TLS(cfg)) actually
// wraps the listener in TLS: an HTTPS client trusting the self-signed cert
// succeeds, and a plain HTTP client speaking cleartext to the same port does
// not get a normal 200 - it is a TLS listener now, not a plaintext one.
func TestTCPTLSServesHTTPSRejectsPlaintext(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("tcptls")
	p.Set("k", "v1")

	cert := generateSelfSignedCert(t)
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}

	s, err := New(
		WithPolicy(AllowAll()),
		WithAuth(stubAuth{}),
		Bind("b", "tcptls://k"),
		WithProvider(p),
		TCP("127.0.0.1:0", TLS(tlsCfg)),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	defer func() { _ = s.Close() }()

	addrs := waitForAddrs(t, s, 1)
	addr := addrs[0].String()
	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // test-only trust of our own throwaway cert
		Timeout:   2 * time.Second,
	}
	defer client.CloseIdleConnections()

	vb := getJSON(t, client, "https://"+addr+"/v1/values/b")
	if string(vb.Bytes) != "v1" {
		t.Fatalf("HTTPS value = %q, want v1", vb.Bytes)
	}

	plainClient := &http.Client{Timeout: 2 * time.Second}
	defer plainClient.CloseIdleConnections()
	plainResp, plainErr := plainClient.Get("http://" + addr + "/v1/values/b")
	if plainErr == nil {
		_, _ = io.Copy(io.Discard, plainResp.Body)
		_ = plainResp.Body.Close()
		if plainResp.StatusCode == http.StatusOK {
			t.Fatalf("plaintext GET to a TLS-only TCP listener got 200, want failure or a non-200 status")
		}
	}
}

// TestTCPWithoutTLSAndWithoutInsecureFailsConstruction confirms TCP(addr)
// with neither TLS(cfg) nor InsecureNoTLS() fails New outright - never a
// Server that would go on to bind a plaintext, credential-serving TCP port.
func TestTCPWithoutTLSAndWithoutInsecureFailsConstruction(t *testing.T) {
	_, err := New(
		WithPolicy(AllowAll()),
		WithAuth(stubAuth{}),
		TCP("127.0.0.1:0"),
	)
	if !errors.Is(err, errTCPRequiresTLS) {
		t.Fatalf("New error = %v, want errTCPRequiresTLS", err)
	}
}

// TestTCPInsecureNoTLSConstructsAndServesPlaintext is InsecureNoTLS's own
// positive proof: with it, TCP(addr) DOES construct (the error
// TestTCPWithoutTLSAndWithoutInsecureFailsConstruction guards against is
// specifically the absence of BOTH options, not TLS itself being
// mandatory), and Serve actually serves plaintext HTTP on it.
func TestTCPInsecureNoTLSConstructsAndServesPlaintext(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("tcpinsecure")
	p.Set("k", "v1")

	s, err := New(
		WithPolicy(AllowAll()),
		WithAuth(stubAuth{}),
		Bind("b", "tcpinsecure://k"),
		WithProvider(p),
		TCP("127.0.0.1:0", InsecureNoTLS()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	defer func() { _ = s.Close() }()

	addrs := waitForAddrs(t, s, 1)
	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	vb := getJSON(t, client, "http://"+addrs[0].String()+"/v1/values/b")
	if string(vb.Bytes) != "v1" {
		t.Fatalf("value = %q, want v1", vb.Bytes)
	}
}

// TestBothTransportsServeIdenticalBindings confirms a Unix listener and a
// TCP listener can run at once, serving the exact same binding table under
// one Policy/Handler, each independently.
func TestBothTransportsServeIdenticalBindings(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("bothtransports")
	p.Set("k", "v1")

	sockPath := shortSocketPath(t)

	s, err := New(
		WithPolicy(AllowAll()),
		WithAuth(stubAuth{}),
		Bind("b", "bothtransports://k"),
		WithProvider(p),
		Unix(sockPath, 0o600),
		TCP("127.0.0.1:0", InsecureNoTLS()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	defer func() { _ = s.Close() }()

	addrs := waitForAddrs(t, s, 2)
	var tcpAddr string
	for _, a := range addrs {
		if a.Network() == "tcp" {
			tcpAddr = a.String()
		}
	}
	if tcpAddr == "" {
		t.Fatalf("no tcp address among Addrs(): %v", addrs)
	}

	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	uc := unixHTTPClient(sockPath)
	defer uc.CloseIdleConnections()
	unixVB := getJSON(t, uc, "http://unix/v1/values/b")
	if string(unixVB.Bytes) != "v1" {
		t.Fatalf("unix value = %q, want v1", unixVB.Bytes)
	}

	tc := &http.Client{Timeout: 2 * time.Second}
	defer tc.CloseIdleConnections()
	tcpVB := getJSON(t, tc, "http://"+tcpAddr+"/v1/values/b")
	if string(tcpVB.Bytes) != "v1" {
		t.Fatalf("tcp value = %q, want v1", tcpVB.Bytes)
	}
}

// TestCloseUnlinksSocketAndReleasesPort confirms Close's shutdown contract
// end to end: once it returns, the Unix socket file is gone and the TCP port
// is genuinely free again (a fresh Listen on the exact same address is the
// only way to be sure the kernel agrees, not just that this process believes
// it shut the listeners down).
func TestCloseUnlinksSocketAndReleasesPort(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("closerelease")
	p.Set("k", "v1")

	sockPath := shortSocketPath(t)

	s, err := New(
		WithPolicy(AllowAll()),
		WithAuth(stubAuth{}),
		Bind("b", "closerelease://k"),
		WithProvider(p),
		Unix(sockPath, 0o600),
		TCP("127.0.0.1:0", InsecureNoTLS()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()

	addrs := waitForAddrs(t, s, 2)
	var tcpAddr string
	for _, a := range addrs {
		if a.Network() == "tcp" {
			tcpAddr = a.String()
		}
	}
	if tcpAddr == "" {
		t.Fatalf("no tcp address among Addrs(): %v", addrs)
	}

	if _, statErr := os.Stat(sockPath); statErr != nil {
		t.Fatalf("socket file missing before Close: %v", statErr)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, statErr := os.Stat(sockPath); !os.IsNotExist(statErr) {
		t.Fatalf("socket file still present after Close (err = %v), want it removed", statErr)
	}

	tcpLn, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		t.Fatalf("rebind %s after Close: %v (port was not released)", tcpAddr, err)
	}
	_ = tcpLn.Close()

	unixLn, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("rebind unix socket %s after Close: %v", sockPath, err)
	}
	_ = unixLn.Close()
	_ = os.Remove(sockPath)
}

// TestServeFailsFastOnBadBind proves the fail-fast contract: when one of
// several configured listeners cannot bind (here, a TCP address already
// occupied by another listener), Serve returns that bind error immediately
// rather than silently only serving the listeners that did succeed - and a
// listener that DID bind successfully before the failing one is still torn
// down cleanly by a subsequent Close (goleak.VerifyNone below is the actual
// proof of that: a leaked accept goroutine or unclosed listener would fail
// it).
func TestServeFailsFastOnBadBind(t *testing.T) {
	defer goleak.VerifyNone(t)

	occupying, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy listen: %v", err)
	}
	defer func() { _ = occupying.Close() }()
	badAddr := occupying.Addr().String()

	p := mamoritest.NewProvider("failfast")
	p.Set("k", "v1")

	sockPath := shortSocketPath(t)

	s, err := New(
		WithPolicy(AllowAll()),
		WithAuth(stubAuth{}),
		Bind("b", "failfast://k"),
		WithProvider(p),
		Unix(sockPath, 0o600),         // binds fine
		TCP(badAddr, InsecureNoTLS()), // fails: address already in use
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := s.Serve(context.Background()); err == nil {
		t.Fatal("Serve succeeded against an already-bound TCP address, want a bind error")
	}

	if _, statErr := os.Stat(sockPath); statErr != nil {
		t.Fatalf("expected the unix socket to have bound successfully before the TCP bind failed: %v", statErr)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, statErr := os.Stat(sockPath); !os.IsNotExist(statErr) {
		t.Fatalf("socket file still present after Close (err = %v), want it removed", statErr)
	}
}

// TestServeRespectsNoAuthOnTCPGateDefensively documents (and pins) that
// Serve re-checks the NoAuth+TCP refusal itself (see errNoAuthOnTCP in
// server.go), even though New already refuses to construct such a Server in
// the first place - see Serve's own doc comment in transport.go for why this
// is intentionally unreachable via the exported API, checked anyway.
func TestServeRespectsNoAuthOnTCPGateDefensively(t *testing.T) {
	s := &Server{policy: AllowAll(), noAuth: true, hasTCP: true}
	if err := s.Serve(context.Background()); !errors.Is(err, errNoAuthOnTCP) {
		t.Fatalf("Serve error = %v, want errNoAuthOnTCP", err)
	}
}

// TestCloseWithNoListenersIsSafe confirms closeTransports (and therefore
// Close as a whole) tolerates a Server that was never Served at all - no
// Unix(...)/TCP(...) options, start never called - the same "safe even if
// nothing ran" contract TestCloseWithoutStartIsSafe (resolve_test.go) pins
// for the resolver half.
func TestCloseWithNoListenersIsSafe(t *testing.T) {
	defer goleak.VerifyNone(t)

	s, err := New(WithPolicy(AllowAll()), NoAuth())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestCloseBeforeServeLeaksNothing pins the fix for the deterministic
// Close-before-Serve leak: a Close that runs before Serve/start ever ran
// must not permanently spend Close's one teardown against a Server that had
// nothing yet to tear down. Before the closed-flag fix (when Close was
// guarded by a bare sync.Once), this exact sequence let Close's
// one-and-only teardown fire with resolveCancel nil and no listeners bound -
// a no-op that nonetheless consumed the Once - so the Serve below would go
// on to bind a real Unix listener, spawn a real accept goroutine, and start
// a real resolver goroutine that no future Close could ever reach again (see
// closed's doc comment in server.go). Serve must instead refuse outright
// (errClosed), binding and spawning nothing at all, so there is nothing left
// for goleak to catch and no socket file ever created.
func TestCloseBeforeServeLeaksNothing(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("closebeforeserve")
	p.Set("k", "v1")

	sockPath := shortSocketPath(t)

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "closebeforeserve://k"),
		WithProvider(p),
		Unix(sockPath, 0o600),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close before Serve: %v", err)
	}

	// Serve is run in a background goroutine and given its own cancelable
	// ctx, and the result is observed through a bounded select rather than a
	// direct blocking call: against the pre-fix sync.Once code this exact
	// sequence leaves Serve blocked forever accepting connections nothing
	// will ever stop (the one Close that could have stopped it already
	// fired, against nothing), so a direct `s.Serve(ctx)` call here would
	// hang the test rather than fail it.
	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { errCh <- s.Serve(ctx) }()

	select {
	case serveErr := <-errCh:
		if !errors.Is(serveErr, errClosed) {
			t.Fatalf("Serve after Close error = %v, want errClosed", serveErr)
		}
	case <-time.After(transportTestWait):
		t.Fatal("Serve did not return promptly after a Close that ran before it started (want an immediate errClosed) - it likely bound listeners a spent Close can never reach again")
	}

	if _, statErr := os.Stat(sockPath); !os.IsNotExist(statErr) {
		t.Fatalf("socket file exists after Serve refused to run (err = %v), want no file ever created", statErr)
	}

	if got := len(s.Addrs()); got != 0 {
		t.Fatalf("Addrs() = %d bound listener(s) after Serve refused to run, want 0", got)
	}
}

// TestCloseIsIdempotentAfterServe confirms the normal Serve-then-Close path
// (unlike TestCloseBeforeServeLeaksNothing above) is unaffected by replacing
// closeOnce with the closed flag: after a real Serve has bound real
// listeners, a second Close (a deferred Close plus an explicit one on an
// error path, say) still returns nil without double-tearing-down anything,
// the socket file stays removed, and the TCP port stays released - the same
// end-to-end contract TestCloseUnlinksSocketAndReleasesPort pins for a
// single Close, now exercised through two.
func TestCloseIsIdempotentAfterServe(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("closetwiceafterserve")
	p.Set("k", "v1")

	sockPath := shortSocketPath(t)

	s, err := New(
		WithPolicy(AllowAll()),
		WithAuth(stubAuth{}),
		Bind("b", "closetwiceafterserve://k"),
		WithProvider(p),
		Unix(sockPath, 0o600),
		TCP("127.0.0.1:0", InsecureNoTLS()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()

	addrs := waitForAddrs(t, s, 2)
	var tcpAddr string
	for _, a := range addrs {
		if a.Network() == "tcp" {
			tcpAddr = a.String()
		}
	}
	if tcpAddr == "" {
		t.Fatalf("no tcp address among Addrs(): %v", addrs)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if _, statErr := os.Stat(sockPath); !os.IsNotExist(statErr) {
		t.Fatalf("socket file still present after Close (err = %v), want it removed", statErr)
	}

	tcpLn, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		t.Fatalf("rebind %s after Close: %v (port was not released)", tcpAddr, err)
	}
	_ = tcpLn.Close()
}

// TestCloseTerminatesActiveSSEConnectionPromptly pins the fix for the
// SSE-vs-Shutdown deadlock: newHTTPServer builds every listener's
// *http.Server with no BaseContext, so a request's r.Context() (what
// handleWatch's poll loop in handler.go actually selects on) is derived from
// the underlying net.Conn, not from this Server's own lifecycle - it is only
// canceled when the CLIENT closes its connection, never when Close runs.
// http.Server.Shutdown, in turn, waits for in-flight handlers to return on
// their own; it never force-closes an active connection. Put those two
// together and a live /v1/watch subscriber makes closeTransports's
// Shutdown(shutdownCtx) block for the full shutdownGrace deadline on every
// single Close, and the handleWatch goroutine for that connection outlives
// Close entirely (it keeps running, blocked in its select, until the client
// eventually disconnects or the process exits) - a goroutine and file
// descriptor leak on top of the slow shutdown.
//
// This test opens a real GET /v1/watch connection over a Unix socket, reads
// its first frame (proving the handler is live and past its setup, sitting
// in the ctx.Done()/heartbeat/poll select loop), and then calls Close
// WITHOUT ever letting the client disconnect. Close must still return well
// within shutdownGrace: it must cancel a serving context that every
// request's BaseContext derives from, so handleWatch's <-ctx.Done() fires on
// its own the moment shutdown begins, independent of the connection ever
// closing. goleak.VerifyNone (deferred) is the second half of the proof: a
// handleWatch goroutine still blocked on that stale connection after Close
// returns is exactly what it would catch.
func TestCloseTerminatesActiveSSEConnectionPromptly(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("sseclose")
	p.Set("k", "v1")

	sockPath := shortSocketPath(t)

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("w", "sseclose://k"),
		WithProvider(p),
		Unix(sockPath, 0o600),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()

	waitForAddrs(t, s, 1)
	waitForLookup(t, s, "w", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	// A plain unixHTTPClient will not do here: its 2-second aggregate
	// Timeout (right for this file's other, short request/response tests)
	// covers the ENTIRE request including reading the response body, so it
	// would silently close this SSE connection on its own well before
	// shutdownGrace (5s) ever came into play - masking the very bug this
	// test exists to catch. This connection must stay open, under nothing
	// but this test's own control, until Close (or a post-timeout
	// t.Fatal) ends it.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
	defer client.CloseIdleConnections()

	req, err := http.NewRequest(http.MethodGet, "http://unix/v1/watch?name=w", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	first := readSSEFrameWithTimeout(t, reader, 3*time.Second)
	if first.event != "update" {
		t.Fatalf("first frame event = %q, want update", first.event)
	}

	// The handler is now live in its select loop, on a connection this test
	// never closes. Measure Close on its own goroutine, bounded by
	// shutdownGrace plus slack, so a pre-fix hang fails this test instead of
	// hanging the whole suite.
	closeDone := make(chan error, 1)
	closeStart := time.Now()
	go func() { closeDone <- s.Close() }()

	select {
	case closeErr := <-closeDone:
		if closeErr != nil {
			t.Fatalf("Close: %v", closeErr)
		}
	case <-time.After(shutdownGrace + transportTestWait):
		t.Fatal("Close did not return at all within shutdownGrace + slack; an active SSE connection blocked graceful shutdown")
	}

	elapsed := time.Since(closeStart)
	if elapsed >= shutdownGrace/2 {
		t.Fatalf("Close took %s against a live SSE connection (shutdownGrace is %s); want it to return promptly because the serving context is canceled at the START of shutdown, not after connections happen to close", elapsed, shutdownGrace)
	}
}

// TestRestartOnSamePathKeepsSuccessorSocket pins the socket-ownership guard
// in closeTransports (see removeOwnedSocket).
//
// Restarting a server on a fixed socket path overlaps the two processes:
// binding removes whatever is at the path, so the incoming server replaces
// the outgoing server's socket file while the outgoing one is still draining.
// Before the guard, the outgoing server's cleanup then deleted the incoming
// server's socket, leaving a healthy server bound to a path nothing could
// dial and clients permanently unable to reconnect. Closing the old server
// must leave the new server's socket, and the new server, reachable.
func TestRestartOnSamePathKeepsSuccessorSocket(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("udsrestart")
	p.Set("k", "v1")

	sockPath := shortSocketPath(t)

	newServer := func() *Server {
		t.Helper()
		s, err := New(
			WithPolicy(AllowAll()),
			NoAuth(),
			Bind("b", "udsrestart://k"),
			WithProvider(p),
			Unix(sockPath, 0o600),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return s
	}

	// The outgoing server.
	oldSrv := newServer()
	oldCtx, oldCancel := context.WithCancel(context.Background())
	defer oldCancel()
	go func() { _ = oldSrv.Serve(oldCtx) }()
	waitForAddrs(t, oldSrv, 1)

	// The incoming server takes the path over, exactly as a restart does.
	newSrv := newServer()
	newCtx, newCancel := context.WithCancel(context.Background())
	defer newCancel()
	go func() { _ = newSrv.Serve(newCtx) }()
	defer func() { newCancel(); _ = newSrv.Close() }()
	waitForAddrs(t, newSrv, 1)
	waitForLookup(t, newSrv, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && k == "" && string(v.Bytes) == "v1"
	})

	// Now retire the outgoing server. Its cleanup must not touch the socket
	// file, which now belongs to the incoming server.
	oldCancel()
	if err := oldSrv.Close(); err != nil {
		t.Fatalf("closing the outgoing server: %v", err)
	}

	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("the outgoing server deleted its successor's socket at %s: %v", sockPath, err)
	}

	// The file surviving is not enough: the successor must still serve on it.
	client := unixHTTPClient(sockPath)
	defer client.CloseIdleConnections()
	vb := getJSON(t, client, "http://unix/v1/values/b")
	if vb.Name != "b" || string(vb.Bytes) != "v1" {
		t.Fatalf("valueBody = %+v, want Name=b Bytes=v1 from the surviving server", vb)
	}
}

// TestCloseRemovesItsOwnSocket is the other half of the guard: when no one
// took the path over, a closing server must still clean its socket file up
// rather than leaving it behind.
func TestCloseRemovesItsOwnSocket(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("udscleanup")
	p.Set("k", "v1")

	sockPath := shortSocketPath(t)

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "udscleanup://k"),
		WithProvider(p),
		Unix(sockPath, 0o600),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	waitForAddrs(t, s, 1)

	cancel()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket file still present at %s after Close (err=%v), want it unlinked", sockPath, err)
	}
}

// TestCloseWaitsForInFlightClose pins Close's contract: when Close returns,
// the server is actually closed, even if a DIFFERENT goroutine started the
// teardown first.
//
// This is easy to hit by accident. Serve calls Close itself when its context
// is cancelled (transport.go), so the ordinary `cancel(); srv.Close()` is two
// concurrent closers. Before, the second caller saw closed=true and returned
// nil immediately, while listeners were still bound and the Unix socket was
// still on disk. A supervisor restarting on a fixed socket path would then
// rebind while the outgoing server was still tearing down.
//
// DrainGrace makes the window deterministic rather than a timing race: the
// winning Close is forced to stay in teardown for the grace period, so a
// non-waiting Close would demonstrably return while the socket still exists.
func TestCloseWaitsForInFlightClose(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("udswait")
	p.Set("k", "v1")

	sockPath := shortSocketPath(t)
	const grace = 300 * time.Millisecond

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "udswait://k"),
		WithProvider(p),
		Unix(sockPath, 0o600),
		DrainGrace(grace),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	waitForAddrs(t, s, 1)

	// Cancelling makes Serve's own watcher call Close, which now holds the
	// teardown open for the grace period.
	cancel()

	// Give that first Close time to win the transition, so this really is
	// the second caller and not the first.
	time.Sleep(grace / 6)

	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	elapsed := time.Since(start)

	// The socket must be gone the moment Close returns. This is the
	// assertion that fails without the wait.
	if _, statErr := os.Stat(sockPath); !os.IsNotExist(statErr) {
		t.Errorf("Close returned after %v but the socket is still present at %s (err=%v); "+
			"Close must not report success while another teardown is still running",
			elapsed, sockPath, statErr)
	}

	// And it must have actually waited, rather than racing through by luck.
	if elapsed < grace/2 {
		t.Errorf("Close returned in %v, too fast to have waited for the in-flight teardown (grace %v)", elapsed, grace)
	}
}

// closeRaceIterations is how many Serve/Close races
// TestCloseRacingServeSetupNeverRacesListenWG drives. The window it aims at
// is a handful of instructions wide, so a single attempt proves nothing: at
// 25 iterations the pre-fix code was caught by -race on 20 of 20 local runs,
// and this leaves an order of magnitude of headroom on top of that for a
// slower or busier CI machine without making the test itself slow (the whole
// loop is well under a second even under -race).
const closeRaceIterations = 250

// TestCloseRacingServeSetupNeverRacesListenWG is the regression test for a
// Close that lands in the sliver of Serve between "the listener is bound and
// visible" and "the goroutine that will serve it has been registered with
// s.listenWG".
//
// Serve is documented to run in the background (`go s.Serve(ctx)`) with Close
// driven independently, so this interleaving is ordinary usage, not an abuse:
// every test in this file that does `go s.Serve(ctx)` + `defer s.Close()` is
// one instance of it, and TestUnixSocketHasRequestedMode is the one that
// happened to lose the coin flip on CI.
//
// What used to go wrong: Serve called s.listenWG.Add(1) with no
// synchronization against Close, while Close -> teardown -> closeTransports
// called s.listenWG.Wait(). sync.WaitGroup requires that "calls with a
// positive delta that occur when the counter is zero must happen before a
// Wait", and nothing in the code established that ordering. Under -race that
// is reported as a write (Wait) racing a read (Add) on the WaitGroup's
// internal semaphore word; without -race, sync itself can panic with
// "WaitGroup misuse: Add called concurrently with Wait". The fix moves the
// Add into the same resolveMu-guarded, closed-checked transition Serve
// already made before spawning anything (beginListening, resolve.go), which
// is exactly how start's resolveWG.Add calls have always been ordered against
// the same teardown.
//
// The loop closes as soon as Addrs() reports the listener, because that is
// the first moment an outside goroutine can observe Serve's progress and is
// therefore the tightest aim at the window. Each iteration also asserts the
// documented contracts still hold: Serve returns either nil or errClosed
// (never a bind failure from a socket the previous iteration failed to clean
// up), and Close always reports success. goleak covers the other half - that
// no interleaving leaves a listener goroutine, a watch goroutine, or a
// channel waiter behind.
func TestCloseRacingServeSetupNeverRacesListenWG(t *testing.T) {
	defer goleak.VerifyNone(t)

	sockPath := shortSocketPath(t)

	for i := 0; i < closeRaceIterations; i++ {
		p := mamoritest.NewProvider("udsrace")
		p.Set("k", "v1")

		s, err := New(
			WithPolicy(AllowAll()),
			NoAuth(),
			Bind("b", "udsrace://k"),
			WithProvider(p),
			Unix(sockPath, 0o600),
		)
		if err != nil {
			t.Fatalf("iteration %d: New: %v", i, err)
		}

		ctx, cancel := context.WithCancel(context.Background())

		var wg sync.WaitGroup
		wg.Add(2)

		var serveErr error
		go func() {
			defer wg.Done()
			serveErr = s.Serve(ctx)
		}()

		var closeErr error
		go func() {
			defer wg.Done()
			// Spin rather than sleep: a sleep of any length lands after the
			// window in nearly every iteration, which is precisely why the
			// existing tests only reproduced this on an unlucky CI run.
			deadline := time.Now().Add(transportTestWait)
			for len(s.Addrs()) == 0 && time.Now().Before(deadline) {
				runtime.Gosched()
			}
			closeErr = s.Close()
		}()

		wg.Wait()
		cancel()

		if serveErr != nil && !errors.Is(serveErr, errClosed) {
			t.Fatalf("iteration %d: Serve = %v, want nil or errClosed", i, serveErr)
		}
		if closeErr != nil {
			t.Fatalf("iteration %d: Close = %v, want nil", i, closeErr)
		}
	}
}
