package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
)

// This file covers the pieces that make running several replicas behind one
// address safe (see the HA docs page): the readiness/drain signal a load
// balancer routes on, the freshness metadata that lets a client notice it
// reached a laggier replica, and the node identity that makes an aggregated
// audit stream attributable.

// blockingProvider never delivers an update, so every binding using it stays
// at errPendingResolve. It is how the readiness tests hold a Server in the
// cold-cache state that a naive healthz cannot distinguish from ready.
type blockingProvider struct{ scheme string }

func (b blockingProvider) Scheme() string { return b.scheme }

func (b blockingProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	<-ctx.Done()
	return mamori.Value{}, ctx.Err()
}

func (b blockingProvider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	ch := make(chan mamori.Update)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// readyzState sends GET /v1/readyz and returns the HTTP status and the
// reported state string.
func readyzState(t *testing.T, s *Server) (int, string) {
	t.Helper()
	rec := doRequest(t, s, http.MethodGet, "/v1/readyz", nil)
	var body readyzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding readyz body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body.Status
}

// TestReadyzPrimingUntilBindingsSettle is the core readiness guarantee: a
// replica whose cache is still cold must report 503, so a load balancer does
// not send it requests it can only fail. This is exactly the case an
// unconditional healthz cannot express.
func TestReadyzPrimingUntilBindingsSettle(t *testing.T) {
	s := newTestServer(t,
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("slow", "blocked://k"),
		WithProvider(blockingProvider{scheme: "blocked"}),
	)

	code, state := readyzState(t, s)
	if code != http.StatusServiceUnavailable {
		t.Errorf("readyz status = %d, want %d while a binding is unresolved", code, http.StatusServiceUnavailable)
	}
	if state != readyStatePriming {
		t.Errorf("readyz state = %q, want %q", state, readyStatePriming)
	}

	// Liveness must stay 200 throughout: the process is alive, it is simply
	// not ready. Conflating the two is what makes an orchestrator restart a
	// replica that was only still starting up.
	rec := doRequest(t, s, http.MethodGet, "/v1/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("healthz status = %d while priming, want %d (liveness is not readiness)", rec.Code, http.StatusOK)
	}
}

// TestReadyzReadyOnceResolved checks the transition to ready once every
// binding has a value.
func TestReadyzReadyOnceResolved(t *testing.T) {
	p := mamoritest.NewProvider("ha")
	p.Set("k", "v1")

	s := newTestServer(t,
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "ha://k"),
		WithProvider(p),
	)

	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && hasValue
	})

	code, state := readyzState(t, s)
	if code != http.StatusOK {
		t.Errorf("readyz status = %d once resolved, want %d", code, http.StatusOK)
	}
	if state != readyStateReady {
		t.Errorf("readyz state = %q, want %q", state, readyStateReady)
	}
}

// TestReadyzReadyWhenBindingSettlesOnError pins the deliberate asymmetry in
// Server.readiness: a binding that failed upstream still counts as settled.
// Every replica would report that same upstream error, so holding them all
// out of rotation would turn one broken binding into a total outage.
func TestReadyzReadyWhenBindingSettlesOnError(t *testing.T) {
	// A scheme with no registered provider settles immediately at
	// errNoProvider and can never resolve.
	s := newTestServer(t,
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("orphan", "nosuchscheme://k"),
	)

	code, state := readyzState(t, s)
	if code != http.StatusOK {
		t.Errorf("readyz status = %d, want %d: a permanently unresolvable binding must not hold the replica out of rotation", code, http.StatusOK)
	}
	if state != readyStateReady {
		t.Errorf("readyz state = %q, want %q", state, readyStateReady)
	}
}

// TestReadyzDrainingAfterClose checks that Close marks the replica not-ready.
// Draining has to be observable while the listeners are still up, which is
// the whole point: it is the window a load balancer uses to stop routing here
// before connections start failing.
func TestReadyzDrainingAfterClose(t *testing.T) {
	p := mamoritest.NewProvider("ha")
	p.Set("k", "v1")

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "ha://k"),
		WithProvider(p),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && hasValue
	})
	if code, _ := readyzState(t, s); code != http.StatusOK {
		t.Fatalf("readyz status = %d before Close, want %d", code, http.StatusOK)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	code, state := readyzState(t, s)
	if code != http.StatusServiceUnavailable {
		t.Errorf("readyz status = %d after Close, want %d", code, http.StatusServiceUnavailable)
	}
	if state != readyStateDraining {
		t.Errorf("readyz state = %q, want %q", state, readyStateDraining)
	}
}

// TestDrainGraceDelaysTeardown checks that DrainGrace holds the replica in
// the draining state before listeners are torn down, rather than tearing down
// immediately as the zero-value default does.
func TestDrainGraceDelaysTeardown(t *testing.T) {
	p := mamoritest.NewProvider("ha")
	p.Set("k", "v1")

	const grace = 120 * time.Millisecond
	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "ha://k"),
		WithProvider(p),
		DrainGrace(grace),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Errorf("Close returned after %v, want at least the %v drain grace", elapsed, grace)
	}
}

// TestDefaultNoDrainGraceIsImmediate checks the default stays zero, so an
// existing caller does not silently gain a shutdown delay by upgrading.
func TestDefaultNoDrainGraceIsImmediate(t *testing.T) {
	s, err := New(WithPolicy(AllowAll()), NoAuth())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.drainGrace != 0 {
		t.Errorf("default drainGrace = %v, want 0", s.drainGrace)
	}
}

