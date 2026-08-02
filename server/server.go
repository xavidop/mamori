// Package server implements mamori's config server (spec 13): a process
// that holds the credentials for one or more backends, resolves a fixed,
// operator-declared table of name-to-ref bindings, and serves those values to
// many local consumers over Unix sockets and TLS TCP, under a mandatory
// authentication and authorization policy.
//
// This is deliberately the highest blast-radius component in mamori: whereas
// a single Load or Watch caller holds only the credentials it was given, a
// config server concentrates every backend credential its bindings touch
// into one process, reachable by every consumer it serves. Its safety is
// therefore structural rather than conventional: a client can never supply
// its own ref (only the operator's own Bind/BindFile calls create bindings),
// New refuses to construct a Server with no authorization Policy or with no
// authentication decision, and the exec: and mamori: schemes (remote command
// execution and, respectively, chaining to another config server) are
// rejected unless explicitly opted into.
//
// This package assembles the Server, its bindings, its Policy, the
// upstream-watching fan-out that keeps each binding's value current (see
// resolve.go: one mamori.WatchRef per binding, serving arbitrarily many
// concurrent reads), the v1 wire protocol handler (handler.go), and the
// transports that actually serve it (transport.go: Unix sockets and TLS
// TCP). New itself only validates and returns a *Server; Serve
// (transport.go) is what begins upstream watching (calling the unexported
// start in resolve.go) and binds and runs the configured listeners, and
// Close (resolve.go, extended by transport.go's closeTransports) tears both
// down again.
package server

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xavidop/mamori"
)

// errNoPolicy is New's answer when no Policy was configured. There is no
// implicit default (not even a deny-all), because a silent default is either
// too permissive to be safe or too restrictive to be usable, and either way
// hides a decision the operator must make and see themselves make.
// AllowAll() exists for the fully-trusted case, but it must be written down.
var errNoPolicy = errors.New("mamori/server: no Policy configured; call WithPolicy (use AllowAll() explicitly if every caller should be trusted - there is no implicit default)")

// errNoAuth is New's answer when neither WithAuth nor NoAuth was called. As
// with the policy gate, authentication is mandatory unless the operator
// explicitly opts out; an unauthenticated config server is a plausible
// choice behind a trusted Unix socket, but it must never be the silent
// default for a caller who forgot to wire up an Authenticator.
var errNoAuth = errors.New("mamori/server: no Authenticator configured; call WithAuth, or call NoAuth() to explicitly run without authentication")

// errNoAuthOnTCP is New's answer when NoAuth() is combined with a TCP
// listener. A Unix socket is reachable only by processes on the same host
// (further narrowed by filesystem permissions and, once PeerCred lands,
// kernel-verified uid/gid), so skipping authentication there is a bounded,
// legible choice. TCP has no such boundary: anything that can route to the
// address can connect, so serving every backend credential to it with no
// authentication at all defeats the point of running a config server rather
// than just handing out cloud credentials directly.
var errNoAuthOnTCP = errors.New("mamori/server: NoAuth() is refused on a TCP listener; TCP has no host boundary, so anonymous access there exposes every configured binding to the network - use WithAuth, or serve only over a Unix socket")

