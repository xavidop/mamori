package hcpvaultsecrets

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// secretEnvelope is the OpenAppSecret response shape. The value is nested two
// levels down, under "secret" then "static_version", rather than returned as
// raw bytes the way providers/cloudflare-kv's single-key GET is, so there is
// always an unwrapping step here.
//
// Both levels are pointers so that a 200 carrying no object at all is
// distinguishable from one carrying an empty value, which is a legal value a
// ref may genuinely resolve to.
//
// Only the static-secret shape is decoded. HCP Vault Secrets also serves
// rotating secrets (rotating_version.values, a map) and dynamic secrets
// (dynamic_instance.values), and this provider deliberately does not guess at
// either: see Resolve's handling of a nil StaticVersion, and the README's
// "Static secrets only".
type secretEnvelope struct {
	Secret *struct {
		Name string `json:"name"`
		// Type is "kv" for a static secret, and something else for a rotating
		// or dynamic one. It is read only to name the problem in an error.
		Type          string `json:"type"`
		StaticVersion *struct {
			Version int64  `json:"version"`
			Value   string `json:"value"`
		} `json:"static_version"`
	} `json:"secret"`
}

// Resolve fetches ref's secret from HCP Vault Secrets.
//
// There is deliberately no cache: mamori.Refresh and mamori.Doctor both call
// Resolve directly, and the OpenAppSecret endpoint exposes no ETag or digest
// this provider could gate a held snapshot on, so every call is a live read of
// the current value. The access token IS cached, by
// httpcore.OAuth2ClientCredentials, because it is a credential rather than a
// value and re-buying it on every poll would double the request count.
//
// ref.Key is handed to mamori.SelectKey, so a fragment is either an RFC 6901
// JSON Pointer or a literal top-level key, identically to every other provider.
//
// A ref path containing a "." or ".." segment is rejected with
// mamori.ErrInvalid. That check is not here: httpcore.Client.Do enforces it for
// every provider built on it, in both the literal and the percent-encoded form,
// so no provider can forget it.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	name, err := secretNameOf(ref)
	if err != nil {
		return mamori.Value{}, err
	}
	s, err := p.settingsFor(ref)
	if err != nil {
		return mamori.Value{}, err
	}
	client, err := p.clientFor()
	if err != nil {
		return mamori.Value{}, err
	}

	resp, err := client.Do(ctx, httpcore.Request{Path: secretPath(s, name)})
	if err != nil {
		// One %w: the cause already carries the sentinel httpcore classified it
		// with, and adding a second would replace that kind rather than
		// duplicate it.
		return mamori.Value{}, fmt.Errorf("mamori/hcp-vs: reading secret %q: %w", name, err)
	}

	var env secretEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		// The decode error is dropped rather than wrapped. encoding/json quotes
		// the offending byte in a syntax error, and this body is, on the
		// success path, the secret itself: no fragment of it may reach an error
		// string. The status and the secret name are enough to act on.
		return mamori.Value{}, fmt.Errorf("mamori/hcp-vs: secret %q response is not JSON: %w", name, mamori.ErrInvalid)
	}
	if env.Secret == nil {
		return mamori.Value{}, fmt.Errorf("mamori/hcp-vs: secret %q response carried no \"secret\" object: %w", name, mamori.ErrInvalid)
	}
	if env.Secret.StaticVersion == nil {
		// A rotating or dynamic secret, whose value is a MAP under a different
		// key, reaches here. Failing loudly is the honest answer: this package
		// has not pinned those shapes against a live backend, and returning an
		// empty value would look like a successful read of an empty secret.
		//
		// ErrInvalid, not ErrNotFound: the secret exists, so reporting it as
		// absent would make mamori apply the field's default and hide a real
		// configuration mismatch.
		return mamori.Value{}, fmt.Errorf("mamori/hcp-vs: secret %q has no static_version (type %q); this provider reads static secrets only: %w",
			name, env.Secret.Type, mamori.ErrInvalid)
	}

	value := []byte(env.Secret.StaticVersion.Value)
	body, err := mamori.SelectKey(value, ref.Key)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("mamori/hcp-vs: secret %q: %w", name, err)
	}

	return mamori.Value{
		Bytes: body,
		// The version describes the whole secret, so it is derived from the
		// whole secret value rather than from the selected fragment: two refs
		// that select different #fields of one secret must agree on when it
		// changed.
		Version: versionOf(env.Secret.StaticVersion.Version, value),
		// Always true. This is a secret manager; there is no per-ref or
		// per-provider switch, because there is no configuration-only mode of
		// HCP Vault Secrets for one to describe.
		Sensitive: true,
	}, nil
}

// secretPath builds the OpenAppSecret path for one secret.
//
// The shape is fixed by the API and every segment is required:
//
//	/secrets/{version}/organizations/{org}/projects/{proj}/apps/{app}/secrets/{name}:open
//
// The trailing ":open" is a Google AIP custom method, not a path segment, which
// is why it is appended after the name rather than being part of it. A colon is
// a legal path character that neither url.PathEscape nor url.URL.EscapedPath
// touches, so the suffix survives to the wire and the two can only ever be told
// apart by position.
//
// The scope segments and the NAME are escaped differently, deliberately.
//
// The organization, project and application come from options or the
// environment, so they are decoded values and are escaped here. They are a UUID
// and an HCP-restricted name today, so none can currently carry a slash, but
// escaping costs nothing and means a future relaxation cannot silently turn one
// segment into several.
//
// The name is passed through UNESCAPED, because it comes from ref.Path and
// mamori.ParseRef performs no percent-decoding: ref.Path is the literal text of
// the source tag, which is already the escaped form httpcore.Request.Path
// documents itself to take. Escaping it again would be wrong twice over. It
// would turn the "%2F" an operator wrote to name a secret containing a slash
// into "%252F", addressing a different secret whose name contains a literal
// percent-two-F. And it would DISABLE httpcore's traversal check: that check
// decodes the path once and looks for a "." or ".." segment, so a
// double-encoded "..%252F.." decodes to "..%2F..", one opaque segment, and
// passes. Passing the name through restores both.
func secretPath(s settings, name string) string {
	return "/secrets/" + apiVersion +
		"/organizations/" + url.PathEscape(s.organizationID) +
		"/projects/" + url.PathEscape(s.projectID) +
		"/apps/" + url.PathEscape(s.appName) +
		"/secrets/" + name + ":open"
}

// versionOf renders the backend's own revision number as mamori's Version,
// falling back to a content hash when the backend supplied none.
//
// A backend revision is strictly better than a hash: it costs nothing to
// compare and it distinguishes a rewrite of the same bytes from no write at
// all. The fallback matters anyway, because rendering an absent version as "0"
// would pin Version to a constant and make change detection impossible for
// every ref at once - the failure mode a poller cannot report, since nothing
// ever looks changed.
func versionOf(version int64, value []byte) string {
	if version > 0 {
		return strconv.FormatInt(version, 10)
	}
	return mamori.VersionHash(value)
}
