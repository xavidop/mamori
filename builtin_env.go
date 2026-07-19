package mamori

import (
	"context"
	"os"
)

// envProvider is the built-in env: provider. It resolves process environment
// variables and is auto-registered (see the init in builtin.go), so `env:NAME`
// refs work out of the box with no extra import.
//
//	LogLevel string `source:"env:LOG_LEVEL" default:"info"`
type envProvider struct{}

func (envProvider) Scheme() string { return "env" }

func (envProvider) Resolve(_ context.Context, ref Ref) (Value, error) {
	v, ok := os.LookupEnv(ref.Path)
	if !ok {
		return Value{}, ErrNotFound
	}
	b := []byte(v)
	return Value{Bytes: b, Version: VersionHash(b)}, nil
}
