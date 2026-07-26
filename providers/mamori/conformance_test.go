// This file is the conformance harness for Task 5 (workstream I): it runs
// providertest.Run against a REAL in-process server.Server (workstream H)
// fronting a mamoritest upstream, over both a Unix socket and a TLS TCP
// listener. This is the strongest conformance the spec calls for: it
// validates this package's client, the v1 wire protocol (server/wire.go and
// server/handler.go), and the server together, rather than the client alone
// against a hand-rolled fake.
//
// # Key normalization
//
// providertest generates some keys dynamically (classify-<kind>-<uniq> and
// does-not-exist-<uniq>), but the server's binding table is fixed at
// server.New. Config.Key below normalizes every dynamic key onto one of a
// small, fixed set of bindings the server is constructed with once:
// classify-* always maps to the single "conformance-classify" slot (safe
// because ErrorClassification's five cases run sequentially, one
// Seed/Fail/Clear at a time - see RunErrorClassification in
// providertest.go), does-not-exist-* maps to "conformance-absent", a name
// deliberately left UNBOUND so the server answers 404 not_found, and every
// other case's key maps to "conformance-"+name.
//
// # Stale-serving vs hard error
//
// server/resolve.go's lookup serves a binding's last-known-good value (200,
// annotated with the current failure's kind) once it has EVER resolved
// successfully, and only reports a hard classified error for a binding that
// has NEVER resolved. Config.Seed exploits this deliberately: it is a no-op
// for the classify slot, leaving it permanently unresolved, so a later
// Config.Fail always produces a hard classified error rather than a stale
// 200 - see RunErrorClassification's need for a real error.
//
// # Synchronizing across the wire
//
// The server's GET /v1/watch poll loop (sseWatchPollInterval, 200ms - see
// server/handler.go) and the upstream fan-out (one mamori.WatchRef per
// binding - see server/resolve.go) both add latency between an
// up.Set/Fail/Clear call and that change becoming visible through THIS
// package's client. Every place below that mutates the mamoritest upstream
// therefore polls the client back through the real wire (pollUntil) until
// the change is actually observable, rather than assuming it already is:
// this is what makes providertest's assertions - which run immediately after
// Seed/Mutate/Fail/Clear return - deterministic.
package mamoriprov

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
	"github.com/xavidop/mamori/providertest"
	"github.com/xavidop/mamori/secret"
	"github.com/xavidop/mamori/server"
)

const (
	// pollTimeout bounds every wait in this file: how long Seed/Mutate/Fail/
	// Clear poll the client for a change to become visible, and the
	// EventuallyTimeout providertest.Run itself is given (so its own
	// watch-delivery waits share the same generous budget). It has to absorb
	// the server's 200ms SSE poll interval plus the upstream watch fan-out,
	// with real headroom for a slow CI machine.
	pollTimeout = 5 * time.Second
	// pollInterval is how often pollUntil re-checks its condition.
	pollInterval = 20 * time.Millisecond
)

// fixedConformanceNames is the complete, static binding table the server is
// constructed with: one entry per fixed key providertest.go generates
// (scheme, resolve, ctxcancel, concurrent, version, watch, watchclose, leak)
// plus "classify", the single reused slot every classify-* case normalizes
// onto. "absent" is deliberately NOT in this list - see Config.Key below.
var fixedConformanceNames = []string{
	"scheme", "resolve", "ctxcancel", "concurrent", "version",
	"watch", "watchclose", "leak", "classify",
}

// conformanceKey builds the normalized binding name for a fixed conformance
// name, e.g. "resolve" -> "conformance-resolve".
func conformanceKey(name string) string { return "conformance-" + name }

// upstreamName recovers the plain name (e.g. "resolve") the mamoritest
// upstream and the server's binding table use from a normalized key (e.g.
// "conformance-resolve") - the inverse of conformanceKey.
func upstreamName(key string) string { return strings.TrimPrefix(key, "conformance-") }

