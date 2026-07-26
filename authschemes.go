package mamori

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/xavidop/mamori/secret"
)

// BasicAuth authenticates HTTP Basic credentials against a fixed user and
// password. Both the username and the password are compared in constant
// time (see basicAuth.Authenticate), so a failed request discloses neither
// the password through response timing nor whether user is even the right
// username. pass is a secret.String so it redacts in logs and error values.
func BasicAuth(user string, pass secret.String) Authenticator {
	return BasicAuthFunc(func() (string, secret.String) { return user, pass })
}

// BasicAuthFunc reads the expected username and password on every request
// rather than freezing them at construction, so a mamori Watcher can rotate
// the admin credential live. If fn returns a zero password (secret.String's
// IsZero), every request is denied: this is what makes rotation safe during
// the window before the credential has been populated for the first time,
// since the alternative, treating "unset" as "no password required", would
// silently open the endpoint.
func BasicAuthFunc(fn func() (string, secret.String)) Authenticator {
	return basicAuth{fn: fn}
}

type basicAuth struct {
	fn func() (string, secret.String)
}

// Authenticate compares both the presented username and password against the
// expected values with crypto/subtle.ConstantTimeCompare. A naive == or
// bytes.Equal short-circuits on the first mismatching byte, which lets an
// attacker recover a valid username or password one byte at a time by timing
// repeated requests; constant-time comparison always walks the full length of
// both operands, so the timing carries no information about where (or
// whether) they diverge.
func (b basicAuth) Authenticate(r *http.Request) (Identity, error) {
	wantUser, wantPass := b.fn()
	if wantPass.IsZero() {
		// Fail closed: an unconfigured credential must never be treated as "no
		// auth required". Deny unconditionally without even looking at r.
		return Identity{}, errors.New("mamori: basic auth not configured")
	}
	gotUser, gotPass, ok := r.BasicAuth()
	if !ok {
		return Identity{}, errors.New("mamori: missing basic credentials")
	}
	userOK := subtle.ConstantTimeCompare([]byte(gotUser), []byte(wantUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(gotPass), wantPass.RevealBytes()) == 1
	if !userOK || !passOK {
		// Name the failure, never the presented value: a message that echoed
		// gotUser or gotPass would leak the credential into logs and error
		// reporting pipelines that are not held to the same secrecy bar as the
		// auth path itself.
		return Identity{}, errors.New("mamori: invalid basic credentials")
	}
	return Identity{Subject: wantUser}, nil
}

// Challenge implements Challenger so a failed request gets a standard
// WWW-Authenticate header, prompting a browser or HTTP client to prompt for
// credentials.
func (b basicAuth) Challenge() string { return `Basic realm="mamori"` }

// BearerToken authenticates requests carrying "Authorization: Bearer <token>"
// against a fixed token. The token is compared in constant time (see
// bearerToken.Authenticate) so a failed request discloses nothing about the
// token through response timing.
func BearerToken(token secret.String) Authenticator {
	return BearerTokenFunc(func() secret.String { return token })
}

// BearerTokenFunc reads the expected token on every request rather than
// freezing it at construction, so a mamori Watcher can rotate it live. A zero
// token (secret.String's IsZero) denies every request; see BasicAuthFunc for
// why unset must never be treated as unauthenticated-allowed.
func BearerTokenFunc(fn func() secret.String) Authenticator {
	return bearerToken{fn: fn}
}

type bearerToken struct {
	fn func() secret.String
}

const bearerPrefix = "Bearer "

// Authenticate extracts the token from the Authorization header and compares
// it against the expected value with crypto/subtle.ConstantTimeCompare. The
// "Bearer " prefix itself is checked with strings.HasPrefix rather than in
// constant time: the prefix is a fixed, public protocol string, not part of
// the secret, so there is nothing to protect by slowing that check down. Only
// the remainder, the actual token, goes through the constant-time compare.
func (b bearerToken) Authenticate(r *http.Request) (Identity, error) {
	want := b.fn()
	if want.IsZero() {
		return Identity{}, errors.New("mamori: bearer token not configured")
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, bearerPrefix) {
		return Identity{}, errors.New("mamori: missing bearer token")
	}
	presented := strings.TrimPrefix(header, bearerPrefix)
	if subtle.ConstantTimeCompare([]byte(presented), want.RevealBytes()) != 1 {
		return Identity{}, errors.New("mamori: invalid bearer token")
	}
	return Identity{Subject: "bearer"}, nil
}

// Challenge implements Challenger. Bearer's WWW-Authenticate value carries no
// realm by convention (RFC 6750 does not require one).
func (b bearerToken) Challenge() string { return "Bearer" }

// APIKey authenticates requests carrying the expected key in a named header
// (for example "X-API-Key") against a fixed key. The key is compared in
// constant time (see apiKey.Authenticate) so a failed request discloses
// nothing about the key through response timing.
func APIKey(header string, key secret.String) Authenticator {
	return APIKeyFunc(header, func() secret.String { return key })
}

// APIKeyFunc reads the expected key on every request rather than freezing it
// at construction, so a mamori Watcher can rotate it live. A zero key
// (secret.String's IsZero) denies every request; see BasicAuthFunc for why
// unset must never be treated as unauthenticated-allowed.
func APIKeyFunc(header string, fn func() secret.String) Authenticator {
	return apiKey{header: header, fn: fn}
}

type apiKey struct {
	header string
	fn     func() secret.String
}

// Authenticate reads a.header from the request and compares it against the
// expected key with crypto/subtle.ConstantTimeCompare, for the same reason as
// BasicAuth and BearerToken: an == or bytes.Equal comparison would leak the
// key a byte at a time through response timing.
//
// APIKey implements no Challenger: an API key is not a scheme a browser or
// generic HTTP client knows how to respond to a WWW-Authenticate challenge
// for, so a failed request gets a bare 401.
func (a apiKey) Authenticate(r *http.Request) (Identity, error) {
	want := a.fn()
	if want.IsZero() {
		return Identity{}, errors.New("mamori: api key not configured")
	}
	got := r.Header.Get(a.header)
	if got == "" {
		return Identity{}, errors.New("mamori: missing api key")
	}
	if subtle.ConstantTimeCompare([]byte(got), want.RevealBytes()) != 1 {
		return Identity{}, errors.New("mamori: invalid api key")
	}
	return Identity{Subject: "apikey"}, nil
}

// MTLSOptions configures certificate-based authentication. Both fields are
// optional allowlists: a name matching either is accepted. If both are empty,
// any client certificate that the TLS stack has already verified is accepted,
// on the theory that verification itself (see MTLS) is the security boundary
// and a further, unconfigured name check would only be security theater.
type MTLSOptions struct {
	// AllowedCNs, if non-empty, permits only certificates whose
	// Subject.CommonName is in this list.
	AllowedCNs []string
	// AllowedDNSNames, if non-empty, permits only certificates with at least
	// one DNS SAN in this list.
	AllowedDNSNames []string
}

// MTLS authenticates a client by its verified TLS client certificate. It
// requires the server to be configured with tls.RequireAndVerifyClientCert
// (see WithAdminTLS): the Go TLS stack has already validated the certificate
// chain by the time Authenticate runs, so this scheme only needs to check
// which verified identity is allowed, not whether the certificate is
// trustworthy. On a non-TLS connection, or a TLS connection presenting no
// client certificate, MTLS denies every request; there is no fallback and no
// separate secret, so for an endpoint exposing operational detail about a
// cluster's secret material this is the strongest option that needs no
// external dependency.
func MTLS(opts MTLSOptions) Authenticator { return mtls{opts: opts} }

type mtls struct {
	opts MTLSOptions
}

// Authenticate denies whenever there is no verified peer certificate to check
// (no TLS at all, or TLS without client-cert verification), then checks the
// leaf certificate's CommonName and DNS SANs against the configured
// allowlists. These are public identifiers on a certificate the TLS
// handshake has already cryptographically verified, not shared secrets, so
// (unlike the password/token/key comparisons above) an ordinary string
// comparison is appropriate here: there is no secret value for timing to
// leak.
func (m mtls) Authenticate(r *http.Request) (Identity, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return Identity{}, errors.New("mamori: no client certificate presented")
	}
	leaf := r.TLS.PeerCertificates[0]
	if len(m.opts.AllowedCNs) == 0 && len(m.opts.AllowedDNSNames) == 0 {
		return Identity{Subject: leaf.Subject.CommonName}, nil
	}
	for _, cn := range m.opts.AllowedCNs {
		if cn == leaf.Subject.CommonName {
			return Identity{Subject: leaf.Subject.CommonName}, nil
		}
	}
	for _, allowed := range m.opts.AllowedDNSNames {
		for _, dns := range leaf.DNSNames {
			if allowed == dns {
				return Identity{Subject: leaf.Subject.CommonName}, nil
			}
		}
	}
	return Identity{}, errors.New("mamori: client certificate not permitted")
}

