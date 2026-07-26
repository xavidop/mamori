// Package violation is a vet-test fixture: it stores a secret-bearing source
// (aws-sm) in a plain string, which mamori vet must flag.
package violation

// Config binds an AWS Secrets Manager secret to a plain string field, so
// `mamori vet` reports a diagnostic naming DBPassword.
type Config struct {
	DBPassword string `source:"aws-sm://prod/db#password"`
}