// refFor builds the mamori.Ref for a normalized binding name. Every key this
// file ever passes it comes from Config.Key below, which only ever produces
// "conformance-"-prefixed names, so ParseRef cannot fail in practice; the
// fallback exists only so a caller gets a deterministic ErrInvalid instead of
// a panic if that invariant is ever violated.
func refFor(key string) mamori.Ref {
	ref, err := mamori.ParseRef("mamori://" + key)
	if err != nil {
		return mamori.Ref{Raw: "mamori://" + key}
	}
	return ref
}

// pollUntil blocks until cond reports true, ctx is done, or pollTimeout
// elapses, whichever comes first. See this file's package doc comment
// ("Synchronizing across the wire") for why every mutation of the mamoritest
// upstream in this file goes through it.
func pollUntil(ctx context.Context, cond func() bool) error {
	deadline := time.Now().Add(pollTimeout)
	for {
		if cond() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mamoriprov conformance: condition not satisfied within %s", pollTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// shortTempDir returns a freshly created, short-named temp directory, for
// tests that bind a Unix domain socket somewhere inside it. Unlike
// t.TempDir(), whose path embeds the full test name (and, for a subtest, its
// parent's name too), this stays well under sockaddr_un's sun_path limit
// (104 bytes on Darwin, 108 on Linux) even for this file's longer test
// names: t.TempDir()'s own path for, say,
// TestErrorsIsReachesSentinelForEveryKind already runs past 100 bytes before
// a socket filename is even appended, which fails to bind with "invalid
// argument" - a real failure this file hit and fixed by switching to this
// helper.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mp")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// newConformanceServer builds a *server.Server bound with the fixed table
// (fixedConformanceNames, each bound to up://<name>), plus serverOpts (the
// transport and auth configuration, which differs between the Unix and TLS
// TCP variants).
func newConformanceServer(t *testing.T, up *mamoritest.Provider, serverOpts ...server.Option) *server.Server {
	t.Helper()
	opts := []server.Option{
		server.WithProvider(up),
		server.WithPolicy(server.AllowAll()),
	}
	for _, name := range fixedConformanceNames {
		opts = append(opts, server.Bind(conformanceKey(name), "up://"+name))
	}
	opts = append(opts, serverOpts...)

	srv, err := server.New(opts...)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return srv
}

// startConformanceServer runs srv.Serve in the background and blocks until it
// has bound at least one listener (so the caller's first request never races
// Serve's own startup), returning the CancelFunc that stops it. It fails the
// test outright if Serve exits before binding anything, or if binding takes
// longer than pollTimeout.
func startConformanceServer(t *testing.T, srv *server.Server) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	deadline := time.Now().Add(pollTimeout)
	for len(srv.Addrs()) == 0 {
		select {
		case err := <-serveErr:
			cancel()
			t.Fatalf("server.Serve exited before binding any listener: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server did not bind a listener within %s", pollTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cancel
}

// newConformanceClient builds a *Provider against endpoint the same way
// New(Config{Endpoint: endpoint, TLSConfig: tlsConfig}, clientOpts...) would,
// except with keep-alive connection reuse disabled on its transport. This
// matters only for runConformance (below), whose clients are what
// providertest's NoGoroutineLeak case (goleak.VerifyNone, with no ignore
// options - see closeServerForLeakTest) inspects: an ordinary transport
// leaves each request's connection sitting in its idle-connection pool
// indefinitely once the request completes (Transport.IdleConnTimeout
// defaults to "no limit"), which keeps that connection's readLoop/writeLoop
// goroutines - and the server's matching per-connection goroutine - alive
// long after the subtest that made the request has returned. Disabling
// keep-alives makes both ends close the connection as soon as its one
// request/response finishes, so nothing outlives the subtest that opened it.
// parseEndpoint is this package's own (unexported, same-package) endpoint
// parser - the exact one New itself uses - reused here rather than
// duplicated.
func newConformanceClient(endpoint string, tlsConfig *tls.Config, clientOpts ...Option) *Provider {
	_, transport, err := parseEndpoint(endpoint, false)
	if err != nil {
		panic(fmt.Sprintf("mamoriprov conformance: invalid endpoint %q: %v", endpoint, err))
	}
	if tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig
	}
	transport.DisableKeepAlives = true
	httpClient := &http.Client{Timeout: 30 * time.Second, Transport: transport}
	return New(Config{Endpoint: endpoint, HTTPClient: httpClient}, clientOpts...)
}

// closeServerForLeakTest is Config.Seed's special case for providertest's
// "leak" key (testNoLeak in providertest.go, run as the LAST case of every
// providertest.Run call). That case's own `defer goleak.VerifyNone(t)`
// inspects every goroutine in the CURRENT PROCESS, with no option to scope
// it to goroutines the provider under test actually started - it has no
// hook by which this harness could tell it "these N goroutines are this
// conformance run's own live server, not a leak". providers/doppler hit the
// identical hazard against a plain httptest.Server (see fakeDoppler's doc
// comment in providers/doppler/doppler_test.go) and worked around it by
// never opening a real listener for its conformance run at all; that option
// is not available here, since a real listening server.Server is the entire
// point of this file.
//
// The "leak" key is exclusively used by testNoLeak, and it is always the
// last case Run executes, so by the time Seed observes it, no other subtest
// still needs the server. Closing it here - before testNoLeak goes on to
// attempt its own Resolve/Watch (both of which discard whatever error a
// now-dead server produces) and then sleeps and checks goleak - removes
// every one of this harness's own goroutines (the listener's Accept loop,
// the per-binding upstream watch goroutines, mamoritest's own internal watch
// bookkeeping goroutines) from the process before the leak check runs, so
// that check actually measures what it is meant to: whether the CLIENT
// leaks, not whether this test's own scaffolding is still running.
func closeServerForLeakTest(cancel context.CancelFunc, srv *server.Server) error {
	cancel()
	return srv.Close()
}

// runConformance runs the full providertest suite against a real server.Server
// reachable at the endpoint endpointFor returns (called once the server has
// bound its listener, so a ":0" port can be discovered via srv.Addrs()).
// serverOpts supplies the transport and auth configuration (Unix+NoAuth or
// TLS TCP+BearerToken); tlsConfig and clientOpts configure the CLIENT side to
// match.
func runConformance(t *testing.T, serverOpts []server.Option, endpointFor func(*server.Server) string, tlsConfig *tls.Config, clientOpts ...Option) {
	t.Helper()

	up := mamoritest.NewProvider("up")
	srv := newConformanceServer(t, up, serverOpts...)
	cancel := startConformanceServer(t, srv)
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})

	endpoint := endpointFor(srv)
	newClient := func() *Provider {
		return newConformanceClient(endpoint, tlsConfig, clientOpts...)
	}
	// probe is used only by Seed/Mutate/Fail/Clear below to poll the server
	// back through the real wire; it is never handed to providertest itself
	// (which builds its own instances via Config.New).
	probe := newClient()

	leakKey := conformanceKey("leak")
	classifyKey := conformanceKey("classify")

	resolveMatches := func(ctx context.Context, key, want string) bool {
		v, err := probe.Resolve(ctx, refFor(key))
		return err == nil && string(v.Bytes) == want
	}

	cfg := providertest.Config{
		New: func() mamori.Provider { return newClient() },
		Ref: func(key string) string { return "mamori://" + key },
		Key: func(name string) string {
			switch {
			case strings.HasPrefix(name, "classify-"):
				return classifyKey
			case strings.HasPrefix(name, "does-not-exist-"):
				return conformanceKey("absent")
			default:
				return conformanceKey(name)
			}
		},
		Seed: func(ctx context.Context, key, val string) error {
			switch key {
			case leakKey:
				return closeServerForLeakTest(cancel, srv)
			case classifyKey:
				// Deliberately a no-op: leave the classify slot unresolved
				// so a later Fail produces a hard classified error instead
				// of a stale-but-serving 200 (see this file's package doc
				// comment, "Stale-serving vs hard error").
				return nil
			}
			up.Set(upstreamName(key), val)
			return pollUntil(ctx, func() bool { return resolveMatches(ctx, key, val) })
		},
		Mutate: func(ctx context.Context, key, val string) error {
			up.Set(upstreamName(key), val)
			return pollUntil(ctx, func() bool { return resolveMatches(ctx, key, val) })
		},
		Fail: func(ctx context.Context, key string, injected error) error {
			wantKind := mamori.ErrorKind(injected)
			up.Fail(upstreamName(key), injected)
			return pollUntil(ctx, func() bool {
				_, err := probe.Resolve(ctx, refFor(key))
				return err != nil && mamori.ErrorKind(err) == wantKind
			})
		},
		Clear: func(ctx context.Context, key string) error {
			up.Clear(upstreamName(key))
			return pollUntil(ctx, func() bool {
				_, err := probe.Resolve(ctx, refFor(key))
				if key == classifyKey {
					// The classify slot was never seeded (see Seed above),
					// so once the injected failure is cleared it goes back
					// to unresolved, which the server reports as not_found
					// (server/resolve.go's errPendingResolve), not success.
					return err != nil && errors.Is(err, mamori.ErrNotFound)
				}
				return err == nil
			})
		},
		EventuallyTimeout: pollTimeout,
	}

	providertest.Run(t, cfg)
}

// TestConformanceOverUnixSocket runs the full providertest suite against a
// real server.Server reachable over a Unix domain socket, unauthenticated
// (NoAuth - a Unix socket's own filesystem permissions are the boundary; see
// server.NoAuth's doc comment).
func TestConformanceOverUnixSocket(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "s.sock")

	runConformance(t,
		[]server.Option{server.NoAuth(), server.Unix(sockPath, 0600)},
		func(*server.Server) string { return "unix://" + sockPath },
		nil,
	)
}