// AnyOf allows a request if any member allows it, for composing schemes such
// as "a static admin token OR mTLS from the mesh sidecar". It evaluates every
// member on every request, even after one has already succeeded or failed,
// so the total work AnyOf performs never depends on which member matched (or
// on how early in the list a mismatch was found). Without this, an attacker
// could measure response timing to learn which scheme very nearly accepted
// their request, narrowing an attack from "guess a token" to "guess a token
// this scheme almost validated".
//
// On total failure it returns an error naming no presented credential (see
// each member's own Authenticate). If any member implements Challenger, the
// first such member in argument order determines AnyOf's own Challenge; if
// none do, the returned Authenticator implements no Challenger either, and a
// failed request gets a bare 401, matching what a single member with the same
// property would produce.
func AnyOf(as ...Authenticator) Authenticator {
	members := append([]Authenticator(nil), as...)
	base := anyOf(members)
	for _, m := range members {
		if c, ok := m.(Challenger); ok {
			return anyOfChallenger{anyOf: base, challenge: c.Challenge()}
		}
	}
	return base
}

type anyOf []Authenticator

// Authenticate walks every member, unconditionally, and only decides after
// all of them have run. The identity of the first member to succeed is kept
// (later successes are evaluated, for constant total work, but do not change
// the result); if none succeed, the first failure's error is returned.
func (a anyOf) Authenticate(r *http.Request) (Identity, error) {
	var (
		identity  Identity
		succeeded bool
		firstErr  error
	)
	for _, m := range a {
		id, err := m.Authenticate(r)
		if err == nil {
			if !succeeded {
				identity, succeeded = id, true
			}
			continue
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if succeeded {
		return identity, nil
	}
	if firstErr == nil {
		// AnyOf with zero members, or every member somehow returning a nil
		// error alongside failure: fail closed rather than silently allow.
		firstErr = errors.New("mamori: no configured scheme allowed the request")
	}
	return Identity{}, firstErr
}

// anyOfChallenger wraps anyOf for the case where at least one member is a
// Challenger. The challenge string is fixed at AnyOf construction time
// (Challenge takes no request, so it cannot vary per call) to the first
// Challenger-implementing member in argument order.
type anyOfChallenger struct {
	anyOf
	challenge string
}

func (a anyOfChallenger) Challenge() string { return a.challenge }

// AllOf allows a request only if every member allows it, for composing
// independent checks that must both hold, such as a bearer token AND an
// mTLS-verified network identity. The first denial fails the whole
// evaluation and the rest are skipped: unlike AnyOf, there is no matching
// timing oracle to defend against here, since a partial failure is already a
// total denial regardless of what a later member would have decided.
//
// The identity of the first member is returned: by convention the first
// member of an AllOf is the primary authenticator (the one that names a
// principal), and later members perform supplementary checks, such as a
// certificate or IP restriction, whose own Identity is typically empty.
func AllOf(as ...Authenticator) Authenticator {
	return allOf(append([]Authenticator(nil), as...))
}

type allOf []Authenticator

func (a allOf) Authenticate(r *http.Request) (Identity, error) {
	var identity Identity
	for i, m := range a {
		id, err := m.Authenticate(r)
		if err != nil {
			return Identity{}, err
		}
		if i == 0 {
			identity = id
		}
	}
	return identity, nil
}
