// This file is the transport layer: it turns the Unix and TCP listener
// specs a caller declares with Unix(...) and TCP(...) into actually-bound
// listeners, wires the v1 wire protocol Handler (handler.go) up to serve on
// every one of them, and owns the runtime state Serve and Close need to do
// that (bind, run, and tear down again) without leaking a goroutine, a
// listening socket, or a stale Unix socket file behind.
//
// # Unix vs TCP
//
// A Unix listener is, by construction, reachable only from the same host,
// further narrowed by the filesystem permissions Unix's mode argument sets
// and, once a caller wires up mamori.PeerCred as its Authenticator, by the
// kernel-verified uid/gid of whatever process is on the other end of the
// socket - see newHTTPServer's capturePeerCred parameter below for how that
// identity gets from the accepted net.Conn into the request context
// mamori.PeerCred reads. A TCP listener has no such boundary: anything that
// can route to the address can connect, which is exactly why New already
// refuses to combine NoAuth() with a TCP listener (see errNoAuthOnTCP in
// server.go) and why TCP(...) itself refuses to construct at all without
// either a TLS(...) config or an explicit InsecureNoTLS() (see
// errTCPRequiresTLS below) - a plaintext, unauthenticated-in-transit TCP
// listener would hand every backend credential this server concentrates to
// anyone who can reach the port.
//
// # Serve and Close
//
// Serve (below) is the one place all of this comes together: it starts
// upstream watching (start, resolve.go), binds every configured listener,
// and runs them all under the same v1 Handler (handler.go) until either one
// of them fails outright or Close stops everything gracefully. Close's own
// definition lives in resolve.go (see its doc comment there for why), but it
// calls closeTransports (this file) as part of the SAME closed-flag-guarded
// teardown that already stops the resolver goroutines, so a caller gets one
// idempotent Close that tears down both halves together, in any order of
// arrival relative to Serve - including a Close that arrives before Serve
// ever runs, which Serve then refuses to act on at all (errClosed, binding
// and spawning nothing) rather than serving behind a Close that could never
// reach it again.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/xavidop/mamori"
)

// Timeouts every listener's http.Server is built with, mirroring the core
// admin endpoint's own budget (adminReadHeaderTimeout etc. in adminhttp.go)
// for the same reason: the v1 wire protocol's routes (handler.go) are cheap,
// bounded reads (GET /v1/watch is the one long-lived exception, and it is
// exempted from ReadTimeout/IdleTimeout by virtue of being a response the
// client keeps reading, not a request these fields bound), so nothing here
// has a legitimate reason to hold a connection open indefinitely - the zero
// value for each of these fields is "no limit", exactly the opening a
// slow-loris-style client would want.
const (
	transportReadHeaderTimeout = 10 * time.Second
	transportReadTimeout       = 15 * time.Second
	transportIdleTimeout       = 60 * time.Second
)

// shutdownGrace bounds how long Close waits, in total, for every listener's
// in-flight requests to finish during its graceful http.Server.Shutdown -
// mirroring adminShutdownGrace in adminhttp.go. It is one shared deadline
// across every listener (see closeTransports below), not one grace period
// per listener, so Close's total blocking time stays bounded regardless of
// how many Unix(...)/TCP(...) listeners a Server was configured with.
const shutdownGrace = 5 * time.Second

// errTCPRequiresTLS is New's answer when a TCP(...) Option was given with
// neither TLS(cfg) nor InsecureNoTLS(). A config server concentrates every
// backend credential its bindings touch; serving that over plaintext TCP,
// where anything that can route to the address can read the wire in transit,
// defeats the point unless an operator has deliberately, visibly opted in -
// which is exactly what InsecureNoTLS's uncomfortable, greppable name is for.
var errTCPRequiresTLS = errors.New("mamori/server: TCP(...) without TLS(...) is refused; call InsecureNoTLS() to explicitly opt in to plaintext TCP (uncomfortable and greppable, on purpose)")

