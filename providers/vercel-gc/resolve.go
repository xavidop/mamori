package vercelgc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// errBodyLimit bounds how much of a failing response body reaches an error
// string, so a hostile or broken upstream cannot put an unbounded response
// into one. It applies to the 404 branch too, which folds the body into the
// returned not-found error rather than discarding it: this is the one place a
// store-not-found body such as "edge_config_not_found" tells a caller what
// actually went wrong. httpcore hands errorDetail at most Config.MaxBody bytes
// and always drains the rest on every branch, so the connection-reuse
// discipline this file used to hand-roll per branch now comes from one place.
const errBodyLimit = 4096

// Resolve fetches the value for ref.
//
// Every call requests the store digest, which Vercel replaces on any edit, and
// refetches the item body only when that hash moved. There is deliberately no
// TTL and no clock: mamori.Refresh and mamori.Doctor both call Resolve
// directly, and a time-based cache would let either return a held value
// without contacting Vercel at all.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	conn, err := p.connection()
	if err != nil {
		return mamori.Value{}, err
	}
	store, key, err := parsePath(ref.Path, conn.storeID)
	if err != nil {
		return mamori.Value{}, err
	}

	digest, err := p.fetchDigest(ctx, conn, store)
	if err != nil {
		return mamori.Value{}, err
	}
	snap, err := p.snapshotFor(ctx, conn, store, digest)
	if err != nil {
		return mamori.Value{}, err
	}

	raw, ok := snap.items[key]
	if !ok {
		return mamori.Value{}, fmt.Errorf("mamori/vercel-gc: key %q not in store %s: %w", key, store, mamori.ErrNotFound)
	}
	return valueFor(raw, ref, store, snap.digest)
}

// snapshotFor returns the store body matching digest, fetching it only when the
// held snapshot is missing or stale.
//
// The fetch happens outside the lock, so two goroutines observing the same
// change may both fetch. That is accepted rather than prevented: the fetch is
// an idempotent GET, it happens only on an actual edit, and avoiding it would
// mean either serializing every digest request or taking on a single-flight
// dependency in a module whose whole appeal is having none. Each caller uses
// the snapshot it built, so nobody ever reads a body another goroutine was
// mid-install of; a losing writer's older snapshot simply causes one extra
// fetch on the next Resolve.
//
// One assumption is worth naming rather than leaving implicit: this only
// converges on the true value if an /items response fetched after observing
// digest D always reflects content at least as new as D. Global Config is a
// globally replicated store, and Vercel documents no read-your-writes or
// monotonic-read guarantee tying the two endpoints together. If the digest and
// items requests can land on replicas at different points in the replication
// stream, this could install {digest: D, items: <content older than D>}, and
// because the digest then matches on every following call, that stale body
// would be served indefinitely - until the next edit moves the digest again,
// not until the replicas catch up. There is nothing to fix here from this
// side of the API; the risk is named rather than closed because closing it
// would need Vercel to document the guarantee, or some other signal this
// provider does not have.
func (p *Provider) snapshotFor(ctx context.Context, c connection, store, digest string) (*snapshot, error) {
	p.mu.Lock()
	held := p.snapshots[store]
	p.mu.Unlock()
	if held != nil && held.digest == digest {
		return held, nil
	}

	items, err := p.fetchItems(ctx, c, store)
	if err != nil {
		return nil, err
	}
	fresh := &snapshot{digest: digest, items: items}

	p.mu.Lock()
	p.snapshots[store] = fresh
	p.mu.Unlock()
	return fresh, nil
}

// fetchDigest returns the current digest of store.
func (p *Provider) fetchDigest(ctx context.Context, c connection, store string) (string, error) {
	body, err := p.get(ctx, c, store, "digest")
	if err != nil {
		return "", err
	}
	return parseDigest(body)
}

