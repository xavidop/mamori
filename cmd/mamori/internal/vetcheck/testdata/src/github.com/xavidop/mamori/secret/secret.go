// Package secret is a minimal stub of github.com/xavidop/mamori/secret used only
// to compile the analysistest fixtures. It defines String and Bytes as named
// struct types so the analyzer sees them as distinct from plain string/[]byte.
package secret

// String is a sensitive string wrapper.
type String struct {
	b []byte
}

// NewString wraps s as a secret.
func NewString(s string) String { return String{b: []byte(s)} }

// Reveal returns the plaintext value.
func (s String) Reveal() string { return string(s.b) }

// Bytes is a sensitive byte-slice wrapper.
type Bytes struct {
	b []byte
}

// NewBytes wraps b as a secret.
func NewBytes(b []byte) Bytes { return Bytes{b: b} }

// Reveal returns the underlying bytes.
func (b Bytes) Reveal() []byte { return b.b }
