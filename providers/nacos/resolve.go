package nacos

import (
	"context"
	"fmt"
	"net/url"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// configPath is Nacos's v1 configuration read endpoint, relative to the servlet
// context path.
//
// The v1 endpoint is used rather than v2 (/nacos/v2/cs/config) deliberately, and
// the reason is the listener: Nacos publishes an HTTP long-poll listener only at
// v1 (/nacos/v1/cs/configs/listener). v2 added a JSON envelope for reads and
// moved change notification to gRPC, which this module cannot speak - the
// dependency budget is the standard library, mamori, and httpcore. Mixing a v2
// read with a v1 listener would also mean two response shapes and two error
// vocabularies for the same configuration. See the module README for the version
// matrix.
const configPath = "v1/cs/configs"

// Resolve fetches the configuration named by ref.
//
// The response body is the configuration content itself, not a JSON envelope, so
// it becomes Value.Bytes unchanged. A #key fragment then selects a field from it
// with mamori.SelectKey.
//
// A configuration that does not exist yields Nacos's 404, which
// httpcore.ClassifyStatus maps to an error satisfying
// errors.Is(err, mamori.ErrNotFound), so mamori applies the field's default.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	c, err := p.coordinatesFor(ref)
	if err != nil {
		return mamori.Value{}, err
	}
	body, err := p.fetch(ctx, c)
	if err != nil {
		return mamori.Value{}, err
	}
	return p.value(body, ref)
}

// fetch performs the configuration read and returns the raw body.
func (p *Provider) fetch(ctx context.Context, c coordinates) ([]byte, error) {
	client, err := p.core()
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(ctx, httpcore.Request{
		Path:  configPath,
		Query: p.configQuery(c),
	})
	if err != nil {
		// One %w: httpcore has already classified the status and its message
		// carries the redacted URL, so a second sentinel here would only
		// duplicate a kind. The coordinates are named because "not found" is
		// otherwise indistinguishable between a wrong dataId and a wrong group,
		// and a wrong group is the mistake a Nacos user actually makes.
		return nil, fmt.Errorf("nacos: read %s: %w", c, err)
	}
	return resp.Body, nil
}

// configQuery builds the query parameters Nacos's read endpoint takes. The
// namespace parameter is named "tenant" on v1; Nacos's own docs describe it as
// "Tenant information. It corresponds to the Namespace ID field in Nacos".
//
// An empty tenant is omitted rather than sent blank, which is what selects the
// public namespace.
func (p *Provider) configQuery(c coordinates) url.Values {
	q := url.Values{
		"dataId": {c.dataID},
		"group":  {c.group},
	}
	if t := p.tenant(); t != "" {
		q.Set("tenant", t)
	}
	return q
}

// value turns a raw configuration body into a mamori.Value, applying ref.Key.
//
// Version is a hash of the bytes actually returned rather than the response's
// Last-Modified header, which is the validator httpcore.Version would otherwise
// prefer. Nacos sends no ETag, and Last-Modified has one-second resolution: two
// publishes of the same configuration inside one second are indistinguishable
// through it. That is not a hypothetical during a rollout, where a config is
// commonly corrected seconds after it is pushed, and the consequence is the bad
// one - mamori compares Versions to decide whether a value changed, so a
// coalesced Last-Modified makes it skip the second publish entirely and leave
// the application on the value that was withdrawn.
//
// Hashing the SELECTED bytes, not the whole body, is equally deliberate: a field
// bound to nacos://app.json#log.level must not report a new Version because some
// unrelated key in the same document moved, which would fire its OnChange and,
// for a credential, force a reconnect for nothing.
func (p *Provider) value(body []byte, ref mamori.Ref) (mamori.Value, error) {
	b := body
	if ref.Key != "" {
		sel, err := mamori.SelectKey(body, ref.Key)
		if err != nil {
			return mamori.Value{}, fmt.Errorf("nacos: selecting %q from ref %q: %w", ref.Key, ref.Raw, err)
		}
		b = sel
	}
	return mamori.Value{Bytes: b, Version: mamori.VersionHash(b)}, nil
}