// parseDigest reads the digest out of the endpoint's JSON response. Vercel
// documents that the endpoint returns JSON but does not pin the shape, so both
// a bare string and an object carrying a "digest" field are accepted rather
// than betting on one.
func parseDigest(body []byte) (string, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "", fmt.Errorf("mamori/vercel-gc: empty digest response: %w", mamori.ErrInvalid)
	}
	var s string
	if err := json.Unmarshal([]byte(trimmed), &s); err == nil {
		if s == "" {
			// Both a bare "" and a bare null unmarshal into the empty string here
			// (json.Unmarshal leaves a *string target untouched on a JSON null),
			// and both must be rejected the same way the object branch already
			// rejects an empty "digest" field. Accepting either would install a
			// snapshot tagged with digest "", which every later call would match
			// forever: /items would never be refetched again for the process
			// lifetime, silently pinning stale config.
			return "", fmt.Errorf("mamori/vercel-gc: empty digest response: %w", mamori.ErrInvalid)
		}
		return s, nil
	}
	var obj struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal([]byte(trimmed), &obj); err == nil && obj.Digest != "" {
		return obj.Digest, nil
	}
	return "", fmt.Errorf("mamori/vercel-gc: unrecognized digest response shape: %w", mamori.ErrInvalid)
}

// fetchItems returns every key in store in one request.
func (p *Provider) fetchItems(ctx context.Context, c connection, store string) (map[string]jsonRaw, error) {
	body, err := p.get(ctx, c, store, "items")
	if err != nil {
		return nil, err
	}
	var items map[string]jsonRaw
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("mamori/vercel-gc: decoding items of store %s: %w: %w", store, mamori.ErrInvalid, err)
	}
	if items == nil {
		items = map[string]jsonRaw{}
	}
	return items, nil
}

// get performs one authenticated GET against a store endpoint and returns the
// body. The token travels in the Authorization header rather than the
// documented token query parameter, so an ordinary request through this
// method cannot leak it into a log line, a trace span, or an error message
// through a URL.
//
// That guarantee is about requests actually reaching Vercel, not about every
// way c.host can be set. WithBaseURL redirects a connection-string-derived
// host rather than being ignored when one is supplied (see its doc comment),
// so passing a full connection string to WithBaseURL by mistake, instead of
// just a bare origin, puts the token into c.host and therefore into this
// method's URL and any transport error a live request against it produces.
// Parsing a malformed connection string is a narrower case: url.Parse can
// echo a fragment of it back into its error, for example invalid port
// ":notaport" after host or invalid URL escape "%zz", but never the token
// itself, because url.Parse neither validates nor unescapes the query string
// the token lives in (see parseConnectionString's doc comment for how that
// error is stripped before it reaches a caller).
func (p *Provider) get(ctx context.Context, c connection, store, endpoint string) ([]byte, error) {
	client, err := p.clientFor(c)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(ctx, httpcore.Request{
		Path:   "/" + url.PathEscape(store) + "/" + endpoint,
		Header: http.Header{"Accept": {"application/json"}},
	})
	if err != nil {
		// One %w on both branches: the cause already carries the sentinel
		// httpcore classified it with, and adding a second would replace that
		// kind rather than duplicate it. The bounded body errorDetail lifted
		// out of the response travels inside that cause, which is what keeps
		// a store-not-found diagnostic such as "edge_config_not_found" in the
		// message rather than discarding it.
		if errors.Is(err, mamori.ErrNotFound) {
			return nil, fmt.Errorf("mamori/vercel-gc: store %s not found: %w", store, err)
		}
		return nil, fmt.Errorf("mamori/vercel-gc: %s of store %s: %w", endpoint, store, err)
	}
	return resp.Body, nil
}