// Server fronts a fixed table of operator-declared bindings (see Binding),
// authenticating and authorizing every request before resolving one.
//
// The zero-value-adjacent construction path is deliberately narrow: the only
// way to obtain a *Server is New, and New enforces the security gates
// (mandatory Policy, mandatory auth-or-NoAuth, NoAuth refused on TCP) before
// handing one back, so there is no code path that produces a Server able to
// skip them.
type Server struct {
	bindings map[string]Binding

	policy Policy
	auth   mamori.Authenticator
	noAuth bool

	audit *slog.Logger

	allowExec     bool
	allowChaining bool

	// hasTCP records whether the option list requested a TCP listener. It is
	// read by New's NoAuth+TCP gate below.
	//
	// The NoAuth+TCP validation is a construction-time policy decision (a
	// caller should learn "this combination is refused" from New returning an
	// error, not by waiting until Serve, or worse, after the plaintext,
	// unauthenticated port is already live). TCP (transport.go) sets this
	// field directly, alongside recording the real listener configuration on
	// Server. Unix(...) never sets it: a Unix listener never trips this check.
	hasTCP bool

	// rawBindings and bindFiles accumulate Bind and BindFile declarations, in
	// the order given to New, across the whole opts list. Resolving them
	// (parsing refs, gating exec:/mamori: schemes, rejecting duplicates) is
	// deferred to New, after every Option has applied - see resolveBindings
	// in bindings.go for why that ordering matters.
	rawBindings []rawBinding
	bindFiles   []string

	// providers maps a ref scheme to the Provider that resolves it, populated
	// by WithProvider. See resolve.go's doc comment for why this - an
	// explicit, operator-supplied map - is the ONLY provider resolution
	// mechanism this package has, unlike core's Load/Watch which also fall
	// back to the process-wide registry (mamori.Register) when a scheme has
	// no explicit provider: that fallback is implemented by an unexported
	// function private to package mamori, so a package outside mamori (this
	// one) has no way to reach it.
	providers map[string]mamori.Provider

	// resolved, resolveCancel, and resolveWG are start's runtime state (see
	// resolve.go): resolved holds one entry per binding, fed by exactly one
	// mamori.WatchRef goroutine each; resolveCancel stops every one of those
	// goroutines; resolveWG lets Close wait for them to actually exit before
	// returning, mirroring Watcher.wg in reconciler.go. They stay nil/zero
	// until start runs.
	//
	// servingCtx and servingCancel are the OTHER context start creates
	// alongside resolveCancel's - not the resolver watches' context, but the
	// one every accepted HTTP request's r.Context() is derived from (via
	// newHTTPServer's BaseContext in transport.go). Without this, an
	// *http.Server built with no BaseContext ties r.Context() to the
	// connection it arrived on, so a long-lived handler like handleWatch
	// (handler.go), which loops on <-r.Context().Done(), only ever unblocks
	// when the CLIENT disconnects - never when this Server shuts down.
	// closeTransports (transport.go) needs a way to unblock every such
	// handler ITSELF, at the start of shutdown, so that
	// http.Server.Shutdown's wait for in-flight handlers to return finishes
	// promptly instead of blocking for the full shutdownGrace on every
	// still-open streaming connection - servingCancel is that way. It is
	// created here, under the same resolveMu-guarded, closed-checked start
	// call as resolveCancel (see the resolveMu paragraph below for why that
	// synchronization matters), and canceled by teardown (resolve.go)
	// BEFORE closeTransports runs Shutdown, so every in-flight handler's
	// ctx.Done() has already fired by the time Shutdown starts waiting.
	//
	// closed records whether Close has already made its one, idempotent
	// teardown transition. It replaces what used to be a sync.Once
	// (closeOnce): a sync.Once has no way to distinguish "my one teardown
	// ran against a Server that had something to tear down" from "my one
	// teardown ran against a Server with NOTHING to tear down yet, because
	// Close fired before Serve/start ever got a chance to start anything" -
	// both consume the Once identically, so a Close called BEFORE Serve
	// would permanently spend it, and a LATER Serve on that same Server
	// would then bind real listeners and spawn real goroutines that no
	// subsequent Close could ever reach again - a deterministic leak, not a
	// theoretical one. closed fixes this from the other end instead: start
	// (resolve.go) and Serve (transport.go) both check closed, under the
	// same resolveMu, before creating anything, and refuse with errClosed if
	// it is already set - so a Close-before-Serve makes the later Serve a
	// no-op error rather than an unreachable teardown. See start's and
	// Close's doc comments in resolve.go, and Serve's in transport.go, for
	// the full lifecycle, including how Serve itself handles a Close that
	// races in concurrently, mid-setup.
	//
	// resolveMu guards resolveCancel, servingCtx, servingCancel, and closed
	// specifically (not resolved: see resolve.go's start doc comment for why
	// resolved needs no lock of its own). Serve (transport.go) runs start in
	// the SAME goroutine that then goes on to build every listener's
	// *http.Server (reading servingCtx to set BaseContext) and run every
	// listener, so that read is always safely ordered after start's write of
	// it by plain program order plus the happens-before edge Go's memory
	// model gives a goroutine's creation - exactly like resolved's own
	// no-lock-needed read, mirrored here for the same reason. But Close can
	// be called from a completely different, unsynchronized goroutine (Serve
	// is meant to be run in the background - `go s.Serve(ctx)` - with Close
	// driven independently, e.g. from a signal handler, possibly before
	// Serve's internal start call has even happened yet), so resolveCancel's,
	// servingCtx's, servingCancel's, and closed's reads and writes across
	// Close and start need their own explicit synchronization; a plain field
	// would otherwise be a genuine data race between the two, not merely a
	// theoretical one - see closeTransports and Close's own doc comment.
	//
	// resolveMu carries one more job beyond guarding those four fields: it is
	// the happens-before edge between every Add to either of this Server's
	// WaitGroup fields and the matching Wait in teardown. (closeTransports
	// has a third WaitGroup, but it is function-local, so even two concurrent
	// invocations get separate instances and it needs no such edge.) Both
	// start (for resolveWG) and
	// beginListening (for listenWG) Add while holding it, having first seen
	// closed as false, and teardown takes it before either Wait at a point
	// where closed is already permanently true - so an Add can never be
	// concurrent with a Wait, which for a WaitGroup is a data race rather
	// than a benign overlap. See beginListening's doc comment in resolve.go.
	resolved      map[string]*resolverState
	resolveCancel context.CancelFunc
	servingCtx    context.Context
	servingCancel context.CancelFunc
	closed        bool
	resolveMu     sync.Mutex
	resolveWG     sync.WaitGroup

	// closeDone is created by the single Close call that wins the closed
	// transition and closed once that call's teardown has finished. Later
	// Close calls block on it, so every caller is told the server is closed
	// only when it actually is. Guarded by resolveMu, like closed itself.
	closeDone chan struct{}

	// unixSpecs and tcpSpecs accumulate Unix and TCP declarations (see
	// transport.go), in the order given to New, the same way rawBindings and
	// bindFiles accumulate Bind/BindFile above. Serve (transport.go) binds
	// every unixSpecs entry before every tcpSpecs entry.
	unixSpecs []unixSpec
	tcpSpecs  []tcpSpec

	// entries, entriesMu, and listenWG are Serve's runtime state
	// (transport.go): entries holds one listenerEntry per bound listener,
	// guarded by entriesMu since Addrs may read it concurrently with Serve
	// still binding later listeners; listenWG lets Close wait for every
	// listener's Serve goroutine to actually exit, mirroring resolveWG above
	// for the transport half of the lifecycle. They stay nil/zero until
	// Serve runs.
	//
	// listenWG is added to under resolveMu, not entriesMu, even though it is
	// otherwise transport state. That is not an oversight in the split: its
	// Add has to be ordered against the listenWG.Wait that Close's teardown
	// reaches through closeTransports (a positive Add made when a WaitGroup's
	// counter is zero must happen before a Wait, or it is a data race and, on
	// an unlucky interleaving without the race detector, a WaitGroup misuse
	// panic), and the only thing that can supply that ordering is the closed
	// flag below, which resolveMu guards. So Serve makes its Adds inside the
	// same resolveMu critical section as its final closed check - see
	// beginListening in resolve.go - exactly as start makes resolveWG's Adds
	// inside its own. entriesMu could not do this job: teardown's snapshot of
	// entries deliberately does not hold it across the Wait.
	entries   []*listenerEntry
	entriesMu sync.Mutex
	listenWG  sync.WaitGroup

	// nodeID names this process in audit records, so a deployment running
	// several replicas can attribute a logged request to the replica that
	// served it. It defaults to the hostname (see New) and is never put on
	// the wire: it appears in this server's own logs only.
	nodeID string

	// draining is flipped by Close before any listener stops accepting, and
	// makes GET /v1/readyz answer 503 (see Server.readiness). It is an atomic
	// rather than a plain bool guarded by resolveMu because every readyz
	// request reads it, and those run concurrently with the Close that writes
	// it, by construction.
	draining atomic.Bool

	// drainGrace is how long Close waits after flipping draining before it
	// begins shutting listeners down, giving a load balancer time to observe
	// the failing readiness probe and stop sending new requests. Zero (the
	// default) skips the wait entirely, preserving the immediate-shutdown
	// behaviour of every caller that has not opted in. See DrainGrace.
	drainGrace time.Duration
}