// TestConformanceOverTLSTCP runs the full providertest suite against a real
// server.Server reachable over TLS TCP, authenticated with a bearer token:
// the server validates it with mamori.BearerToken, and the client presents
// it via WithHeader. The server's certificate is a self-signed, in-test
// leaf (generateSelfSignedCert) whose CA the client trusts via
// Config.TLSConfig.RootCAs, proving both the transport and the auth-header
// path in one run.
func TestConformanceOverTLSTCP(t *testing.T) {
	cert, pool := generateSelfSignedCert(t)
	const token = "conformance-tls-token"

	runConformance(t,
		[]server.Option{
			server.WithAuth(mamori.BearerToken(secret.NewString(token))),
			server.TCP("127.0.0.1:0", server.TLS(&tls.Config{Certificates: []tls.Certificate{cert}})),
		},
		func(srv *server.Server) string {
			addr := srv.Addrs()[0].(*net.TCPAddr)
			return fmt.Sprintf("https://127.0.0.1:%d", addr.Port)
		},
		&tls.Config{RootCAs: pool},
		WithHeader("Authorization", "Bearer "+token),
	)
}

// generateSelfSignedCert builds a self-signed, in-memory TLS certificate
// valid for 127.0.0.1, for the TLS TCP conformance run: the server presents
// it via server.TLS, and the returned pool (containing the same leaf as its
// own CA) is what the client trusts it with via Config.TLSConfig.RootCAs.
func generateSelfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating conformance TLS key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mamori-conformance"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("creating conformance TLS certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing conformance TLS certificate: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}, pool
}

