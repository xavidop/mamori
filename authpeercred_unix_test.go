//go:build linux || darwin

package mamori_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// realUnixPeer listens on a real, short-lived Unix-domain socket, dials it
// from the same process, and returns the server-side accepted connection.
// It deliberately does NOT use t.TempDir(): that helper nests the socket
// under a directory named after the test function, and on Darwin a
// sockaddr_un's path is capped at 104 bytes total (unix(4)) - a limit this
// package's own longer test names (TestPeerCredAuthenticatesRealUnixPeer...)
// blow straight through, failing bind with EINVAL. os.MkdirTemp against the
// bare default temp dir, with a short prefix and an even shorter socket
// filename, keeps the whole path comfortably under that cap on every
// platform this file builds for (Linux's own cap, 108 bytes, is more
// forgiving but not unlimited either).
func realUnixPeer(t *testing.T) net.Conn {
	t.Helper()
	dir, err := os.MkdirTemp("", "mpc")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	type acceptResult struct {
		conn net.Conn
		err  error
	}
	acceptCh := make(chan acceptResult, 1)
	go func() {
		c, err := ln.Accept()
		acceptCh <- acceptResult{conn: c, err: err}
	}()

	cli, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	select {
	case res := <-acceptCh:
		if res.err != nil {
			t.Fatalf("Accept: %v", res.err)
		}
		t.Cleanup(func() { _ = res.conn.Close() })
		return res.conn
	case <-time.After(2 * time.Second):
		t.Fatal("Accept timed out")
		return nil // unreachable, satisfies the compiler
	}
}

// TestPeerCredFromConnRealUnixSocket is the real-socket test the brief asks
// for on a build platform that actually supports peer credentials: it reads
// a real, accepted Unix connection's peer credentials with the platform's
// own PeerCredFromConn (SO_PEERCRED on Linux, GetsockoptXucred on Darwin -
// see authpeercred_linux.go / authpeercred_darwin.go), asserting the kernel
// reports back this same test process's own uid: since both ends of the
// socket are this process (Dial and Accept both run in the test binary),
// the peer the kernel names IS this process, the one deterministic fact a
// real cred read can be checked against without a second process.
func TestPeerCredFromConnRealUnixSocket(t *testing.T) {
	srv := realUnixPeer(t)

	cred, err := mamori.PeerCredFromConn(srv)
	if err != nil {
		t.Fatalf("PeerCredFromConn: %v", err)
	}
	if want := os.Getuid(); cred.UID != want {
		t.Fatalf("PeerCredFromConn UID = %d, want %d (this process's own uid)", cred.UID, want)
	}
}

// TestPeerCredAuthenticatesRealUnixPeer is the uid-match happy path the
// brief calls out as needing a real Unix-socket peer: it feeds a real,
// kernel-read Ucred (from PeerCredFromConn, via the same real socket
// realUnixPeer sets up) through the exact same seam a wired-up server would
// use - ContextWithPeerCred, then PeerCred.Authenticate - proving the full
// core-side path end to end on a platform where peer credentials are
// supported. What is NOT exercised here is the server module's ConnContext
// plumbing itself (the listener wrapper that calls PeerCredFromConn and
// ContextWithPeerCred per accepted connection): that is server-specific and
// lives in a later task, covered by that module's own integration test.
func TestPeerCredAuthenticatesRealUnixPeer(t *testing.T) {
	srv := realUnixPeer(t)

	cred, err := mamori.PeerCredFromConn(srv)
	if err != nil {
		t.Fatalf("PeerCredFromConn: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(mamori.ContextWithPeerCred(req.Context(), cred))

	a := mamori.PeerCred(mamori.PeerCredOptions{UIDs: []int{cred.UID}})
	id, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate denied the real peer's own uid: %v", err)
	}
	if want := "uid:" + strconv.Itoa(cred.UID); id.Subject != want {
		t.Fatalf("Identity.Subject = %q, want %q", id.Subject, want)
	}

	// A disjoint uid/gid allowlist must still deny the same real peer.
	deny := mamori.PeerCred(mamori.PeerCredOptions{UIDs: []int{cred.UID + 1000003}, GIDs: []int{cred.GID + 1000003}})
	if _, err := deny.Authenticate(req); err == nil {
		t.Fatal("Authenticate allowed the real peer against a disjoint allowlist, want deny")
	}
}
