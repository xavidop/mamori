package a

import "github.com/xavidop/mamori/secret"

// Config exercises every branch of the analyzer.
type Config struct {
	// GOOD: secret-bearing schemes stored in redacting secret types.
	APIKey secret.String `source:"aws-sm://prod/api#key"`
	TLSKey secret.Bytes  `source:"vault://kv/data/tls#key"`

	// BAD: secret-bearing schemes stored in plain string / []byte.
	DBPassword string `source:"aws-sm://prod/db#password"`     // want `field "DBPassword" has a secret-bearing source scheme "aws-sm" but stores it in a plain string; use secret.String or secret.Bytes`
	VaultToken string `source:"vault://kv/data/token"`         // want `field "VaultToken" has a secret-bearing source scheme "vault" but stores it in a plain string; use secret.String or secret.Bytes`
	GCPSecret  []byte `source:"gcp-sm://projects/p/secrets/s"` // want `field "GCPSecret" has a secret-bearing source scheme "gcp-sm" but stores it in a plain \[\]byte; use secret.String or secret.Bytes`
	OnePass    string `source:"op://vault/item/field"`         // want `field "OnePass" has a secret-bearing source scheme "op" but stores it in a plain string; use secret.String or secret.Bytes`

	// OK: non-secret schemes are ignored even when plain.
	LogLevel string `source:"env:LOG_LEVEL"`
	Endpoint string `source:"file:///etc/app/endpoint"`
	Consul   string `source:"consul://app/config"`

	// OK: no source tag at all.
	Plain    string
	Internal []byte `json:"internal"`
}
