package mamori

import (
	"log/slog"
)

// WithLogger installs a structured logger for engine events: resolve failures
// and recoveries, watch errors, rejected candidates, applied changes, stale
// values, and dropped change events.
//
// mamori logs nothing by default. A library that writes to the application's
// stderr merely because it was linked in has taken a decision that belongs to
// the application, so the zero configuration is a discard logger and
// WithLogger(slog.Default()) is the one-line opt-in. Passing nil resets to
// silent rather than panicking, so a caller wiring this up conditionally can
// pass nil for the off case.
//
// Two things worth knowing before choosing a handler.
//
// Records never contain a resolved value. They carry the field path, the
// scheme, the ref with inline credentials redacted, the version, and the
// error. That is deliberate and tested: a config log is exactly the artifact
// most likely to be shipped off the host, so a secret must never reach it.
//
// The handler is called from the reconciler goroutine, so a handler that
// blocks blocks reconciliation, the same constraint OnError carries. A handler
// writing synchronously to a remote collector will stall the engine; buffer it.
//
// WithLogger and OnError are independent and may both be set. An error reaches
// both, and neither suppresses the other.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l == nil {
			l = slog.New(slog.DiscardHandler)
		}
		o.logger = l
	}
}

// log returns the configured logger, never nil.
//
// Call sites go through this rather than touching o.logger directly, so a
// future change to the default (or to how a nil is handled) has one place to
// live.
func (o *options) log() *slog.Logger {
	if o.logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return o.logger
}

// Attribute keys, fixed and shared across every call site. A consistent
// vocabulary is what makes structured output queryable, and is the whole
// reason to emit records rather than formatted strings, so these are constants
// rather than string literals repeated at each site.
const (
	logAttrField   = "field"   // dotted struct path, e.g. "Redis.Password"
	logAttrScheme  = "scheme"  // provider scheme, e.g. "aws-sm"
	logAttrRef     = "ref"     // the ref, with sensitive query options redacted
	logAttrVersion = "version" // provider version of the value
	logAttrKind    = "kind"    // the mamori.Kind classification of an error
	logAttrErr     = "err"     // the error text
	logAttrCount   = "count"   // how many items an event covers
)

// errAttrs renders an error as its text plus its classification, so an
// operator can filter on kind without parsing messages.
func errAttrs(err error) []any {
	if err == nil {
		return nil
	}
	return []any{logAttrErr, err.Error(), logAttrKind, string(ErrorKind(err))}
}
