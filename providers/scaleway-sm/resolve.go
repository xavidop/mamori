package scalewaysm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"net/url"
	"strconv"
	"strings"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// errBodyLimit bounds how much of a failing response body reaches an error
// string, so a hostile or broken upstream cannot put an unbounded response
// into one. httpcore hands errorDetail at most Config.MaxBody bytes and always
// drains the rest on every branch, so the connection-reuse discipline this
// file used to hand-roll per branch now comes from one place; this constant is
// only about how much of that body is worth quoting.
const errBodyLimit = 4096

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
// The project id travels in the query string, which is exactly where
// httpcore's URL redaction removes it from: every error httpcore returns
// renders the request URL with its query and any userinfo stripped, and a
// transport failure's own *url.Error is rebuilt with that redacted URL rather
// than discarded, so errors.Is still reaches the underlying cause. That is
// what this package used to keep with a hand-rolled sanitizeTransportError at
// four call sites. The secret key never appears in a URL at all: it travels in
// the X-Auth-Token header, which is never rendered.
func (p *Provider) access(ctx context.Context, s settings, path, name, revision string) (accessResponse, error) {
	client, err := p.clientFor(s)
	if err != nil {
		return accessResponse{}, err
	}

	resp, err := client.Do(ctx, httpcore.Request{
		Path: "/regions/" + url.PathEscape(s.region) +
			"/secrets-by-path/versions/" + url.PathEscape(revision) + "/access",
		Query: url.Values{
			"secret_path": {path},
			"secret_name": {name},
			"project_id":  {s.projectID},
		},
	})
	if err != nil {
		if errors.Is(err, mamori.ErrNotFound) {
			// The secret name is absent, the requested revision does not
			// exist, or the requested revision is DISABLED (Scaleway makes a
			// disabled version inaccessible, not merely non-default - see the
			// package doc comment on the ?revision default); the response does
			// not reliably distinguish any of these.
			//
			// That a disabled revision arrives here as a 404 specifically is
			// the one part of this not confirmed from a published source.
			// Scaleway documents the inaccessibility but not the status it
			// returns, and 404 is the reading consistent with an inaccessible
			// version being absent from the caller's view. If it turns out to
			// be a 403, the sentinel changes from ErrNotFound to
			// ErrPermissionDenied and a disabled revision would fail loudly
			// rather than degrading to the field's default. The live
			// integration test is what would reveal it.
			return accessResponse{}, fmt.Errorf("mamori/scaleway-sm: secret %q not found: %w", name, mamori.ErrNotFound)
		}
		// One %w: the cause already carries the sentinel httpcore classified
		// it with, and adding a second would replace that kind rather than
		// duplicate it.
		return accessResponse{}, fmt.Errorf("mamori/scaleway-sm: accessing secret %q: %w", name, err)
	}

	var body accessResponse
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return accessResponse{}, fmt.Errorf("mamori/scaleway-sm: decoding access response for secret %q: %w: %w", name, mamori.ErrInvalid, err)
	}
	return body, nil
}

// clientFor builds the httpcore client for one read.
//
// It is built per call rather than cached because the secret key, project id
// and region are resolved per resolve, reading the environment lazily (see
// settingsFor): caching would pin whichever set happened to be resolved first.
// Construction performs no network call and reuses the provider's
// *http.Client, so the connection pool is shared across every read.
//
// The closed check runs first, so a closed provider refuses locally rather
// than building a client and reaching for the network.
func (p *Provider) clientFor(s settings) (*httpcore.Client, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("mamori/scaleway-sm: provider is closed: %w", mamori.ErrUnavailable)
	}

	c, err := httpcore.New(httpcore.Config{
		BaseURL:     p.baseURL,
		HTTPClient:  p.httpClient,
		Auth:        httpcore.HeaderAuth("X-Auth-Token", s.secretKey),
		ErrorDetail: errorDetail,
	})
	if err != nil {
		// The base URL is operator-supplied and carries no credential: the
		// project id is added per request as a query parameter, and the secret
		// key is a header. httpcore quotes only the base URL it was given.
		return nil, fmt.Errorf("mamori/scaleway-sm: building the API client: %w", err)
	}
	return c, nil
}

// errorDetail is httpcore's Config.ErrorDetail hook: it quotes a bounded
// prefix of a failing response's body into the error message.
//
// Embedding the body verbatim, bounded by errBodyLimit, is a deliberate house
// convention shared with providers/doppler, providers/cloudflare-kv and
// providers/vercel-gc: the verbatim text is what actually tells an operator
// what the upstream rejected the request for, and the bound is what stops a
// hostile or broken upstream turning that diagnostic into an unbounded string.
//
// httpcore calls this only for a status it has already decided is a failure,
// which is what makes it safe in a secret manager: on a 200 the body carries
// the secret payload, and that body is never shown this hook.
func errorDetail(_ int, body []byte) string {
	if len(body) > errBodyLimit {
		body = body[:errBodyLimit]
	}
	return strings.TrimSpace(string(body))
}

