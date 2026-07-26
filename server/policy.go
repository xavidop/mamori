package server

import (
	"path"

	"github.com/xavidop/mamori"
)

// Policy decides whether an authenticated identity may access a binding by
// name. It is mandatory: New refuses to construct a Server with no Policy
// configured (see WithPolicy), because authorization is the second half of
// "who can see this value" - Authenticator only answers "who is this",
// leaving authorization unaddressed if nothing enforces it.
//
// Allow returns nil to permit the request and a non-nil error to deny it. A
// denial must not let the caller distinguish "this name exists but you may
// not have it" from "this name does not exist at all": Policy has no
// visibility into the binding table in the first place (it is handed only an
// Identity and a name string), so any Policy implementation that returns the
// same error shape for both cases automatically satisfies this - see
// PrefixPolicy below for the constructor this package ships that does.
type Policy interface {
	Allow(id mamori.Identity, name string) error
}

// PolicyFunc adapts a plain function to Policy, the same pattern as
// mamori.AuthFunc for Authenticator.
type PolicyFunc func(id mamori.Identity, name string) error

// Allow calls f.
func (f PolicyFunc) Allow(id mamori.Identity, name string) error { return f(id, name) }

// ErrDenied is the error every Policy constructor in this package returns on
// denial. It carries no name, subject, or reason, deliberately: a message
// that varied per name or subject would let repeated requests map out which
// bindings exist and which identities can reach them, turning the
// authorization layer into an enumeration oracle. errors.Is(err,
// server.ErrDenied) is how a caller (including the wire handler added later)
// recognizes a policy denial.
var ErrDenied = mamori.ErrPermissionDenied

// AllowAll returns a Policy that permits every identity access to every
// binding name. It exists for a trusted-sidecar deployment - a process
// running as the sole consumer, or one where access control is already fully
// enforced upstream - where per-name glob rules would add configuration
// without adding security. Because New refuses to start with no Policy at
// all, choosing AllowAll is always an explicit, greppable line in the
// operator's own code, never an implicit default.
func AllowAll() Policy {
	return PolicyFunc(func(mamori.Identity, string) error { return nil })
}

// PrefixPolicy returns a Policy that grants access by subject: for an
// identity whose Subject matches a key in rules, the corresponding glob
// patterns are matched against the requested binding name using
// path.Match's grammar:
//
//   - '*' matches any sequence of non-separator characters
//   - '?' matches any single non-separator character
//   - '[...]' matches a character class (optionally negated with '[^...]')
//   - any other character matches itself literally
//
// path.Match treats '/' as a separator the same way it would in a file path:
// '*' and '?' do not match across a '/'. Binding names are ordinarily flat
// (no slashes), so this rarely matters in practice; if you do give bindings
// hierarchical names, keep that in mind when writing globs.
//
// A subject with no entry in rules is denied - there is no fallback
// default-allow - so a mistyped or simply forgotten subject fails closed
// rather than open. A subject present in rules but whose globs do not match
// the requested name is denied with the exact same error (ErrDenied) as a
// subject absent from rules entirely, so neither response reveals which case
// occurred, or whether the requested name is even a real binding.
func PrefixPolicy(rules map[string][]string) Policy {
	return PolicyFunc(func(id mamori.Identity, name string) error {
		globs, ok := rules[id.Subject]
		if !ok {
			return ErrDenied
		}
		for _, g := range globs {
			if matched, err := path.Match(g, name); err == nil && matched {
				return nil
			}
		}
		return ErrDenied
	})
}
