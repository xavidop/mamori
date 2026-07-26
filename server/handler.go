// This file is the v1 wire protocol handler: the only code in this package
// that a network request ever reaches. Its whole job is to enforce, on every
// single request, the ordering the rest of this package exists to make
// possible:
//
//  1. Authenticate (mamori.Authenticator.Authenticate, or skipped entirely
//     in NoAuth mode - see authenticate below).
//  2. Authorize the SPECIFIC name being requested (Policy.Allow - see
//     authorize below), separately for every name in a batch or watch
//     subscription.
//  3. ONLY THEN read the binding (Server.lookup, resolve.go).
//
// A denied caller gets 403 for the requested name whether or not it is a
// real binding: step 2 runs, and fails closed, before step 3 ever touches
// the binding table, so Policy is never a way to enumerate what is
// configured (see policy.go's Policy doc comment, and TestPolicyDenialTakesPriorityOverExistence
// in handler_test.go, which pins exactly this ordering).
//
// GET /v1/healthz is the one exception to all of the above: it never calls
// Authenticate or Policy.Allow, and its response never names a binding (see
// handleHealthz). A liveness probe has to work with no credential at all,
// and must never become a way to learn what this server is configured to
// serve.
//
// Audit logging (see requestOutcome and logAudit) runs alongside every
// decision above, recording identity, name, allow/deny, resulting kind, and
// latency - but never a value's bytes. That guarantee is structural, not a
// promise to be careful: requestOutcome has no field a value's bytes could
// be put into, so there is no line of code in this file that could log one
// even by mistake. See TestAuditNeverLogsValueBytes in handler_test.go.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/xavidop/mamori"
)

// maxBatchBodyBytes bounds how much of POST /v1/values's request body is
// read before decoding gives up. Without a limit, a caller (malicious or
// merely buggy) sending an arbitrarily large body forces this server to
// buffer all of it before json.Decode ever reports a problem; 1 MiB is far
// beyond any real "names" list while still being cheap to reject.
const maxBatchBodyBytes = 1 << 20

// sseWatchPollInterval bounds the extra delay between a binding's local
// snapshot changing (resolve.go's bindingSnapshot, kept current by that
// binding's own upstream watch goroutine) and a subscribed GET /v1/watch
// client hearing about it as an "update" frame. This package has no
// separate change-notification channel for lookup results - resolve.go
// exposes only the atomic snapshot read - so handleWatch polls it, which is
// cheap (a single atomic pointer load per watched name per tick) and does
// not require resolve.go to grow a pub/sub mechanism it does not otherwise
// need.
const sseWatchPollInterval = 200 * time.Millisecond

// sseHeartbeatInterval bounds how long an idle /v1/watch connection goes
// without ANY frame at all. Some proxies and load balancers close a
// connection they judge to be idle; a periodic SSE comment line (see
// writeSSEHeartbeat) resets that idle clock at every hop without meaning
// anything to the client itself.
const sseHeartbeatInterval = 15 * time.Second

// requestOutcome is what one audit record needs, accumulated as a request is
// handled and logged exactly once (see logAudit) when that handling
// finishes - so success, a denial at either gate, and a resolve failure are
// all captured by the same log call, and none can be skipped by a code path
// that forgets to log.
//
// There is deliberately no field here capable of holding a resolved value's
// bytes: subject and name are both plain strings naming WHO asked for WHAT,
// decision and kind describe the outcome, and latency is a duration. A
// developer extending this file cannot accidentally wire a Value into an
// audit record, because requestOutcome has nowhere to put one.
type requestOutcome struct {
	subject  string
	name     string
	decision string // "allow" or "deny" - see logAudit
	kind     mamori.Kind
	status   int
	method   string
	path     string
	latency  time.Duration
}

// logAudit writes one audit record for o, in the shape WithAudit's doc
// comment (server.go) promises: identity subject, binding name, allow/deny
// decision, resulting kind, and latency. A nil s.audit (audit logging is off
// by default) makes this a no-op, matching WithAudit's own doc comment that
// audit logging is diagnostic, not itself the enforcement mechanism.
func (s *Server) logAudit(o requestOutcome) {
	if s.audit == nil {
		return
	}
	s.audit.Info("mamori/server: request",
		"subject", o.subject,
		"name", o.name,
		"decision", o.decision,
		"kind", string(o.kind),
		"status", o.status,
		"method", o.method,
		"path", o.path,
		"latency_ms", o.latency.Milliseconds(),
	)
}