// pollResolve polls p.Resolve(ref) until it succeeds or pollTimeout elapses,
// for the targeted tests below that need a value visible through the real
// wire before asserting on it, without relying on a fixed sleep.
func pollResolve(t *testing.T, p mamori.Provider, ref mamori.Ref) mamori.Value {
	t.Helper()
	var (
		v    mamori.Value
		last error
	)
	deadline := time.Now().Add(pollTimeout)
	for time.Now().Before(deadline) {
		var err error
		v, err = p.Resolve(context.Background(), ref)
		if err == nil {
			return v
		}
		last = err
		time.Sleep(pollInterval)
	}
	t.Fatalf("Resolve(%q) did not succeed within %s: %v", ref.Raw, pollTimeout, last)
	return mamori.Value{}
}

// sensitiveProvider is a minimal, test-only mamori.Provider that always
// resolves to a Value with Sensitive set. mamoritest cannot produce a
// sensitive value (its valueOf helper hardcodes Sensitive: false), so
// TestSensitiveSurvivesTheHop drives a real sensitive value through the
// server with this instead, proving the wire's "sensitive" field actually
// round-trips end to end rather than only being wired on the decode side.
type sensitiveProvider struct{ scheme string }

func (p sensitiveProvider) Scheme() string { return p.scheme }

