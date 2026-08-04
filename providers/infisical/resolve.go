package infisical

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// secretEnvelope is the read endpoint's response shape. The value is nested
// one level under "secret" rather than returned as raw bytes, unlike
// providers/cloudflare-kv's single-key GET, so there is always an unwrapping
// step here.
//
// Secret is a pointer so that a 200 carrying no "secret" object at all is
// distinguishable from one carrying an empty secret value, which is a legal
// value a ref may genuinely resolve to.
type secretEnvelope struct {
	Secret *struct {
		SecretKey   string `json:"secretKey"`
		SecretValue string `json:"secretValue"`
		Version     int64  `json:"version"`
	} `json:"secret"`
}

// Resolve fetches ref's secret from Infisical.
//
// There is deliberately no cache: mamori.Refresh and mamori.Doctor both call
// Resolve directly, and Infisical exposes no ETag or digest this provider could
// gate a held snapshot on, so every call is a live read of the current value.
// The access token IS cached, because it is a credential rather than a value
// and re-buying it on every poll would triple the request count.
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

	// projectId and secretPath always travel; environment only when one is
	// configured, since the API treats it as optional and sending an empty
	// value is not the same as omitting it.
	q := url.Values{
		"projectId":  {s.projectID},
		"secretPath": {s.secretPath},
	}
	if s.environment != "" {
		q.Set("environment", s.environment)
	}

	// url.PathEscape keeps a name containing a slash as ONE path segment.
	// httpcore joins Request.Path in its escaped form precisely so that works;
	// writing the name in unescaped would address a different, nested resource.
	resp, err := client.Do(ctx, httpcore.Request{
		Path:  secretsPath + url.PathEscape(name),
		Query: q,
	})
	if err != nil {
		// One %w: the cause already carries the sentinel httpcore classified it
		// with, and adding a second would replace that kind rather than
		// duplicate it.
		return mamori.Value{}, fmt.Errorf("mamori/infisical: reading secret %q: %w", name, err)
	}

	var env secretEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		// The decode error is dropped rather than wrapped. encoding/json quotes
		// the offending byte in a syntax error, and this body is, on the
		// success path, the secret itself: no fragment of it may reach an error
		// string. The status and the secret name are enough to act on.
		return mamori.Value{}, fmt.Errorf("mamori/infisical: secret %q response is not JSON: %w", name, mamori.ErrInvalid)
	}
	if env.Secret == nil {
		return mamori.Value{}, fmt.Errorf("mamori/infisical: secret %q response carried no \"secret\" object: %w", name, mamori.ErrInvalid)
	}

	value := []byte(env.Secret.SecretValue)
	body, err := mamori.SelectKey(value, ref.Key)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("mamori/infisical: secret %q: %w", name, err)
	}

	return mamori.Value{
		Bytes: body,
		// The version describes the whole secret, so it is derived from the
		// whole secret value rather than from the selected fragment: two refs
		// that select different #fields of one secret must agree on when it
		// changed.
		Version: versionOf(env.Secret.Version, value),
		// Always true. This is a secret manager; there is no per-ref or
		// per-provider switch, because there is no configuration-only mode of
		// Infisical for one to describe.
		Sensitive: true,
	}, nil
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