// authenticate runs the configured Authenticator and reports whether the
// request may proceed. In NoAuth mode it is skipped entirely - see
// server.go's NoAuth doc comment - returning an empty Identity and ok=true
// without ever calling anything; authorize (below) still runs regardless, so
// NoAuth turns off authentication only, never authorization.
//
// On failure it writes the response itself - 401, with the Authenticator's
// Challenge() header if it implements mamori.Challenger, UNLESS the
// Authenticator specifically returned mamori.ErrForbidden, which is treated
// as authenticated-but-forbidden (403) instead - mirroring core's own admin
// endpoint (handler.go's authOK) exactly, since mamori.Authenticator is one
// interface shared by both surfaces (see auth.go's doc comment).
//
// authenticate never consults Policy. Authorization is a separate gate,
// applied per requested binding name by authorize below (once, per name -
// see handleBatch and handleWatch, which each call it once per name in a
// multi-name request). Do not treat authenticate's ok=true as "this caller
// may read this binding": it only means "this caller is who they say they
// are."
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (id mamori.Identity, kind mamori.Kind, status int, ok bool) {
	if s.noAuth {
		return mamori.Identity{}, "", 0, true
	}
	id, err := s.auth.Authenticate(r)
	if err == nil {
		return id, "", 0, true
	}
	if errors.Is(err, mamori.ErrForbidden) {
		writeError(w, http.StatusForbidden, mamori.KindPermissionDenied, "forbidden")
		return mamori.Identity{}, mamori.KindPermissionDenied, http.StatusForbidden, false
	}
	if c, isChallenger := s.auth.(mamori.Challenger); isChallenger {
		w.Header().Set("WWW-Authenticate", c.Challenge())
	}
	writeError(w, http.StatusUnauthorized, mamori.KindUnauthenticated, "unauthenticated")
	return mamori.Identity{}, mamori.KindUnauthenticated, http.StatusUnauthorized, false
}

// authorize runs Policy.Allow for id/name and reports whether the request
// may proceed to read the binding. On denial it writes 403 with
// mamori.KindPermissionDenied - always that kind and that status,
// regardless of what error the Policy actually returned - because a Policy
// denial must not vary in a way that reveals anything about name (see
// policy.go's Policy and ErrDenied doc comments: a Policy implementation is
// expected to return the same error shape whether name exists or not, and
// this function completes that guarantee on the wire side by never
// forwarding the Policy's own error message either).
func (s *Server) authorize(w http.ResponseWriter, id mamori.Identity, name string) bool {
	if err := s.policy.Allow(id, name); err != nil {
		writeError(w, http.StatusForbidden, mamori.KindPermissionDenied, "permission denied")
		return false
	}
	return true
}

// Handler returns the v1 wire protocol http.Handler: GET /v1/values/{name},
// POST /v1/values, GET /v1/watch, and GET /v1/healthz. Every route except
// /v1/healthz runs authenticate-then-authorize-then-read, in that order (see
// this file's package doc comment). Mount the result under whatever
// listener a later task's transports wire up (Unix socket, TLS TCP); it
// does not bind or serve anything itself.
//
// Every pattern registered below is an EXACT path - no trailing "/", no "..."
// wildcard - which under Go 1.22+ ServeMux semantics means each one matches
// only that literal path, never as a subtree/prefix. A path outside these
// four routes therefore 404s by ServeMux's own default, with no explicit
// catch-all handler needed; a method mismatch on one of these paths (POST
// /v1/watch, say) gets ServeMux's own 405.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/values/{name}", s.handleValue)
	mux.HandleFunc("POST /v1/values", s.handleBatch)
	mux.HandleFunc("GET /v1/watch", s.handleWatch)
	mux.HandleFunc("GET /v1/healthz", s.handleHealthz)
	return mux
}

