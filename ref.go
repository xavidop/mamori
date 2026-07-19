package mamori

import (
	"fmt"
	"net/url"
	"strings"
)

// Ref is a parsed reference to a value in a provider. It is produced from the
// `source` struct tag by [ParseRef]. The general grammar is:
//
//	<scheme>://<path>[#<key>][?<opt>=<v>&...]
//
// Opaque schemes such as env: and exec: take everything after the colon as the
// Path (no "//" authority section):
//
//	env:LOG_LEVEL
//	exec:echo hello
type Ref struct {
	// Scheme selects the provider, e.g. "aws-sm", "vault", "env", "file".
	Scheme string
	// Path is the provider-specific location of the value, e.g. "prod/db",
	// "kv/data/api", "/etc/tls/tls.crt", or "LOG_LEVEL".
	Path string
	// Key selects a single field from a structured payload (the URL fragment,
	// i.e. the part after '#'). It is empty when no key is requested.
	Key string
	// Opts holds provider-specific options parsed from the query string, plus a
	// small set of core-recognized options (debounce, optional, version).
	Opts url.Values
	// Raw is the original, unparsed tag value, retained for error messages.
	Raw string
}

// String renders the Ref back into its canonical tag form. Query options are
// omitted if empty. It is primarily useful for diagnostics.
func (r Ref) String() string {
	if r.Raw != "" {
		return r.Raw
	}
	var b strings.Builder
	b.WriteString(r.Scheme)
	b.WriteString("://")
	b.WriteString(r.Path)
	if r.Key != "" {
		b.WriteByte('#')
		b.WriteString(r.Key)
	}
	if len(r.Opts) > 0 {
		b.WriteByte('?')
		b.WriteString(r.Opts.Encode())
	}
	return b.String()
}

// Opt returns the first value for the named option, or "" if unset.
func (r Ref) Opt(name string) string {
	if r.Opts == nil {
		return ""
	}
	return r.Opts.Get(name)
}

// ParseRef parses a `source` tag value into a Ref. It returns an error for an
// empty tag or a tag without a scheme.
//
// The grammar is scheme-agnostic and, per the mamori spec, places the optional
// #key fragment BEFORE the optional ?opts query (the reverse of a standard URL).
// Parsing is therefore done by hand rather than via net/url:
//
//	scheme://path[#key][?opts]   (hierarchical: aws-sm, vault, file, ...)
//	scheme:path[#key][?opts]     (opaque: env, exec)
func ParseRef(tag string) (Ref, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return Ref{}, fmt.Errorf("mamori: empty source ref")
	}
	scheme, remainder, ok := strings.Cut(tag, ":")
	if !ok || scheme == "" {
		return Ref{}, fmt.Errorf("mamori: source ref %q missing scheme", tag)
	}
	ref := Ref{Scheme: scheme, Raw: tag, Opts: url.Values{}}

	// A hierarchical ref's remainder starts with "//"; strip it. The authority
	// and path are treated as one opaque provider path (e.g. "prod/db"), except
	// that a fully-slashed form like file:///etc/x keeps its leading slash.
	rest := remainder
	if strings.HasPrefix(rest, "//") {
		rest = rest[2:]
	}

	// Split off the query (?opts) first - it is always last in the grammar.
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		q, err := url.ParseQuery(rest[i+1:])
		if err != nil {
			return Ref{}, fmt.Errorf("mamori: source ref %q bad query: %w", tag, err)
		}
		ref.Opts = q
		rest = rest[:i]
	}
	// Then split off the fragment (#key).
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		ref.Key = rest[i+1:]
		rest = rest[:i]
	}
	ref.Path = rest
	return ref, nil
}
