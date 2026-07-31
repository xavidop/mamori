package mamori

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound is the sentinel error providers wrap (or return) when a referenced
// value does not exist. Consumers and mamori itself test for it with
// errors.Is(err, ErrNotFound); mamori applies defaults / optional handling only
// for not-found, never for other errors.
var ErrNotFound = errors.New("mamori: not found")

// ErrNoSuchSnapshot is returned by Watcher.Pin when the requested version is not
// retained. Increase WithHistory to pin older versions.
var ErrNoSuchSnapshot = errors.New("mamori: no such snapshot version")

// errWatcherClosed is sendPin's answer when a Pin/PinCurrent/Unpin call races
// (or arrives after) Close: with no reconciler goroutine left to deliver the
// command to, the caller gets this back rather than blocking forever. It is
// unexported because a caller only ever observes it wrapped into Pin's
// returned error, not as something application code is expected to test for
// with errors.Is; a closed watcher rejecting further calls is already
// unambiguous from the fact that Close was called.
var errWatcherClosed = errors.New("mamori: watcher closed")

// ErrReentrantCall reports a control-channel command issued from the goroutine
// that is currently running one of the watcher's own callbacks.
//
// Two callbacks run ON the reconciler goroutine, inline: a PreApply hook, and an
// OnError callback (OnChange does not - it is delivered from the dispatch queue,
// on its own goroutine, and is unaffected by any of this). Pin, PinCurrent,
// Unpin and Refresh are commands SERVICED by the reconciler goroutine. Calling
// one from inside either callback asks that goroutine to answer a message it
// cannot reach until the callback it is running returns: before this was
// detected, it blocked until Close, with no reconciliation, no OnChange and no
// further OnError in the meantime. This sentinel converts that permanent, silent
// wedge into an immediate, diagnosable refusal that leaves pin state, and the
// configuration, untouched.
//
// Three of the four commands take no context at all, so they had no way out to
// begin with. Refresh does take one, and is refused just the same: a context
// makes the wedge escapable, not absent, and the obvious call to write inside a
// callback is Refresh(context.Background()), which has no deadline to escape on
// either.
//
// The refusal is keyed on WHICH goroutine is inside the callback, not merely
// that one is, so a command issued from any other goroutine - including one that
// happens to overlap a running callback - queues on the control channel and is
// serviced normally, exactly as it always was.
//
// Unlike errWatcherClosed it is exported, because the two errors sit at
// opposite ends of what a caller can do about them: a closed watcher is an
// expected lifecycle race with Close and needs no test, while this one is a
// programming mistake in the caller's own callback, and a test that wants to
// prove the mistake is caught (or a wrapper that wants to translate it) needs
// errors.Is to reach it.
//
// Pin and Refresh return it directly. PinCurrent returns version 0 and Unpin
// does nothing; see their doc comments for why, and for what each leaves
// observable instead.
//
// Two kinds of addition belong here rather than beside it. A future command
// serviced by the reconciler goroutine: route it through sendPinCtx's guard
// (pin.go). A future callback this package runs inline on that goroutine: arm
// armReentrancy (preapply.go) around it, as emitErr does. Either one left out
// is a wedge that this sentinel's own wording promises does not exist.
var ErrReentrantCall = errors.New("mamori: Pin, PinCurrent, Unpin and Refresh cannot be called from the goroutine running a PreApply hook or an OnError callback, which occupies the reconciler goroutine that services them; Get is safe there, but these must be called from another goroutine")

// Kind is a coarse, provider-independent classification of a resolve failure.
// It exists so diagnostics can distinguish conditions that need human action
// (a missing secret, a denied permission) from transient ones (an unreachable
// backend, a throttled request). Providers produce it by wrapping one of the
// sentinels below; consumers read it with ErrorKind.
type Kind string

const (
	// KindNotFound represents a missing key, secret, path, or version. This is the
	// only kind that triggers a field's default: or optional: handling; the rest are
	// diagnostic only.
	KindNotFound Kind = "not_found"
	// KindPermissionDenied means the caller has authenticated successfully but is
	// not authorized to access the requested value, such as an IAM deny, Vault
	// policy, or Kubernetes RBAC denial. Distinguish this from KindUnauthenticated,
	// where the caller has not proven their identity.
	KindPermissionDenied Kind = "permission_denied"
	// KindUnauthenticated means credentials are missing, malformed, or expired, or a
	// token renewal failed. The caller has not proven who they are. Distinguish from
	// KindPermissionDenied, where the caller has authenticated but been refused.
	KindUnauthenticated Kind = "unauthenticated"
	// KindUnavailable means the backend could not be reached or did not respond,
	// due to network failure, DNS issues, timeout, a 5xx response, or an open
	// circuit breaker.
	KindUnavailable Kind = "unavailable"
	// KindRateLimited means the backend deliberately rejected this request due to
	// throttling, quota exhaustion, or similar rate control. Distinguish from
	// KindUnavailable, where the backend is reachable and healthy.
	KindRateLimited Kind = "rate_limited"
	// KindInvalid means the ref is malformed for this provider, or the returned
	// payload could not be parsed.
	KindInvalid Kind = "invalid"
	// KindUnknown is the honest answer for an error a provider cannot map. It is
	// a legal outcome, not a failure: a provider that guesses is worse than one
	// that admits it does not know.
	KindUnknown Kind = "unknown"
)

