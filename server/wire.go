package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/xavidop/mamori"
)

// This file is the v1 wire protocol's JSON (and SSE-framed JSON) vocabulary:
// the shapes handler.go encodes and decodes, and the small pure functions
// that build them from mamori.Value/mamori.Kind. Nothing in this file talks
// to the network, a Policy, or an Authenticator - see handler.go for the
// routing, auth, authz, and audit logic that produces the values these types
// carry.
//
// The single rule every shape here is built to uphold: a value's bytes only
// ever travel inside valueBody.Bytes, base64-encoded by encoding/json's
// standard []byte handling. No other type in this file, and no error path
// through it, has a field capable of holding a resolved payload - so an
// audit log, an error body, or a malformed request can never accidentally
// carry a secret (see handler.go's requestOutcome for the audit-side half
// of this same guarantee).

// errorDetail is the wire shape of one error: kind is a mamori.Kind's string
// form (see errors.go's Kind constants), round-tripped so a client can
// recover the original sentinel with mamori.SentinelFor(mamori.Kind(kind)) -
// e.g. "permission_denied" -> mamori.ErrPermissionDenied. message is a
// human-readable string for logs and debugging; a client's control flow
// should key off kind, not message text, since message is not a stable
// contract the way kind is.
type errorDetail struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// errorResponse is the top-level body of a request that fails outright, with
// no associated binding name in the response (GET /v1/values/{name}'s
// failure body, and any whole-request failure such as a malformed batch
// body or a 401/403 before any name was even looked at). It matches the wire
// spec's `{"error":{"kind":"...","message":"..."}}` exactly.
type errorResponse struct {
	Error errorDetail `json:"error"`
}

// newErrorResponse builds an errorResponse for kind, with a caller-supplied
// message. It exists mainly so call sites read as "an error of this kind,
// meaning this" rather than repeating the nested-struct literal everywhere.
func newErrorResponse(kind mamori.Kind, message string) errorResponse {
	return errorResponse{Error: errorDetail{Kind: string(kind), Message: message}}
}

// valueBody is the wire shape of one resolved binding: the whole body of a
// successful GET /v1/values/{name}, one element of POST /v1/values's
// "values" array, and the payload of an SSE "update"/"error" frame. It maps
// field-for-field onto mamori.Value, plus Name (which mamori.Value does not
// carry - a Value on its own does not know what binding it came from) and
// two wire-only additions:
//
//   - Kind carries the SAME round-trippable mamori.Kind string as
//     errorDetail.Kind, but here it means something different: this is a
//     SUCCESSFUL response (status 200, real bytes attached), and Kind is only
//     ever non-empty when Bytes is a last-known-good value being served
//     while the binding's upstream is CURRENTLY failing (see resolve.go's
//     lookup doc comment: "kind != "" with a non-zero value means serving
//     stale"). A client sees Kind == "" as "fresh", and a non-empty Kind as
//     "usable, but this reflects an in-progress upstream problem" - it is an
//     annotation on a success, never a failure signal by itself.
//   - Error is set only on a batch or SSE entry describing a single name
//     that failed (denied, not found, or a hard resolve failure with no
//     last-known-good value at all); every other field is then left at its
//     zero value and omitted from the JSON. GET /v1/values/{name}'s own
//     failure path does not use this field - it returns an errorResponse
//     instead, matching the wire spec's un-nested `{"error":{...}}` shape for
//     that route.
//
// Bytes, Version, Sensitive, and NotAfter all carry `omitempty`, which has
// one small, deliberate wrinkle: a real success value whose Bytes happens to
// be legitimately empty (a zero-length secret) would have its "bytes" key
// omitted exactly the same as an Error entry that never touched Bytes at
// all. That is an acceptable, documented tradeoff against the alternative
// (two near-duplicate wire types, one for success and one for batch/SSE
// entries) for what is, in practice, a vanishingly rare payload shape.
type valueBody struct {
	Name      string            `json:"name"`
	Bytes     []byte            `json:"bytes,omitempty"`
	Version   string            `json:"version,omitempty"`
	Sensitive bool              `json:"sensitive,omitempty"`
	NotAfter  *time.Time        `json:"not_after,omitempty"`
	Metadata  map[string]string `json:"metadata"`
	Kind      string            `json:"kind,omitempty"`
	Error     *errorDetail      `json:"error,omitempty"`
}

