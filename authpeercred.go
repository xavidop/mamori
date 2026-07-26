package mamori

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// Ucred is a Unix-socket peer's kernel-verified credentials, as read once by
// a listener at accept time (SO_PEERCRED on Linux, LOCAL_PEERCRED via
// GetsockoptXucred on Darwin - see the platform-specific PeerCredFromConn in
// authpeercred_linux.go / authpeercred_darwin.go). A connection's peer
// identity cannot change over its lifetime, so reading it once per accepted
// connection, rather than re-deriving it per request, loses nothing.
type Ucred struct {
	UID int
	GID int
	PID int
}

// peerCredContextKey is the well-known key both halves of the ConnContext
// seam (see PeerCred's doc comment) use to agree on where a request's peer
// credentials live. It is an unexported type, not merely an unexported value
// of an exported type, so nothing outside this package can construct a
// colliding key by accident or on purpose - the standard Go idiom for a
// context key meant to be unforgeable from outside its defining package.
type peerCredContextKey struct{}

// ContextWithPeerCred returns a copy of ctx carrying cred as the peer
// credentials of the Unix-socket connection ctx (or a context derived from
// it) belongs to. This is the injection half of the ConnContext seam: an
// HTTP server whose Listener accepts Unix connections calls this from
// http.Server.ConnContext, once per accepted connection - see PeerCred's doc
// comment for the full plumbing this is designed to support. The listener
// wrapper that calls it lives in the server module (a later task); core
// provides only this injection point and the matching reader
// (peerCredFromContext) so both sides agree on the seam without core
// depending on the server's listener machinery.
func ContextWithPeerCred(ctx context.Context, cred Ucred) context.Context {
	return context.WithValue(ctx, peerCredContextKey{}, cred)
}

// peerCredFromContext is the read half of the ConnContext seam:
// PeerCred.Authenticate looks up what ContextWithPeerCred stashed into the
// request's context. ok is false whenever nothing was ever stashed there - a
// non-unix listener, or any server that has not wired up ConnContext with
// ContextWithPeerCred - never because a zero-value Ucred (uid/gid/pid all 0)
// was legitimately stored: the two cases must never be confused, since the
// former must always deny and the latter is a real, if unusual, peer
// identity (root talking to root over a socket).
func peerCredFromContext(ctx context.Context) (Ucred, bool) {
	cred, ok := ctx.Value(peerCredContextKey{}).(Ucred)
	return cred, ok
}

// PeerCredOptions configures PeerCred. UIDs and GIDs are both optional
// allowlists: a peer is permitted if its uid is in UIDs, OR its gid is in
// GIDs (either suffices; they are not ANDed). If both are empty, any peer
// whose credentials were successfully read is permitted - the same
// "verification itself is the security boundary" default MTLSOptions
// documents for an empty certificate allowlist: needing no further name
// check is not the same as needing no verification at all, and Authenticate
// still denies outright whenever there are no kernel-verified credentials to
// check in the first place (a non-unix connection, or a unix one whose
// creds were never plumbed into the request context - see PeerCred's doc
// comment).
type PeerCredOptions struct {
	UIDs []int
	GIDs []int
}

// PeerCred authenticates a Unix-domain-socket client by its kernel-verified
// peer uid/gid. Because the identity comes from the kernel at accept time
// rather than anything the client presents in the request, it cannot be
// spoofed by a client that can merely connect to the socket - the strongest
// authenticator in this package for a sidecar deployment where the config
// server listens only on a Unix socket shared with trusted local processes.
//
// Support is platform-specific: SO_PEERCRED on Linux (authpeercred_linux.go),
// LOCAL_PEERCRED via GetsockoptXucred on Darwin (authpeercred_darwin.go), and
// an unconditional deny with a clear error on every other platform
// (authpeercred_other.go) - never a silent allow just because credentials
// could not be checked there.
//
// # The ConnContext seam
//
// By the time Authenticate runs, only the *http.Request and its context are
// in scope; net/http gives no direct path from a request back to the
// net.Conn it arrived on. The seam that bridges the two is split across core
// and the server module so both sides agree on it without core depending on
// the server's listener machinery:
//
//   - Core (this package) exports Ucred, ContextWithPeerCred, and, per
//     supported platform, PeerCredFromConn: a way to read a connection's
//     peer credentials and a way to stash them in a context.
//   - The server module's Unix-socket listener wrapper (a later task) calls
//     PeerCredFromConn once per accepted connection and passes the result to
//     ContextWithPeerCred from http.Server.ConnContext, so every request
//     arriving on that connection derives a context carrying the same peer
//     identity the kernel reported at accept time.
//   - PeerCred.Authenticate reads it back with the unexported
//     peerCredFromContext.
//
// A request whose context carries no such value - a non-unix listener, TLS,
// or simply a server that never wired up ConnContext - is denied outright,
// never treated as "no restriction configured".
func PeerCred(opts PeerCredOptions) Authenticator {
	return peerCred{opts: opts}
}

type peerCred struct {
	opts PeerCredOptions
}

// authenticate is the platform-independent core of PeerCred.Authenticate:
// look up the peer credentials the ConnContext seam stashed into r's
// context, deny outright if there are none, then apply the uid/gid
// allowlist. Linux's and Darwin's Authenticate (authpeercred_linux.go,
// authpeercred_darwin.go) both delegate to this directly; authpeercred_other.go
// does not, since on an unsupported platform there is nothing this could
// ever legitimately find in the context anyway, and the point of that
// platform's Authenticate is to say so with a clearer error than "no peer
// credentials", not to reach the same conclusion by a longer path.
func (p peerCred) authenticate(r *http.Request) (Identity, error) {
	cred, ok := peerCredFromContext(r.Context())
	if !ok {
		return Identity{}, errors.New("mamori: no peer credentials on request (non-unix connection, or ConnContext not wired to ContextWithPeerCred)")
	}
	if len(p.opts.UIDs) > 0 || len(p.opts.GIDs) > 0 {
		allowed := false
		for _, uid := range p.opts.UIDs {
			if uid == cred.UID {
				allowed = true
				break
			}
		}
		if !allowed {
			for _, gid := range p.opts.GIDs {
				if gid == cred.GID {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			return Identity{}, fmt.Errorf("mamori: peer uid %d gid %d not permitted", cred.UID, cred.GID)
		}
	}
	return Identity{
		Subject: "uid:" + strconv.Itoa(cred.UID),
		Attrs: map[string][]string{
			"uid": {strconv.Itoa(cred.UID)},
			"gid": {strconv.Itoa(cred.GID)},
			"pid": {strconv.Itoa(cred.PID)},
		},
	}, nil
}