// valueFor turns resp into a mamori.Value: it verifies resp.DataCrc32
// against the decoded payload when present, applies ref's #key selection
// when present, and reports the secret's revision as both Version and
// Metadata["revision"].
//
// Version is resp.Revision rendered as a decimal string whenever the server
// reports one, never a content hash and never affected by a #field
// selection that narrows the returned bytes. Reporting a real backend
// version rather than a content hash is not unique to this module across the
// whole repo - providers/aws, providers/gcp, providers/azure,
// providers/vault, providers/k8s, and providers/onepassword all do the same
// for a secret-bearing backend, as do providers/etcd and providers/consul,
// general-purpose stores that happen to expose a native revision anyway -
// but within THIS trio of recent additions it is new: providers/vercel-gc
// and providers/cloudflare-kv both fall back to mamori.VersionHash because
// their backends expose no revision at all, which makes two byte-identical
// values at two different points in time indistinguishable to them. A real
// secret manager does not have that excuse - the revision already
// identifies exactly which write produced these bytes - so reporting
// anything else here would throw away information mamori.Value.Version was
// designed to carry. A #field selection changes which bytes are returned,
// not which secret version they came from, so Version stays the revision of
// the underlying secret even when the resolved payload is only part of it;
// it changes only when the secret itself is rewritten, i.e. when the
// revision advances.
//
// Scaleway numbers real revisions from 1 (see the package doc comment on
// the ?revision default), so resp.Revision == 0 means the field was absent
// from the response, not a legitimate "revision zero" - unlikely against the
// real API, but not a case this provider may silently mishandle: mamori's
// poller detects a rotation by comparing Version, and an unguarded Version
// of a constant "0" would make every subsequent write invisible to it
// forever. Every comparable provider in this repo falls back to
// mamori.VersionHash in exactly this situation (providers/aws/sm.go,
// providers/gcp/gcp.go, providers/azure/azure.go, providers/vault/vault.go),
// and this module does the same, hashing resp.Data - the full payload,
// before any #field selection - so the fallback still honors the same
// "Version does not depend on #field" guarantee the real-revision path gives
// above.
func valueFor(resp accessResponse, ref mamori.Ref, region string) (mamori.Value, error) {
	if resp.DataCrc32 != nil {
		// Neither the computed sum nor the server's own data_crc32 is
		// rendered into the error: nothing derived from the secret may reach
		// an error message, and saying the CRC did not match is enough.
		if crc32.ChecksumIEEE(resp.Data) != *resp.DataCrc32 {
			return mamori.Value{}, fmt.Errorf("mamori/scaleway-sm: data_crc32 mismatch: %w", mamori.ErrInvalid)
		}
	}

	version := strconv.FormatUint(uint64(resp.Revision), 10)
	if resp.Revision == 0 {
		version = mamori.VersionHash(resp.Data)
	}

	b := resp.Data
	if ref.Key != "" {
		sel, err := mamori.SelectKey(b, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		b = sel
	}

	return mamori.Value{
		Bytes:     b,
		Version:   version,
		Sensitive: true,
		// Metadata carries the region and the revision, and NOTHING else:
		// not the secret id, not the project id, not the path, and never
		// the value. A secret's location is itself information - this is a
		// secret manager, not a config store - and Metadata reaches the
		// admin HTTP endpoint and the status report, both broader-audience
		// surfaces than "whoever holds the resolved value". "revision" is
		// simply version, so in the resp.Revision == 0 fallback above it
		// holds a content hash rather than a numbered revision, same as
		// Version does; that discloses nothing new, but a reader should not
		// assume this key is always a number. That fallback should not occur
		// against the real API in the first place (see Version's paragraph
		// above).
		Metadata: map[string]string{
			"region":   region,
			"revision": version,
		},
	}, nil
}

// Status classification comes from httpcore.ClassifyStatus, inside
// httpcore.Client.Do, rather than from a mapping copied into this package. 404
// is the one status access takes back, to give it a message of its own.
//
// One caveat of that 404 is worth being explicit about, because it is exactly
// the kind of thing a misconfiguration hides behind rather than announces: a
// 404 from this API does not distinguish an unknown secret from a KNOWN secret
// whose requested revision does not exist OR whose requested revision has been
// disabled - disabling a version makes it inaccessible on Scaleway, not merely
// non-default, so a caller who pinned ?revision=latest before a revocation
// gets the identical 404 an entirely absent secret would. Either way it
// degrades silently to the field's default or optional handling, exactly as if
// the secret had never existed at all - see the package doc comment for why
// ?revision defaults to "latest_enabled" rather than "latest" for precisely
// this reason. Scaleway has not published a stable enough error-code
// vocabulary in the response body to key anything on but the status.
