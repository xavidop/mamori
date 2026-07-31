package cloudflarekv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/xavidop/mamori"
)

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
		return nil, fmt.Errorf("mamori/cloudflare-kv: key %q not found: %w", key, mamori.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		// Read a bounded amount of the error body for diagnostics. Never log it.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		statusErr := fmt.Errorf("mamori/cloudflare-kv: unexpected status %d reading key %q: %s",
			resp.StatusCode, key, strings.TrimSpace(string(msg)))
		return nil, classifyStatus(resp.StatusCode, statusErr)
	}
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
// branch in get and never reaches this function.
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
