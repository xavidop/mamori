package mamori

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// adminTestConfig is a small fixture with a default, so tests in this file
// never need to set an environment variable just to get a healthy watcher.
type adminTestConfig struct {
	A string `source:"env:MAMORI_ADMINHTTP_TEST_A" default:"alpha"`
}

// freeAddr asks the OS for a free port by binding to it and immediately
// releasing it, so the returned address is very likely (though never
// perfectly, guaranteed) free for the caller to bind next.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeAddr: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("freeAddr: close: %v", err)
	}
	return addr
}

// TestAdminHTTPServesHealthz confirms WithAdminHTTP actually binds and
// serves the same routes Handler would, on the address the OS chose for
// "127.0.0.1:0". AdminAddr is how the test (and any real caller using port
// 0) discovers what that address turned out to be.
func TestAdminHTTPServesHealthz(t *testing.T) {
	defer goleak.VerifyNone(t)
	w, err := Watch[adminTestConfig](context.Background(), WithAdminHTTP("127.0.0.1:0"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	addr := w.AdminAddr()
	if addr == nil {
		t.Fatal("AdminAddr() = nil, want the bound address")
	}

	resp, err := http.Get("http://" + addr.String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	defer http.DefaultClient.CloseIdleConnections()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
}

// TestAdminHTTPOffByDefaultNoGoroutineNoListener is the off-by-default half
// of the lifecycle contract: with no WithAdminHTTP, Watch starts no extra
// goroutine (goleak.VerifyNone would fail otherwise) and binds no listener
// (a connect attempt to a pre-chosen, otherwise-idle port must fail).
func TestAdminHTTPOffByDefaultNoGoroutineNoListener(t *testing.T) {
	defer goleak.VerifyNone(t)

	addr := freeAddr(t) // a port nothing should be listening on

	w, err := Watch[adminTestConfig](context.Background())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.AdminAddr(); got != nil {
		t.Fatalf("AdminAddr() = %v, want nil when WithAdminHTTP was not used", got)
	}

	conn, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		t.Fatalf("dial to %s unexpectedly succeeded; no listener should be bound without WithAdminHTTP", addr)
	}
}

// TestAdminHTTPBindFailureFailsWatch is the fail-fast half of the contract:
// a bind failure (here, the address is already occupied) must fail Watch
// itself with the bind error and a nil watcher, exactly like Watch's
// existing fail-fast behavior on the initial Load, rather than starting the
// watcher and silently never serving admin traffic.
func TestAdminHTTPBindFailureFailsWatch(t *testing.T) {
	defer goleak.VerifyNone(t)

	occupying, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy listen: %v", err)
	}
	defer func() { _ = occupying.Close() }()
	addr := occupying.Addr().String()

	w, err := Watch[adminTestConfig](context.Background(), WithAdminHTTP(addr))
	if err == nil {
		_ = w.Close()
		t.Fatal("Watch succeeded against an already-bound address, want a bind error")
	}
	if w != nil {
		t.Fatalf("Watch returned a non-nil watcher alongside an error: %+v", w)
	}
}

// TestAdminHTTPCloseReleasesPort proves the other half of the shutdown
// contract: once Close returns, the port is actually free, not just that the
// process believes it shut the server down. A fresh Listen on the exact same
// address is the only way to be sure the kernel agrees.
func TestAdminHTTPCloseReleasesPort(t *testing.T) {
	defer goleak.VerifyNone(t)

	w, err := Watch[adminTestConfig](context.Background(), WithAdminHTTP("127.0.0.1:0"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	addr := w.AdminAddr()
	if addr == nil {
		t.Fatal("AdminAddr() = nil")
	}
	addrStr := addr.String()

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ln, err := net.Listen("tcp", addrStr)
	if err != nil {
		t.Fatalf("rebind %s after Close: %v (port was not released)", addrStr, err)
	}
	_ = ln.Close()
}

// generateSelfSignedCert builds a throwaway self-signed certificate valid
// for 127.0.0.1, entirely with the standard library, so
// TestAdminHTTPTLSServesHTTPS has no external test-fixture dependency.
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

// TestAdminHTTPTLSServesHTTPS confirms WithAdminTLS actually wraps the
// listener in TLS: an HTTPS client with the self-signed cert trusted
// succeeds, and a plain HTTP client speaking cleartext to the same port
// fails, since it is a TLS listener now, not a plaintext one.
func TestAdminHTTPTLSServesHTTPS(t *testing.T) {
	defer goleak.VerifyNone(t)

	cert := generateSelfSignedCert(t)
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}

	w, err := Watch[adminTestConfig](context.Background(),
		WithAdminHTTP("127.0.0.1:0"), WithAdminTLS(tlsCfg))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	addr := w.AdminAddr()
	if addr == nil {
		t.Fatal("AdminAddr() = nil")
	}

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // test-only trust of our own throwaway cert
		Timeout:   2 * time.Second,
	}
	defer client.CloseIdleConnections()
	resp, err := client.Get("https://" + addr.String() + "/healthz")
	if err != nil {
		t.Fatalf("HTTPS GET /healthz: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTPS GET /healthz status = %d, want 200", resp.StatusCode)
	}

	// A plain HTTP client speaking cleartext to what is now a TLS-only
	// listener must not get a normal 200: either the request fails outright
	// (connection reset, handshake-adjacent error), or, as Go's http.Server
	// does, it detects the plaintext request and answers 400 rather than
	// serving the route. Either way, "got healthz's 200 in the clear" is what
	// must never happen.
	plainClient := &http.Client{Timeout: 2 * time.Second}
	defer plainClient.CloseIdleConnections()
	plainResp, plainErr := plainClient.Get("http://" + addr.String() + "/healthz")
	if plainErr == nil {
		_, _ = io.Copy(io.Discard, plainResp.Body)
		_ = plainResp.Body.Close()
		if plainResp.StatusCode == http.StatusOK {
			t.Fatalf("plain HTTP GET to a TLS-only admin port got 200, want failure or a non-200 status")
		}
	}
}
