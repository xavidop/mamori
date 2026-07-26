//go:build linux || darwin

// PeerCred's kernel-verified-credential reading (mamori.PeerCredFromConn) is
// only implemented on Linux and Darwin (see mamori's authpeercred_linux.go /
// authpeercred_darwin.go); every other platform's PeerCredFromConn
// (authpeercred_other.go) unconditionally fails, by design (see that file's
// doc comment). So the integration proof below - that a REAL Unix-domain
// socket connection's peer uid actually reaches mamori.PeerCred.Authenticate
// through the ConnContext seam newHTTPServer wires up (transport.go) - can
// only pass on those two platforms; everywhere else it would only prove "no
// peer credentials were ever available", which is not what this file exists
// to test. Build-tagged out rather than skipped at runtime, so `go test`
// output on an unsupported platform does not carry a permanently-skipped
// test that could be mistaken for coverage that exists.
package server

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
	"go.uber.org/goleak"
)

// TestPeerCredOverRealUDSAuthenticatesByUID is the happy-path integration
// proof for the ConnContext seam (see transport.go's newHTTPServer,
// PeerCred's own doc comment in mamori's authpeercred.go, and this file's
// package doc comment for why it needs a real platform): a genuine client
// process connecting over a genuine Unix-domain socket has ITS
// kernel-verified uid captured at accept time and reaches
// mamori.PeerCred.Authenticate - never anything the client presents in the
// request itself (there is nothing here for it to present: the request
// carries no credential of any kind).
func TestPeerCredOverRealUDSAuthenticatesByUID(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("peercred")
	p.Set("k", "v1")

	sockPath := shortSocketPath(t)
	uid := os.Getuid()

	s, err := New(
		WithPolicy(AllowAll()),
		WithAuth(mamori.PeerCred(mamori.PeerCredOptions{UIDs: []int{uid}})),
		Bind("b", "peercred://k"),
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
	if string(vb.Bytes) != "v1" {
		t.Fatalf("value = %q, want v1", vb.Bytes)
	}
}

// TestPeerCredOverRealUDSDeniesUnmatchedUID is the other half: a real
// connection whose kernel-verified peer uid (this test process's own real
// uid - there is no way to dial as a different uid without actually running
// as one) is NOT in PeerCredOptions.UIDs must be denied, proving PeerCred
// enforces its allowlist end to end over a real socket rather than passing
// every successfully-read credential regardless of the option.
func TestPeerCredOverRealUDSDeniesUnmatchedUID(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := mamoritest.NewProvider("peercreddeny")
	p.Set("k", "v1")

	sockPath := shortSocketPath(t)
	// An arbitrarily offset uid, not this test process's own - the point is
	// only that it must not equal os.Getuid().
	notMyUID := os.Getuid() + 999983

	s, err := New(
		WithPolicy(AllowAll()),
		WithAuth(mamori.PeerCred(mamori.PeerCredOptions{UIDs: []int{notMyUID}})),
		Bind("b", "peercreddeny://k"),
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

	resp, err := client.Get("http://unix/v1/values/b")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = 200, want a denial (peer uid %d not in allowlist)", os.Getuid())
	}
}
