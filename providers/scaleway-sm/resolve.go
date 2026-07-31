package scalewaysm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/xavidop/mamori"
)

// Resolve fetches ref's secret from Secret Manager.
//
// There is deliberately no cache and no TTL: mamori.Refresh and mamori.Doctor
// both call Resolve directly, and this provider holds no snapshot between
// calls to gate a cache on. Unlike providers/vercel-gc, which gates a held
// snapshot on a store digest, there is no digest here either - every call is
// a live GET against the current revision - and holding secret material in a
// provider-level cache would only extend how long it stays resident in
// process memory, for no gain.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	s, err := p.settingsFor()
	if err != nil {
		return mamori.Value{}, err
	}
	path, name, revision, err := parseRef(ref)
	if err != nil {
		return mamori.Value{}, err
	}
	resp, err := p.access(ctx, s, path, name, revision)
	if err != nil {
		return mamori.Value{}, err
	}
	return valueFor(resp, ref, s.region)
}

// accessResponse mirrors the JSON returned by the by-path access route.
// Data is []byte deliberately: encoding/json base64-decodes into it for
// free, so there is no manual decode step and none should be added.
type accessResponse struct {
	SecretID  string  `json:"secret_id"`
	Revision  uint32  `json:"revision"`
	Data      []byte  `json:"data"`
	DataCrc32 *uint32 `json:"data_crc32"`
	Type      string  `json:"type"`
}

// access issues one authenticated GET against the by-path access route for
// the secret at (path, name) and revision selector revision ("latest",
// "latest_enabled", or a decimal revision number - see parseRef's doc
// comment), and returns the decoded envelope.
//
// The full request URL, including its query string, is assembled into u
// BEFORE http.NewRequestWithContext is called, deliberately: a malformed
// WithBaseURL makes url.Parse fail inside NewRequestWithContext itself, and
// that failure - like every other transport-level failure this method can
// produce - is a *url.Error whose Error() renders the exact string handed to
// it. Building the query string in first, rather than adding it to req.URL
// afterwards, means that failure mode genuinely carries the project id, the
// same way the live-transport failure below does; see
// sanitizeTransportError's doc comment for why that matters.
func (p *Provider) access(ctx context.Context, s settings, path, name, revision string) (accessResponse, error) {
	u := p.baseURL + "/regions/" + url.PathEscape(s.region) +
		"/secrets-by-path/versions/" + url.PathEscape(revision) + "/access"

	q := url.Values{}
	q.Set("secret_path", path)
	q.Set("secret_name", name)
	q.Set("project_id", s.projectID)
	u += "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return accessResponse{}, sanitizeTransportError(err)
	}
	req.Header.Set("X-Auth-Token", s.secretKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return accessResponse{}, sanitizeTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		// Either the secret name or the requested revision is absent; see
		// classifyStatus's doc comment for why the response does not
		// reliably distinguish them. Drain and discard a bounded amount of
		// the body before returning: this provider caches nothing (see
		// Resolve's doc comment), so an absent secret is read again on
		// every poll tick, and leaving the body unread here would prevent
		// the connection being reused, paying a fresh TCP and TLS
		// handshake on every one of those ticks.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return accessResponse{}, fmt.Errorf("mamori/scaleway-sm: secret %q not found: %w", name, mamori.ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		// Read a bounded amount of the error body for diagnostics. Never log it.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		statusErr := fmt.Errorf("mamori/scaleway-sm: unexpected status %d accessing secret %q: %s",
			resp.StatusCode, name, strings.TrimSpace(string(msg)))
		return accessResponse{}, classifyStatus(resp.StatusCode, statusErr)
	}

	var body accessResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return accessResponse{}, fmt.Errorf("mamori/scaleway-sm: decoding access response for secret %q: %w: %w", name, mamori.ErrInvalid, err)
	}
	return body, nil
}