// handleValue serves GET /v1/values/{name}: authenticate, authorize name,
// then read - see this file's package doc comment for why that order is
// non-negotiable. found=false (name is not a bound name at all) and a
// resolved-but-upstream-reports-not-found binding both answer 404 with the
// same generic message, deliberately: by the time either is reached,
// authorize has already run, so neither response is about hiding
// existence - both are just "there is nothing here to return" - but keeping
// the wording identical means a difference in phrasing can never become a
// secondary way to distinguish the two cases either.
func (s *Server) handleValue(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	name := r.PathValue("name")
	w.Header().Set("Cache-Control", "no-store")

	o := requestOutcome{name: name, method: r.Method, path: r.URL.Path}
	defer func() { o.latency = time.Since(start); s.logAudit(o) }()

	id, kind, status, ok := s.authenticate(w, r)
	o.subject = id.Subject
	if !ok {
		o.decision, o.kind, o.status = "deny", kind, status
		return
	}

	if !s.authorize(w, id, name) {
		o.decision, o.kind, o.status = "deny", mamori.KindPermissionDenied, http.StatusForbidden
		return
	}

	v, k, hasValue, found := s.lookup(name)
	o.decision = "allow"
	switch {
	case !found:
		writeError(w, http.StatusNotFound, mamori.KindNotFound, "binding not found")
		o.kind, o.status = mamori.KindNotFound, http.StatusNotFound
	case k != "" && !hasValue:
		st := statusForKind(k)
		writeError(w, st, k, "binding resolve failed")
		o.kind, o.status = k, st
	default:
		writeJSON(w, http.StatusOK, newValueBody(name, v, k))
		o.kind, o.status = k, http.StatusOK
	}
}

// handleBatch serves POST /v1/values: body {"names":[...]}, response
// {"values":[...]} with one entry per requested name, in request order.
//
// Semantics chosen: each name is authorized and resolved independently, so
// one denied or missing name in a batch of many becomes that ONE entry's
// Error, never a whole-request 403 or 404. This is deliberately the more
// useful of the two options the spec allows (see this package's task brief):
// a caller asking for 50 names should not have to retry 49 of them one at a
// time just because it lacked access to the 50th, and a per-name error is no
// more revealing than the SAME caller making 50 individual GET
// /v1/values/{name} calls would already be - this endpoint is a convenience
// over that, not a different trust boundary. Authentication is still
// whole-request: one 401/403 ends the entire batch before any name is even
// looked at, since there is only one Identity for the whole connection.
//
// Every name is audited exactly like a standalone GET would be - one record
// per name (see authorize's call site below) - so the audit trail reads the
// same regardless of which route reached a given binding.
func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	w.Header().Set("Cache-Control", "no-store")

	id, kind, status, ok := s.authenticate(w, r)
	if !ok {
		s.logAudit(requestOutcome{subject: id.Subject, method: r.Method, path: r.URL.Path,
			decision: "deny", kind: kind, status: status, latency: time.Since(start)})
		return
	}

	var req batchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBatchBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, mamori.KindInvalid, "malformed request body")
		s.logAudit(requestOutcome{subject: id.Subject, method: r.Method, path: r.URL.Path,
			decision: "deny", kind: mamori.KindInvalid, status: http.StatusBadRequest, latency: time.Since(start)})
		return
	}

	entries := make([]valueBody, 0, len(req.Names))
	for _, name := range req.Names {
		entryStart := time.Now()
		o := requestOutcome{subject: id.Subject, name: name, method: r.Method, path: r.URL.Path}

		if err := s.policy.Allow(id, name); err != nil {
			entries = append(entries, newErrorValueBody(name, mamori.KindPermissionDenied, "permission denied"))
			o.decision, o.kind, o.status = "deny", mamori.KindPermissionDenied, http.StatusForbidden
			o.latency = time.Since(entryStart)
			s.logAudit(o)
			continue
		}

		v, k, hasValue, found := s.lookup(name)
		o.decision = "allow"
		switch {
		case !found:
			entries = append(entries, newErrorValueBody(name, mamori.KindNotFound, "binding not found"))
			o.kind, o.status = mamori.KindNotFound, http.StatusNotFound
		case k != "" && !hasValue:
			st := statusForKind(k)
			entries = append(entries, newErrorValueBody(name, k, "binding resolve failed"))
			o.kind, o.status = k, st
		default:
			entries = append(entries, newValueBody(name, v, k))
			o.kind, o.status = k, http.StatusOK
		}
		o.latency = time.Since(entryStart)
		s.logAudit(o)
	}

	writeJSON(w, http.StatusOK, batchResponse{Values: entries})
}

