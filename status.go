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
// HTTP: Ref has sensitive query options redacted, and no field value appears -
// including for a Derived entry, which carries no value at all.
type FieldStatus struct {
	Path      string        // dotted field path, e.g. "Redis.Password"
	Scheme    string        // provider scheme of the ref
	Ref       string        // the ref, with sensitive query options redacted
	Version   string        // provider version of the currently observed value; a content hash when Derived is true
	LastOK    time.Time     // last successful resolve, zero if never
	Age       time.Duration // GeneratedAt minus LastOK, recomputed at read time
	Stale     bool          // Age exceeds the configured WithStale threshold
	LastError string        // text of the last resolve error, empty if none
	LastKind  Kind          // classification of LastError, empty if none
	Sensitive bool          // field is a secret.String or secret.Bytes

	// Derived marks a row emitted for a WithDerive-declared write path in its
	// own right. It has no ref, so Scheme, Ref, LastOK, Age, and Stale are all
	// zero for it. It is reported so an operator can see the field exists and
	// is maintained, which is the whole reason a caller declares it.
	//
	// Not every declared write path produces one. A path that also names a
	// source-tagged field keeps that field's own row, which carries a real
	// Scheme and Ref and reports Derived false, rather than gaining a second
	// row here; and a path naming no readable field on T produces nothing at
	// all. See buildReport (report.go) for both gates.
	//
	// A Derived entry appears in both Watcher.Status and a Doctor report.
	// Version is a content hash of the computed value (see derivedVersion,
	// derivedversion.go) when that value was actually computed, and it hashes
	// the real content wherever it sits: a secret nested inside the derived
	// value is revealed for hashing rather than redacted, and a pointer is
	// hashed by what it points at. In Watcher.Status Version is always
	// populated: a failing hook rejects the whole candidate in buildCandidate,
	// so a published config never contains one.
	//
	// In a Doctor report (see doctorDerivedFields, doctor.go) Version can be
	// blank instead, in three different cases:
	//
	//   - The hook ran and returned an error: LastKind is KindInvalid,
	//     LastError is the hook's own error, and the report is unhealthy.
	//   - A sourced field produced no value for the hooks to read, so they
	//     never ran: the row carries a LastError saying it was not evaluated
	//     and no LastKind. This is not the same as the report being unhealthy
	//     - a field failing with a self-healing kind (unavailable, rate
	//     limited, unknown) leaves the report healthy and still has no value
	//     to feed a hook.
	//   - The hooks could not be typed to T at all, because one was written
	//     for a different config or declares an empty write path. Load and
	//     Watch reject those Options outright with ErrInvalid, so Doctor
	//     reports one KindInvalid row per declared write path rather than
	//     passing a config that cannot start as healthy.
	Derived bool
}

// Report is a point-in-time snapshot of a Watcher's health, or the result of a
// one-shot Doctor probe. Fields are in struct declaration order, except for any
// declared WithDerive write paths (FieldStatus.Derived): a derived field has no
// fieldSpec and therefore no spot in that order, so its entries are appended
// after every sourced field instead, in WithDerive registration order.
//
// Both Watcher.Status and Doctor append their derived entries this same way,
// after every sourced field, in WithDerive registration order, gated through
// the same hasSpecPath / fieldByPath checks (report.go, doctor.go), so the
// two can never disagree about which paths produce a row. Doctor makes one
// exception, for hooks it cannot type to its T at all: those Options fail
// Load and Watch outright, so no Watcher.Status report exists to disagree
// with, and Doctor reports every declared write path as invalid rather than
// applying gates it has no typed config to evaluate.
type Report struct {
	Fields      []FieldStatus
	Snapshot    uint64    // version of the snapshot Get currently returns (the pinned version, while Pinned)
	Live        uint64    // newest validated snapshot; diverges from Snapshot while Pinned
	Pinned      bool      // true when Get is frozen at Snapshot while Live keeps advancing; see Watcher.Pin
	Healthy     bool      // no field is stale or carries a terminal error kind
	GeneratedAt time.Time // when this report was built

	// Source names where the configuration being served came from. It is
	// SourceBackend while WithBootstrapCache is configured and every value came
	// from its provider, and SourceBootstrapCache while the cache is covering for
	// a backend this process has not reached since it started. It is empty for a
	// process that does not use the option at all, exactly as Bootstrap is nil
	// for one.
	//
	// It is reported from the first second rather than only once the situation
	// turns unhealthy, because "we booted off disk" is the single fact an
	// operator most needs during an outage and it is invisible otherwise: a
	// process serving a restored config looks identical to a healthy one.
	//
	// The omitempty is a compatibility promise, not a formatting preference. A
	// Report is served over HTTP to a separately released CLI, and the mamori
	// 1.9 CLI validates the admin body by its exact top-level key set: any key
	// it does not know makes it refuse the response as "not a Report". Emitting
	// these two unconditionally would therefore have broken `mamori doctor` for
	// every process that merely linked a newer mamori, cache or no cache. Kept
	// off the wire unless the option is configured, the payload of a process not
	// using the cache stays byte-for-byte what that CLI expects. A process that
	// does use it is a break that CLI cannot avoid, and one its operator opted
	// into.
	Source ConfigSource `json:",omitempty"`

	// Bootstrap describes the WithBootstrapCache snapshot, and is nil when the
	// option is not configured. See BootstrapStatus, and see Source above for
	// why it is omitted from the JSON rather than serialised as null.
	Bootstrap *BootstrapStatus `json:",omitempty"`
}