// sanitizeTransportError strips a *url.Error down to its underlying reason,
// discarding the request URL it would otherwise render into Error().
//
// The request URL built in access embeds the project id in its query string
// (see access's doc comment), and http.Client.Do wraps every
// transport-level failure - a refused connection, a timeout, a cancelled
// context - in a *url.Error whose Error() renders the full request URL.
// Without this, an ordinary network hiccup, not even a bug in this
// provider, would put the project id into a returned error's text. This is
// the same class of leak providers/cloudflare-kv and providers/vercel-gc
// each shipped and had to fix, reached here through the client's transport
// (and, for a malformed WithBaseURL, through url.Parse inside
// http.NewRequestWithContext itself) instead of a hand-rolled url.Parse
// call.
//
// Wrapping urlErr.Err with %w, rather than discarding it, keeps
// errors.Is(_, context.Canceled) (and similar checks) working: *url.Error
// already unwraps to the same underlying error via its own Unwrap method,
// so this changes only the rendered message, never the errors.Is chain.
//
// The final `return err` is deliberate, not a missed case: by construction
// only a *url.Error carries a rendered request URL, so any other error type
// already has nothing to strip, and wrapping it here would add noise
// without removing anything sensitive.
func sanitizeTransportError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("mamori/scaleway-sm: request failed: %w", urlErr.Err)
	}
	return err
}

// valueFor turns resp into a mamori.Value: it verifies resp.DataCrc32
// against the decoded payload when present, applies ref's #key selection
// when present, and reports the secret's revision as both Version and
// Metadata["revision"].
//
// Version is ALWAYS resp.Revision rendered as a decimal string, never a
// content hash and never affected by a #field selection that narrows the
// returned bytes. This is deliberate, and it is the entire reason this
// module exists in this trio: every sibling provider that reads a
// general-purpose config store falls back to mamori.VersionHash because its
// backend exposes no revision, which makes two byte-identical values at two
// different points in time indistinguishable to it. A real secret manager
// does not have that excuse - the revision already identifies exactly which
// write produced these bytes - so reporting anything else here would throw
// away information mamori.Value.Version was designed to carry. A #field
// selection changes which bytes are returned, not which secret version they
// came from, so Version stays the revision of the underlying secret even
// when the resolved payload is only part of it; it changes only when the
// secret itself is rewritten, i.e. when the revision advances.
func valueFor(resp accessResponse, ref mamori.Ref, region string) (mamori.Value, error) {
	if resp.DataCrc32 != nil {
		if got := crc32.ChecksumIEEE(resp.Data); got != *resp.DataCrc32 {
			return mamori.Value{}, fmt.Errorf(
				"mamori/scaleway-sm: data_crc32 mismatch (got %d, want %d): %w",
				got, *resp.DataCrc32, mamori.ErrInvalid)
		}
	}

	b := resp.Data
	if ref.Key != "" {
		sel, err := mamori.SelectKey(b, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		b = sel
	}

	revision := strconv.FormatUint(uint64(resp.Revision), 10)
	return mamori.Value{
		Bytes:     b,
		Version:   revision,
		Sensitive: true,
		// Metadata carries the region and the revision, and NOTHING else:
		// not the secret id, not the project id, not the path, and never
		// the value. A secret's location is itself information - this is a
		// secret manager, not a config store - and Metadata reaches the
		// admin HTTP endpoint and the status report, both broader-audience
		// surfaces than "whoever holds the resolved value".
		Metadata: map[string]string{
			"region":   region,
			"revision": revision,
		},
	}, nil
}

// classifyStatus maps a Secret Manager access-route status onto a mamori
// classification sentinel, wrapping statusErr so both the sentinel and the
// diagnostic context survive in the errors.Is chain. 404 is handled by its
// own branch in access and never reaches this function.
//
// The mapping follows ordinary HTTP semantics: 401 for a missing or invalid
// API secret key, 403 for a key that authenticates but lacks permission to
// read this secret, 429 for rate limiting, and 400 for a malformed request.
// One caveat is worth being explicit about, because it is exactly the kind
// of thing a misconfiguration hides behind rather than announces: a 404
// from this API does not distinguish an unknown secret from a KNOWN secret
// whose requested revision does not exist. A ref asking for ?revision=99
// against a secret that has only ever reached revision 12 gets the same 404
// an entirely absent secret name would, and therefore degrades silently to
// the field's default: or optional handling, exactly as if the secret had
// never existed at all. Scaleway has not published a stable enough
// error-code vocabulary in the response body to key this mapping on
// anything but the status, so codes not listed here report unknown rather
// than being guessed at.
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
