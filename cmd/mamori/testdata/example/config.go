// Package example is a fixture package for cmd/mamori's static extraction
// tests (extract.go / explain.go). golang.org/x/tools/go/packages loads
// packages in module mode, so this fixture lives in its own self-contained
// module (see go.mod) with a local stub of github.com/xavidop/mamori/secret
// (see stub/secret), rather than depending on the real core module and its
// full dependency graph: hermetic and fast, with no real-core dependency
// tree in this fixture's go.sum, rather than pointing the replace at the
// real core module.
package example

import "github.com/xavidop/mamori/secret"

// Config is the primary fixture struct. It exercises: a plain source-tagged
// field, a secret.String field, a two-ref precedence chain, a defaulted and
// optional field, a field with no source tag at all (skipped entirely), and
// a nested struct field with no source tag of its own (recursed into for
// dotted paths).
type Config struct {
	// LogLevel: plain field, single (unchained) source, not sensitive.
	LogLevel string `source:"env:LOG_LEVEL"`

	// APIKey: secret.String type, so Sensitive is true via the Go type
	// regardless of the source scheme.
	APIKey secret.String `source:"aws-sm://prod/api#key"`

	// DBHost: a two-ref precedence chain (env, then AWS Secrets Manager).
	// Sensitive is true via the aws-sm scheme even though the Go type is a
	// plain string.
	DBHost string `source:"env:DB_HOST,aws-sm://prod/db#host"`

	// Port: defaulted and optional.
	Port string `source:"env:PORT" default:"8080" optional:"true"`

	// Workers: an int field with a numeric range (schema.go's schema
	// command translates gte/lte to minimum/maximum) and a default, which
	// schema.go emits as a typed JSON number rather than a string.
	Workers int `source:"env:WORKERS" default:"4" validate:"gte=1,lte=256"`

	// Level: a oneof validate rule (schema.go translates it to an enum).
	// Deliberately a distinct field from LogLevel above, which carries no
	// validate tag at all, so both shapes (with and without oneof) exist
	// side by side in the fixture.
	Level string `source:"env:LEVEL" validate:"oneof=debug info warn error"`

	// Region: explicitly validate:"required", even though it would also
	// count as required by the "neither optional nor defaulted" default
	// rule (schema.go's isRequiredField) -- exercising the explicit tag
	// path, not just the fallback.
	Region string `source:"env:REGION" validate:"required"`

	// ServiceName: a string length range. On a string field, schema.go
	// translates validate's min/max to minLength/maxLength (string length),
	// never minimum/maximum (which is reserved for gte/lte and, on numeric
	// fields only, min/max -- see schema.go's mapping comment).
	ServiceName string `source:"env:SERVICE_NAME" validate:"min=3,max=64"`

	// RequestTimeout: an aws-ps:// (AWS Systems Manager Parameter Store)
	// ref, exercised by "mamori policy --format=aws-iam" (task 5): its
	// ssm:GetParameter/ssm:GetParameters statement's Resource list is built
	// from this field's ref path. Distinct scheme from APIKey/DBHost's
	// aws-sm:// refs above (a different AWS service, a different pair of
	// IAM actions), so both statements have something to populate in the
	// fixture.
	RequestTimeout string `source:"aws-ps://prod/app/config#timeout"`

	// GCPSecret: a gcp-sm:// (Google Cloud Secret Manager) ref, exercised
	// by "mamori policy --format=gcp". Deliberately not the
	// "gcp-sm://projects/p/secrets/s" shape a naive reading of the AWS/GCP
	// resource-name grammar might suggest: providers/gcp/gcp.go's own
	// Resolve documents (and parses via strings.Cut(ref.Path, "/")) the
	// real grammar as gcp-sm://<project>/<secret>, e.g.
	// "gcp-sm://my-project/db-password" -- the project IS the ref's first
	// path segment, not a placeholder. policy.go's GCP writer mirrors that
	// same split.
	GCPSecret string `source:"gcp-sm://acme-prod/db-password"`

	// Debug carries no source tag and is not a struct type: Extract skips
	// it entirely, and it never appears in the field list.
	Debug bool

	// Redis carries no source tag but is a struct type: Extract recurses
	// into it, contributing dotted paths Redis.Addr and Redis.Password.
	Redis RedisConfig

	// Computed carries no source tag but is validated, which mamori enforces
	// because the validator runs against the whole struct. It is the fixture
	// for KindValidate: schema must emit it, explain and policy must not.
	Computed string `validate:"required"`

	// Nested carries no source tag but its type, TaggedNest, does carry its
	// own validate tag on this field. walkFields must still recurse into it
	// to reach Nested.Addr: this is the regression fixture for the ordering
	// trap (see TaggedNest's doc comment).
	Nested TaggedNest `validate:"required"`

	// DSN carries no source tag and no validate tag: nothing in walkFields
	// would ever emit it. It exists only for derives.go's
	// mamori.WithDerive(fn, "DSN") call to declare, making it the fixture for
	// KindDerived (see extract.go's FieldKind and cmd/mamori/derives.go):
	// the only way this field can appear in `mamori explain` or `mamori
	// schema` is by reading that call site statically.
	DSN string
}