func (p sensitiveProvider) Resolve(context.Context, mamori.Ref) (mamori.Value, error) {
	return mamori.Value{Bytes: []byte("top-secret"), Version: "v1", Sensitive: true}, nil
}

// TestSensitiveSurvivesTheHop proves Value.Sensitive round-trips through the
// server: a binding backed by sensitiveProvider (which always resolves a
// Sensitive value) is bound on a real server, and the client's own Resolve
// of it must come back with Sensitive still true.
func TestSensitiveSurvivesTheHop(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "sensitive.sock")

	srv, err := server.New(
		server.NoAuth(),
		server.WithPolicy(server.AllowAll()),
		server.WithProvider(sensitiveProvider{scheme: "sensitive-test"}),
		server.Bind("sensitive-binding", "sensitive-test://x"),
		server.Unix(sockPath, 0600),
	)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	cancel := startConformanceServer(t, srv)
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})

	p := New(Config{Endpoint: "unix://" + sockPath})
	v := pollResolve(t, p, refFor("sensitive-binding"))

	if !v.Sensitive {
		t.Fatal("resolved Value.Sensitive = false, want true: the wire's sensitive field did not survive the hop")
	}
	if string(v.Bytes) != "top-secret" {
		t.Fatalf("resolved Value.Bytes = %q, want %q", v.Bytes, "top-secret")
	}
}

// TestErrorsIsReachesSentinelForEveryKind proves, for each of the five
// classified sentinels, that errors.Is(err, sentinel) holds against a value
// resolved through a REAL server (not just through providertest's own
// ErrorClassification case, which this file's TestConformanceOverUnixSocket
// and TestConformanceOverTLSTCP already exercise via runConformance's Fail).
// This test binds a slot that is never seeded (mirroring the classify-slot
// trick in runConformance, for the same reason: a hard error, not a stale
// 200) and drives each sentinel through it in turn.
func TestErrorsIsReachesSentinelForEveryKind(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "kinds.sock")
	up := mamoritest.NewProvider("up-kinds")

	srv, err := server.New(
		server.NoAuth(),
		server.WithPolicy(server.AllowAll()),
		server.WithProvider(up),
		server.Bind("kind-slot", "up-kinds://slot"),
		server.Unix(sockPath, 0600),
	)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	cancel := startConformanceServer(t, srv)
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})

	p := New(Config{Endpoint: "unix://" + sockPath})
	ref := refFor("kind-slot")

	sentinels := []error{
		mamori.ErrPermissionDenied,
		mamori.ErrUnauthenticated,
		mamori.ErrUnavailable,
		mamori.ErrRateLimited,
		mamori.ErrInvalid,
	}

	for _, sentinel := range sentinels {
		sentinel := sentinel
		wantKind := mamori.ErrorKind(sentinel)
		t.Run(string(wantKind), func(t *testing.T) {
			up.Fail("slot", sentinel)

			var resolveErr error
			if err := pollUntil(context.Background(), func() bool {
				_, resolveErr = p.Resolve(context.Background(), ref)
				return resolveErr != nil && mamori.ErrorKind(resolveErr) == wantKind
			}); err != nil {
				t.Fatalf("waiting for kind %q: %v (last error: %v)", wantKind, err, resolveErr)
			}
			if !errors.Is(resolveErr, sentinel) {
				t.Fatalf("error %v does not satisfy errors.Is(err, %v)", resolveErr, sentinel)
			}
		})
	}
}

