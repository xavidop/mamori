// Package authjwt is a JWT [mamori.Authenticator] for mamori's admin HTTP
// endpoint and the config server. It lives outside the core mamori module
// because it depends on a JWT library (github.com/golang-jwt/jwt/v5), and the
// core module takes no non-stdlib dependencies.
//
// Usage:
//
//	auth, err := authjwt.New(authjwt.Config{
//	    Key:       authjwt.HMAC(secretBytes),
//	    Issuer:    "https://issuer.example.com/",
//	    Audiences: []string{"mamori-admin"},
//	    Claims:    []string{"groups", "scope"},
//	})
//	if err != nil {
//	    return err
//	}
//
//	w, err := mamori.Watch[Config](ctx,
//	    mamori.WithAdminHTTP("127.0.0.1:9090", mamori.WithAuth(auth)),
//	)
//
// # Security posture
//
// New validates its Config up front and refuses to build an authenticator for
// a nonsensical configuration, rather than failing (or worse, silently
// under-checking) on the first request:
//
//   - The set of algorithms Authenticate will accept is always fixed at
//     construction, never inferred per-token from the token's own "alg"
//     header. This is enforced with jwt.WithValidMethods, restricted to
//     exactly the algorithms implied by the configured key type. A token
//     signed with "alg": "none" is rejected because "none" is never in that
//     set. A token signed with HS256 while an RSA (or ECDSA, or EdDSA) key is
//     configured is rejected for the same reason: this is what defeats the
//     classic RSA/HMAC key-confusion attack, where an attacker signs a
//     forged token with HS256 using the server's own RSA *public* key
//     (public, and therefore known to the attacker) as the HMAC secret.
//   - Every key-material constructor's Keyfunc additionally checks the
//     signing method's concrete type before returning key material, as
//     defense in depth against ever handing key material of one family to a
//     verification path for another.
//   - Expiration is mandatory: jwt.WithExpirationRequired rejects a token
//     with no "exp" claim, not only an expired one.
//   - Issuer and audience, when configured, are validated with
//     jwt.WithIssuer / jwt.WithAudience.
//   - The token is read only from the "Authorization" header with a
//     case-insensitive "Bearer " scheme prefix; anything else (no header, a
//     different scheme, an empty token) is rejected.
//
// A missing, malformed, or invalid token is unauthenticated: Authenticate
// returns a plain error (never [mamori.ErrForbidden]), so the admin HTTP
// handler answers 401, and the returned authenticator implements
// [mamori.Challenger] to send "WWW-Authenticate: Bearer" (with a realm, if
// configured).
package authjwt

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/xavidop/mamori"
)

// KeyOption supplies both the key material used to verify a token's
// signature and the set of algorithms Authenticate will accept. Obtain one
// from [HMAC], [RSAPublicKey], [ECDSAPublicKey], or [EdDSAPublicKey]: each
// constructor fixes the allowed algorithms to exactly those valid for that
// key type, so a caller cannot accidentally configure an unrestricted
// algorithm set (the classic alg-confusion footgun). The zero KeyOption is
// invalid; it is only ever produced by one of the constructors above.
type KeyOption struct {
	keyfunc    jwt.Keyfunc
	algorithms []string
}

func (k KeyOption) isZero() bool { return k.keyfunc == nil }

// HMAC returns a KeyOption that verifies a token's signature against secret
// using the HMAC-SHA family, restricting the accepted algorithms to
// HS256, HS384, and HS512.
func HMAC(secret []byte) KeyOption {
	key := append([]byte(nil), secret...)
	return KeyOption{
		keyfunc: func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("authjwt: unexpected signing method %T for an HMAC key", t.Method)
			}
			return key, nil
		},
		algorithms: []string{"HS256", "HS384", "HS512"},
	}
}

// RSAPublicKey returns a KeyOption that verifies a token's signature against
// key, restricting the accepted algorithms to RS256, RS384, RS512, PS256,
// PS384, and PS512 (PKCS#1 v1.5 and RSA-PSS).
func RSAPublicKey(key *rsa.PublicKey) KeyOption {
	return KeyOption{
		keyfunc: func(t *jwt.Token) (any, error) {
			switch t.Method.(type) {
			case *jwt.SigningMethodRSA, *jwt.SigningMethodRSAPSS:
				return key, nil
			default:
				return nil, fmt.Errorf("authjwt: unexpected signing method %T for an RSA key", t.Method)
			}
		},
		algorithms: []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512"},
	}
}

// ECDSAPublicKey returns a KeyOption that verifies a token's signature
// against key, restricting the accepted algorithms to ES256, ES384, and
// ES512.
func ECDSAPublicKey(key *ecdsa.PublicKey) KeyOption {
	return KeyOption{
		keyfunc: func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodECDSA); !ok {
				return nil, fmt.Errorf("authjwt: unexpected signing method %T for an ECDSA key", t.Method)
			}
			return key, nil
		},
		algorithms: []string{"ES256", "ES384", "ES512"},
	}
}

