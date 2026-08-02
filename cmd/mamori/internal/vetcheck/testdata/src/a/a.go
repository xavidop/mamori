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

	// PassPtr reaches the same secret.String Reveal through a pointer. Go
	// dereferences automatically, so this is the same reveal; asserting
	// *types.Named on the receiver missed it.
	PassPtr *secret.String

	// Label is a second plain write path, so a hook can write two plain
	// fields and only one of them be the laundering one.
	Label string
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

// OK: the multi-path hook that a hook-scoped reveal check got wrong. This is
// the safe pattern the rule itself recommends (reveal into a secret.String)
// sitting beside an ordinary non-secret derived field, in one hook declaring
// both paths. A single Reveal anywhere in the body used to flag every plain
// declared path of the hook, so "FullName" drew a diagnostic positioned at the
// SafeDSN line. Nothing here may be reported.
var _ = mamori.WithDerive(func(c *DeriveCfg) error {
	c.SafeDSN = secret.NewString("postgres://" + c.Pass.Reveal() + "@h/db")
	c.FullName = c.First + " " + c.Last
	return nil
}, "SafeDSN", "FullName")

// BAD, but only on one of three paths: the reveal must be attributed to the
// assignment that actually carries it, and reported there. Label and FullName
// are plain strings written by the same hook and must stay clean, and the
// diagnostic must land on the PlainDSN line, not on whichever line the first
// Reveal happens to sit on.
var _ = mamori.WithDerive(func(c *DeriveCfg) error {
	c.Label = "primary"
	c.PlainDSN = "postgres://" + c.Pass.Reveal() + "@h/db" // want `derive hook writes revealed secret material into "PlainDSN", a plain string; use secret.String or secret.Bytes`
	c.FullName = c.First + " " + c.Last
	return nil
}, "Label", "PlainDSN", "FullName")

// BAD: the plaintext reaches the plain field through a local variable. Scoping
// the reveal to one assignment must not cost this true positive.
var _ = mamori.WithDerive(func(c *DeriveCfg) error {
	plain := c.Pass.Reveal()
	c.FullName = c.First + " " + c.Last
	c.PlainDSN = "postgres://" + plain + "@h/db" // want `derive hook writes revealed secret material into "PlainDSN", a plain string; use secret.String or secret.Bytes`
	return nil
}, "PlainDSN", "FullName")

// BAD: the reveal is reached through a *secret.String. Go dereferences
// automatically, so this launders exactly as the value receiver does.
var _ = mamori.WithDerive(func(c *DeriveCfg) error {
	c.PlainDSN = "postgres://" + c.PassPtr.Reveal() + "@h/db" // want `derive hook writes revealed secret material into "PlainDSN", a plain string; use secret.String or secret.Bytes`
	return nil
}, "PlainDSN")
