package mamori

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
)

// execProvider is the opt-in exec: provider. It runs a command and uses its
// stdout as the value. It is DISABLED by default and must be enabled explicitly
// with WithExecProvider, because executing commands from configuration is a
// meaningful attack surface.
//
//	Token secret.String `source:"exec:vault-agent token"`
//
// For safety, the command is taken verbatim from the ref and is never
// interpolated from other resolved values, so there is no way to chain one
// secret's value into another's command (no injection chains).
type execProvider struct{}

func (execProvider) Scheme() string { return "exec" }

func (execProvider) Resolve(ctx context.Context, ref Ref) (Value, error) {
	fields, err := splitArgs(ref.Path)
	if err != nil {
		return Value{}, fmt.Errorf("mamori: exec %q: %w", ref.Path, err)
	}
	if len(fields) == 0 {
		return Value{}, fmt.Errorf("mamori: exec: %w: empty command", ErrInvalid)
	}
	cmd := exec.CommandContext(ctx, fields[0], fields[1:]...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// A binary that is not on PATH means mamori could not even attempt to
		// fetch the value; it is not evidence the value itself is absent, so it
		// must not be classified as ErrNotFound (which would trigger default:
		// or optional handling). It stays unclassified below, same as a binary
		// that ran and exited non-zero for reasons mamori cannot determine.
		if errors.Is(err, fs.ErrPermission) {
			return Value{}, fmt.Errorf("mamori: exec %q: %w: %w", ref.Path, ErrPermissionDenied, err)
		}
		return Value{}, fmt.Errorf("mamori: exec %q: %w: %s", ref.Path, err, strings.TrimSpace(stderr.String()))
	}
	b := out.Bytes()
	return Value{Bytes: b, Version: VersionHash(b), Sensitive: true}, nil
}

// splitArgs splits a command line into argv, honoring single and double quotes
// so an argument can contain spaces.
//
// strings.Fields was used here before, which meant no argument could ever
// contain a space: `mytool --msg "hello world"` became four arguments, and
// `sh -c 'echo hi'` handed sh the argument `'echo` and then failed with a
// shell syntax error. Both look like they should work.
//
// This parses quotes; it does not run a shell. The binary is still exec'd
// directly, so there is no globbing, no pipes, no command substitution, and no
// shell injection. A caller who genuinely wants shell semantics now has a way
// to ask for one explicitly by invoking it themselves, which is a decision
// they make rather than one mamori makes for every exec: ref.
//
// The quoting rules are the common subset of every POSIX shell, kept
// deliberately small:
//
//   - Single quotes are literal. Nothing inside them is special, not even a
//     backslash.
//   - Double quotes group, and a backslash escapes the next character.
//   - Outside quotes, a backslash escapes the next character, and unquoted
//     whitespace separates arguments.
//
// An unterminated quote or a trailing backslash is an error rather than a
// guess: silently closing the quote would run a command the author did not
// write.
func splitArgs(s string) ([]string, error) {
	var (
		args []string
		cur  strings.Builder
		has  bool // cur holds an argument, even if it is the empty string ""
	)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("%w: unterminated single quote", ErrInvalid)
			}
			cur.WriteString(s[i+1 : i+1+end])
			has = true
			i += end + 1
		case '"':
			j := i + 1
			closed := false
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					cur.WriteByte(s[j+1])
					j += 2
					continue
				}
				if s[j] == '"' {
					closed = true
					break
				}
				cur.WriteByte(s[j])
				j++
			}
			if !closed {
				return nil, fmt.Errorf("%w: unterminated double quote", ErrInvalid)
			}
			has = true
			i = j
		case '\\':
			if i+1 >= len(s) {
				return nil, fmt.Errorf("%w: trailing backslash", ErrInvalid)
			}
			cur.WriteByte(s[i+1])
			has = true
			i++
		case ' ', '\t', '\n', '\r':
			if has {
				args = append(args, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(s[i])
			has = true
		}
	}
	if has {
		args = append(args, cur.String())
	}
	return args, nil
}

// WithExecProvider enables the exec: provider for this Load or Watch call only.
// It is not registered globally; you must opt in explicitly.
func WithExecProvider() Option {
	return func(o *options) { o.providers["exec"] = execProvider{} }
}