// Classification sentinels. Providers wrap the underlying SDK error with the
// matching sentinel using two %w verbs, which preserves both errors.Is for the
// sentinel and errors.As access to the original SDK error type:
//
//	return mamori.Value{}, fmt.Errorf("%w: %w", mamori.ErrPermissionDenied, err)
//
// A single %w with the SDK error formatted as %v only wraps the sentinel; the
// SDK error becomes a flattened string and errors.As can no longer reach it.
//
// Only ErrNotFound changes mamori's behavior (it is what triggers `default:` and
// `optional` handling). The rest are diagnostic.
var (
	ErrPermissionDenied = errors.New("mamori: permission denied")
	ErrUnauthenticated  = errors.New("mamori: unauthenticated")
	ErrUnavailable      = errors.New("mamori: unavailable")
	ErrRateLimited      = errors.New("mamori: rate limited")
	// ErrInvalid covers both a malformed ref and a payload that could not be
	// parsed.
	ErrInvalid = errors.New("mamori: invalid")
)

// kindSentinels pairs each Kind with its sentinel, in the order ErrorKind tests
// them. ErrNotFound is first so it wins when an error carries more than one
// sentinel, since it is the only kind that drives resolution behavior.
var kindSentinels = [...]struct {
	kind Kind
	err  error
}{
	{KindNotFound, ErrNotFound},
	{KindPermissionDenied, ErrPermissionDenied},
	{KindUnauthenticated, ErrUnauthenticated},
	{KindUnavailable, ErrUnavailable},
	{KindRateLimited, ErrRateLimited},
	{KindInvalid, ErrInvalid},
}

// ErrorKind classifies err by walking its errors.Is chain. It returns the empty
// Kind for a nil error and KindUnknown for an error carrying no sentinel.
//
// An error that lost its chain (a provider that formatted with %v rather than
// %w) reports KindUnknown. That is the failure the providertest conformance
// case exists to catch.
//
// context.DeadlineExceeded and context.Canceled are handled explicitly, after
// the sentinel checks so an explicit mamori sentinel always wins, and are
// deliberately classified asymmetrically:
//
//   - context.DeadlineExceeded reports KindUnavailable: the backend genuinely
//     did not respond in time, which is exactly what KindUnavailable denotes.
//   - context.Canceled still reports KindUnknown: the caller withdrew the
//     request, which says nothing about the backend's health. This is a
//     deliberate decision, not an oversight.
func ErrorKind(err error) Kind {
	if err == nil {
		return ""
	}
	for _, ks := range kindSentinels {
		if errors.Is(err, ks.err) {
			return ks.kind
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindUnavailable
	}
	return KindUnknown
}

// SentinelFor returns the sentinel error corresponding to k, or nil for
// KindUnknown and the empty Kind, neither of which has one.
//
// It is the inverse of ErrorKind, and exists so a classification that arrived
// as a value rather than an error (over the wire, from a config file) can be
// turned back into an error that errors.Is matches.
//
// A nil return never means the operation succeeded. It means no sentinel
// corresponds to k: either k is KindUnknown (a real failure whose cause could
// not be classified) or k is some string that does not name a recognized
// Kind at all, such as one from an untrusted or forward-versioned wire value.
// Callers reconstructing an error from such a value must treat a nil return as
// an unclassified failure, never as absence of an error. Do not write
// `if err := SentinelFor(k); err != nil { ... }` as a substitute for checking
// whether the original operation failed.
func SentinelFor(k Kind) error {
	for _, ks := range kindSentinels {
		if ks.kind == k {
			return ks.err
		}
	}
	return nil
}

// ProviderError wraps an error from a specific provider resolve, tagging it with
// the scheme and ref for diagnostics and metrics. It is delivered to OnError for
// runtime resolve failures.
type ProviderError struct {
	Scheme string
	Ref    string
	Err    error
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("mamori: provider %q resolving %q: %v", e.Scheme, e.Ref, e.Err)
}

// Unwrap allows errors.Is/As to reach the underlying error (e.g. ErrNotFound).
func (e *ProviderError) Unwrap() error { return e.Err }

// ValidationError wraps a validation failure. When an updated snapshot fails
// validation the update is rejected atomically and this error is delivered to
// OnError; Get continues to return the last valid config.
type ValidationError struct {
	Err error
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("mamori: validation failed: %v", e.Err)
}

func (e *ValidationError) Unwrap() error { return e.Err }

// DeriveError is delivered to OnError when a WithDerive hook returns an error,
// and returned by Watch and Load when the failure happens on the initial
// resolve. The update is rejected atomically, exactly as a validation failure
// is, and Get continues to return the last valid config.
//
// Rejecting rather than continuing is deliberate: a configuration whose derived
// fields were not built is not one anyone should serve, and half-applying it
// would produce a snapshot where some fields reflect a rotated credential and a
// value derived from them still reflects the old one.
type DeriveError struct {
	Err error
}

func (e *DeriveError) Error() string {
	return fmt.Sprintf("mamori: derive failed: %v", e.Err)
}

func (e *DeriveError) Unwrap() error { return e.Err }

// StaleError is returned/delivered when a value has exceeded the configured
// WithStale max age without a successful refresh.
type StaleError struct {
	Ref string
	Err error
}

func (e *StaleError) Error() string {
	return fmt.Sprintf("mamori: value %q is stale: %v", e.Ref, e.Err)
}

func (e *StaleError) Unwrap() error { return e.Err }

// HealthError is returned by Watcher.Health when one or more fields are
// unhealthy. It names the offending fields so a readiness probe can log which
// config is broken rather than a bare "unhealthy".
type HealthError struct {
	Fields []FieldStatus
}

func (e *HealthError) Error() string {
	names := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		names[i] = f.Path
	}
	return fmt.Sprintf("mamori: %d unhealthy field(s): %s", len(e.Fields), strings.Join(names, ", "))
}
