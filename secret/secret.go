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

// Clone returns a copy backed by its own bytes, so the caller can Zero it
// without touching any other copy.
//
// This is the safe way to wipe a secret that came from Watcher.Get: that
// returns your config by value, which shares the secret's backing array with
// the reconciler and with every other caller. Clone breaks the sharing, and
// only then is Zero yours to call.
//
// A cloned nil or empty secret stays empty rather than allocating.
func (s String) Clone() String {
	if len(s.b) == 0 {
		return String{}
	}
	return String{b: append([]byte(nil), s.b...)}
}

// Zero best-effort wipes the underlying bytes. This is a defense-in-depth
// measure only: Go's garbage collector may have already copied the value
// elsewhere (during string conversion, interface boxing, or GC compaction), so
// zeroization cannot be guaranteed.
//
// Only call this on a secret whose bytes you own. A String is a struct holding
// a slice, so copying one copies the slice header and shares the backing
// array: every copy reads through to the same bytes, and zeroing any of them
// zeroes all of them.
//
// That matters because Watcher.Get returns your config by value, which copies
// the struct without copying the secret's bytes. Zeroing a secret obtained
// that way does not wipe "your" copy, it wipes the live one the reconciler is
// still serving, and every other caller's too. A request in flight would
// authenticate with null bytes.
//
// Use Clone to take ownership first when you need to wipe:
//
//	pw := cfg.DBPassword.Clone()
//	defer pw.Zero()
//	db.Connect(pw.Reveal())
//
// mamori itself never calls Zero. It cannot know when the last caller has
// finished with a superseded value, so wiping one on rotation would be a use
// after free with extra steps.
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

// Clone returns a copy backed by its own bytes, so the caller can Zero it
// without touching any other copy. See String.Clone.
func (b Bytes) Clone() Bytes {
	if len(b.b) == 0 {
		return Bytes{}
	}
	return Bytes{b: append([]byte(nil), b.b...)}
}

// Zero best-effort wipes the underlying bytes. Only call it on a secret whose
// bytes you own: copies share one backing array, so zeroing any copy zeroes
// every copy, including the live one the reconciler is serving. Use Clone to
// take ownership first. See String.Zero for the full rationale.
func (b *Bytes) Zero() {
	for i := range b.b {
		b.b[i] = 0
	}
	b.b = nil
}