// Option configures New.
type Option func(*Server)

// Bind declares a binding: consumers may request name and, when authorized,
// receive whatever ref currently resolves to. ref is parsed with
// [mamori.ParseRef] and validated (including the exec:/mamori: scheme gates)
// when New is called, not here, so that gating does not depend on where in
// the opts list AllowExec/AllowChaining happen to appear.
//
// Bind is the ONLY way a binding comes into existence: the wire protocol
// (handler.go) takes a binding name from the client, never a ref, so a
// client can never cause the server to resolve a ref of its own choosing
// (e.g. file:///etc/shadow, or an exec: command it supplies).
func Bind(name, ref string) Option {
	return func(s *Server) {
		s.rawBindings = append(s.rawBindings, rawBinding{name: name, ref: ref})
	}
}

// BindFile declares bindings from a YAML file shaped:
//
//	bindings:
//	  db-password: vault://secret/data/db#password
//	  api-key: aws-sm://prod/api-key
//
// The file is read and parsed by New, at the same point Bind's declarations
// are resolved (see resolveBindings), so a BindFile error and a Bind error
// are reported identically and neither can bypass the exec:/mamori: gates or
// the duplicate-name check.
func BindFile(path string) Option {
	return func(s *Server) {
		s.bindFiles = append(s.bindFiles, path)
	}
}