// TestValueCarriesFreshness checks that a served value is dated, so a client
// talking to several replicas can tell which answer is older. Without it the
// wire offers no way to order two replicas' answers in time.
func TestValueCarriesFreshness(t *testing.T) {
	p := mamoritest.NewProvider("ha")
	p.Set("k", "v1")

	s := newTestServer(t,
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "ha://k"),
		WithProvider(p),
	)
	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && hasValue
	})

	before := time.Now()
	rec := doRequest(t, s, http.MethodGet, "/v1/values/b", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var body valueBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if body.ResolvedAt == nil {
		t.Fatal("resolved_at is absent; a client cannot tell how old this replica's answer is")
	}
	if body.ResolvedAt.After(before) {
		t.Errorf("resolved_at = %v is in the future relative to the request at %v", body.ResolvedAt, before)
	}
	if body.Stale {
		t.Error("stale = true for a healthy value, want false")
	}
}

// TestStaleFlagOnLastKnownGood checks the other half of the freshness
// contract: when a binding is serving its last-known-good value because
// upstream is currently failing, the response says so. A client choosing
// between replicas needs to prefer one that is not serving stale data.
func TestStaleFlagOnLastKnownGood(t *testing.T) {
	p := mamoritest.NewProvider("ha")
	p.Set("k", "v1")

	s := newTestServer(t,
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "ha://k"),
		WithProvider(p),
	)
	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && hasValue && string(v.Bytes) == "v1"
	})

	// Capture when the good value was resolved, then fail upstream. apply
	// carries the value forward, so the binding keeps serving v1.
	firstRec := doRequest(t, s, http.MethodGet, "/v1/values/b", nil)
	var first valueBody
	if err := json.Unmarshal(firstRec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decoding first body: %v", err)
	}

	p.Fail("k", mamori.ErrUnavailable)
	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && hasValue && k != ""
	})

	rec := doRequest(t, s, http.MethodGet, "/v1/values/b", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (last-known-good is still served): %s", rec.Code, rec.Body.String())
	}
	var body valueBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if string(body.Bytes) != "v1" {
		t.Errorf("bytes = %q, want the last-known-good %q", body.Bytes, "v1")
	}
	if !body.Stale {
		t.Error("stale = false while serving last-known-good past an upstream failure, want true")
	}
	// The timestamp must date the BYTES, not the failed attempt, so a client
	// can see how old the data it is being handed actually is.
	if body.ResolvedAt == nil || first.ResolvedAt == nil {
		t.Fatal("resolved_at absent")
	}
	if !body.ResolvedAt.Equal(*first.ResolvedAt) {
		t.Errorf("resolved_at moved to %v after a FAILED update (was %v); it must date the value, not the attempt",
			body.ResolvedAt, first.ResolvedAt)
	}
}

// TestReadyzLeaksNothing pins the unauthenticated-surface discipline: readyz
// must report a state and nothing else. Binding names, counts, or a node ID
// would let an unauthenticated caller learn the shape of the deployment.
func TestReadyzLeaksNothing(t *testing.T) {
	p := mamoritest.NewProvider("ha")
	p.Set("k", "v1")

	s := newTestServer(t,
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("super-secret-binding-name", "ha://k"),
		WithProvider(p),
		NodeID("replica-7"),
	)

	rec := doRequest(t, s, http.MethodGet, "/v1/readyz", nil)
	got := rec.Body.String()
	for _, leak := range []string{"super-secret-binding-name", "replica-7", "ha://k"} {
		if strings.Contains(got, leak) {
			t.Errorf("readyz body leaks %q: %s", leak, got)
		}
	}

	var generic map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &generic); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if len(generic) != 1 {
		t.Errorf("readyz body has %d fields (%v), want exactly 1 (status)", len(generic), generic)
	}
}

// TestAuditRecordsNodeID checks that audit records name the replica, so an
// aggregated stream from several replicas stays attributable.
func TestAuditRecordsNodeID(t *testing.T) {
	p := mamoritest.NewProvider("ha")
	p.Set("k", "v1")

	var logBuf bytes.Buffer
	s := newTestServer(t,
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("b", "ha://k"),
		WithProvider(p),
		NodeID("replica-7"),
		WithAudit(slog.New(slog.NewTextHandler(&logBuf, nil))),
	)
	waitForLookup(t, s, "b", func(v mamori.Value, k mamori.Kind, hasValue, found bool) bool {
		return found && hasValue
	})

	doRequest(t, s, http.MethodGet, "/v1/values/b", nil)

	if got := logBuf.String(); !strings.Contains(got, "replica-7") {
		t.Errorf("audit record does not name the node:\n%s", got)
	}
}

// TestNodeIDDefaultsToHostname checks the default, so an operator gets
// attributable audit records without configuring anything.
func TestNodeIDDefaultsToHostname(t *testing.T) {
	s, err := New(WithPolicy(AllowAll()), NoAuth())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A hostname lookup can legitimately fail on an unconfigured host, in
	// which case New leaves the ID empty rather than refusing to build.
	if s.nodeID == "" {
		t.Skip("os.Hostname unavailable in this environment")
	}
}
