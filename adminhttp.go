package mamori

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// adminShutdownGrace bounds how long Close waits for the admin server's
// in-flight requests to finish during a graceful Shutdown. It is
// deliberately short: the routes served here (see handler.go) are cheap
// reads with no reason to run long, and Close must return in bounded time
// regardless of what is happening to the rest of the process.
const adminShutdownGrace = 5 * time.Second

// Timeouts for the admin http.Server. The docs contemplate this endpoint
// being exposed beyond localhost, and its routes (see handler.go) are cheap,
// bounded reads with no legitimate reason to hold a connection open: without
// these, a slow or hung client (deliberately or not) can tie up a connection
// indefinitely (a slow-loris style resource exhaustion), since the zero
// value for each of these fields on http.Server means no limit at all.
// ReadHeaderTimeout alone bounds the classic slow-loris (a client that trickles
// header bytes); ReadTimeout and IdleTimeout are added too, since this
// endpoint has no business with a long-lived request or a connection kept
// idle waiting for reuse.
const (
	adminReadHeaderTimeout = 10 * time.Second
	adminReadTimeout       = 15 * time.Second
	adminIdleTimeout       = 60 * time.Second
)

// WithAdminHTTP makes Watch run its own HTTP server on addr, serving the same
// routes Handler would (see handler.go): GET / and GET /healthz. It exists so
// a caller who does not already run a mux of their own does not have to wire
// one up just to expose health.
//
// It is off by default: with no WithAdminHTTP option, no listener is
// constructed, no port is bound, and no goroutine is started. When set, Watch
// binds the listener before it returns, so a bind failure (port in use,
// permission denied) fails Watch with the bind error rather than logging it
// and leaving the caller believing the endpoint is up; this mirrors Watch's
// existing fail-fast behavior on the initial Load.
//
// The server's lifetime is tied to the Watcher's: its goroutine is tracked by
// the same wait group as the reconciliation engine, and Watcher.Close shuts
// it down gracefully (bounded by a grace period) before returning, so Close
// returning means the port is free again.
//
// Load accepts this option too, since Load and Watch share the same Option
// type, but Load has no long-lived Watcher to run a server against and so it
// silently ignores WithAdminHTTP.
func WithAdminHTTP(addr string, opts ...HandlerOption) Option {
	return func(o *options) {
		o.adminAddr = addr
		o.adminOpts = opts
	}
}

// WithAdminTLS serves the WithAdminHTTP endpoint over TLS instead of
// plaintext, using cfg's certificates and, if set, its ClientCAs/ClientAuth
// for mutual TLS. It has no effect without WithAdminHTTP.
//
// The shipped Authenticator schemes (basic auth, bearer tokens) are only as
// safe as the transport they run over: a credential sent in the clear is not
// really a credential. WithAdminTLS exists so those schemes can be used
// safely on the self-hosted admin endpoint.
func WithAdminTLS(cfg *tls.Config) Option {
	return func(o *options) { o.adminTLS = cfg }
}

// startAdminHTTP binds the admin listener and starts serving in a goroutine
// registered with w.wg, matching how the engine's own goroutines register
// (see engine.start in reconciler.go) so goleak sees a clean shutdown after
// Close.
//
// It is called from Watch, after the Watcher is constructed but before
// Watch's background goroutines start, so a bind failure can be reported to
// the caller without anything else having been started yet.
func startAdminHTTP[T any](w *Watcher[T], o *options) error {
	ln, err := net.Listen("tcp", o.adminAddr)
	if err != nil {
		return fmt.Errorf("mamori: admin http: listen on %s: %w", o.adminAddr, err)
	}
	if o.adminTLS != nil {
		ln = tls.NewListener(ln, o.adminTLS)
	}

	srv := &http.Server{
		Handler:           Handler[T](w, o.adminOpts...),
		ReadHeaderTimeout: adminReadHeaderTimeout,
		ReadTimeout:       adminReadTimeout,
		IdleTimeout:       adminIdleTimeout,
	}
	w.adminServer = srv
	w.adminAddrVal = ln.Addr()

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			// http.ErrServerClosed is Serve's normal return after Shutdown; any
			// other error is unexpected but the server goroutine itself must
			// still exit cleanly, so it is reported to OnError (when
			// configured) rather than left silent.
			if o.onError != nil {
				o.onError(fmt.Errorf("mamori: admin http server: %w", serveErr))
			}
		}
	}()
	return nil
}

// shutdownAdminHTTP gracefully stops the admin server, if one was started,
// bounded by adminShutdownGrace so Close cannot block forever on a stuck
// connection. It is called from Watcher.Close before wg.Wait, so that by the
// time Close returns the port is released.
func shutdownAdminHTTP(srv *http.Server) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminShutdownGrace)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