// newValueBody builds the success wire shape for a resolved binding. kind is
// the binding's CURRENT upstream classification from lookup (resolve.go):
// pass the empty Kind for a healthy value, or a non-empty one for a
// last-known-good value being served while upstream is failing (see
// valueBody's doc comment on its own Kind field).
//
// Metadata is never left nil on the wire: mamori.Value.Metadata may be nil
// (most providers never set it), but encoding/json marshals a nil map as
// JSON `null`, not `{}`. The wire spec's example response shows
// `"metadata":{}`, and a client deserializing straight into a
// map[string]string would rather see an empty map than have to nil-check
// before every read, so a nil Metadata is normalized to an empty map here.
func newValueBody(name string, v mamori.Value, kind mamori.Kind) valueBody {
	md := v.Metadata
	if md == nil {
		md = map[string]string{}
	}
	vb := valueBody{
		Name:      name,
		Bytes:     v.Bytes,
		Version:   v.Version,
		Sensitive: v.Sensitive,
		Metadata:  md,
		Kind:      string(kind),
	}
	if !v.NotAfter.IsZero() {
		t := v.NotAfter
		vb.NotAfter = &t
	}
	return vb
}

// newErrorValueBody builds a batch/SSE entry describing a single name that
// failed - denied, not found, or a hard resolve failure - leaving every
// value-shaped field at its zero value (and so omitted from the JSON: see
// valueBody's field tags) except Name and Error.
func newErrorValueBody(name string, kind mamori.Kind, message string) valueBody {
	return valueBody{Name: name, Error: &errorDetail{Kind: string(kind), Message: message}}
}

// batchRequest is POST /v1/values's body: the names to resolve, in the order
// the caller wants them answered (the response's "values" array preserves
// this order - see handleBatch in handler.go).
type batchRequest struct {
	Names []string `json:"names"`
}

// batchResponse is POST /v1/values's body: one valueBody per requested name,
// each independently either a resolved value or an Error entry. See
// handleBatch's doc comment in handler.go for why a single denied or missing
// name in the batch does not fail the whole request.
type batchResponse struct {
	Values []valueBody `json:"values"`
}

// kindStatus maps a mamori.Kind to the HTTP status a hard failure of that
// kind is reported as, per the wire spec's kind->status table. It is a var,
// not embedded in statusForKind, so the table itself - the part of this file
// most likely to be read during an incident or a security review - is one
// flat, greppable literal rather than buried in branches.
var kindStatus = map[mamori.Kind]int{
	mamori.KindNotFound:         http.StatusNotFound,
	mamori.KindInvalid:          http.StatusBadRequest,
	mamori.KindUnauthenticated:  http.StatusUnauthorized,
	mamori.KindPermissionDenied: http.StatusForbidden,
	mamori.KindRateLimited:      http.StatusTooManyRequests,
	mamori.KindUnavailable:      http.StatusServiceUnavailable,
	mamori.KindUnknown:          http.StatusInternalServerError,
}

// statusForKind returns kind's HTTP status per kindStatus, defaulting to 500
// for a Kind absent from the table - which should only ever be the empty
// Kind (a healthy value, which never reaches this function - callers only
// call it once they already know they are reporting a failure) or a Kind
// this version of the table does not yet know about. Either way, 500 is the
// honest answer: an error whose classification this server cannot map is
// exactly what "internal server error" means.
func statusForKind(kind mamori.Kind) int {
	if status, ok := kindStatus[kind]; ok {
		return status
	}
	return http.StatusInternalServerError
}

// writeJSON encodes v as the response body with the given status code. Like
// core's own handler.go writeJSON, it runs after WriteHeader has committed
// the status, so an encoding failure (which should never happen for the
// fixed shapes in this file) cannot change the status code; there is nothing
// more useful to do with such an error than drop it, since the response is
// already underway.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes an errorResponse for kind/message at status. It is the
// single call site every whole-request failure (auth, authz, malformed body,
// a hard resolve failure on the single-value route) goes through, so the
// error shape stays uniform without each handler re-building errorResponse
// by hand.
func writeError(w http.ResponseWriter, status int, kind mamori.Kind, message string) {
	writeJSON(w, status, newErrorResponse(kind, message))
}

