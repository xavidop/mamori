package mamori

import (
	"fmt"
	"os"
	"strings"
)

// WithRefVars supplies the variables available to ${VAR} expansion in `source`
// struct tags. Expansion happens once, when Load, Watch, or Doctor walks the
// config struct, before any ref is parsed, so a variable may supply a scheme, a
// path segment, a fragment, or a query value.
//
// Nothing is expanded unless it appears here. mamori never reads the ambient
// environment for this, and that is a deliberate security property rather than
// an omission: a ref decides which secret a process reads, so expanding one
// from ambient state would let anything able to set an environment variable
// redirect that read. This is the same reasoning that makes the exec: provider
// opt-in via WithExecProvider. Use EnvVars to opt in to named environment
// variables explicitly.
//
// Applying WithRefVars more than once merges, with later calls winning per key.
// (WithAuth rejects a second application instead, because "which authenticator
// wins" has two plausible answers while merging maps has one.)
//
// Values must not be secrets. After expansion a ref's Raw holds the expanded
// string, which appears in Status, Report, and mamori doctor output. Variables
// are for environment names, regions, service names, and tenant identifiers.
func WithRefVars(vars map[string]string) Option {
	return func(o *options) {
		if o.refVars == nil {
			o.refVars = make(map[string]string, len(vars))
		}
		for k, v := range vars {
			o.refVars[k] = v
		}
	}
}

// EnvVars reads the named environment variables into a map suitable for
// WithRefVars:
//
//	mamori.WithRefVars(mamori.EnvVars("ENVIRONMENT", "REGION"))
//
// Naming each variable is the point: it keeps the set of things that can
// influence which secret a process reads enumerable and greppable at the call
// site, rather than "any environment variable at all".
//
// A name that is not set in the environment is omitted from the result rather
// than mapped to the empty string, so expansion reports the undefined-variable
// error instead of silently producing a ref with a hole in it.
func EnvVars(names ...string) map[string]string {
	out := make(map[string]string, len(names))
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok {
			out[n] = v
		}
	}
	return out
}

// expandRefVars substitutes ${VAR} references in a raw `source` tag.
//
// Only the braced form is recognized. A bare $VAR is left untouched, so
// passwords, exec: commands, and paths containing '$' pass through unchanged.
// "$$" is a literal '$'. An unterminated "${" and an undefined variable are
// both errors: expanding either to nothing would yield a ref like
// "aws-sm:///db", which resolves not-found and then quietly takes the field's
// default:, turning a deployment misconfiguration into a silently wrong value.
func expandRefVars(tag string, vars map[string]string) (string, error) {
	if !strings.Contains(tag, "$") {
		return tag, nil
	}
	var b strings.Builder
	b.Grow(len(tag))
	for i := 0; i < len(tag); i++ {
		if tag[i] != '$' {
			b.WriteByte(tag[i])
			continue
		}
		if i+1 < len(tag) && tag[i+1] == '$' {
			b.WriteByte('$')
			i++
			continue
		}
		if i+1 >= len(tag) || tag[i+1] != '{' {
			b.WriteByte('$')
			continue
		}
		end := strings.IndexByte(tag[i+2:], '}')
		if end < 0 {
			return "", fmt.Errorf("mamori: source %q: unterminated ${: %w", tag, ErrInvalid)
		}
		name := tag[i+2 : i+2+end]
		v, ok := vars[name]
		if !ok {
			return "", fmt.Errorf("mamori: source %q: undefined ref variable %q (pass it with WithRefVars): %w", tag, name, ErrInvalid)
		}
		b.WriteString(v)
		i += 2 + end
	}
	return b.String(), nil
}
