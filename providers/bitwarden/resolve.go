package bitwarden

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// secretsPath is the API path prefix for a single secret read,
// GET /secrets/<id>.
const secretsPath = "secrets/"

// secretResponse is the subset of Bitwarden's SecretResponseModel this
// provider reads.
//
// Key, Value and Note are EncString ciphertext; the rest is plaintext the
// server can see. Only Value is decrypted: Key is the secret's name, which the
// caller already knows because they addressed it by id, and Note is operator
// commentary. Decrypting either would put plaintext this provider has no use
// for into memory.
type secretResponse struct {
	ID           string `json:"id"`
	Value        string `json:"value"`
	RevisionDate string `json:"revisionDate"`
}

// Resolve fetches the secret named by ref.Path, decrypts its value with the
// organization key, and selects ref.Key out of it when a fragment is present.
//
// The returned Value is always Sensitive: every value Secrets Manager holds is
// a secret by construction, unlike a generic HTTP endpoint that may serve
// either.
//
// Version is the secret's revisionDate rather than a hash of the plaintext.
// The API returns it beside the ciphertext in cleartext, so change detection
// costs nothing and never has to touch the decrypted value. A missing
// revisionDate falls back to the ciphertext itself, which is a faithful change
// signal (a re-encryption changes it) and is safe to hold as a version string
// because it is not the plaintext.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return mamori.Value{}, fmt.Errorf("bitwarden: provider is closed: %w", mamori.ErrUnavailable)
	}
	if p.err != nil {
		return mamori.Value{}, p.err
	}

	id, err := secretID(ref)
	if err != nil {
		return mamori.Value{}, err
	}

	// The organization key is fetched first so the single access-token
	// exchange is shared with the Authorization header the request below
	// carries: both read the same cache entry, so a cold start costs one
	// identity round trip rather than two.
	_, orgKey, err := p.sess.ensure(ctx)
	if err != nil {
		return mamori.Value{}, err
	}

	resp, err := p.api.Do(ctx, httpcore.Request{Path: secretsPath + id})
	if err != nil {
		return mamori.Value{}, fmt.Errorf("bitwarden: secret %s: %w", id, err)
	}

	var sr secretResponse
	if err := json.Unmarshal(resp.Body, &sr); err != nil {
		return mamori.Value{}, fmt.Errorf("bitwarden: secret %s: response is not JSON: %w: %w", id, mamori.ErrInvalid, err)
	}
	if sr.Value == "" {
		return mamori.Value{}, fmt.Errorf("bitwarden: secret %s: response carried no value: %w", id, mamori.ErrNotFound)
	}

	enc, err := parseEncString(sr.Value)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("bitwarden: secret %s: %w", id, err)
	}
	plaintext, err := orgKey.decrypt(enc)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("bitwarden: secret %s: %w", id, err)
	}

	version := sr.RevisionDate
	if version == "" {
		version = sr.Value
	}

	out, err := mamori.SelectKey(plaintext, ref.Key)
	if err != nil {
		return mamori.Value{}, selectError(id, ref.Key, err)
	}

	return mamori.Value{
		Bytes:     out,
		Version:   version,
		Sensitive: true,
	}, nil
}

// selectError rebuilds a mamori.SelectKey failure without its message.
//
// SelectKey is handed the DECRYPTED value, and on its "payload is not a JSON
// object" path it wraps encoding/json's own error, which quotes the input it
// choked on. Wrapping that through would put fragments of the plaintext into
// an error message, and error messages get logged. So the classification is
// preserved and the text is not, the same trade httpcore's OAuth2 exchange
// makes when it declines to repeat a cause derived from a client secret.
//
// Only the two sentinels SelectKey produces are distinguished, and anything
// unrecognized becomes ErrInvalid rather than ErrNotFound: ErrNotFound is the
// one kind that makes mamori apply a field's default, so it is never the safe
// guess.
func selectError(id, key string, err error) error {
	if errors.Is(err, mamori.ErrNotFound) {
		return fmt.Errorf("bitwarden: secret %s: %q is not present in the decrypted value: %w", id, key, mamori.ErrNotFound)
	}
	return fmt.Errorf("bitwarden: secret %s: cannot select %q from the decrypted value: %w", id, key, mamori.ErrInvalid)
}

// secretID extracts the secret UUID from a ref path.
//
// The UUID shape is validated here rather than left to the API for two
// reasons. A ref path reaches this provider with ${VAR} interpolation already
// applied, so an unset variable arrives as an empty or partial path, and a
// bare 404 would be indistinguishable from a secret that was deleted.
// ErrNotFound is also the one kind that makes mamori apply a field's default,
// so a malformed ref that classified as not-found would silently become a
// default value instead of an error.
func secretID(ref mamori.Ref) (string, error) {
	id := strings.Trim(ref.Path, "/")
	switch {
	case id == "":
		return "", fmt.Errorf("bitwarden: ref %q names no secret; want bitwarden-sm://<secret-uuid>: %w", ref.Raw, mamori.ErrInvalid)
	case strings.Contains(id, "/"):
		return "", fmt.Errorf("bitwarden: ref %q has more than one path segment; a secret is addressed by its UUID alone: %w", ref.Raw, mamori.ErrInvalid)
	case !isUUID(id):
		return "", fmt.Errorf("bitwarden: ref %q does not name a UUID; Bitwarden addresses a secret by its id, not its name: %w", ref.Raw, mamori.ErrInvalid)
	}
	return id, nil
}