// writeSSEValue writes body as an SSE "update" frame: an event line naming
// the event type, a data line carrying body as one JSON object (SSE forbids
// embedded newlines in a data line, which is why body is marshaled first,
// as a single line, rather than written through a streaming encoder), and
// the blank line SSE requires to terminate the event. flush is called after
// writing so the frame reaches the client immediately rather than sitting in
// a buffer.
func writeSSEValue(w io.Writer, flush func(), body valueBody) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: update\ndata: %s\n\n", data); err != nil {
		return err
	}
	flush()
	return nil
}

// writeSSEError writes an "error" frame for one name in a /v1/watch
// subscription (a denied name, an unbound name, or a hard resolve failure),
// using the same valueBody{Name, Error} shape a batch response's per-name
// error entry uses - see newErrorValueBody - so a client parses both the
// same way.
func writeSSEError(w io.Writer, flush func(), name string, kind mamori.Kind, message string) error {
	data, err := json.Marshal(newErrorValueBody(name, kind, message))
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: error\ndata: %s\n\n", data); err != nil {
		return err
	}
	flush()
	return nil
}

// sendWatchSnapshot writes the appropriate SSE frame for one binding's
// current (value, kind, hasValue, found) state - an "error" frame for an
// unbound name or a hard resolve failure (no last-known-good value at all),
// an "update" frame otherwise - and reports whether the write succeeded, so
// handleWatch's caller can end the connection on a write failure (a client
// that has gone away, most commonly) instead of looping on a dead
// connection. hasValue is resolve.go's lookup's own authoritative record of
// whether the binding has ever resolved, not inferred from v's shape - see
// lookup's doc comment in resolve.go for why guessing from Bytes/Version is
// unsound. It applies the exact same found/kind/hasValue classification
// handleValue and handleBatch use, so a client sees the same state shape
// regardless of which route it reached a binding through.
func sendWatchSnapshot(w io.Writer, flush func(), name string, v mamori.Value, kind mamori.Kind, hasValue, found bool) bool {
	var err error
	switch {
	case !found:
		err = writeSSEError(w, flush, name, mamori.KindNotFound, "binding not found")
	case kind != "" && !hasValue:
		err = writeSSEError(w, flush, name, kind, "binding resolve failed")
	default:
		err = writeSSEValue(w, flush, newValueBody(name, v, kind))
	}
	return err == nil
}

// healthzBody is GET /v1/healthz's response shape: a bare liveness signal,
// deliberately with no other field. See handleHealthz's doc comment in
// handler.go for why this must never grow a binding-detail field.
type healthzBody struct {
	Status string `json:"status"`
}

// writeSSEHeartbeat writes an SSE comment line, which the SSE spec defines
// as a frame with no event name and no data - clients ignore it entirely -
// but which still counts as connection activity to every intermediary
// between here and the client (a load balancer or proxy that closes
// connections after a period of silence). See handler.go's
// sseHeartbeatInterval for why this is sent periodically.
func writeSSEHeartbeat(w io.Writer, flush func()) error {
	if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
		return err
	}
	flush()
	return nil
}

// snapshotChanged reports whether the (value, kind, found) triple lookup
// returns for a binding differs from the previously observed one, for
// handleWatch's poll loop to decide whether a fresh SSE frame is needed.
//
// It mirrors mamori.Value's own unexported `changed` method (Version-first,
// falling back to a byte comparison when neither side has one) rather than
// calling it directly, because that method is unexported to package mamori
// and this package cannot reach it; the comparison logic itself is small
// enough that duplicating it here is clearer than inventing an exported
// equivalent in core just for this one caller. found and kind are compared
// first and cheaply (a binding transitioning between "resolved" and "never
// resolved", or between two different failure kinds, is a change regardless
// of what Version/Bytes happen to hold in that state).
func snapshotChanged(prevValue mamori.Value, prevKind mamori.Kind, prevFound bool, value mamori.Value, kind mamori.Kind, found bool) bool {
	if prevFound != found || prevKind != kind {
		return true
	}
	if value.Version != "" || prevValue.Version != "" {
		return value.Version != prevValue.Version
	}
	return !bytes.Equal(value.Bytes, prevValue.Bytes)
}
