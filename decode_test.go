package mamori

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/mamori/secret"
)

// TestWalkSpecsChain covers Task 1's fieldSpec plumbing: a multi-ref source
// tag parses into fieldSpec.Refs (in order), and onfail:"default" is rejected
// at walk time when the field carries no default: tag.
func TestWalkSpecsChain(t *testing.T) {
	t.Run("multi-ref source produces ordered Refs and HasDefault", func(t *testing.T) {
		type cfg struct {
			Port string `source:"env:A,env:B" default:"x"`
		}
		specs, err := fieldSpecs(reflect.TypeOf(cfg{}), nil)
		if err != nil {
			t.Fatalf("fieldSpecs: unexpected error: %v", err)
		}
		if len(specs) != 1 {
			t.Fatalf("got %d specs, want 1", len(specs))
		}
		spec := specs[0]
		if len(spec.Refs) != 2 {
			t.Fatalf("Refs = %+v, want 2 refs", spec.Refs)
		}
		if spec.Refs[0].Scheme != "env" || spec.Refs[0].Path != "A" {
			t.Errorf("Refs[0] = %+v, want scheme env path A", spec.Refs[0])
		}
		if spec.Refs[1].Scheme != "env" || spec.Refs[1].Path != "B" {
			t.Errorf("Refs[1] = %+v, want scheme env path B", spec.Refs[1])
		}
		if !spec.HasDefault || spec.Default != "x" {
			t.Errorf("HasDefault/Default = %v/%q, want true/%q", spec.HasDefault, spec.Default, "x")
		}
		if spec.OnFail != onFailKeepLast {
			t.Errorf("OnFail = %v, want default onFailKeepLast", spec.OnFail)
		}
	})

	t.Run("single-ref source still yields a one-element Refs slice", func(t *testing.T) {
		type cfg struct {
			Port string `source:"env:PORT"`
		}
		specs, err := fieldSpecs(reflect.TypeOf(cfg{}), nil)
		if err != nil {
			t.Fatalf("fieldSpecs: unexpected error: %v", err)
		}
		if len(specs) != 1 || len(specs[0].Refs) != 1 {
			t.Fatalf("specs = %+v, want one spec with one ref", specs)
		}
	})

	t.Run("onfail default without a default tag is rejected", func(t *testing.T) {
		type cfg struct {
			Port string `source:"env:PORT" onfail:"default"`
		}
		_, err := fieldSpecs(reflect.TypeOf(cfg{}), nil)
		if err == nil {
			t.Fatal("fieldSpecs: expected error, got nil")
		}
		if !strings.Contains(err.Error(), "onfail") {
			t.Errorf("error = %v, want it to mention onfail", err)
		}
	})

	t.Run("onfail default with a default tag is accepted", func(t *testing.T) {
		type cfg struct {
			Port string `source:"env:PORT" default:"8080" onfail:"default"`
		}
		specs, err := fieldSpecs(reflect.TypeOf(cfg{}), nil)
		if err != nil {
			t.Fatalf("fieldSpecs: unexpected error: %v", err)
		}
		if specs[0].OnFail != onFailUseDefault {
			t.Errorf("OnFail = %v, want onFailUseDefault", specs[0].OnFail)
		}
	})

	t.Run("onfail fail is accepted without a default", func(t *testing.T) {
		type cfg struct {
			Port string `source:"env:PORT" onfail:"fail"`
		}
		specs, err := fieldSpecs(reflect.TypeOf(cfg{}), nil)
		if err != nil {
			t.Fatalf("fieldSpecs: unexpected error: %v", err)
		}
		if specs[0].OnFail != onFailFail {
			t.Errorf("OnFail = %v, want onFailFail", specs[0].OnFail)
		}
	})

	t.Run("unknown onfail value is rejected", func(t *testing.T) {
		type cfg struct {
			Port string `source:"env:PORT" onfail:"bogus"`
		}
		_, err := fieldSpecs(reflect.TypeOf(cfg{}), nil)
		if err == nil {
			t.Fatal("fieldSpecs: expected error, got nil")
		}
	})
}

