package mamori

import (
	"errors"
	"net/http"
)

// Authenticator decides whether an HTTP request may proceed, and says who the
// caller is. A nil error allows the request; any error denies it.
//
// The returned Identity is ignored by the admin endpoint (which only serves
// metadata) and consumed by the config server, whose authorization policy is
// expressed in terms of it. It is one interface rather than two so an
// Authenticator written for one surface works unchanged on the other.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// Identity is the authenticated caller. Subject is a stable principal name;
// Attrs carries scheme-specific detail (certificate SANs, token claims, a peer
// uid). Attrs is multi-valued because authorization commonly needs
// multi-valued claims: groups, scopes, token audiences, or multiple
// certificate SANs; a single string per key would force a scheme to
// join-encode them. Both may be empty for schemes that authenticate without
// naming a principal, such as a shared bearer token.
type Identity struct {
	Subject string
	Attrs   map[string][]string
}

// Challenger is optionally implemented by an Authenticator to supply the value
// of the WWW-Authenticate header sent with a 401. A scheme that does not
// implement it produces a bare 401.
type Challenger interface {
	Challenge() string
}

// AuthFunc adapts a plain function to Authenticator.
type AuthFunc func(r *http.Request) (Identity, error)

// Authenticate calls f.
func (f AuthFunc) Authenticate(r *http.Request) (Identity, error) { return f(r) }

// ErrForbidden, returned from Authenticate, produces a 403 rather than a 401.
// Use it when the caller is authenticated but not permitted. Any other error
// produces a 401.
var ErrForbidden = errors.New("mamori: forbidden")
