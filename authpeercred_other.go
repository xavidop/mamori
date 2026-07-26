//go:build !linux && !darwin

package mamori

import (
	"errors"
	"net"
	"net/http"
)

// Authenticate always denies on this platform: mamori has no way to read
// kernel-verified peer credentials for a Unix-domain-socket connection
// outside Linux (SO_PEERCRED) and Darwin (LOCAL_PEERCRED), so PeerCred must
// refuse every request here rather than risk a caller believing it is
// checking an identity it cannot actually verify. This is a hard,
// unconditional deny - never a fallthrough to "no restriction configured" -
// regardless of PeerCredOptions or of anything that might otherwise be
// found in the request context.
func (p peerCred) Authenticate(*http.Request) (Identity, error) {
	return Identity{}, errors.New("mamori: peer credentials unsupported on this platform")
}

// PeerCredFromConn always fails on this platform, with the same signature as
// its Linux (authpeercred_linux.go) and Darwin (authpeercred_darwin.go)
// counterparts: mamori has no way to read kernel-verified peer credentials
// for a Unix-domain-socket connection outside those two. It exists here so a
// caller outside this package - specifically, the server module's
// Unix-socket listener wrapper (see PeerCred's doc comment in
// authpeercred.go for the full ConnContext plumbing this feeds) - can call
// mamori.PeerCredFromConn unconditionally, without its own per-platform build
// tags: the error this returns is exactly what that wrapper is already
// expected to treat as "do not stash a peer cred for this connection", so a
// PeerCred authenticator then denies for the same reason Authenticate above
// does directly, on every platform this file's build tag covers.
func PeerCredFromConn(net.Conn) (Ucred, error) {
	return Ucred{}, errors.New("mamori: peer credentials unsupported on this platform")
}