// unixSpec is one Unix(...) call's configuration, applied by Serve when it
// binds that socket.
type unixSpec struct {
	path string
	mode os.FileMode
}

// tcpSpec is one TCP(...) call's configuration: the address to bind, and
// either a TLS config (from the TLS TCPOption) or an explicit opt-out
// (InsecureNoTLS) - New's validation (errTCPRequiresTLS) requires at least
// one of the two.
type tcpSpec struct {
	addr          string
	tlsConfig     *tls.Config
	insecureNoTLS bool
}

// TCPOption configures a single TCP(...) listener.
type TCPOption func(*tcpSpec)

// TLS installs cfg as a TCP(...) listener's TLS configuration: the listener
// is wrapped with tls.NewListener(ln, cfg) at bind time (see bindTCP), so
// every connection is a TLS handshake before any HTTP request is read.
func TLS(cfg *tls.Config) TCPOption {
	return func(s *tcpSpec) { s.tlsConfig = cfg }
}

// InsecureNoTLS opts a TCP(...) listener out of the mandatory-TLS default.
// The name is deliberately uncomfortable and greppable: `grep -r
// InsecureNoTLS` finds every place an operator chose to serve this server's
// bindings over plaintext TCP, which should never be an accident nor a
// silent default. Prefer TLS(cfg) in every deployment that is not fully
// isolated at the network layer already (e.g. a private, trusted overlay
// network where TLS termination happens elsewhere).
func InsecureNoTLS() TCPOption {
	return func(s *tcpSpec) { s.insecureNoTLS = true }
}

// Unix declares a Unix-domain-socket listener at path, created with
// permission bits mode - 0600 (owner read/write only) is recommended for
// most deployments, since anyone who can connect to this socket can request
// any binding this Server's Policy allows them. Multiple Unix(...) options
// may be given, to listen on more than one path under the same
// policy/handler.
//
// Serve removes ("unlinks") any file already at path before binding (see
// bindUnix) - a config server that crashed without reaching Close must not
// fail to restart just because its old socket file is still there - and
// Close removes it again once the listener is shut down, so neither this run
// nor the next one ever trips over a stale file either way.
//
// Every connection accepted on this listener has its kernel-verified peer
// credentials (uid/gid/pid) captured and stashed into that connection's
// request context via http.Server.ConnContext (see newHTTPServer's
// capturePeerCred parameter), so a mamori.PeerCred Authenticator (see
// mamori's authpeercred.go) can authenticate callers by peer identity rather
// than anything they present in the request itself.
func Unix(path string, mode os.FileMode) Option {
	return func(s *Server) {
		s.unixSpecs = append(s.unixSpecs, unixSpec{path: path, mode: mode})
	}
}

// TCP declares a TLS TCP listener at addr (":0", or "host:0", to let the OS
// choose a free port - see Addrs for how to discover which one it picked).
// TLS(cfg) supplies the certificate to serve; without it, construction fails
// with errTCPRequiresTLS UNLESS InsecureNoTLS() is also given. Multiple
// TCP(...) options may be given, to listen on more than one address.
//
// TCP(...) also marks the Server as having a TCP listener for New's
// NoAuth-refused-on-TCP gate (see errNoAuthOnTCP and hasTCP in server.go):
// NoAuth() combined with any TCP(...) call fails New outright, well before
// Serve could ever bind a plaintext-reachable, unauthenticated port.
func TCP(addr string, opts ...TCPOption) Option {
	return func(s *Server) {
		spec := tcpSpec{addr: addr}
		for _, opt := range opts {
			opt(&spec)
		}
		s.tcpSpecs = append(s.tcpSpecs, spec)
		s.hasTCP = true
	}
}

