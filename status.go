package mamori

import (
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/xavidop/mamori/secret"
)

// FieldStatus is the live state of one configured field, as reported by
// Watcher.Status and Doctor. It is safe to serialize and safe to serve over
// HTTP: Ref has sensitive query options redacted, and no field value appears.
type FieldStatus struct {
	Path      string        // dotted field path, e.g. "Redis.Password"
	Scheme    string        // provider scheme of the ref
	Ref       string        // the ref, with sensitive query options redacted
	Version   string        // provider version of the currently observed value
	LastOK    time.Time     // last successful resolve, zero if never
	Age       time.Duration // GeneratedAt minus LastOK, recomputed at read time
	Stale     bool          // Age exceeds the configured WithStale threshold
	LastError string        // text of the last resolve error, empty if none
	LastKind  Kind          // classification of LastError, empty if none
	Sensitive bool          // field is a secret.String or secret.Bytes
}

// Report is a point-in-time snapshot of a Watcher's health, or the result of a
// one-shot Doctor probe. Fields are in struct declaration order.
type Report struct {
	Fields      []FieldStatus
	Snapshot    uint64    // version of the snapshot Get currently returns (the pinned version, while Pinned)
	Live        uint64    // newest validated snapshot; diverges from Snapshot while Pinned
	Pinned      bool      // true when Get is frozen at Snapshot while Live keeps advancing; see Watcher.Pin
	Healthy     bool      // no field is stale or carries a terminal error kind
	GeneratedAt time.Time // when this report was built
}

// sensitiveOptKeys are query-option names whose values are redacted from a Ref
// before it appears in a Report. Refs are not generally secret, but some
// providers accept an inline credential as an option, and a Report is designed
// to be safe to serve over HTTP.
var sensitiveOptKeys = map[string]struct{}{
	"token": {}, "password": {}, "secret": {}, "key": {},
	"apikey": {}, "api_key": {}, "sas": {}, "credential": {},
	"client_secret": {}, "secret_access_key": {}, "access_key": {},
	"private_key": {}, "secret_key": {}, "pwd": {}, "passwd": {},
}

// redactRef renders ref as a string with any sensitive query-option value
// replaced by secret.Redacted. The scheme, path, key, and non-sensitive options
// are preserved so the ref stays useful for diagnostics.
func redactRef(ref Ref) string {
	if len(ref.Opts) == 0 {
		if ref.Raw != "" {
			return ref.Raw
		}
		return ref.String()
	}
	// Build the query manually so the redaction placeholder is emitted literally
	// for sensitive keys while every other value is percent-encoded normally.
	// Round-tripping the placeholder through url.Values.Encode would percent-
	// encode its brackets, and undoing that with a blind string replace could
	// corrupt an unrelated value that happens to contain the same token.
	keys := make([]string, 0, len(ref.Opts))
	for k := range ref.Opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		_, sensitive := sensitiveOptKeys[strings.ToLower(k)]
		for _, v := range ref.Opts[k] {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(url.QueryEscape(k))
			b.WriteByte('=')
			if sensitive {
				b.WriteString(secret.Redacted)
			} else {
				b.WriteString(url.QueryEscape(v))
			}
		}
	}

	var out strings.Builder
	out.WriteString(ref.Scheme)
	out.WriteString("://")
	out.WriteString(ref.Path)
	if ref.Key != "" {
		out.WriteByte('#')
		out.WriteString(ref.Key)
	}
	if b.Len() > 0 {
		out.WriteByte('?')
		out.WriteString(b.String())
	}
	return out.String()
}
