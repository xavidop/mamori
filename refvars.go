package mamori

import (
	"fmt"
	"os"
	"strings"
)

// WithRefVars supplies the variables available to ${VAR} expansion in `source`
// struct tags. Expansion happens once, when Load, Watch, or Doctor walks the
// config struct, before any ref is parsed, so a variable may supply a scheme, a
// path segment, a fragment, or a query value. Because expansion runs against
// the whole raw tag string before it is split into refs, a variable's value
// can also inject a comma and thereby change how a multi-ref precedence chain
// splits. Variables are operator-supplied through this function, so that is
// part of the same trust model as the rest of this doc comment, not a
// separate gap.
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
// "$$" is a literal '$'. An unterminated "${", an undefined variable, and an
// empty "${}" name are all errors: expanding an unterminated or undefined
// reference to nothing would yield a ref like "aws-sm:///db", which resolves
// not-found and then quietly takes the field's default:, turning a deployment
// misconfiguration into a silently wrong value.
//
// Compatibility: this scan runs unconditionally on every `source` tag,
// whether or not the caller ever passes WithRefVars. With vars == nil, "$$"
// still collapses to a literal '$' and an unterminated "${" still hard-errors.
// That is a behavior change for a pre-existing tag that happens to contain a
// literal "$$" or a stray "${", even for a caller who never touches ${VAR}
// expansion: it now means something different, or fails, on upgrade. The
// realistic case is an exec: command - execProvider (builtin_exec.go) splits
// its argument with strings.Fields and runs it via exec.CommandContext with no
// shell, so a "${...}" there was previously inert literal text passed
// straight to the child process, and now is not. Skipping the scan when vars
// is nil is deliberately NOT the fix: that would let a caller who forgot
// WithRefVars silently get "${ENV}" left literal in the ref, which resolves
// not-found and then quietly takes default: - exactly the silent
// misconfiguration this feature exists to prevent. Unconditional is correct;
// it only needed to be written down.
//
// Expansion is not recursive: a variable's own value is inserted verbatim and
// never rescanned, so "${A}" expanding to a value containing "${B}" leaves
// that "${B}" literal in the output rather than resolving it. This is
// deliberate - recursive expansion would risk an infinite loop from a
// self-referencing or cyclic variable value - not an oversight.
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
		if name == "" {
			return "", fmt.Errorf("mamori: source %q: empty variable name in ${}: %w", tag, ErrInvalid)
		}
		v, ok := vars[name]
		if !ok {
			return "", fmt.Errorf("mamori: source %q: undefined ref variable %q (pass it with WithRefVars): %w", tag, name, ErrInvalid)
		}
		b.WriteString(v)
		i += 2 + end
	}
	return b.String(), nil
}