// countingRoundTripper wraps an http.RoundTripper and counts every request
// it forwards, for TestBatchIssuesOneRequestForMultiBindingStruct to prove
// mamori.Load groups several mamori:// bindings into exactly one HTTP
// request instead of one per field.
type countingRoundTripper struct {
	next  http.RoundTripper
	count int32
}

func (c *countingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	atomic.AddInt32(&c.count, 1)
	return c.next.RoundTrip(r)
}

// batchConformanceTarget is a struct with three independent mamori://
// bindings, all resolved through the same provider instance/scheme, so
// resolveAll (core's resolve.go) groups them into a single BatchProvider
// call.
type batchConformanceTarget struct {
	A string `source:"mamori://batch-a"`
	B string `source:"mamori://batch-b"`
	C string `source:"mamori://batch-c"`
}

// TestBatchIssuesOneRequestForMultiBindingStruct proves that loading a
// struct with several mamori:// bindings issues exactly one POST /v1/values,
// not one GET per field: it counts client-side, via a counting
// http.RoundTripper injected through Config.HTTPClient, over a real TLS TCP
// server, so the request-batching behavior is exercised over the exact same
// transport the other conformance runs use, not a fake.
func TestBatchIssuesOneRequestForMultiBindingStruct(t *testing.T) {
	cert, pool := generateSelfSignedCert(t)
	const token = "batch-tls-token"
	up := mamoritest.NewProvider("up-batch")

	srv, err := server.New(
		server.WithAuth(mamori.BearerToken(secret.NewString(token))),
		server.WithPolicy(server.AllowAll()),
		server.WithProvider(up),
		server.Bind("batch-a", "up-batch://a"),
		server.Bind("batch-b", "up-batch://b"),
		server.Bind("batch-c", "up-batch://c"),
		server.TCP("127.0.0.1:0", server.TLS(&tls.Config{Certificates: []tls.Certificate{cert}})),
	)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	cancel := startConformanceServer(t, srv)
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})

	addr := srv.Addrs()[0].(*net.TCPAddr)
	endpoint := fmt.Sprintf("https://127.0.0.1:%d", addr.Port)

	probe := New(Config{Endpoint: endpoint, TLSConfig: &tls.Config{RootCAs: pool}}, WithHeader("Authorization", "Bearer "+token))
	seeds := []struct{ name, val string }{{"batch-a", "va"}, {"batch-b", "vb"}, {"batch-c", "vc"}}
	up.Set("a", "va")
	up.Set("b", "vb")
	up.Set("c", "vc")
	for _, s := range seeds {
		s := s
		if err := pollUntil(context.Background(), func() bool {
			v, err := probe.Resolve(context.Background(), refFor(s.name))
			return err == nil && string(v.Bytes) == s.val
		}); err != nil {
			t.Fatalf("waiting for %q to seed: %v", s.name, err)
		}
	}

	counter := &countingRoundTripper{next: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	client := New(Config{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: counter, Timeout: 30 * time.Second},
	}, WithHeader("Authorization", "Bearer "+token))

	got, err := mamori.Load[batchConformanceTarget](context.Background(), mamori.WithProvider(client))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.A != "va" || got.B != "vb" || got.C != "vc" {
		t.Fatalf("Load result = %+v, want {va vb vc}", got)
	}
	if n := atomic.LoadInt32(&counter.count); n != 1 {
		t.Fatalf("HTTP request count = %d, want exactly 1 (ResolveBatch must issue a single POST /v1/values)", n)
	}
}

