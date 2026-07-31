package vercelgc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/xavidop/mamori"
)

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
// documented token query parameter, so it can never reach a log line, a trace
// span, or an error message through a URL.
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
		// Read a bounded amount of the error body for diagnostics. Never log it.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		statusErr := fmt.Errorf("mamori/vercel-gc: unexpected status %d from %s of store %s: %s",
			resp.StatusCode, endpoint, store, strings.TrimSpace(string(msg)))
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("mamori/vercel-gc: store %s not found: %w", store, mamori.ErrNotFound)
		}
		return nil, classifyStatus(resp.StatusCode, statusErr)
	}
	return io.ReadAll(resp.Body)
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