func TestFieldSpecsRejectsUnknownDecodeCoding(t *testing.T) {
	type cfg struct {
		A string `source:"env:A?decode=rot13"`
	}
	_, err := fieldSpecs(reflect.TypeOf(cfg{}), nil)
	if err == nil {
		t.Fatal("want an error for an unknown decode coding, got nil")
	}
	if !strings.Contains(err.Error(), "rot13") {
		t.Errorf("error %q should name the offending coding", err)
	}
	if !strings.Contains(err.Error(), "A") {
		t.Errorf("error %q should name the offending field", err)
	}
}

// TestFieldSpecsRejectsUnknownDecodeCodingInLowerChainPosition is the reason
// the spec walk checks every ref rather than only Refs[0]: a lower-precedence
// position is not consulted until the ones above it are all absent, so a typo
// there would otherwise stay silent for as long as the primary source keeps
// answering, and then fail the process at the worst possible moment - during
// the outage that finally promotes it.
func TestFieldSpecsRejectsUnknownDecodeCodingInLowerChainPosition(t *testing.T) {
	type cfg struct {
		A string `source:"env:A,env:B?decode=rot13"`
	}
	_, err := fieldSpecs(reflect.TypeOf(cfg{}), nil)
	if err == nil {
		t.Fatal("want an error for an unknown decode coding on the second chain position, got nil")
	}
	if !strings.Contains(err.Error(), "rot13") {
		t.Errorf("error %q should name the offending coding", err)
	}
	if !strings.Contains(err.Error(), "A") {
		t.Errorf("error %q should name the offending field", err)
	}
}

func TestFieldSpecsAcceptsKnownDecodeCodings(t *testing.T) {
	type cfg struct {
		A string `source:"env:A?decode=base64,gzip"`
	}
	if _, err := fieldSpecs(reflect.TypeOf(cfg{}), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadFlattenTOML(t *testing.T) {
	type Redis struct {
		Host     string        `mapstructure:"host"`
		Port     int           `mapstructure:"port"`
		Password secret.String `mapstructure:"password"`
		Timeout  time.Duration `mapstructure:"timeout"`
	}
	type Config struct {
		Redis Redis `source:"env:REDIS_TOML" flatten:"toml"`
	}

	t.Setenv("REDIS_TOML", "host = \"cache.internal\"\nport = 6379\npassword = \"hunter2\"\ntimeout = \"5s\"\n")

	cfg, err := Load[Config](context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Redis.Host != "cache.internal" {
		t.Errorf("Host = %q, want cache.internal", cfg.Redis.Host)
	}
	if cfg.Redis.Port != 6379 {
		t.Errorf("Port = %d, want 6379", cfg.Redis.Port)
	}
	if cfg.Redis.Password.Reveal() != "hunter2" {
		t.Errorf("Password did not decode through flattenHook")
	}
	if cfg.Redis.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", cfg.Redis.Timeout)
	}
}

func TestLoadFlattenTOMLNestedTable(t *testing.T) {
	type Inner struct {
		User string `mapstructure:"user"`
	}
	type Outer struct {
		Name  string `mapstructure:"name"`
		Creds Inner  `mapstructure:"creds"`
	}
	type Config struct {
		App Outer `source:"env:APP_TOML" flatten:"toml"`
	}

	t.Setenv("APP_TOML", "name = \"svc\"\n\n[creds]\nuser = \"admin\"\n")

	cfg, err := Load[Config](context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.App.Creds.User != "admin" {
		t.Errorf("nested table did not decode: Creds.User = %q", cfg.App.Creds.User)
	}
}

func TestLoadFlattenTOMLMalformedIsLoud(t *testing.T) {
	type Inner struct {
		Host string `mapstructure:"host"`
	}
	type Config struct {
		App Inner `source:"env:BAD_TOML" flatten:"toml"`
	}

	t.Setenv("BAD_TOML", "this is not = = valid toml")

	_, err := Load[Config](context.Background())
	if err == nil {
		t.Fatal("malformed TOML decoded silently; want an error")
	}
	if !strings.Contains(err.Error(), "toml flatten") {
		t.Errorf("error %q does not name the toml flatten stage", err)
	}
}