// listenerEntry is one listener Serve bound, plus the http.Server serving it
// and (for a Unix listener) the socket path Close must unlink afterward.
// Serve appends one of these per configured Unix(...)/TCP(...) spec, in that
// order (every Unix(...) listener before every TCP(...) listener), and
// Close/Addrs both read s.entries back in that same order.
type listenerEntry struct {
	ln       net.Listener
	srv      *http.Server
	unixPath string // "" for a TCP listener

	// unixFile identifies the socket file bindUnix created at unixPath, so
	// shutdown can verify the file still at that path is the same one before
	// unlinking it (see closeTransports). Nil for a TCP listener, and also
	// nil when the post-bind stat failed, which is read as "cannot prove
	// ownership" and leaves the file alone.
	unixFile os.FileInfo
}

// newHTTPServer builds the http.Server that serves handler on one listener,
// with the shared timeout budget above. When capturePeerCred is true (the
// Unix listener only - see Unix's doc comment), ConnContext reads each
// accepted connection's kernel-verified peer credentials via
// mamori.PeerCredFromConn and stashes a successful read into that
// connection's request context with mamori.ContextWithPeerCred, completing
// the seam PeerCred's own doc comment (mamori's authpeercred.go) describes.
//
// A failed read - including "unsupported on this platform" on anything
// other than Linux/Darwin, see authpeercred_other.go's PeerCredFromConn -
// simply does not stash anything: mamori.PeerCred.Authenticate then denies
// the request exactly as it would for any connection with no peer
// credentials in its context (peerCredFromContext's ok=false case), never a
// silent allow just because the read could not be completed.
//
// BaseContext is set to always return s.servingCtx - start's (resolve.go)
// serving context, by now already created (Serve calls start before it ever
// reaches this method - see start's doc comment) - rather than left unset.
// Left unset, net/http's default makes every request's r.Context() derive
// from context.Background() instead, canceled only when the connection it
// arrived on closes; a long-lived handler like handleWatch (handler.go),
// which loops on <-r.Context().Done(), would then only ever unblock when
// the CLIENT disconnects, never when this Server itself shuts down, which
// is exactly what used to make closeTransports' Shutdown calls (below)
// block for the full shutdownGrace against any still-open streaming
// connection - see servingCtx's doc comment in server.go and teardown's in
// resolve.go for the other half of this: cancelling servingCancel BEFORE
// Shutdown runs is what actually unblocks handlers built on this
// BaseContext.
func (s *Server) newHTTPServer(handler http.Handler, capturePeerCred bool) *http.Server {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: transportReadHeaderTimeout,
		ReadTimeout:       transportReadTimeout,
		IdleTimeout:       transportIdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return s.servingCtx },
	}
	if capturePeerCred {
		srv.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
			cred, err := mamori.PeerCredFromConn(c)
			if err != nil {
				return ctx
			}
			return mamori.ContextWithPeerCred(ctx, cred)
		}
	}
	return srv
}

// bindUnix removes any stale file at spec.path, listens on it, and sets its
// permission bits to spec.mode. net.Listen("unix", ...) creates the socket
// file honoring the process umask, not spec.mode directly, so an explicit
// os.Chmod after Listen is required to actually get the requested bits; a
// Chmod failure closes the listener it just opened rather than leaving an
// unreachable-by-design socket bound with the wrong permissions.
// It also returns an os.FileInfo identifying the socket file it created, so
// shutdown can tell that file apart from a DIFFERENT socket that later came
// to occupy the same path. That matters because binding here starts by
// removing whatever is already at the path: during a restart on a fixed
// socket path, the incoming process removes and replaces the outgoing
// process's socket while the outgoing one is still draining. Without an
// identity to compare against, the outgoing process's own cleanup would then
// delete the incoming process's socket and leave it bound to a path no
// client can reach. See closeTransports.
func bindUnix(spec unixSpec) (net.Listener, os.FileInfo, error) {
	if err := os.Remove(spec.path); err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("mamori/server: unix: remove stale socket %s: %w", spec.path, err)
	}
	ln, err := net.Listen("unix", spec.path)
	if err != nil {
		return nil, nil, fmt.Errorf("mamori/server: unix: listen on %s: %w", spec.path, err)
	}
	if err := os.Chmod(spec.path, spec.mode); err != nil {
		_ = ln.Close()
		return nil, nil, fmt.Errorf("mamori/server: unix: chmod %s: %w", spec.path, err)
	}

	// Take ownership of the unlink. net.UnixListener removes the socket file
	// on Close by default, with no check that the file still belongs to it,
	// which is the very deletion described above. Turning that off and doing
	// the removal in closeTransports lets it be guarded by the identity
	// captured here.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}

	// A stat failure is not fatal: it only costs the guard its reference
	// point, and closeTransports treats a nil FileInfo as "cannot prove this
	// socket is still mine", which errs toward leaving the file in place.
	fi, err := os.Stat(spec.path)
	if err != nil {
		return ln, nil, nil
	}
	return ln, fi, nil
}