// handleWatch serves GET /v1/watch?name=a&name=b: an SSE stream that opens
// once auth passes, authorizes every requested name up front (same per-name
// semantics as handleBatch - see its doc comment), and then pushes an
// "update" frame for each allowed name whenever its resolved state changes,
// plus a periodic heartbeat comment to keep the connection alive through
// idle-closing proxies.
//
// A denied name gets one "error" frame at subscribe time and is then simply
// excluded from the poll loop below - it does not close the stream for
// whichever OTHER requested names were allowed. If every requested name is
// denied, the connection still opens (200) and stays alive on heartbeats
// alone; the client sees exactly what it is entitled to see and nothing
// more, which is consistent whether that turns out to be all, some, or none
// of what it asked for.
//
// The stream ends when the client disconnects (r.Context() is canceled) or
// a write fails (the same signal, observed the other way around). Neither
// case is treated as an error to recover from: reconnecting is the CLIENT's
// job, per the wire spec's own /v1/watch entry, not this handler's - it
// simply returns, its two tickers stop via defer, and net/http reclaims the
// connection.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	w.Header().Set("Cache-Control", "no-store")

	id, kind, status, ok := s.authenticate(w, r)
	if !ok {
		s.logAudit(requestOutcome{subject: id.Subject, method: r.Method, path: r.URL.Path,
			decision: "deny", kind: kind, status: status, latency: time.Since(start)})
		return
	}

	names := r.URL.Query()["name"]
	if len(names) == 0 {
		writeError(w, http.StatusBadRequest, mamori.KindInvalid, "at least one name query parameter is required")
		s.logAudit(requestOutcome{subject: id.Subject, method: r.Method, path: r.URL.Path,
			decision: "deny", kind: mamori.KindInvalid, status: http.StatusBadRequest, latency: time.Since(start)})
		return
	}

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		writeError(w, http.StatusInternalServerError, mamori.KindUnknown, "streaming unsupported")
		s.logAudit(requestOutcome{subject: id.Subject, method: r.Method, path: r.URL.Path,
			decision: "deny", kind: mamori.KindUnknown, status: http.StatusInternalServerError, latency: time.Since(start)})
		return
	}
	flush := flusher.Flush

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// watched is this connection's per-name last-observed state, used only
	// to decide (via snapshotChanged) whether the poll loop below has
	// anything new to send; it is local to this goroutine (one per HTTP
	// connection), so it needs no locking of its own.
	type watched struct {
		name  string
		value mamori.Value
		kind  mamori.Kind
		found bool
	}
	var subs []watched

	for _, name := range names {
		entryStart := time.Now()
		o := requestOutcome{subject: id.Subject, name: name, method: r.Method, path: r.URL.Path}

		if err := s.policy.Allow(id, name); err != nil {
			_ = writeSSEError(w, flush, name, mamori.KindPermissionDenied, "permission denied")
			o.decision, o.kind, o.status = "deny", mamori.KindPermissionDenied, http.StatusForbidden
			o.latency = time.Since(entryStart)
			s.logAudit(o)
			continue
		}

		v, k, hasValue, found := s.lookup(name)
		sendWatchSnapshot(w, flush, name, v, k, hasValue, found)
		o.decision = "allow"
		switch {
		case !found:
			o.kind, o.status = mamori.KindNotFound, http.StatusNotFound
		case k != "" && !hasValue:
			o.kind, o.status = k, statusForKind(k)
		default:
			o.kind, o.status = k, http.StatusOK
		}
		o.latency = time.Since(entryStart)
		s.logAudit(o)
		subs = append(subs, watched{name: name, value: v, kind: k, found: found})
	}

	if len(subs) == 0 {
		// Every requested name was denied; the client already has an
		// "error" frame for each one. There is nothing left to watch, so
		// the connection can end here rather than staying open on
		// heartbeats alone for a subscription with zero live names.
		return
	}

	poll := time.NewTicker(sseWatchPollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if err := writeSSEHeartbeat(w, flush); err != nil {
				return
			}
		case <-poll.C:
			for i := range subs {
				v, k, hasValue, found := s.lookup(subs[i].name)
				if !snapshotChanged(subs[i].value, subs[i].kind, subs[i].found, v, k, found) {
					continue
				}
				subs[i].value, subs[i].kind, subs[i].found = v, k, found
				if !sendWatchSnapshot(w, flush, subs[i].name, v, k, hasValue, found) {
					return
				}
			}
		}
	}
}

// handleHealthz answers GET /v1/healthz: liveness only. It is the one route
// exempt from both authenticate and authorize (see this file's package doc
// comment) - a readiness/liveness probe has to succeed with no credential at
// all - and its response is a fixed, unconditional 200 {"status":"ok"} that
// never names a binding, never reports per-binding health, and never
// branches on anything about the request. Do not extend this handler with
// binding detail of any kind: doing so would turn an intentionally
// unauthenticated endpoint into a way to enumerate what this server serves.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, healthzBody{Status: "ok"})
}
