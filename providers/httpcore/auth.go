package httpcore

import (
	"context"
	"net/http"
)

// Authenticator injects credentials into an outbound request. Implementations
// must be safe for concurrent use, since one Client serves every resolve for
// its backend.
//
// An Authenticator must never log the credential it carries, and must not put
// one anywhere mamori might surface it: not in an error message, not in a
// Value's Metadata, not in a URL path that appears in a ref.
type Authenticator interface {
	Apply(ctx context.Context, req *http.Request) error
}

// AuthenticatorFunc adapts a plain function to Authenticator, allowing a
// callable to satisfy the interface without explicit methods.
type AuthenticatorFunc func(ctx context.Context, req *http.Request) error

// Apply calls f, allowing AuthenticatorFunc to satisfy the Authenticator interface.
func (f AuthenticatorFunc) Apply(ctx context.Context, req *http.Request) error {
	return f(ctx, req)
}

// Bearer authenticates with an RFC 6750 bearer token in the Authorization
// header. This is what most REST config and secret backends want.
func Bearer(token string) Authenticator {
	return AuthenticatorFunc(func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	})
}

// HeaderAuth authenticates with a fixed header, for backends that use a named
// API-key header such as X-Api-Key rather than Authorization.
func HeaderAuth(name, value string) Authenticator {
	return AuthenticatorFunc(func(_ context.Context, req *http.Request) error {
		req.Header.Set(name, value)
		return nil
	})
}

// BasicAuth authenticates with HTTP Basic credentials, for backends that
// require or prefer this standard scheme over other methods.
func BasicAuth(user, pass string) Authenticator {
	return AuthenticatorFunc(func(_ context.Context, req *http.Request) error {
		req.SetBasicAuth(user, pass)
		return nil
	})
}

// QueryAuth authenticates with a query parameter, for the small number of
// backends that accept no header form.
//
// Prefer any of the header-based authenticators where the backend allows one: a
// query parameter travels in the request line, so it reaches proxy logs and
// server access logs that a header would not.
//
// Existing query parameters are preserved, so an Endpoint's fixed Query survives.
func QueryAuth(name, value string) Authenticator {
	return AuthenticatorFunc(func(_ context.Context, req *http.Request) error {
		q := req.URL.Query()
		q.Set(name, value)
		req.URL.RawQuery = q.Encode()
		return nil
	})
}
