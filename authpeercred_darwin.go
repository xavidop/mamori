//go:build darwin

package mamori

import (
	"fmt"
	"net"
	"net/http"

	"golang.org/x/sys/unix"
)

// Authenticate implements Authenticator by delegating to the
// platform-independent core in authpeercred.go (peerCred.authenticate);
// Darwin's own contribution to PeerCred is PeerCredFromConn below.
func (p peerCred) Authenticate(r *http.Request) (Identity, error) {
	return p.authenticate(r)
}

// PeerCredFromConn reads conn's peer credentials via LOCAL_PEERCRED
// (unix.GetsockoptXucred), the Darwin/BSD kernel's record of the uid/gid of
// the process on the other end of a Unix-domain-socket connection at the
// time it was accepted. See authpeercred_linux.go's PeerCredFromConn for the
// Linux counterpart, and PeerCred's doc comment in authpeercred.go for the
// full ConnContext plumbing this feeds.
//
// Xucred carries no pid on Darwin (LOCAL_PEERPID is a separate sockopt, not
// read here), so the returned Ucred's PID is always 0; Identity.Attrs
// therefore always reports "pid":"0" on Darwin - documented here rather than
// silently wrong.
func PeerCredFromConn(conn net.Conn) (Ucred, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return Ucred{}, fmt.Errorf("mamori: peer creds: not a unix connection (%T)", conn)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Ucred{}, fmt.Errorf("mamori: peer creds: %w", err)
	}
	var cred *unix.Xucred
	var sockErr error
	if ctlErr := raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); ctlErr != nil {
		return Ucred{}, fmt.Errorf("mamori: peer creds: %w", ctlErr)
	}
	if sockErr != nil {
		return Ucred{}, fmt.Errorf("mamori: peer creds: %w", sockErr)
	}
	gid := 0
	if cred.Ngroups > 0 {
		gid = int(cred.Groups[0])
	}
	return Ucred{UID: int(cred.Uid), GID: gid}, nil
}
