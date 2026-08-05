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
// A ref path containing a "." or ".." segment is rejected with
// mamori.ErrInvalid, so a ref cannot escape the path prefix its endpoint's
// BaseURL declares. That check is not here: httpcore enforces it for every
// provider built on it, so no provider can forget it, and the encoded form
// ("%2e%2e") is caught with the literal one. See httpcore's hasDotSegment.
//
// Query options are passed through untouched. Decoding a resolved value is
// core's job, not a provider's.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return mamori.Value{}, fmt.Errorf("https: provider is closed: %w", mamori.ErrUnavailable)
	}

	name, path := splitEndpoint(ref.Path)
	ep, ok := p.endpoints[name]
	if !ok {
		return mamori.Value{}, fmt.Errorf("https: no endpoint named %q is registered (ref %q): %w", name, ref.Raw, mamori.ErrInvalid)
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
