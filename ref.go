package mamori

import (
	"fmt"
	"net/url"
	"regexp"
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
	rest := strings.TrimPrefix(remainder, "//")

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

// schemeStart matches a scheme-like token at the start of a string, e.g.
// "aws-sm:" or "env:". It is how ParseRefs decides a comma is a chain
// separator rather than a comma inside a path or query.
//
// Leading whitespace is skipped so that a chain written the way a human writes
// a list - `env:PORT, aws-ps://svc/port` - splits identically to its compact
// form. Without that, the space would stop the scheme from matching, the whole
// tail would collapse into the first ref's path, and the resulting broken ref
// would fail silently at resolve time rather than at parse time. Whitespace
// alone is never enough: what follows it must still look like a scheme, so
// `exec:echo a, b` stays the single ref it has always been.
var schemeStart = regexp.MustCompile(`^\s*[a-zA-Z][a-zA-Z0-9+.\-]*:`)

// ParseRefs parses a `source` tag that may hold a comma-separated precedence
// chain of refs, e.g. "env:PORT,aws-ps://svc/port". This is the entry point
// for chained sources (spec 10.2): the first ref to yield a value wins, so the
// chain lets a field prefer a fast/local source and fall back to a slower or
// more authoritative one without the caller writing that fallback logic by
// hand.
//
// A comma is treated as a separator only when the text after it begins with a
// scheme-like token (see schemeStart); a comma inside a query option (?tags=a,b)
// or an opaque exec: path (exec:echo a,b) is preserved as part of that ref. To
// force a literal comma that would otherwise be read as a separator,
// percent-encode it as %2C.
//
// Whitespace around a separator is ignored, so the spaced form
// "env:PORT, aws-ps://svc/port" yields exactly the same chain as the compact
// "env:PORT,aws-ps://svc/port". A space does not by itself make a comma a
// separator: what follows it must still look like a scheme, so
// "exec:echo a, b" remains one ref.
//
// Because of that rule, a doubled or trailing comma is not a split point (no
// scheme token follows it) and is instead kept as part of the adjacent ref's
// value, e.g. "env:A,,env:B" yields a first ref with path "A," rather than an
// empty chain entry. Such a malformed ref simply resolves not-found at lookup
// time and the chain falls through to the next entry, so this is treated as a
// caller error rather than something ParseRefs rejects outright.
//
// A single-ref tag (the common case) yields a one-element slice, so callers
// that do not use chains see no behavior change from switching to ParseRefs.
func ParseRefs(tag string) ([]Ref, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil, fmt.Errorf("mamori: empty source ref")
	}
	parts := splitChain(tag)
	refs := make([]Ref, 0, len(parts))
	for _, p := range parts {
		ref, err := ParseRef(p)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// splitChain splits tag on commas that begin a new scheme-prefixed ref,
// leaving every other comma (inside a query string or an opaque path)
// untouched.
func splitChain(tag string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(tag); i++ {
		if tag[i] != ',' {
			continue
		}
		if schemeStart.MatchString(tag[i+1:]) {
			parts = append(parts, tag[start:i])
			start = i + 1
		}
	}
	parts = append(parts, tag[start:])
	return parts
}
