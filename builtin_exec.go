package mamori

import (
	"bytes"
	"context"
	"fmt"
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
		return Value{}, fmt.Errorf("mamori: exec: empty command")
	}
	cmd := exec.CommandContext(ctx, fields[0], fields[1:]...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
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
