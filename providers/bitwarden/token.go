package bitwarden

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/xavidop/mamori"
)

// accessTokenVersion is the only version prefix Bitwarden has minted for a
// machine account access token. A different one means the format changed and
// guessing at it would be worse than refusing.
const accessTokenVersion = "0"

// accessTokenSecretSize is the length, in bytes, of the raw key carried after
// the ':' in an access token. It is the input to the HKDF derivation, not a
// key that is used directly.
const accessTokenSecretSize = 16

// accessToken is a parsed BWS_ACCESS_TOKEN.
//
// A Secrets Manager access token is three credentials in one string:
//
//	0.<uuid>.<client_secret>:<base64 16-byte key>
//
// The UUID is the OAuth2 client_id, the middle segment is the client_secret,
// and the trailing base64 is the key material that unwraps the organization
// key. Only the first of those is safe to render.
type accessToken struct {
	// id is the OAuth2 client_id, a UUID identifying the machine account. It
	// is an identifier rather than a credential, so it is an ordinary field
	// and is deliberately allowed into error messages: without it a
	// misconfigured deployment cannot tell which machine account failed.
	id string

	// clientSecret and key are behind closures so fmt's %v, %+v and %#v cannot
	// reach them. See the comment on symKey for why a field plus a String
	// method would not be enough.
	clientSecret func() string
	key          symKey
}

// parseAccessToken splits and validates a machine account access token and
// derives the key that decrypts the identity endpoint's encrypted payload.
//
// Every failure is mamori.ErrInvalid and none of them echoes any part of the
// token. That is stricter than it may look: the client_secret and the trailing
// key are both credentials, and even the shape of a malformed token is worth
// withholding when the usual cause is a truncated paste of a live one. The
// error says which structural expectation failed, which is enough to fix a
// misconfiguration without reproducing the material.
//
// Parsing is strict rather than forgiving because `mamori doctor` resolves
// every ref before deployment: a token that is subtly wrong should fail there,
// loudly, rather than become an authentication error in production.
func parseAccessToken(raw string) (accessToken, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return accessToken{}, fmt.Errorf("bitwarden: access token is empty; set BWS_ACCESS_TOKEN or pass WithAccessToken: %w", mamori.ErrInvalid)
	}

	head, keyB64, ok := strings.Cut(raw, ":")
	if !ok {
		return accessToken{}, fmt.Errorf("bitwarden: access token carries no ':' and so no encryption key: %w", mamori.ErrInvalid)
	}

	parts := strings.Split(head, ".")
	if len(parts) != 3 {
		return accessToken{}, fmt.Errorf("bitwarden: access token has %d '.'-separated parts before the ':', want 3: %w", len(parts), mamori.ErrInvalid)
	}
	version, id, clientSecret := parts[0], parts[1], parts[2]

	if version != accessTokenVersion {
		return accessToken{}, fmt.Errorf("bitwarden: access token version is %q, want %q: %w", version, accessTokenVersion, mamori.ErrInvalid)
	}
	if !isUUID(id) {
		// The id is not secret, but echoing it here would also echo it from a
		// token that was mistyped into some other credential entirely.
		return accessToken{}, fmt.Errorf("bitwarden: access token identifier is not a UUID: %w", mamori.ErrInvalid)
	}
	if clientSecret == "" {
		return accessToken{}, fmt.Errorf("bitwarden: access token carries an empty client secret: %w", mamori.ErrInvalid)
	}

	secret, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		// The decoder quotes its input, which is key material, so its error is
		// not wrapped.
		return accessToken{}, fmt.Errorf("bitwarden: access token encryption key is not valid base64: %w", mamori.ErrInvalid)
	}
	defer clear(secret)
	if len(secret) != accessTokenSecretSize {
		return accessToken{}, fmt.Errorf("bitwarden: access token encryption key is %d bytes, want %d: %w",
			len(secret), accessTokenSecretSize, mamori.ErrInvalid)
	}

	key, err := deriveAccessTokenKey(secret)
	if err != nil {
		return accessToken{}, err
	}

	return accessToken{
		id:           id,
		clientSecret: func() string { return clientSecret },
		key:          key,
	}, nil
}

// isUUID reports whether s is a canonical 8-4-4-4-12 hexadecimal UUID.
//
// This is a shape check, not a version or variant check: its only job is to
// catch a truncated or shuffled paste at parse time rather than as a 400 from
// the identity endpoint. Writing it out avoids a dependency for six lines,
// which matters in a module whose whole premise is the standard library.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}