// WithAuth installs the Authenticator every request must pass before any
// binding is read. See core's mamori.Authenticator; the config server reuses
// it unchanged so an Authenticator written for the admin HTTP endpoint works
// here too.
func WithAuth(a mamori.Authenticator) Option {
	return func(s *Server) { s.auth = a }
}

// NoAuth explicitly disables authentication. It exists for a Unix-socket-only
// deployment where the filesystem (and, once wired up, kernel-verified peer
// credentials) already bound who can connect. New refuses to combine it with
// a TCP listener (see errNoAuthOnTCP).
func NoAuth() Option {
	return func(s *Server) { s.noAuth = true }
}

// WithPolicy installs the mandatory authorization Policy. See policy.go.
func WithPolicy(p Policy) Option {
	return func(s *Server) { s.policy = p }
}

// WithProvider registers p as the Provider for its own Scheme() (see
// mamori.Provider.Scheme), for every binding whose ref uses that scheme.
// Calling WithProvider twice for the same scheme keeps the later one: like
// Bind/BindFile, this only takes effect once New has applied every Option, so
// (as with AllowExec/AllowChaining) it does not matter where in the opts list
// a WithProvider call appears relative to the Bind it is meant to resolve.
//
// This mirrors mamori.WithProvider's per-call explicit-provider map exactly
// (see reconcile.go's options.providers), which is deliberate: a config
// server's bindings must resolve through the SAME provider a consumer's own
// Load or Watch would have used for that scheme, not a second, potentially
// different provider instance wired up independently. Unlike
// mamori.WithProvider, though, there is no registry fallback for a scheme
// with no WithProvider call - see resolve.go's package doc for why the
// process-wide registry (mamori.Register) is not reachable from here, and
// New's exec:/mamori: gates are a construction-time check, not a promise
// that a provider exists for every OTHER scheme too: a binding whose scheme
// has no registered provider still constructs cleanly and simply reports a
// resolution error once start runs (see resolve.go), the same way a required
// field with no matching provider fails at Load rather than at New/Watch
// construction.
func WithProvider(p mamori.Provider) Option {
	return func(s *Server) {
		if s.providers == nil {
			s.providers = map[string]mamori.Provider{}
		}
		s.providers[p.Scheme()] = p
	}
}

// WithAudit installs a structured logger that records identity, binding name,
// allow/deny outcome, error kind, and latency for every request - but never
// a resolved value. Audit logging is off by default (a nil logger), since it
// is diagnostic rather than a security control: the enforcement is Policy and
// Authenticator, not the log.
func WithAudit(l *slog.Logger) Option {
	return func(s *Server) { s.audit = l }
}

// AllowExec opts a Server in to serving exec: bindings. Without it, Bind or
// BindFile declaring an exec: ref makes New fail: an exec: ref runs an
// arbitrary command on the server's host, so allowing it to be bound (and
// therefore reachable by every authorized consumer) is a deliberate,
// greppable choice, not a default.
//
// AllowExec only lifts the construction-time gate; it does not by itself
// make an exec: binding resolve. Core's exec: provider has no exported
// mamori.Provider constructor, so enabling AllowExec also requires the
// operator to supply their own exec provider via WithProvider (remote
// command execution reachable through a config server is dangerous, so this
// extra friction - AllowExec plus a provider the operator wrote or vetted
// themselves - is intentional, not an oversight).
func AllowExec() Option {
	return func(s *Server) { s.allowExec = true }
}

