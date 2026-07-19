package mamori

import "time"

// Value is what a Provider returns for a resolved Ref. It carries the raw bytes
// plus the metadata mamori needs for change detection, redaction, and
// lease-aware refresh.
type Value struct {
	// Bytes is the raw resolved payload.
	Bytes []byte
	// Version is a provider-supplied revision identifier (Secrets Manager
	// VersionId, Vault version, a file mtime+size hash, ...). It enables cheap
	// change detection: mamori treats a changed Version as a changed value
	// without comparing bytes. If empty, mamori falls back to byte comparison.
	Version string
	// Sensitive marks the value as secret, driving redaction downstream. It is
	// set by secret-bearing providers and by the decode layer for
	// secret.String / secret.Bytes fields.
	Sensitive bool
	// NotAfter, when non-zero, is the time at which this value is known to expire
	// (e.g. a Vault lease). mamori schedules a refresh before this instant rather
	// than waiting for the next poll tick.
	NotAfter time.Time
	// Metadata carries optional provider-specific annotations. It must never
	// contain the secret payload.
	Metadata map[string]string
}

// changed reports whether o represents a different value than v, using Version
// when both sides supply one and falling back to a byte comparison otherwise.
func (v Value) changed(o Value) bool {
	if v.Version != "" || o.Version != "" {
		return v.Version != o.Version
	}
	return string(v.Bytes) != string(o.Bytes)
}
