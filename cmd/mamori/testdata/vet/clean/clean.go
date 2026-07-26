// Package clean is a vet-test fixture with no violations: its secret-bearing
// field uses the redacting secret.String wrapper and its other field uses a
// non-secret source, so mamori vet reports nothing.
package clean

import "github.com/xavidop/mamori/secret"

// Config stores its secret in secret.String and reads a non-secret value from
// the environment, so `mamori vet` exits clean.
type Config struct {
	APIKey   secret.String `source:"aws-sm://prod/api#key"`
	LogLevel string        `source:"env:LOG_LEVEL"`
}