// AllowChaining opts a Server in to serving mamori: bindings, which resolve
// through another mamori config server. Without it, Bind or BindFile
// declaring a mamori: ref makes New fail: chaining config servers can form a
// cycle (a server that, directly or transitively, points back at itself),
// so allowing it is a deliberate choice the operator must make, not a
// default that could quietly wire up an infinite loop.
func AllowChaining() Option {
	return func(s *Server) { s.allowChaining = true }
}

// NodeID names this process in its own audit records, so that when several
// replicas serve the same bindings behind one address, an aggregated audit
// stream still says which replica served a given request. Without it, "which
// replica returned that stale value" is unanswerable after the fact.
//
// It defaults to the OS hostname, which is already the right answer under
// most schedulers (a Kubernetes pod's hostname is its pod name). Set it
// explicitly when the hostname is not meaningful, for example several
// replicas sharing a host.
//
// The node ID is never written to a response. It appears only in this
// server's own logs, so it cannot be used by a client to discover the shape
// of the deployment.
func NodeID(id string) Option {
	return func(s *Server) { s.nodeID = id }
}

// DrainGrace makes Close wait d after marking this replica not-ready (see
// GET /v1/readyz) before it begins shutting listeners down.
//
// This is the difference between a rolling restart that drops requests and
// one that does not. A load balancer only stops sending new requests once it
// has observed a failing readiness probe, which takes up to its own probe
// interval; without a grace window, Close stops accepting immediately and
// everything the balancer sends in that gap is refused. Set d to at least the
// balancer's probe interval times its unhealthy threshold.
//
// The default is zero: Close tears down immediately, exactly as it always
// has, so no existing caller changes behaviour by upgrading. Note the wait
// happens on the Close path, so a caller that cancels Serve's context instead
// of calling Close does not get it.
func DrainGrace(d time.Duration) Option {
	return func(s *Server) { s.drainGrace = d }
}

// withTCPListener marks that the option list requested a TCP listener, for
// New's NoAuth+TCP gate. It is unexported and exists only so this package's
// own tests can exercise that gate without a real listener; TCP
// (transport.go) sets the same Server.hasTCP field directly.
func withTCPListener() Option {
	return func(s *Server) { s.hasTCP = true }
}

// New constructs a Server from opts, validating the security-critical
// invariants before returning one: a Policy must be configured, an
// Authenticator must be configured unless NoAuth() was called, and NoAuth()
// must not be combined with a TCP listener. Bindings are parsed and gated
// (duplicate names rejected; exec:/mamori: schemes rejected unless their
// allow-opt is set) as part of this same call.
//
// New does not start serving anything: no listener is created, no upstream
// watch is started - that is Serve's job (transport.go), which calls the
// unexported start in resolve.go. It returns a *Server ready for Serve to be
// called on.
func New(opts ...Option) (*Server, error) {
	s := &Server{}
	for _, opt := range opts {
		opt(s)
	}

	bindings, err := resolveBindings(s.rawBindings, s.bindFiles, s.allowExec, s.allowChaining)
	if err != nil {
		return nil, err
	}
	s.bindings = bindings
	s.rawBindings = nil
	s.bindFiles = nil

	if s.policy == nil {
		return nil, errNoPolicy
	}
	if s.auth == nil && !s.noAuth {
		return nil, errNoAuth
	}
	if s.noAuth && s.hasTCP {
		return nil, errNoAuthOnTCP
	}
	for _, spec := range s.tcpSpecs {
		if spec.tlsConfig == nil && !spec.insecureNoTLS {
			return nil, errTCPRequiresTLS
		}
	}

	// Default the node ID to the hostname (see NodeID). A hostname lookup can
	// fail on an unconfigured host, which is not a reason to refuse to build
	// a Server: the node ID is a log annotation, so an empty one costs an
	// operator some clarity in an aggregated audit stream and nothing else.
	if s.nodeID == "" {
		if host, hostErr := os.Hostname(); hostErr == nil {
			s.nodeID = host
		}
	}

	return s, nil
}