// EdDSAPublicKey returns a KeyOption that verifies a token's signature
// against key, restricting the accepted algorithm to EdDSA (Ed25519).
func EdDSAPublicKey(key ed25519.PublicKey) KeyOption {
	return KeyOption{
		keyfunc: func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
				return nil, fmt.Errorf("authjwt: unexpected signing method %T for an EdDSA key", t.Method)
			}
			return key, nil
		},
		algorithms: []string{"EdDSA"},
	}
}

// Config configures New. Exactly one of Key or Keyfunc supplies the key
// material:
//
//   - Key, built from [HMAC], [RSAPublicKey], [ECDSAPublicKey], or
//     [EdDSAPublicKey], is the normal path: it supplies both the key and the
//     algorithms it is valid for, so the two can never drift apart.
//   - Keyfunc is the escape hatch for cases the helpers do not cover, most
//     commonly a JWKS endpoint with key rotation (a jwt.Keyfunc that picks a
//     key by the token's "kid" header). Because Keyfunc can return key
//     material for any algorithm the caller's function is willing to
//     produce, Algorithms MUST also be set explicitly when Keyfunc is used;
//     leaving it empty is a Config error rather than a permissive default,
//     since an unrestricted algorithm set reopens the alg-confusion
//     vulnerability the Key helpers exist to close. Algorithms must not be
//     set together with Key: Key already fixes the allowed algorithms, and
//     allowing Algorithms to widen or override that would defeat the point
//     of the helper.
type Config struct {
	// Key supplies key material via one of HMAC, RSAPublicKey,
	// ECDSAPublicKey, or EdDSAPublicKey. Exactly one of Key or Keyfunc must
	// be set.
	Key KeyOption

	// Keyfunc supplies key material directly, for cases such as JWKS with
	// key rotation. Exactly one of Key or Keyfunc must be set. Algorithms is
	// required whenever Keyfunc is set.
	Keyfunc jwt.Keyfunc

	// Algorithms is the set of JWT "alg" values Authenticate accepts. It is
	// required (and only used) alongside Keyfunc; it must be left unset
	// alongside Key, whose constructor already fixes it.
	Algorithms []string

	// Issuer, if set, must match the token's "iss" claim exactly.
	Issuer string

	// Audiences, if non-empty, requires the token's "aud" claim to contain
	// at least one of these values.
	Audiences []string

	// SubjectClaim names the claim copied into Identity.Subject. Defaults to
	// "sub".
	SubjectClaim string

	// Claims lists additional claim names to copy into Identity.Attrs. A
	// string-valued claim becomes a single-element slice; a []string or
	// JSON array of strings becomes a multi-element slice; the "scope" and
	// "scp" claims are treated specially even when string-valued, and are
	// split on spaces into multiple values, matching how OAuth2/OIDC encode
	// a space-delimited scope list as a single string claim. A claim listed
	// here that is absent from the token, or whose value is not a
	// recognized shape, is simply absent from Attrs; it is not an error.
	Claims []string

	// Realm, if set, is sent as the realm parameter of the
	// "WWW-Authenticate: Bearer" challenge. Optional.
	Realm string
}

// scopeClaimNames are the claim names, per common OAuth2/IdP convention,
// whose string value is a space-delimited list rather than a single value.
var scopeClaimNames = map[string]bool{
	"scope": true,
	"scp":   true,
}

// New builds a JWT [mamori.Authenticator] from cfg. It validates cfg and
// returns an error for a nonsensical configuration (missing key material, or
// a raw Keyfunc without an explicit algorithm allowlist) instead of
// deferring that failure to the first request.
//
// The returned Authenticator also implements [mamori.Challenger], sending
// "WWW-Authenticate: Bearer" (with a realm, if cfg.Realm is set) on a failed
// request.
func New(cfg Config) (mamori.Authenticator, error) {
	hasKey := !cfg.Key.isZero()
	hasKeyfunc := cfg.Keyfunc != nil

	switch {
	case hasKey && hasKeyfunc:
		return nil, errors.New("authjwt: Config must set exactly one of Key or Keyfunc, not both")
	case !hasKey && !hasKeyfunc:
		return nil, errors.New("authjwt: Config requires a key source: set Key (see HMAC, RSAPublicKey, ECDSAPublicKey, EdDSAPublicKey) or Keyfunc with Algorithms")
	}

	var keyfunc jwt.Keyfunc
	var algorithms []string
	if hasKey {
		if len(cfg.Algorithms) > 0 {
			return nil, errors.New("authjwt: Algorithms must not be set together with Key; Key already fixes the allowed algorithms")
		}
		keyfunc = cfg.Key.keyfunc
		algorithms = cfg.Key.algorithms
	} else {
		if len(cfg.Algorithms) == 0 {
			return nil, errors.New("authjwt: Keyfunc requires Algorithms to be set explicitly; an unrestricted algorithm set enables alg-confusion attacks")
		}
		keyfunc = cfg.Keyfunc
		algorithms = append([]string(nil), cfg.Algorithms...)
	}

	subjectClaim := cfg.SubjectClaim
	if subjectClaim == "" {
		subjectClaim = "sub"
	}

	return &jwtAuth{
		keyfunc:      keyfunc,
		algorithms:   algorithms,
		issuer:       cfg.Issuer,
		audiences:    append([]string(nil), cfg.Audiences...),
		subjectClaim: subjectClaim,
		claims:       append([]string(nil), cfg.Claims...),
		realm:        cfg.Realm,
	}, nil
}