// ConfigSource names where the configuration a Report describes came from.
type ConfigSource string

const (
	// SourceBackend means every value was resolved from its provider. This is
	// the ordinary case for a process using WithBootstrapCache; a process not
	// using it reports no source at all (see Report.Source).
	SourceBackend ConfigSource = "backend"
	// SourceBootstrapCache means the cold-start resolve failed with a transient
	// kind and the configuration came off disk instead. It reverts to
	// SourceBackend once every field has been resolved live at least once since
	// the process started, because at that point nothing being served is still
	// coming from the snapshot.
	SourceBootstrapCache ConfigSource = "bootstrap_cache"
)

// BootstrapStatus is the state of the WithBootstrapCache snapshot, as reported
// by Watcher.Status and by Doctor. It carries no configuration value and no
// snapshot bytes: a snapshot holds live credentials, so only its existence, its
// age, and whether it still fits this build's config struct leave the process.
//
// The file's path is deliberately absent. A Report is designed to be served over
// HTTP, and telling an unauthenticated reader exactly where the encrypted
// credential file lives buys them a step for nothing in return.
type BootstrapStatus struct {
	// Present is true when a snapshot exists and opened. In a running watcher
	// that means this process wrote or restored one; in a Doctor report it means
	// the file was found on disk and decrypted.
	Present bool

	// Restored is true when this process booted from the snapshot. It is a fact
	// about the boot and never changes, which is what makes it the field a
	// post-mortem wants: an incident an hour later still shows that this pod
	// started during the outage.
	//
	// It is not what Health's max-age rule keys on. Source is, because a process
	// that restored and has since reconciled every field against its backend is
	// no longer at risk of serving an unnoticed rotation, and bounding the age of
	// a snapshot it has stopped depending on would take a healthy pod out. It is
	// always false in a Doctor report, which starts no watcher.
	Restored bool

	// WrittenAt is when the snapshot was written, and Age is how long ago that
	// was, recomputed at read time so a watcher that has gone quiet does not
	// keep reporting the age it had at the last reconcile.
	//
	// The snapshot is rewritten on every applied update, so its age is the time
	// since the configuration last CHANGED, not the time since it was last
	// confirmed against the backend. A configuration that genuinely never
	// changes therefore ages without bound; see BootstrapMaxAge, which is meant
	// to be set to the rotation window of the shortest-lived credential in it.
	WrittenAt time.Time
	Age       time.Duration

	// FingerprintMatch reports whether the snapshot was written for the same
	// config struct this build declares. A false here means the snapshot cannot
	// be restored at all, whatever its age. In a running watcher it is always
	// true when Present, since this process either wrote the file or checked it
	// before restoring; it earns its place in a Doctor report, which is where a
	// schema drift is worth learning about before the outage rather than during
	// one.
	FingerprintMatch bool

	// Problem explains why the snapshot is unusable, and is empty when it is
	// not. It names the failure only: a wrong key, an altered file, a drifted
	// schema. It never contains any part of the snapshot.
	Problem string
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

// redactRef is the package-internal spelling of Ref.Redacted, kept because most
// of the package reads better calling it as a function on a ref it already has.
func redactRef(ref Ref) string { return ref.Redacted() }

// Redacted renders the ref as a string with any sensitive query-option value
// replaced by secret.Redacted, preserving the scheme, path, key, and
// non-sensitive options so it stays useful for diagnostics.
//
// Use this, never Raw, anywhere a ref leaves the process: a log line, an audit
// record, a span attribute, an error message, an HTTP body. Some providers
// accept an inline credential as a query option, so Raw can carry a live secret
// (see sensitiveOptKeys for the names covered). Everything mamori itself
// publishes already goes through this; it is exported so a Meter, Tracer, or
// middleware outside this package can hold the same line.
func (r Ref) Redacted() string {
	ref := r
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
