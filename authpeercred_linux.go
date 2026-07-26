//go:build linux

package mamori

import (
	"fmt"
	"net"
	"net/http"

	"golang.org/x/sys/unix"
)

// Authenticate implements Authenticator by delegating to the
// platform-independent core in authpeercred.go (peerCred.authenticate);
// Linux's own contribution to PeerCred is PeerCredFromConn below, which a
// Unix-socket listener wrapper calls to produce the Ucred that ends up in
// the request context this reads.
func (p peerCred) Authenticate(r *http.Request) (Identity, error) {
	return p.authenticate(r)
}

// PeerCredFromConn reads conn's peer credentials via SO_PEERCRED, the Linux
// kernel's record of the uid/gid/pid of the process on the other end of a
// Unix-domain-socket connection at the time it was accepted. It is exported
// for the server module's Unix-socket listener wrapper (a later task; see
// PeerCred's doc comment in authpeercred.go for the full ConnContext
// plumbing this feeds): the listener calls this once per accepted
// connection and passes the result to ContextWithPeerCred.
func PeerCredFromConn(conn net.Conn) (Ucred, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return Ucred{}, fmt.Errorf("mamori: peer creds: not a unix connection (%T)", conn)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Ucred{}, fmt.Errorf("mamori: peer creds: %w", err)
	}
	var cred *unix.Ucred
	var sockErr error
	if ctlErr := raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); ctlErr != nil {
		return Ucred{}, fmt.Errorf("mamori: peer creds: %w", ctlErr)
	}
	if sockErr != nil {
		return Ucred{}, fmt.Errorf("mamori: peer creds: %w", sockErr)
	}
	return Ucred{UID: int(cred.Uid), GID: int(cred.Gid), PID: int(cred.Pid)}, nil
}
