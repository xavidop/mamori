package a

import (
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/secret"
)

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

	// BAD: chained source tag whose sensitive ref is NOT first. The analyzer
	// must check every ref in the chain, not just the first, or this goes
	// unflagged.
	ChainedPassword string `source:"env:LEVEL,aws-sm://prod/db#password"` // want `field "ChainedPassword" has a secret-bearing source scheme "aws-sm" but stores it in a plain string; use secret.String or secret.Bytes`

	// OK: chained source tag where every ref is a non-secret scheme.
	ChainedLevel string `source:"env:LEVEL,file:///etc/app/level"`
}

// DeriveCfg exercises the WithDerive-laundering rule: a hook that reveals a
// secret.String and writes the plaintext into a plain string/[]byte field
// with no source: tag of its own, which the tag-based check above cannot see
// at all.
type DeriveCfg struct {
	Pass     secret.String `source:"aws-sm://prod/db#password"`
	First    string        `source:"env:FIRST"`
	Last     string        `source:"env:LAST"`
	PlainDSN string
	SafeDSN  secret.String
	FullName string
}

// BAD: reveals a secret into a plain string.
var _ = mamori.WithDerive(func(c *DeriveCfg) error {
	c.PlainDSN = "postgres://" + c.Pass.Reveal() + "@h/db" // want `derive hook writes revealed secret material into "PlainDSN", a plain string; use secret.String or secret.Bytes`
	return nil
}, "PlainDSN")

// OK: reveals into a redacting type.
var _ = mamori.WithDerive(func(c *DeriveCfg) error {
	c.SafeDSN = secret.NewString("postgres://" + c.Pass.Reveal() + "@h/db")
	return nil
}, "SafeDSN")

// OK: no Reveal anywhere, so no secret material moved.
var _ = mamori.WithDerive(func(c *DeriveCfg) error {
	c.FullName = c.First + " " + c.Last
	return nil
}, "FullName")

// OK: Reveal on an unrelated type is not the secret package's.
type fakeSecret struct{}

func (fakeSecret) Reveal() string { return "" }

var _ = mamori.WithDerive(func(c *DeriveCfg) error {
	c.FullName = fakeSecret{}.Reveal()
	return nil
}, "FullName")