// TaggedNest is a nested struct carrying its own validate tag. walkFields must
// still recurse into it to reach Addr; emitting TaggedNest as a validate-only
// leaf would make every nested source field disappear.
type TaggedNest struct {
	Addr string `source:"env:NEST_ADDR"`
}

// RedisConfig is nested inside Config (via the Redis field above) and also
// independently qualifies as its own top-level struct, because Extract finds
// every struct type with at least one source-tagged field, not only the
// ones reachable from a single chosen root.
type RedisConfig struct {
	Addr     string        `source:"env:REDIS_ADDR"`
	Password secret.String `source:"vault://kv/redis#password" onfail:"keeplast"`
}

// ServerConfig is a second, unrelated struct, so --type=Config is
// meaningful: it excludes ServerConfig's fields.
type ServerConfig struct {
	Host string `source:"env:SERVER_HOST"`
	Port string `source:"env:SERVER_PORT" default:"9090" onfail:"default"`
}

// IncompleteConfig backs TestExplainNotesIncompleteDerives
// (explain.go's derivesIncompleteNote): it carries a real source: tagged
// field (so it appears in Extract's output at all); derives.go's init
// declares a WithDerive call for it whose write path is a variable, which
// findDerives cannot read statically. Its type is declared here, alongside
// Config and friends, rather than in derives.go, deliberately: taggedStructs
// orders same-package structs by token.Pos, and that ordering is only
// reliable within one file's AST -- across files in the same package it can
// vary between otherwise-identical runs (parsing runs concurrently per
// file), which would make explain.table.golden/explain.json.golden flake.
type IncompleteConfig struct {
	Name string `source:"env:INCOMPLETE_NAME"`
}

// DeriveOverlap backs TestExtractDerivedPathKeepsSourceTaggedEntry and
// TestExtractDerivedPathKeepsValidateRules: a WithDerive write path is allowed
// to name a field that ALSO carries a source: or validate: tag (the derive
// runs after decoding and simply wins -- see
// site/src/pages/docs/usage/derived-fields.md), which means walkFields and
// derivedFields can both have something to say about the same Path. Only the
// walkFields entry may survive; a second, KindDerived entry carries no
// Default/Optional/Validate at all, and schema.go's last-write-wins
// builderNode.insert let it erase them.
//
// Declared here in config.go rather than in derives.go for the same reason
// IncompleteConfig is (see its doc comment): taggedStructs orders same-package
// structs by token.Pos, which is only reliable within one file.
type DeriveOverlap struct {
	// Port is the source-tagged shape: defaulted and optional, and also a
	// declared derive write path. The duplicate entry dropped its "8080"
	// default and pushed Port into the schema's "required" array despite
	// optional:"true".
	Port string `source:"env:OVERLAP_PORT" default:"8080" optional:"true"`

	// DSN is the validate-only shape: no source tag, so walkFields emits it as
	// KindValidate, and also a declared derive write path. The duplicate entry
	// carried an empty Validate, which dropped minLength from the schema.
	DSN string `validate:"required,min=10"`

	// Derived carries neither tag, so walkFields never emits it and the
	// declared write path is the only thing that can: the skip above must not
	// swallow the genuinely derive-only case it is surrounded by.
	Derived string
}
