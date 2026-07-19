package mamori

import (
	"context"
	"errors"
	"testing"

	"github.com/xavidop/mamori/secret"
)

// memProvider is an in-memory fake provider for tests. Scheme is configurable.
type memProvider struct {
	scheme string
	data   map[string]Value
}

func newMem(scheme string) *memProvider {
	return &memProvider{scheme: scheme, data: map[string]Value{}}
}

func (m *memProvider) Scheme() string { return m.scheme }

func (m *memProvider) put(path string, v string) {
	m.data[path] = Value{Bytes: []byte(v), Version: v}
}

func (m *memProvider) Resolve(_ context.Context, ref Ref) (Value, error) {
	key := ref.Path
	if ref.Key != "" {
		key += "#" + ref.Key
	}
	v, ok := m.data[key]
	if !ok {
		return Value{}, ErrNotFound
	}
	return v, nil
}

type loadConfig struct {
	DBPassword secret.String `source:"mem://prod/db#password"`
	LogLevel   string        `source:"mem://cfg/loglevel" default:"info"`
	Workers    int           `source:"mem://cfg/workers" default:"4" validate:"gte=1,lte=256"`
	Optional   string        `source:"mem://cfg/missing" optional:"true"`
}

func TestLoadBasic(t *testing.T) {
	m := newMem("mem")
	m.put("prod/db#password", "s3cr3t")
	m.put("cfg/loglevel", "debug")
	m.put("cfg/workers", "8")

	cfg, err := Load[loadConfig](context.Background(), WithProvider(m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBPassword.Reveal() != "s3cr3t" {
		t.Errorf("DBPassword = %q, want s3cr3t", cfg.DBPassword.Reveal())
	}
	if cfg.DBPassword.String() != secret.Redacted {
		t.Errorf("DBPassword not redacted in String()")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.Workers != 8 {
		t.Errorf("Workers = %d, want 8", cfg.Workers)
	}
	if cfg.Optional != "" {
		t.Errorf("Optional = %q, want empty", cfg.Optional)
	}
}

func TestLoadDefaultApplied(t *testing.T) {
	m := newMem("mem")
	m.put("prod/db#password", "x")
	// loglevel and workers missing -> defaults info / 4
	cfg, err := Load[loadConfig](context.Background(), WithProvider(m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel default = %q, want info", cfg.LogLevel)
	}
	if cfg.Workers != 4 {
		t.Errorf("Workers default = %d, want 4", cfg.Workers)
	}
}

func TestLoadValidationError(t *testing.T) {
	m := newMem("mem")
	m.put("prod/db#password", "x")
	m.put("cfg/workers", "9999") // exceeds lte=256

	_, err := Load[loadConfig](context.Background(), WithProvider(m))
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %T (%v), want *ValidationError", err, err)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	m := newMem("mem")
	// password missing, no default, not optional -> not found error
	_, err := Load[loadConfig](context.Background(), WithProvider(m))
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

type flattenConfig struct {
	Redis RedisConfig `source:"mem://prod/redis" flatten:"json"`
}

type RedisConfig struct {
	Addr     string        `mapstructure:"addr"`
	Password secret.String `mapstructure:"password"`
	DB       int           `mapstructure:"db"`
}

func TestLoadFlattenJSON(t *testing.T) {
	m := newMem("mem")
	m.put("prod/redis", `{"addr":"localhost:6379","password":"redispw","db":2}`)

	cfg, err := Load[flattenConfig](context.Background(), WithProvider(m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Redis.Addr != "localhost:6379" {
		t.Errorf("Addr = %q", cfg.Redis.Addr)
	}
	if cfg.Redis.Password.Reveal() != "redispw" {
		t.Errorf("Password = %q, want redispw", cfg.Redis.Password.Reveal())
	}
	if cfg.Redis.DB != 2 {
		t.Errorf("DB = %d, want 2", cfg.Redis.DB)
	}
}

func TestLoadNoProvider(t *testing.T) {
	_, err := Load[loadConfig](context.Background())
	if err == nil {
		t.Fatal("expected error when no provider registered for mem scheme")
	}
}
