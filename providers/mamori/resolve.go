package mamoriprov

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/xavidop/mamori"
)

// errBodyLimit bounds how much of a non-200 response body Resolve reads, so a
// hostile or broken server returning an unbounded error body cannot make a
// client stream indefinitely.
const errBodyLimit = 8 * 1024

// valueBody is the wire shape of GET /v1/values/{name}'s success body (and,
// later, one element of a batch/SSE response). It mirrors server/wire.go's
// valueBody field-for-field: Bytes is a []byte so encoding/json base64-decodes
// it automatically, NotAfter is a *time.Time so an absent "not_after" decodes
// to nil rather than the zero time, and Kind carries the same round-trippable
// mamori.Kind string as errorDetail.Kind, but here it means "this success is
// serving a last-known-good value while the upstream is currently failing"
// (stale-but-serving) rather than a failure.
type valueBody struct {
	Name      string            `json:"name"`
	Bytes     []byte            `json:"bytes,omitempty"`
	Version   string            `json:"version,omitempty"`
	Sensitive bool              `json:"sensitive,omitempty"`
	NotAfter  *time.Time        `json:"not_after,omitempty"`
	Metadata  map[string]string `json:"metadata"`
	Kind      string            `json:"kind,omitempty"`
	Error     *errorDetail      `json:"error,omitempty"`

	// ResolvedAt is when the replica that answered last successfully fetched
	// THESE bytes from upstream, and Stale reports that it is serving them as
	// last-known-good because upstream is currently failing. Both are the
	// freshness half of the server's HA support and both are optional on the
	// wire: a server that sends neither is a perfectly valid server, and every
	// path below must keep working unchanged against one.
	//
	// ResolvedAt is a *time.Time for the same reason NotAfter is, but the
	// distinction matters more here: it is the only way to tell "this server
	// never sends resolved_at" (nil) from "this value was resolved at the zero
	// time", and Watch's freshness guard keys its entire behavior off exactly
	// that difference - see freshnessGuard in freshness.go.
	//
	// Only the watch path acts on either field. Resolve and ResolveBatch decode
	// them and drop them, the same way they drop Kind: mamori.Value has nowhere
	// to carry them, and a single request has no earlier answer to be out of
	// order with anyway.
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	Stale      bool       `json:"stale,omitempty"`
}

// errorDetail is the wire shape of one error: Kind is a mamori.Kind's string
// form, Message is a human-readable string for logs and debugging. It mirrors
// server/wire.go's errorDetail exactly.
type errorDetail struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// errorEnvelope is the top-level body of a request that fails outright, e.g.
// GET /v1/values/{name}'s failure body. It matches the wire spec's
// `{"error":{"kind":"...","message":"..."}}` exactly, mirroring
// server/wire.go's errorResponse.
type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

// wireKindSentinel maps a wire error kind to the sentinel a client
// reconstructs from it, mirroring the server's own kindStatus table (see
// server/wire.go). It is a flat, greppable literal rather than a switch so
// the classification the client reproduces from the server is easy to audit
// against server/wire.go's kindStatus side by side. mamori.KindUnknown and
// any kind this table does not recognize are deliberately absent: looking
// those up returns nil, which sentinelForKind treats as "no sentinel, report
// a bare classified error" rather than guessing.
var wireKindSentinel = map[mamori.Kind]error{
	mamori.KindNotFound:         mamori.ErrNotFound,
	mamori.KindPermissionDenied: mamori.ErrPermissionDenied,
	mamori.KindUnauthenticated:  mamori.ErrUnauthenticated,
	mamori.KindUnavailable:      mamori.ErrUnavailable,
	mamori.KindRateLimited:      mamori.ErrRateLimited,
	mamori.KindInvalid:          mamori.ErrInvalid,
}

