// Package secret provides wrapper types for sensitive configuration values that
// redact themselves everywhere a value is normally rendered - String, fmt,
// encoding/json, and log/slog - so a secret cannot leak through a stray log line
// or error message. Access the underlying value only through the explicit,
// greppable Reveal method.
package secret

import (
	"log/slog"
)

// Redacted is the placeholder rendered in place of a secret value.
const Redacted = "[REDACTED]"

// String is a sensitive string. Its zero value is a valid empty secret.
//
// String deliberately does NOT expose the value through String(), fmt verbs,
// JSON marshaling, or slog. Use Reveal to obtain the plaintext at the exact
// point it is needed - those call sites are easy to audit in code review.
type String struct {
	b []byte
}

// NewString wraps s as a secret.
func NewString(s string) String { return String{b: []byte(s)} }

// NewStringBytes wraps raw bytes as a secret string, taking ownership of b.
func NewStringBytes(b []byte) String { return String{b: b} }

// Reveal returns the plaintext value. This is the only way to read it; keep such
// call sites minimal and reviewable.
func (s String) Reveal() string { return string(s.b) }

// RevealBytes returns the underlying bytes. Callers must not mutate the result.
func (s String) RevealBytes() []byte { return s.b }

// String implements fmt.Stringer and returns the redaction placeholder.
func (s String) String() string { return Redacted }

// GoString implements fmt.GoStringer so %#v also redacts.
func (s String) GoString() string { return Redacted }

// MarshalJSON renders the redaction placeholder, never the value.
func (s String) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// LogValue implements slog.LogValuer so structured logs redact by construction.
func (s String) LogValue() slog.Value { return slog.StringValue(Redacted) }

// Sensitive always reports true.
func (s String) Sensitive() bool { return true }

// IsZero reports whether the secret holds no bytes.
func (s String) IsZero() bool { return len(s.b) == 0 }

// Zero best-effort wipes the underlying bytes. This is a defense-in-depth
// measure only: Go's garbage collector may have already copied the value
// elsewhere (during string conversion, interface boxing, or GC compaction), so
// zeroization cannot be guaranteed. It is still worth doing on rotation.
func (s *String) Zero() {
	for i := range s.b {
		s.b[i] = 0
	}
	s.b = nil
}

// Bytes is a sensitive byte slice with the same redaction contract as String.
type Bytes struct {
	b []byte
}

// NewBytes wraps b as a secret, taking ownership of the slice.
func NewBytes(b []byte) Bytes { return Bytes{b: b} }

// Reveal returns the underlying bytes. Callers must not mutate the result.
func (b Bytes) Reveal() []byte { return b.b }

// String implements fmt.Stringer and returns the redaction placeholder.
func (b Bytes) String() string { return Redacted }

// GoString implements fmt.GoStringer so %#v also redacts.
func (b Bytes) GoString() string { return Redacted }

// MarshalJSON renders the redaction placeholder, never the value.
func (b Bytes) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// LogValue implements slog.LogValuer so structured logs redact by construction.
func (b Bytes) LogValue() slog.Value { return slog.StringValue(Redacted) }

// Sensitive always reports true.
func (b Bytes) Sensitive() bool { return true }

// IsZero reports whether the secret holds no bytes.
func (b Bytes) IsZero() bool { return len(b.b) == 0 }

// Zero best-effort wipes the underlying bytes (see String.Zero for caveats).
func (b *Bytes) Zero() {
	for i := range b.b {
		b.b[i] = 0
	}
	b.b = nil
}