// clientFor builds the httpcore client for one request.
//
// It is built per call rather than cached because the connection is resolved
// per resolve, reading GLOBAL_CONFIG and EDGE_CONFIG lazily (see connection):
// caching would pin whichever one happened to be resolved first. Construction
// performs no network call and reuses the provider's *http.Client, so the
// connection pool is shared across every request.
//
// The token travels in the Authorization header rather than the documented
// token query parameter, so an ordinary request cannot leak it into a log
// line, a trace span, or an error message through a URL. httpcore adds a
// second guarantee on top of that one: it renders the request URL into its
// errors with the query string and any userinfo stripped, and rebuilds a
// transport failure's own *url.Error the same way, so even the misuse
// WithBaseURL's doc comment warns about - passing a whole connection string
// where a bare origin belongs, which puts the token in c.host - cannot put
// the token into an error's text.
func (p *Provider) clientFor(c connection) (*httpcore.Client, error) {
	client, err := httpcore.New(httpcore.Config{
		BaseURL:     c.host,
		HTTPClient:  p.httpClient,
		Auth:        httpcore.Bearer(c.token),
		ErrorDetail: errorDetail,
	})
	if err != nil {
		return nil, fmt.Errorf("mamori/vercel-gc: building the API client: %w", err)
	}
	return client, nil
}

// errorDetail is httpcore's Config.ErrorDetail hook: it quotes a bounded
// prefix of a failing response's body into the error message.
//
// Embedding the body verbatim, bounded by errBodyLimit, is a deliberate house
// convention shared with providers/doppler, providers/cloudflare-kv and
// providers/scaleway-sm. It matters most on the 404: "edge_config_not_found"
// is the one thing that distinguishes a store that does not exist from a token
// that cannot see it. httpcore calls this only for a status it has already
// decided is a failure, never for the 200 whose body is the store's items.
func errorDetail(_ int, body []byte) string {
	if len(body) > errBodyLimit {
		body = body[:errBodyLimit]
	}
	return strings.TrimSpace(string(body))
}

// ResolveBatch resolves every ref, grouping by store so each store costs one
// digest request and one items request instead of one pair per ref. mamori
// calls it automatically on the Load path.
//
// A ref whose key is absent, or whose #field selection is absent from an
// otherwise-present JSON value, is omitted from the result map rather than
// failing the batch, per the BatchProvider contract, so mamori applies that
// field's default. This must agree with Resolve, which reports the same two
// cases as an error satisfying mamori.ErrNotFound rather than killing the
// whole call. An ErrInvalid from selection (for example selecting a field of
// a string-valued key) is deliberately not treated as not-found and still
// fails the batch, as does a ref that cannot be parsed or a store that cannot
// be read: those are configuration and connectivity faults rather than an
// absent value.
func (p *Provider) ResolveBatch(ctx context.Context, refs []mamori.Ref) (map[string]mamori.Value, error) {
	if len(refs) == 0 {
		return map[string]mamori.Value{}, nil
	}
	conn, err := p.connection()
	if err != nil {
		return nil, err
	}

	// Group by store, preserving each ref so its Raw can key the result.
	type target struct {
		ref mamori.Ref
		key string
	}
	byStore := map[string][]target{}
	for _, r := range refs {
		store, key, err := parsePath(r.Path, conn.storeID)
		if err != nil {
			return nil, err
		}
		byStore[store] = append(byStore[store], target{ref: r, key: key})
	}

	out := make(map[string]mamori.Value, len(refs))
	for store, targets := range byStore {
		digest, err := p.fetchDigest(ctx, conn, store)
		if err != nil {
			return nil, err
		}
		snap, err := p.snapshotFor(ctx, conn, store, digest)
		if err != nil {
			return nil, err
		}
		for _, t := range targets {
			raw, ok := snap.items[t.key]
			if !ok {
				continue // omit not-found refs; mamori applies the default
			}
			v, err := valueFor(raw, t.ref, store, snap.digest)
			if err != nil {
				if errors.Is(err, mamori.ErrNotFound) {
					continue // an absent selected field is still not-found; mamori applies the default
				}
				return nil, err
			}
			out[t.ref.Raw] = v
		}
	}
	return out, nil
}

// Status classification comes from httpcore.ClassifyStatus, inside
// httpcore.Client.Do, rather than from a mapping copied into this package. 404
// is the one status get takes back, to give it a message of its own.
//
// One caveat of the mapping is worth stating rather than hiding: Vercel's
// documented error body for a request missing an authentication token carries
// "code": "forbidden", so a 403 from this API can mean an absent credential
// rather than an insufficient one. Vercel has not published the full
// error-code vocabulary, so classification keys on the status rather than
// guessing at a body it cannot rely on.