// sentinelForKind returns the sentinel matching a wire error kind, or nil for
// mamori.KindUnknown or a kind this table does not recognize (an unversioned
// wire value from a server newer than this client). A nil return tells the
// caller to report a bare, unclassified error rather than fabricate a
// sentinel that was never actually reported.
func sentinelForKind(kind string) error { return wireKindSentinel[mamori.Kind(kind)] }

// Resolve implements mamori.Provider by issuing GET /v1/values/{name}, where
// name is ref.Path: the mamori:// ref's path is always a binding name
// registered with the upstream mamori config server, never an upstream ref
// itself (the client-side half of decision D9). The server resolves the
// binding to its own upstream provider and returns only the resulting value,
// so Resolve cannot tell (and does not need to know) what kind of provider
// backs the binding on the other end.
//
// With several replicas configured (Config.Endpoints), the request is tried
// against each in turn until one answers or one answers authoritatively; see
// tryEndpoints and shouldFailover. With a single endpoint the walk is one
// attempt and nothing else changes.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	name := ref.Path
	if name == "" {
		return mamori.Value{}, fmt.Errorf("%w: mamori:// ref %q has no binding name", mamori.ErrInvalid, ref.Raw)
	}

	return tryEndpoints(ctx, p, func(ctx context.Context, ep endpoint) (mamori.Value, error) {
		return p.resolveOnce(ctx, ep, name)
	})
}

// resolveOnce is one GET /v1/values/{name} against one endpoint: the whole of
// Resolve's wire handling, minus the choice of which replica to ask.
//
// It is split out so tryEndpoints can call it once per endpoint, and so the
// error it returns is the fully classified, sentinel-carrying error that
// shouldFailover needs in order to tell "this replica is broken" apart from
// "this is the answer, and every replica would give it".
func (p *Provider) resolveOnce(ctx context.Context, ep endpoint, name string) (mamori.Value, error) {
	resp, err := p.do(ctx, ep, http.MethodGet, "/v1/values/"+url.PathEscape(name), nil)
	if err != nil {
		return mamori.Value{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		var vb valueBody
		if err := json.NewDecoder(resp.Body).Decode(&vb); err != nil {
			return mamori.Value{}, fmt.Errorf("%w: decoding mamori value response for %q: %s", mamori.ErrInvalid, name, err)
		}

		var notAfter time.Time
		if vb.NotAfter != nil {
			notAfter = *vb.NotAfter
		}

		// vb.Kind non-empty here means the server is serving a last-known-good
		// value while the binding's upstream is currently failing
		// (stale-but-serving). That is an annotation on a success, never a
		// failure by itself: the value is still usable, so it is returned with
		// a nil error and the stale kind is deliberately dropped rather than
		// surfaced (mamori.Value has no field for it).
		return mamori.Value{
			Bytes:     vb.Bytes,
			Version:   vb.Version,
			Sensitive: vb.Sensitive,
			NotAfter:  notAfter,
			Metadata:  vb.Metadata,
		}, nil
	}

	limited := io.LimitReader(resp.Body, errBodyLimit)
	var env errorEnvelope
	if err := json.NewDecoder(limited).Decode(&env); err != nil {
		if resp.StatusCode == http.StatusNotFound {
			return mamori.Value{}, fmt.Errorf("%w: mamori server returned 404 for %q with an undecodable body", mamori.ErrNotFound, name)
		}
		return mamori.Value{}, fmt.Errorf("mamori: server returned status %d for %q with an undecodable body: %s", resp.StatusCode, name, err)
	}

	if sentinel := sentinelForKind(env.Error.Kind); sentinel != nil {
		return mamori.Value{}, fmt.Errorf("%w: %s", sentinel, env.Error.Message)
	}

	// The kind is "unknown" or unrecognized: report a bare, unclassified error
	// (mamori.ErrorKind reports mamori.KindUnknown for it) rather than guess a
	// sentinel the server never actually reported.
	return mamori.Value{}, fmt.Errorf("mamori: server returned status %d kind %q for %q: %s", resp.StatusCode, env.Error.Kind, name, env.Error.Message)
}