// bindTCP listens on spec.addr, wrapping the listener with tls.NewListener
// when spec.tlsConfig is set. New already refuses to construct a Server
// whose tcpSpec has neither a TLS config nor InsecureNoTLS (errTCPRequiresTLS),
// so by the time Serve calls this, an unset tlsConfig can only mean
// InsecureNoTLS was explicitly chosen.
func bindTCP(spec tcpSpec) (net.Listener, error) {
	ln, err := net.Listen("tcp", spec.addr)
	if err != nil {
		return nil, fmt.Errorf("mamori/server: tcp: listen on %s: %w", spec.addr, err)
	}
	if spec.tlsConfig != nil {
		ln = tls.NewListener(ln, spec.tlsConfig)
	}
	return ln, nil
}

// addEntry records e under s.entriesMu, so Addrs (readable at any time,
// including concurrently with Serve still binding later listeners) never
// observes a torn append.
func (s *Server) addEntry(e *listenerEntry) {
	s.entriesMu.Lock()
	s.entries = append(s.entries, e)
	s.entriesMu.Unlock()
}

// snapshotEntries returns a copy of s.entries, safe to range over without
// holding s.entriesMu for the duration (Serve and closeTransports both do
// this before their respective loops, which each take a while and must not
// serialize behind Addrs callers).
func (s *Server) snapshotEntries() []*listenerEntry {
	s.entriesMu.Lock()
	defer s.entriesMu.Unlock()
	out := make([]*listenerEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

// Addrs returns the bound address of every listener Serve has bound so far,
// in the order Unix(...) and TCP(...) options were given to New (every Unix
// listener before every TCP listener - see listenerEntry's doc comment).
// Before Serve has bound anything it returns nil (not yet meaningful to
// call); it is most useful against a TCP(...) address ending in ":0" (bind
// to an OS-chosen free port), called from a second goroutine once Serve is
// known to be running, to discover which port the kernel actually chose.
func (s *Server) Addrs() []net.Addr {
	entries := s.snapshotEntries()
	addrs := make([]net.Addr, 0, len(entries))
	for _, e := range entries {
		addrs = append(addrs, e.ln.Addr())
	}
	return addrs
}

// Serve starts this Server: it begins upstream watching (start, resolve.go,
// so every binding has at least a pending-resolve snapshot before any
// listener can accept a connection - see start's own doc comment), binds
// every configured Unix(...)/TCP(...) listener (fail-fast: a bind failure
// returns immediately, mirroring core's own WithAdminHTTP in adminhttp.go),
// and then runs them all under the same v1 Handler (handler.go) until either
// one of them fails outright or the whole Server is shut down.
//
// Serve blocks. It returns nil once every listener has stopped cleanly (via
// Close, called explicitly by another goroutine, or because ctx was
// canceled - Serve watches ctx.Done() itself and calls Close when it fires,
// so a caller driving shutdown from a canceled context does not also have to
// call Close by hand), or the first error any listener's Serve reports that
// is not the ordinary http.ErrServerClosed a graceful Shutdown produces -
// whichever happens first. A listener that bound successfully before a
// LATER one failed to is still recorded (see addEntry) and still gets torn
// down by a subsequent Close, even though Serve itself already returned; a
// caller is expected to `defer s.Close()` unconditionally after New,
// regardless of whether Serve ever returns nil (mirroring resolve.go's
// Close doc comment for start).
//
// Serve refuses to run at all if this Server is somehow both NoAuth and
// configured with a TCP listener: New already refuses to construct such a
// Server (errNoAuthOnTCP in server.go), so this is unreachable in practice,
// but Serve checks again anyway rather than ever silently serving a
// plaintext, unauthenticated port.
//
// Serve also refuses to run - binding and spawning nothing - if this Server
// has already been Close()d: start (resolve.go) is the first thing Serve
// calls, and start itself checks closed before creating anything, returning
// errClosed if Close got there first (see closed's doc comment in server.go
// for why this matters: without it, a Close that fires before Serve/start
// ever ran would permanently spend its one teardown against a Server with
// nothing to tear down, and this later Serve would then bind real listeners
// no subsequent Close could ever reach). A Close that instead arrives
// concurrently, WHILE Serve is still binding listeners below, is handled
// from the other end: once every configured listener has been bound, Serve
// checks isClosed one more time and, if a concurrent Close raced in and beat
// it, tears down everything it just built itself (teardown, resolve.go)
// before returning errClosed - see Close's own doc comment (resolve.go) for
// why that self-teardown, rather than relying on the racing Close, is what
// actually reclaims the listeners in that interleaving.
func (s *Server) Serve(ctx context.Context) error {
	if s.noAuth && s.hasTCP {
		return errNoAuthOnTCP
	}

	if err := s.start(ctx); err != nil {
		return err
	}

	handler := s.Handler()

	for _, spec := range s.unixSpecs {
		ln, fi, err := bindUnix(spec)
		if err != nil {
			return err
		}
		s.addEntry(&listenerEntry{ln: ln, srv: s.newHTTPServer(handler, true), unixPath: spec.path, unixFile: fi})
	}

	for _, spec := range s.tcpSpecs {
		ln, err := bindTCP(spec)
		if err != nil {
			return err
		}
		s.addEntry(&listenerEntry{ln: ln, srv: s.newHTTPServer(handler, false)})
	}

	// Close may have already raced in concurrently with the setup above -
	// Serve is meant to run in the background (`go s.Serve(ctx)`) with Close
	// driven independently, e.g. from a signal handler (see resolveMu's doc
	// comment in server.go) - and found nothing to tear down yet, exactly as
	// a Close-before-Serve would: Close only ever fires its teardown once,
	// so a Close that ran before every listener above was bound already
	// considers itself done and will never run again on its own. Detect
	// that here and tear down everything just built ourselves, via the same
	// teardown Close itself uses, rather than leaving it orphaned.
	if s.isClosed() {
		s.teardown()
		return errClosed
	}

	entries := s.snapshotEntries()

	// errCh is fully buffered (one slot per entry) so every listener
	// goroutine below can always send its result and return, even if Serve
	// itself already returned on an earlier entry's failure and nothing is
	// left reading errCh - otherwise a goroutine whose listener was closed
	// out from under it by a later Close call would block forever trying to
	// report a result nobody consumes, which goleak would catch as a leak.
	errCh := make(chan error, len(entries))
	for _, e := range entries {
		s.listenWG.Add(1)
		go func(e *listenerEntry) {
			defer s.listenWG.Done()
			err := e.srv.Serve(e.ln)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
				return
			}
			errCh <- nil
		}(e)
	}

	// A canceled ctx drives the same shutdown a caller's own explicit Close
	// would: this goroutine exits either way (ctx firing, or Serve's own
	// return closing stopWatch below), so it never outlives Serve.
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-stopWatch:
		}
	}()

	for range entries {
		if err := <-errCh; err != nil {
			return err
		}
	}
	return nil
}

