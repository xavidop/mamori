package cloudflarekv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/xavidop/mamori"
)

// bulkMaxKeys is the maximum number of keys Cloudflare's bulk/get endpoint
// accepts in a single request. ResolveBatch chunks any namespace with more
// keys than this into multiple requests and merges the results: silently
// truncating at this ceiling would drop every key past the 100th with no
// error at all.
const bulkMaxKeys = 100

// errBodyLimit bounds how much of a non-200 response body get and bulkGet
// read, so a hostile or broken upstream cannot put an unbounded response
// into an error string. Both functions' 404 branches drain within this same
// bound (see get's doc comment on why an absent key needs that drain), and
// both 200 paths now drain within it too - get already does by reading the
// whole body via io.ReadAll, and bulkGet's Decode-based path drains
// explicitly right after decoding - so every branch that can be entered
// shares one connection-reuse discipline rather than the 200 and 404 paths
// silently differing.
const errBodyLimit = 4096

// Resolve fetches the value for ref from Workers KV.
//
// There is deliberately no cache and no TTL: mamori.Refresh and mamori.Doctor
// both call Resolve directly, and unlike providers/vercel-gc - which gates a
// held snapshot on a store digest - Workers KV exposes no digest or ETag for
// this provider to gate a cache on. Every call is a live GET against the
// current value.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	s, err := p.settingsFor(ref)
	if err != nil {
		return mamori.Value{}, err
	}
	key, err := keyOf(ref)
	if err != nil {
		return mamori.Value{}, err
	}
	b, err := p.get(ctx, s, key)
	if err != nil {
		return mamori.Value{}, err
	}
	return valueFor(b, ref, s.namespace)
}

// bulkGetRequestBody is the body POSTed to the bulk/get endpoint. Type is
// always "text": without it Cloudflare may attempt to JSON-parse a stored
// value and return an object rather than the string bytes this provider
// treats as opaque.
type bulkGetRequestBody struct {
	Keys []string `json:"keys"`
	Type string   `json:"type"`
}

