package https

import (
	"context"
	"fmt"
	"strings"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// Resolve fetches ref's value from its endpoint.
//
// The ref path's first segment is the endpoint name and the remainder is the
// request path. An unknown endpoint wraps mamori.ErrInvalid rather than
// mamori.ErrNotFound: ErrNotFound is the one kind that makes mamori apply a
// field's default, and a misconfigured ref must never be hidden behind one.
//
// ref.Key is handed to mamori.SelectKey, so a fragment is either an RFC 6901
// JSON Pointer or a literal top-level key, identically to every other provider.
//
// Query options are passed through untouched. Decoding a resolved value is
// core's job, not a provider's.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	name, path := splitEndpoint(ref.Path)
	ep, ok := p.endpoints[name]
	if !ok {
		return mamori.Value{}, fmt.Errorf("https: no endpoint named %q is registered (ref %q): %w", name, ref.Raw, mamori.ErrInvalid)
	}
	if hasDotSegment(path) {
		return mamori.Value{}, fmt.Errorf("https: ref %q path contains a dot segment, which would escape endpoint %q's BaseURL: %w", ref.Raw, name, mamori.ErrInvalid)
	}

	resp, err := ep.reval.Get(ctx, ref.Raw, httpcore.Request{
		Path:   path,
		Query:  ep.query,
		Header: ep.header,
	})
	if err != nil {
		return mamori.Value{}, fmt.Errorf("https: endpoint %q: %w", name, err)
	}

	body, err := mamori.SelectKey(resp.Body, ref.Key)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("https: endpoint %q: %w", name, err)
	}

	return mamori.Value{
		Bytes:     body,
		Version:   httpcore.Version(resp, resp.Body),
		Sensitive: ep.sensitive,
	}, nil
}

// splitEndpoint splits a ref path into its endpoint name and the remaining
// request path. "billing/cfg/main" yields ("billing", "cfg/main"), and
// "billing" alone yields ("billing", "").
func splitEndpoint(refPath string) (name, path string) {
	trimmed := strings.TrimPrefix(refPath, "/")
	name, path, _ = strings.Cut(trimmed, "/")
	return name, path
}

// hasDotSegment reports whether p contains a "." or ".." path segment.
//
// Such a segment is rejected rather than cleaned. httpcore's joinPath does not
// resolve dot segments, so "../.." in a ref path would reach outside the path
// prefix its endpoint declares: for an endpoint scoped to
// https://api/v1/tenants/acme, that is another tenant's configuration, reached
// without ever leaving the declared host and therefore without tripping the
// endpoint check that exists to contain exactly this.
//
// It is reachable rather than theoretical, because a ref path is not only what
// the struct tag says. expandRefVars substitutes ${VAR} from WithRefVars
// (decode.go), whose values the application supplies at runtime, so a ref of
// https://billing/${TENANT}/cfg carries whatever TENANT holds.
//
// Rejecting is the loud option. Cleaning silently would change which value a ref
// names, and a ref that quietly means something other than it says is worse than
// one that fails. mamori doctor resolves every ref before deployment, so this
// surfaces there rather than in production.
//
// Backslash counts as a separator here, not only '/'. Splitting on '/' alone
// leaves `a\..\..\secrets` as a single segment that matches neither "." nor
// "..", so the check passes and the request goes out. url.URL.String percent
// encodes the backslashes, so the wire carries "%5C", which most backends treat
// as an ordinary character. IIS and ASP.NET are the well known exceptions: they
// decode it and honour '\' as a directory separator, which is the classic
// backslash traversal bypass. Endpoint.BaseURL is operator supplied with no
// platform restriction, so this package cannot assume the backend is not one of
// them. Splitting on both keeps `a\b` usable as an ordinary key while refusing
// `a\..\b`.
//
// The percent-encoded form needs no separate check: mamori's ParseRef does not
// decode escapes, so "%2e%2e" stays literal, and url.URL.String re-encodes the
// percent sign, leaving the backend with "%252e%252e" rather than a traversal.
// TestResolveRejectsDotSegments pins both halves of that.
//
// Segments of three or more dots are deliberately not matched. RFC 3986 section
// 5.2.4 defines dot-segment removal over exactly "." and "..", so "..." is an
// ordinary segment name rather than a traversal.
func hasDotSegment(p string) bool {
	isSep := func(r rune) bool { return r == '/' || r == '\\' }
	for _, seg := range strings.FieldsFunc(p, isSep) {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}
