package mamori

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// handlerOptions collects the configuration built up by HandlerOption values.
type handlerOptions struct {
	prefix     string
	auth       Authenticator
	authSet    bool
	middleware []func(http.Handler) http.Handler
}

// HandlerOption configures Handler.
type HandlerOption func(*handlerOptions)

// HandlerPrefix strips prefix from request paths before routing, so the
// handler can be mounted under a subpath of an existing mux (for example
// "/admin") instead of always owning the root of whatever mux it is attached
// to.
func HandlerPrefix(prefix string) HandlerOption {
	return func(o *handlerOptions) { o.prefix = strings.TrimSuffix(prefix, "/") }
}

// WithAuth requires every request to pass a before it is served, with one
// exemption: /healthz still answers without credentials, just without the
// failing-field detail (see serveHealthz). Applying WithAuth more than once
// is a construction error rather than a silent overwrite, since "the second
// call wins" and "the first call wins" are both plausible reads of the call
// site and neither is obviously right; compose multiple schemes explicitly
// with AnyOf or AllOf instead so the intended semantics are visible at the
// call site.
func WithAuth(a Authenticator) HandlerOption {
	return func(o *handlerOptions) {
		if o.authSet {
			panic("mamori: WithAuth applied more than once; compose schemes with AnyOf or AllOf")
		}
		o.auth = a
		o.authSet = true
	}
}

// HandlerMiddleware wraps the handler with a non-authentication concern such
// as request logging or rate limiting. It runs outside the Authenticator (and
// outside HandlerPrefix's stripping), so it sees the request before either
// applies, in the order the options were given.
func HandlerMiddleware(mw func(http.Handler) http.Handler) HandlerOption {
	return func(o *handlerOptions) { o.middleware = append(o.middleware, mw) }
}

// Handler returns an http.Handler exposing a Watcher's health over HTTP. It
// serves exactly two routes: GET / (the Report, as JSON) and GET /healthz (a
// bare liveness/readiness signal). Every other path, and every other method,
// is 404. There is no route that serves a configuration value, under any
// option: the Report returned by w.Status() already redacts refs and omits
// values, so exposing it as-is is safe by construction, and Handler has no
// way to add a field beyond what Report already carries.
//
// Mount the result on your own mux, optionally under HandlerPrefix.
func Handler[T any](w *Watcher[T], opts ...HandlerOption) http.Handler {
	o := &handlerOptions{}
	for _, opt := range opts {
		opt(o)
	}

	mux := http.NewServeMux()
	// "GET /{$}" matches only the exact root. The plain pattern "GET /" is a
	// subtree match in Go 1.22+ ServeMux semantics: it matches every path that
	// no more specific pattern claims, which would silently turn unknown paths
	// such as /values or /config into 200s from the root handler instead of
	// 404s. "{$}" pins the match to the empty remaining path, so anything past
	// the root falls through to ServeMux's default 404.
	mux.HandleFunc("GET /{$}", func(rw http.ResponseWriter, r *http.Request) {
		if !authOK(rw, r, o) {
			return
		}
		writeJSON(rw, http.StatusOK, w.Status())
	})
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, r *http.Request) {
		serveHealthz(rw, r, w, o)
	})

	var h http.Handler = mux
	if o.prefix != "" {
		h = http.StripPrefix(o.prefix, h)
	}
	for i := len(o.middleware) - 1; i >= 0; i-- {
		h = o.middleware[i](h)
	}
	return h
}

// authOK reports whether r may proceed. When no auth is configured every
// request proceeds. When auth is configured and denies the request, authOK
// writes the response itself (401, with a Challenger's WWW-Authenticate
// header if the Authenticator implements one, or 403 for ErrForbidden) and
// returns false, so the caller can simply return.
func authOK(rw http.ResponseWriter, r *http.Request, o *handlerOptions) bool {
	if o.auth == nil {
		return true
	}
	_, err := o.auth.Authenticate(r)
	if err == nil {
		return true
	}
	if errors.Is(err, ErrForbidden) {
		writeJSON(rw, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return false
	}
	if c, ok := o.auth.(Challenger); ok {
		rw.Header().Set("WWW-Authenticate", c.Challenge())
	}
	writeJSON(rw, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	return false
}

// healthzBody is the wire shape of GET /healthz. Fields is present only for
// an authenticated caller (see serveHealthz); an unauthenticated caller, or
// one probing a deployment with no auth configured, gets Status and nothing
// else omitted by the omitempty tag below.
type healthzBody struct {
	Status string        `json:"status"`
	Fields []FieldStatus `json:"fields,omitempty"`
}

// serveHealthz answers a liveness/readiness probe. It never returns 401: a
// Kubernetes probe (or any other unauthenticated caller) always gets a bare
// status, 200 {"status":"ok"} or 503 {"status":"unhealthy"}, so liveness never
// depends on holding a credential. The failing-field list, which names paths,
// redacted refs, and error kinds, is included only when the request
// authenticates against the configured Authenticator; that is exactly the
// same check authOK makes for GET /, just never turned into a 401 here. When
// no auth is configured at all there is no credential to distinguish callers
// by, so every caller gets the full detail, matching the posture the operator
// already chose by not configuring auth.
func serveHealthz[T any](rw http.ResponseWriter, r *http.Request, w *Watcher[T], o *handlerOptions) {
	err := w.Health()
	authed := o.auth == nil
	if o.auth != nil {
		if _, aerr := o.auth.Authenticate(r); aerr == nil {
			authed = true
		}
	}

	if err == nil {
		writeJSON(rw, http.StatusOK, healthzBody{Status: "ok"})
		return
	}
	body := healthzBody{Status: "unhealthy"}
	if authed {
		var he *HealthError
		if errors.As(err, &he) {
			body.Fields = he.Fields
		}
	}
	writeJSON(rw, http.StatusServiceUnavailable, body)
}

// writeJSON encodes v as the response body with the given status code. It
// runs after WriteHeader has committed the status, so an encoding failure
// (which should never happen for the fixed shapes this file produces) cannot
// change the status code; there is nothing more useful to do with such an
// error than drop it, since the response is already underway.
func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}