// closeTransports shuts down every listener Serve bound, called from Close
// (resolve.go) inside the same closed-flag-guarded teardown that stops the
// resolver goroutines - see this file's package doc comment for why the two
// halves share one Close, and Close's own doc comment (resolve.go) for how
// this can also be invoked directly by Serve itself, when Serve's post-setup
// isClosed check finds a concurrent Close already ran.
//
// Every entry's http.Server is offered a graceful Shutdown first, all of
// them concurrently, sharing ONE bounded deadline (shutdownGrace) rather
// than one grace period per listener, so Close's total blocking time stays
// bounded no matter how many listeners this Server has. Shutdown waits for
// every in-flight handler to return on its own before it reports quiescent -
// it never force-closes an active connection - which is why teardown
// (resolve.go), this function's one caller, cancels s.servingCtx BEFORE
// calling this: every request's r.Context() derives from servingCtx
// (newHTTPServer's BaseContext), so by the time Shutdown starts polling,
// every long-lived handler still in flight - handleWatch's SSE loop
// (handler.go), most notably - has already seen its own <-ctx.Done() fire
// and returned, and Shutdown finds every connection quiescent almost
// immediately instead of blocking out to shutdownGrace on every one that
// happened to still be open. Shutdown closes only the listeners it actually
// accepted connections on, though: an entry Serve bound but never got to
// call srv.Serve(ln) for (the fail-fast bad-bind case - an earlier listener
// bound fine, a later one failed, and Serve returned before starting any
// goroutine for the ones already bound) needs its OWN net.Listener.Close()
// to actually release its port or socket, which is why every entry's raw
// listener is also closed directly afterward, unconditionally - redundant
// and harmless for an entry Shutdown already closed, load-bearing for one it
// never touched.
//
// It then waits on s.listenWG so every srv.Serve(ln) goroutine Serve started
// has actually returned before this function does - the transport half of
// the goleak-clean contract Close's own doc comment (resolve.go) promises,
// mirrored here the same way resolveWG covers the resolver goroutines.
//
// Finally, every Unix listener's socket file is removed, so neither this run
// nor a restart trips over a stale file left behind (see Unix's doc comment
// for the other half of this - the same removal also runs before Serve
// binds, in case a PREVIOUS run's process died without reaching Close at
// all).
func (s *Server) closeTransports() {
	entries := s.snapshotEntries()
	if len(entries) == 0 {
		return
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e *listenerEntry) {
			defer wg.Done()
			_ = e.srv.Shutdown(shutdownCtx)
		}(e)
	}
	wg.Wait()

	for _, e := range entries {
		_ = e.ln.Close()
	}

	s.listenWG.Wait()

	for _, e := range entries {
		if e.unixPath != "" {
			removeOwnedSocket(e.unixPath, e.unixFile)
		}
	}
}

// removeOwnedSocket unlinks path only if the file there is still the very one
// this listener created (identified by owned, captured in bindUnix).
//
// The guard exists because a socket path is a rendezvous point that another
// process can take over. Binding removes whatever is already at the path, so
// restarting a server on a fixed path means the incoming process replaces the
// outgoing process's socket file while the outgoing one is still draining,
// which can take up to shutdownGrace when a long-lived watch stream is open.
// An unguarded unlink here would then delete the INCOMING server's socket,
// leaving it listening on a path nothing can dial and every client
// permanently unable to reconnect. The failure is worse than a leaked file
// because it strands a healthy server.
//
// Every uncertain case leaves the file in place. A leftover socket is
// harmless (the next bind removes it), whereas deleting a live server's
// socket is not, so this is deliberately biased toward doing nothing.
func removeOwnedSocket(path string, owned os.FileInfo) {
	if owned == nil {
		return
	}
	current, err := os.Stat(path)
	if err != nil {
		// Already gone, or unreadable. Nothing to do either way.
		return
	}
	if !os.SameFile(current, owned) {
		// A newer listener owns this path now. Leave it alone.
		return
	}
	_ = os.Remove(path)
}
