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
	fields := strings.Fields(ref.Path)
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

// WithExecProvider enables the exec: provider for this Load or Watch call only.
// It is not registered globally; you must opt in explicitly.
func WithExecProvider() Option {
	return func(o *options) { o.providers["exec"] = execProvider{} }
}