type jwtAuth struct {
	keyfunc      jwt.Keyfunc
	algorithms   []string
	issuer       string
	audiences    []string
	subjectClaim string
	claims       []string
	realm        string
}

const authorizationHeader = "Authorization"

// Authenticate extracts a bearer token from the Authorization header and
// verifies it: signature (against an algorithm-restricted keyfunc, see
// [KeyOption]), expiration (required), and issuer/audience when configured.
// A missing header, a non-Bearer scheme, an empty token, or any verification
// failure is unauthenticated: it returns a plain error, never
// [mamori.ErrForbidden], and never one that echoes the presented token or
// the token's claims.
func (a *jwtAuth) Authenticate(r *http.Request) (mamori.Identity, error) {
	tokenString, err := bearerToken(r)
	if err != nil {
		return mamori.Identity{}, err
	}

	parserOpts := []jwt.ParserOption{
		jwt.WithValidMethods(a.algorithms),
		jwt.WithExpirationRequired(),
	}
	if a.issuer != "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(a.issuer))
	}
	if len(a.audiences) > 0 {
		parserOpts = append(parserOpts, jwt.WithAudience(a.audiences...))
	}

	claims := jwt.MapClaims{}
	if _, err := jwt.ParseWithClaims(tokenString, claims, a.keyfunc, parserOpts...); err != nil {
		// Wrap the library's error (which names only claim/algorithm names,
		// e.g. "token is expired", never claim values) but never the token
		// itself: the token and the full claim set must never land anywhere
		// that could be logged.
		return mamori.Identity{}, fmt.Errorf("authjwt: invalid token: %w", err)
	}

	return mamori.Identity{
		Subject: subjectFromClaims(claims, a.subjectClaim),
		Attrs:   attrsFromClaims(claims, a.claims),
	}, nil
}

// Challenge implements [mamori.Challenger].
func (a *jwtAuth) Challenge() string {
	if a.realm == "" {
		return "Bearer"
	}
	return fmt.Sprintf("Bearer realm=%q", a.realm)
}

// bearerToken extracts the token from the Authorization header. It accepts
// only a case-insensitive "Bearer" scheme; a missing header, any other
// scheme, or an empty token is rejected.
func bearerToken(r *http.Request) (string, error) {
	header := r.Header.Get(authorizationHeader)
	if header == "" {
		return "", errors.New("authjwt: missing authorization header")
	}
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", errors.New("authjwt: authorization header is not a bearer token")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("authjwt: empty bearer token")
	}
	return token, nil
}

// subjectFromClaims returns the string value of claims[name], or "" if the
// claim is absent or not a string.
func subjectFromClaims(claims jwt.MapClaims, name string) string {
	v, ok := claims[name]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// attrsFromClaims copies each of names, when present in claims, into an
// Identity.Attrs-shaped map; see Config.Claims for the value-shape mapping.
// A name absent from claims, or whose value has no recognized shape, is
// simply omitted, never an error.
func attrsFromClaims(claims jwt.MapClaims, names []string) map[string][]string {
	if len(names) == 0 {
		return nil
	}
	var attrs map[string][]string
	for _, name := range names {
		v, ok := claims[name]
		if !ok {
			continue
		}
		values := claimValues(name, v)
		if len(values) == 0 {
			continue
		}
		if attrs == nil {
			attrs = make(map[string][]string, len(names))
		}
		attrs[name] = values
	}
	return attrs
}

// claimValues converts a single decoded claim value into Attrs's []string
// shape: a string is one value, unless name is "scope"/"scp", in which case
// it is split on whitespace into multiple values; a []string, or a JSON
// array decoded as []any whose elements are all strings, becomes one value
// per element. Any other shape yields no values.
func claimValues(name string, v any) []string {
	switch val := v.(type) {
	case string:
		if scopeClaimNames[name] {
			return strings.Fields(val)
		}
		return []string{val}
	case []string:
		return append([]string(nil), val...)
	case []any:
		values := make([]string, 0, len(val))
		for _, item := range val {
			s, ok := item.(string)
			if !ok {
				continue
			}
			values = append(values, s)
		}
		return values
	default:
		return nil
	}
}
