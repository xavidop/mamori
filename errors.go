package mamori

import (
	"errors"
	"fmt"
)

// ErrNotFound is the sentinel error providers wrap (or return) when a referenced
// value does not exist. Consumers and mamori itself test for it with
// errors.Is(err, ErrNotFound); mamori applies defaults / optional handling only
// for not-found, never for other errors.
var ErrNotFound = errors.New("mamori: not found")

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
