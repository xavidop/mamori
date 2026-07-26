package mamoriprov

import (
	"context"
	"fmt"

	"github.com/xavidop/mamori"
)

// authoritativeKinds lists the failure classifications that every replica of
// the same config server answers identically, and which therefore must NOT
// move a request on to the next endpoint. It is a flat literal rather than a
// switch, for the same reason wireKindSentinel is: the failover policy is the
// kind of thing an operator debugging a fleet reads once and needs to be able
// to check at a glance.
//
//   - not_found, permission_denied, unauthenticated: the binding, the policy
//     that guards it and the credential presented for it are all server-side
//     configuration shared by the whole replica set. Failing over would turn
//     one clean 403 into N pointless requests, one per replica, and still end
//     in a 403.
//   - invalid: either the server rejected the request as malformed, or this
//     client could not parse the response. Neither gets better by asking a
//     replica running the same code the same question.
//   - rate_limited: a deliberate load-shedding signal, not a broken replica.
//     Replaying the request against every other replica is exactly the
//     amplification the throttle exists to prevent; waiting is the caller's
//     job, not the failover list's.
//
// Everything else fails over, deliberately including mamori.KindUnknown: an
// unclassified failure (a bare 500, a refused dial, a TLS handshake failure,
// a truncated body) is evidence that THIS replica is broken, which says
// nothing at all about the next one. Guessing "authoritative" for an error
// nobody could classify would turn one sick replica into a hard outage, which
// is precisely the failure mode running N replicas is meant to prevent.
var authoritativeKinds = map[mamori.Kind]bool{
	mamori.KindNotFound:         true,
	mamori.KindPermissionDenied: true,
	mamori.KindUnauthenticated:  true,
	mamori.KindInvalid:          true,
	mamori.KindRateLimited:      true,
}

// shouldFailover reports whether err, as produced by one endpoint, is worth
// retrying against the next endpoint in the list.
//
// It classifies with mamori.ErrorKind rather than by inspecting HTTP statuses
// or net error types, which means it works uniformly on all three sources of
// failure a single attempt can produce: a transport error from the client (no
// sentinel, so mamori.KindUnknown - fail over), a classified error rebuilt
// from the server's wire kind (see sentinelForKind), and a context deadline
// (mamori.ErrorKind reports KindUnavailable for it - fail over, another
// replica may well be faster).
func shouldFailover(err error) bool {
	if err == nil {
		return false
	}
	return !authoritativeKinds[mamori.ErrorKind(err)]
}

// tryEndpoints runs attempt against each of p's endpoints in configured
// order, returning the first success, and otherwise the error that ended the
// walk.
//
// The walk stops early, returning that endpoint's error verbatim, when the
// failure was authoritative (see shouldFailover) or when ctx is already done.
// If every endpoint failed in a way worth moving on from, the LAST error is
// returned - not the first, and not an errors.Join of all of them. Last,
// because it is the freshest evidence of what the fleet is doing right now;
// verbatim, because a caller's errors.Is(err, mamori.ErrUnavailable) has to
// keep holding after failover exactly as it did before it existed.
//
// With a single endpoint configured the loop body runs exactly once and
// returns exactly what attempt returned, so a one-replica deployment behaves
// as it always has: one request, no retries, no added latency.
//
// It is a package-level function rather than a method because Resolve and
// ResolveBatch return different types and Go does not allow a method to
// introduce its own type parameter.
func tryEndpoints[T any](ctx context.Context, p *Provider, attempt func(context.Context, endpoint) (T, error)) (T, error) {
	var zero T

	if p.endpointErr != nil {
		return zero, p.endpointErr
	}
	if len(p.endpoints) == 0 {
		// Unreachable by construction: newEndpoints returns an error (recorded
		// as endpointErr, and returned just above) rather than an empty list.
		// Guarded anyway because the alternative is falling straight out of
		// the loop below and returning a zero value with a nil error, which a
		// caller would read as a successfully resolved empty value.
		return zero, fmt.Errorf("%w: no mamori endpoint configured", mamori.ErrInvalid)
	}

	var lastErr error
	for _, ep := range p.endpoints {
		result, err := attempt(ctx, ep)
		if err == nil {
			return result, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			// The caller withdrew the request or ran out of time. No replica
			// can help with that, and walking the rest of the list would only
			// burn the remaining endpoints on requests that are already dead.
			return zero, err
		}
		if !shouldFailover(err) {
			return zero, err
		}
	}
	return zero, lastErr
}