// TestWatchReconnectsOnServerRestartMidWatch proves the client's own
// reconnect-with-backoff loop (watch.go's watchLoop) recovers a watch across
// a real server restart, not just an idle-timeout disconnect: it opens a
// watch against a first server instance, kills that instance outright
// (simulating a crash/restart), binds a SECOND, independent *server.Server on
// the exact same Unix socket path (backed by the same upstream data), and
// asserts the ORIGINAL channel - never re-Watched - eventually delivers a
// value set after the restart.
//
// server.Server.Close shuts its listeners down via net/http.Server.Shutdown,
// which is deliberately graceful: it only closes IDLE connections, and an
// open SSE stream's handler goroutine (handleWatch) never returns on its
// own, so from net/http's point of view that connection is never idle -
// confirmed empirically (a standalone repro against a plain net/http.Server
// left both the handler's r.Context() and the client's Read blocked well
// past Shutdown giving up and returning "context deadline exceeded"). There
// is no in-process equivalent of the OS reclaiming a crashed process's file
// descriptors, and server.Server exposes no forceful-close option, so a
// graceful srv1.Close() alone would leave this test's watch connection open
// and silently stuck against a now-orphaned handler forever, never observing
// the restart at all. To get an observable disconnect without depending on
// anything the server package does not actually provide, the client here is
// built with a short overall http.Client.Timeout instead: once it fires, the
// client's own request fails exactly like a genuine dead connection would,
// and watch.go's normal reconnect-with-backoff path takes over from there.
func TestWatchReconnectsOnServerRestartMidWatch(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "reconnect.sock")
	up := mamoritest.NewProvider("up-reconnect")
	up.Set("slot", "before")

	newSrv := func() *server.Server {
		srv, err := server.New(
			server.NoAuth(),
			server.WithPolicy(server.AllowAll()),
			server.WithProvider(up),
			server.Bind("reconnect-slot", "up-reconnect://slot"),
			server.Unix(sockPath, 0600),
		)
		if err != nil {
			t.Fatalf("server.New: %v", err)
		}
		return srv
	}

	srv1 := newSrv()
	srvCancel1 := startConformanceServer(t, srv1)

	const watchRequestTimeout = 600 * time.Millisecond
	_, transport, err := parseEndpoint("unix://"+sockPath, false)
	if err != nil {
		t.Fatalf("parseEndpoint: %v", err)
	}
	p := New(Config{
		Endpoint:   "unix://" + sockPath,
		HTTPClient: &http.Client{Timeout: watchRequestTimeout, Transport: transport},
	})
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()

	ch, err := p.Watch(watchCtx, refFor("reconnect-slot"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	select {
	case u := <-ch:
		if u.Err != nil || string(u.Value.Bytes) != "before" {
			t.Fatalf("baseline update = %+v, want value %q", u, "before")
		}
	case <-time.After(pollTimeout):
		t.Fatal("timed out waiting for the baseline watch update")
	}

	// Simulate a server crash-and-restart: stop accepting on the first
	// instance (Shutdown closes its listener immediately, even though it
	// cannot force the still-open watch connection closed - see this
	// function's doc comment) and close it out in the background rather than
	// blocking on its graceful drain, then bind a fresh Server on the exact
	// same socket path, backed by the same upstream data. The client's watch
	// context and channel are never touched, so recovery has to come from
	// the client's own reconnect-with-backoff loop (forced along by
	// watchRequestTimeout above), never from a fresh Watch call.
	srvCancel1()
	srv1CloseDone := make(chan error, 1)
	go func() { srv1CloseDone <- srv1.Close() }()
	t.Cleanup(func() {
		select {
		case <-srv1CloseDone:
		case <-time.After(10 * time.Second):
		}
	})

	srv2 := newSrv()
	srvCancel2 := startConformanceServer(t, srv2)
	t.Cleanup(func() {
		srvCancel2()
		_ = srv2.Close()
	})

	up.Set("slot", "after")

	deadline := time.After(pollTimeout)
	for {
		select {
		case u, open := <-ch:
			if !open {
				t.Fatal("watch channel closed before delivering the post-restart value")
			}
			if u.Err == nil && string(u.Value.Bytes) == "after" {
				return
			}
		case <-deadline:
			t.Fatal("watch did not reconnect and deliver the post-restart value within the timeout")
		}
	}
}
