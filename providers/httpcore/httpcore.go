// Package httpcore is the shared HTTP resolve core for mamori providers whose
// backend is a REST API.
//
// Sixteen of mamori's providers speak HTTP, and before this package each one
// hand-rolled request building, credential injection, status classification,
// and response body hygiene. That duplication is what issue #107 surfaced as
// inconsistent body draining. This package exists so a provider author writes
// the part that is actually specific to their backend and inherits the rest.
//
// # What it does not do
//
// It does not retry. mamori's reconciler already backs off and retries a failed
// resolve (see backoff.go in the core module), and a second retry layer inside
// the provider would multiply against it, turning a configured five attempts
// into twenty-five.
//
// It does not parse vendor error envelopes. [ClassifyStatus] takes the detail
// string from its caller because a response body can contain the resolved value
// itself, and only the provider knows which field of its backend's error shape
// is safe to surface.
//
// # Units
//
//   - [Client] performs one round trip with a bounded, always-drained body.
//   - [Authenticator] injects credentials.
//   - [ClassifyStatus] maps an HTTP status onto a mamori error sentinel.
//   - [Revalidator] turns a repeated poll into a conditional GET.
package httpcore