// bulkGetResponseBody is the JSON envelope every bulk/get response wraps its
// values in - unlike the single-key GET endpoint's raw bytes (see get's doc
// comment). A key absent from the namespace is simply absent from Values,
// with no per-key error, which is what lets ResolveBatch treat an absent key
// as a plain map miss.
type bulkGetResponseBody struct {
	Success bool `json:"success"`
	Result  struct {
		Values map[string]string `json:"values"`
	} `json:"result"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// ResolveBatch resolves every ref in one bulk request per namespace, each
// chunked to at most bulkMaxKeys keys since that is the largest the bulk/get
// endpoint accepts. mamori calls it automatically on the Load path so a Load
// with many refs costs a handful of requests rather than one GET per field.
//
// A key absent from a namespace's bulk response is omitted from the result
// map rather than failing the batch, per the BatchProvider contract, so
// mamori applies that field's default. The same holds for a ref whose #field
// selection is absent from an otherwise-present JSON value: valueFor reports
// that case as an error satisfying mamori.ErrNotFound, and it is treated
// exactly like an absent key here, matching Resolve. This split is
// deliberate: providers/vercel-gc originally returned that error verbatim
// from its ResolveBatch, which failed the entire batch over one missing
// optional field and took every sibling ref down with it. An ErrInvalid from
// selection (for example selecting a #field of a value that is not a JSON
// object) is a different class of problem - a malformed request against the
// payload, not an absence - and still fails the batch, as does a namespace
// that fails for any reason other than a plain 404.
//
// A 404 on a namespace's bulk request is that same not-an-absence-failure
// split applied one level up: bulkGet reports it as mamori.ErrNotFound, and
// the chunk loop below skips that chunk's keys - leaving their refs absent
// from the result map, exactly like an absent key - instead of returning the
// error and failing every sibling ref in the batch, in this namespace or any
// other. A single misconfigured namespace among many refs must not be able to
// take the whole batch down with it, any more than a missing optional field
// can.
func (p *Provider) ResolveBatch(ctx context.Context, refs []mamori.Ref) (map[string]mamori.Value, error) {
	if len(refs) == 0 {
		return map[string]mamori.Value{}, nil
	}

	// Group refs by namespace, deduplicating keys within a namespace so two
	// refs selecting different #fields of the same key (e.g. api-config#a and
	// api-config#b) cost one bulk-request slot rather than two.
	type namespaceGroup struct {
		settings settings
		keys     []string // deduplicated, first-seen order
		byKey    map[string][]mamori.Ref
	}
	groups := map[string]*namespaceGroup{}

	for _, r := range refs {
		s, err := p.settingsFor(r)
		if err != nil {
			return nil, err
		}
		key, err := keyOf(r)
		if err != nil {
			return nil, err
		}
		g, ok := groups[s.namespace]
		if !ok {
			g = &namespaceGroup{settings: s, byKey: map[string][]mamori.Ref{}}
			groups[s.namespace] = g
		}
		if _, seen := g.byKey[key]; !seen {
			g.keys = append(g.keys, key)
		}
		g.byKey[key] = append(g.byKey[key], r)
	}

	out := make(map[string]mamori.Value, len(refs))
	for ns, g := range groups {
		for start := 0; start < len(g.keys); start += bulkMaxKeys {
			end := min(start+bulkMaxKeys, len(g.keys))
			chunkKeys := g.keys[start:end]

			values, err := p.bulkGet(ctx, g.settings, chunkKeys)
			if err != nil {
				if errors.Is(err, mamori.ErrNotFound) {
					// The namespace itself does not exist (see bulkGet's 404
					// branch); skip this chunk's keys so their refs fall back
					// to defaults, exactly as a namespace-not-found Resolve
					// lets its own caller's onfail/default handling take
					// over. This is a namespace-level decision, distinct from
					// the per-ref #field-not-found swallow around valueFor
					// below: one bad namespace must not fail every sibling
					// ref sharing this batch, in any other namespace or in
					// this one.
					continue
				}
				return nil, err
			}
			for _, key := range chunkKeys {
				b, ok := values[key]
				if !ok {
					continue // absent key; mamori applies the default
				}
				for _, r := range g.byKey[key] {
					v, err := valueFor(b, r, ns)
					if err != nil {
						if errors.Is(err, mamori.ErrNotFound) {
							continue // an absent selected field is still not-found; mamori applies the default
						}
						return nil, err
					}
					out[r.Raw] = v
				}
			}
		}
	}
	return out, nil
}

// bulkGet issues one authenticated POST against the bulk/get endpoint for
// keys (at most bulkMaxKeys of them) and returns the values that came back,
// keyed by the requested key. A key absent from the namespace is simply
// absent from the returned map; the caller (ResolveBatch) treats that as an
// omission, not an error.
func (p *Provider) bulkGet(ctx context.Context, s settings, keys []string) (map[string][]byte, error) {
	u := p.baseURL + "/accounts/" + url.PathEscape(s.account) +
		"/storage/kv/namespaces/" + url.PathEscape(s.namespace) + "/bulk/get"

	reqBody, err := json.Marshal(bulkGetRequestBody{Keys: keys, Type: "text"})
	if err != nil {
		return nil, fmt.Errorf("mamori/cloudflare-kv: encoding bulk request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(reqBody))
	if err != nil {
		return nil, sanitizeTransportError(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, sanitizeTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		// Unlike get, an absent key never reaches this branch: the bulk
		// endpoint has no per-key 404 - a missing key is simply omitted from
		// a successful response's Values map (see bulkGetResponseBody's doc
		// comment). A 404 here can therefore only mean the namespace itself
		// does not exist, so it is classified as mamori.ErrNotFound before
		// classifyStatus ever sees it, exactly as get's namespace-not-found
		// case is. ResolveBatch's call site treats this the same way it
		// treats an absent key: this chunk's keys are skipped so their refs
		// fall back to their defaults, rather than one bad namespace failing
		// every sibling ref in the batch.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errBodyLimit))
		return nil, fmt.Errorf("mamori/cloudflare-kv: namespace not found for bulk get of %d key(s): %w", len(keys), mamori.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		// Read a bounded amount of the error body for diagnostics. Never log
		// it. Embedding the body verbatim, bounded by errBodyLimit, is a
		// deliberate house convention shared with providers/doppler,
		// providers/vercel-gc, and providers/scaleway-sm, not something
		// invented here: the verbatim text is what actually tells an operator
		// what the upstream rejected the request for, and the bound is what
		// stops a hostile or broken upstream turning that diagnostic into an
		// unbounded string.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		statusErr := fmt.Errorf("mamori/cloudflare-kv: unexpected status %d from bulk get of %d key(s): %s",
			resp.StatusCode, len(keys), strings.TrimSpace(string(msg)))
		return nil, classifyStatus(resp.StatusCode, statusErr)
	}

	var body bulkGetResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("mamori/cloudflare-kv: decoding bulk response: %w: %w", mamori.ErrInvalid, err)
	}
	// Decode consumes a well-formed single JSON value, so this is normally a
	// no-op; draining explicitly, rather than relying on that, is what makes
	// this path's connection-reuse behavior a decision instead of an
	// accident of Decode's internals, matching the 404 branch above and
	// get's own 200 path (see errBodyLimit's doc comment).
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errBodyLimit))
	if !body.Success {
		// A 200 status carrying "success":false is Cloudflare's own reported
		// failure, distinct from an HTTP-level status code, and is a malformed
		// request against the API rather than an absence.
		msg := "bulk get reported failure"
		if len(body.Errors) > 0 {
			msg = body.Errors[0].Message
		}
		return nil, fmt.Errorf("mamori/cloudflare-kv: %s: %w", msg, mamori.ErrInvalid)
	}

	values := make(map[string][]byte, len(body.Result.Values))
	for k, v := range body.Result.Values {
		values[k] = []byte(v)
	}
	return values, nil
}

// get issues one authenticated GET against the single-key value endpoint and
// returns the response body.
//
// This endpoint is the API's first surprise: a single-key GET responds with
// the value's raw stored bytes, not a JSON envelope. Task 3's bulk endpoint,
// by contrast, wraps every value inside {"result":{"values":{...}}}. There is
// no unwrapping step here because there is no envelope to unwrap: whatever
// bytes Cloudflare stored are the bytes this returns, verbatim.
//
// key is url.PathEscape'd before it is built into the URL. Workers KV keys
// are up to 512 bytes of any printable, non-whitespace character, so a key
// like "config/prod/log-level" must travel as one escaped path segment
// (config%2Fprod%2Flog-level) rather than as three, which is why keyOf takes
// the entire ref path as one key and never splits it on '/'.
func (p *Provider) get(ctx context.Context, s settings, key string) ([]byte, error) {
	u := p.baseURL + "/accounts/" + url.PathEscape(s.account) +
		"/storage/kv/namespaces/" + url.PathEscape(s.namespace) +
		"/values/" + url.PathEscape(key)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, sanitizeTransportError(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, sanitizeTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		// Either the key or the namespace is absent; see classifyStatus's doc
		// comment for why the response does not reliably distinguish them.
		// Drain and discard a bounded amount of the body before returning:
		// this provider caches nothing, so an absent key is read again on
		// every poll tick, and leaving the body unread here would prevent
		// the connection being reused, paying a fresh TCP and TLS handshake
		// on every one of those ticks.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errBodyLimit))
		return nil, fmt.Errorf("mamori/cloudflare-kv: key %q not found: %w", key, mamori.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		// Read a bounded amount of the error body for diagnostics. Never log
		// it (same errBodyLimit bound and rationale as bulkGet's status
		// branch, above).
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		statusErr := fmt.Errorf("mamori/cloudflare-kv: unexpected status %d reading key %q: %s",
			resp.StatusCode, key, strings.TrimSpace(string(msg)))
		return nil, classifyStatus(resp.StatusCode, statusErr)
	}
	// The 200 path needs no matching drain step: it already reads the whole
	// body via io.ReadAll below, so the connection is already fully consumed
	// by the time this function returns (see errBodyLimit's doc comment).
	return io.ReadAll(resp.Body)
}

// sanitizeTransportError strips a *url.Error down to its underlying reason,
// discarding the request URL it would otherwise render into Error().
//
// The request URL built in get embeds the account id and the namespace id,
// and http.Client.Do wraps every transport-level failure - a refused
// connection, a timeout, a cancelled context - in a *url.Error whose Error()
// renders the full request URL. Without this, an ordinary network hiccup,
// not even a bug in this provider, would put the account id into a returned
// error's text. This is the same class of leak providers/vercel-gc fixed in
// parseConnectionString (see its doc comment), reached here through the
// client's transport instead of url.Parse.
//
// Wrapping urlErr.Err with %w, rather than discarding it, keeps
// errors.Is(_, context.Canceled) (and similar checks) working: *url.Error
// already unwraps to the same underlying error via its own Unwrap method, so
// this changes only the rendered message, never the errors.Is chain.
//
// The final `return err` is deliberate, not a missed case: by construction
// only a *url.Error carries a rendered request URL, so any other error type
// already has nothing to strip, and wrapping it here would add noise without
// removing anything sensitive.
func sanitizeTransportError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("mamori/cloudflare-kv: request failed: %w", urlErr.Err)
	}
	return err
}

// valueFor turns the raw response body into a mamori.Value, applying the
// ref's #key selection when present.
//
// There is no unwrapping step analogous to providers/vercel-gc's JSON-item
// decoding: Workers KV stores opaque bytes, not a JSON envelope around each
// value, so whatever SelectKey returns (or the untouched body, when ref.Key
// is empty) is the final byte payload.
func valueFor(b []byte, ref mamori.Ref, namespace string) (mamori.Value, error) {
	if ref.Key != "" {
		sel, err := mamori.SelectKey(b, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		b = sel
	}
	return mamori.Value{
		Bytes:     b,
		Version:   mamori.VersionHash(b),
		Sensitive: false,
		Metadata: map[string]string{
			"namespace": namespace,
		},
	}, nil
}

// classifyStatus maps a Workers KV REST API status onto a mamori
// classification sentinel, wrapping statusErr so both the sentinel and the
// diagnostic context survive in the errors.Is chain. 404 is handled by its own
// branch in both get and bulkGet and never reaches this function.
//
// The mapping follows ordinary HTTP semantics: 401 for a missing or invalid
// API token, 403 for a token that authenticates but lacks permission to read
// this namespace, 429 for rate limiting, and 400 for a malformed request. One
// caveat is worth being honest about, because it is the kind of thing a
// misconfiguration hides behind rather than announces: a 404 from this API
// means either an absent key or an absent namespace, and Cloudflare's response
// does not reliably distinguish the two, so a namespace id that is simply
// wrong presents as every key in it silently falling back to its default,
// exactly like a genuinely absent key would. Cloudflare has not published a
// stable enough error-code vocabulary in the response body to key this
// mapping on anything but the status, so codes not listed here report
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
