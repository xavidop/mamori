package vercelgc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/xavidop/mamori"
)

// errBodyLimit bounds how much of a non-200 response body get reads, so a
// hostile or broken upstream cannot put an unbounded response into an error
// string. The 404 branch reads within this same bound and folds the result
// into the returned not-found error (rather than building it and discarding
// it, which is what this provider used to do), which doubles as the drain
// that lets net/http reuse the connection - the same reason the other
// branch below reads a bounded amount rather than none at all. The 200 path
// needs no matching drain step: it already reads the whole body via
// io.ReadAll, so the connection is already fully consumed by the time this
// function returns.
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
	url := c.host + "/" + store + "/" + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Read a bounded amount of the error body for diagnostics (and, on the
		// 404 branch below, fold it into the returned error instead of
		// discarding it: this is the one place a store-not-found body such as
		// "edge_config_not_found" would tell a caller what actually went
		// wrong). Never log it.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		trimmed := strings.TrimSpace(string(msg))
		if resp.StatusCode == http.StatusNotFound {
			if trimmed == "" {
				return nil, fmt.Errorf("mamori/vercel-gc: store %s not found: %w", store, mamori.ErrNotFound)
			}
			return nil, fmt.Errorf("mamori/vercel-gc: store %s not found: %w: %s", store, mamori.ErrNotFound, trimmed)
		}
		statusErr := fmt.Errorf("mamori/vercel-gc: unexpected status %d from %s of store %s: %s",
			resp.StatusCode, endpoint, store, trimmed)
		return nil, classifyStatus(resp.StatusCode, statusErr)
	}
	return io.ReadAll(resp.Body)
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

// classifyStatus maps a Global Config read API status onto a mamori
// classification sentinel, wrapping statusErr so both the sentinel and the
// diagnostic context survive in the errors.Is chain. 404 is handled by its own
// branch in get and never reaches this function.
//
// The mapping follows ordinary HTTP semantics. One caveat is worth stating
// rather than hiding: Vercel's documented error body for a request missing an
// authentication token carries "code": "forbidden", so a 403 from this API can
// mean an absent credential rather than an insufficient one. Vercel has not
// published the full error-code vocabulary, so the mapping keys on status
// rather than guessing at a body it cannot rely on. Codes not listed report
// unknown rather than being guessed at.
func classifyStatus(code int, statusErr error) error {
	if statusErr == nil {
		return nil
	}
	var sentinel error
	switch {
	case code == http.StatusUnauthorized:
		sentinel = mamori.ErrUnauthenticated
	case code == http.StatusForbidden:
		sentinel = mamori.ErrPermissionDenied
	case code == http.StatusTooManyRequests:
		sentinel = mamori.ErrRateLimited
	case code == http.StatusBadRequest:
		sentinel = mamori.ErrInvalid
	case code >= 500:
		sentinel = mamori.ErrUnavailable
	default:
		return statusErr
	}
	return fmt.Errorf("%w: %w", sentinel, statusErr)
}
