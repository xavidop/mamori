package viper

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	spf "github.com/spf13/viper"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

func resolve(t *testing.T, p *Provider, raw string) (mamori.Value, error) {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return p.Resolve(context.Background(), ref)
}

func TestResolveRendersEachKind(t *testing.T) {
	v := spf.New()
	v.Set("s", "hello")
	v.Set("b", true)
	v.Set("i", 42)
	v.Set("f", 1.5)
	v.Set("table", map[string]any{"port": 5432})
	p := New(WithViper(v))

	tests := []struct{ name, ref, want string }{
		{"string", "viper://s", "hello"},
		{"bool", "viper://b", "true"},
		{"int", "viper://i", "42"},
		{"float", "viper://f", "1.5"},
		{"table", "viper://table", `{"port":5432}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolve(t, p, tt.ref)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if string(got.Bytes) != tt.want {
				t.Errorf("Bytes = %q, want %q", got.Bytes, tt.want)
			}
			if got.Sensitive {
				t.Error("Sensitive = true, want false: Viper holds application configuration")
			}
			if got.Version == "" {
				t.Error("Version is empty")
			}
		})
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(WithViper(spf.New()))
	if _, err := resolve(t, p, "viper://nope"); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("Resolve error = %v, want errors.Is(ErrNotFound)", err)
	}
}

// TestResolveUnsetBoundFlagNotFound strengthens the not-found guard beyond
// TestResolveNotFound. On a bare Viper instance Get already returns nil for an
// absent key, so skipping the IsSet check in Resolve still resolves to
// ErrNotFound through render's own nil case, and TestResolveNotFound alone
// cannot tell the two code paths apart.
//
// A pflag bound with BindPFlag but never actually set on the command line is
// the case that does distinguish them: Viper's Get falls back to the flag's
// zero value ("false", here) once nothing else answers, while IsSet correctly
// reports false because the flag's HasChanged is false. Without the IsSet
// guard this would resolve to "false" instead of ErrNotFound, silently
// reporting an unset flag as configured.
func TestResolveUnsetBoundFlagNotFound(t *testing.T) {
	v := spf.New()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.Bool("myflag", false, "")
	if err := v.BindPFlag("myflag", fs.Lookup("myflag")); err != nil {
		t.Fatalf("BindPFlag: %v", err)
	}
	if _, err := resolve(t, New(WithViper(v)), "viper://myflag"); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("Resolve error = %v, want errors.Is(ErrNotFound)", err)
	}
}

// TestResolveDefaultCountsAsSet pins a deliberate behaviour. Viper's IsSet
// reports true for a key whose only source is SetDefault, and this provider
// inherits that on purpose: a Viper default is a real configured value, and a
// team migrating incrementally will have keys that come only from one.
// Treating it as missing would silently substitute mamori's default for
// Viper's, which is a value change wearing the costume of a lookup.
func TestResolveDefaultCountsAsSet(t *testing.T) {
	v := spf.New()
	v.SetDefault("only.default", "from-default")
	got, err := resolve(t, New(WithViper(v)), "viper://only.default")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got.Bytes) != "from-default" {
		t.Errorf("Bytes = %q, want %q", got.Bytes, "from-default")
	}
}

// TestResolveRespectsPrecedence is the reason this provider exists: a ref
// must return the winner Viper's own precedence chain picked, not any one
// particular layer. The three subtests each pin one adjacent pair in that
// chain (Set > flags > env > config file > k/v store > defaults) rather than
// only the top-vs-bottom pair, since a middle layer winning over its
// neighbor is just as much this provider's job to preserve.
func TestResolveRespectsPrecedence(t *testing.T) {
	t.Run("set over default", func(t *testing.T) {
		v := spf.New()
		v.SetDefault("server.port", 8080)
		v.Set("server.port", 9090)
		got, err := resolve(t, New(WithViper(v)), "viper://server.port")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if string(got.Bytes) != "9090" {
			t.Errorf("Bytes = %q, want %q: the ref must return the value Viper resolved, not a single layer", got.Bytes, "9090")
		}
	})

	t.Run("config file over default", func(t *testing.T) {
		v := spf.New()
		v.SetDefault("server.port", 8080)
		v.SetConfigType("yaml")
		if err := v.ReadConfig(strings.NewReader("server:\n  port: 9091\n")); err != nil {
			t.Fatalf("ReadConfig: %v", err)
		}
		got, err := resolve(t, New(WithViper(v)), "viper://server.port")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if string(got.Bytes) != "9091" {
			t.Errorf("Bytes = %q, want %q: a config-file value must outrank a default", got.Bytes, "9091")
		}
	})

	t.Run("env over config file", func(t *testing.T) {
		v := spf.New()
		v.SetConfigType("yaml")
		if err := v.ReadConfig(strings.NewReader("server:\n  port: 9091\n")); err != nil {
			t.Fatalf("ReadConfig: %v", err)
		}
		v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
		v.AutomaticEnv()
		t.Setenv("SERVER_PORT", "9092")
		got, err := resolve(t, New(WithViper(v)), "viper://server.port")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if string(got.Bytes) != "9092" {
			t.Errorf("Bytes = %q, want %q: an env binding must outrank the config file", got.Bytes, "9092")
		}
	})
}

// TestResolveLargeFloatRendersPlainDecimal pins a value-corruption bug found
// in review. Viper's JSON decoding stores every number as float64, including
// whole numbers like a byte size or a millisecond timeout, and
// strconv.FormatFloat's 'g' verb switches to exponent notation once the
// exponent reaches 6. A config value as ordinary as 10485760 (10MiB, a
// typical max-upload byte count) was rendering as "1.048576e+07", which
// mamori's own int decode path (strconv.ParseInt) rejects outright. render
// must produce plain decimal digits that still parse as an int, matching
// what a YAML/TOML source (which preserves int) already gives for the same
// value.
func TestResolveLargeFloatRendersPlainDecimal(t *testing.T) {
	v := spf.New()
	v.Set("maxBytes", float64(10485760)) // as Viper's JSON decoder would store it
	got, err := resolve(t, New(WithViper(v)), "viper://maxBytes")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got.Bytes) != "10485760" {
		t.Errorf("Bytes = %q, want %q (plain decimal)", got.Bytes, "10485760")
	}
	if _, err := strconv.ParseInt(string(got.Bytes), 10, 64); err != nil {
		t.Errorf("rendered bytes %q do not parse as an int: %v", got.Bytes, err)
	}
}

// TestResolveDuration pins a value-corruption bug found in review.
// v.SetDefault("timeout", 30*time.Second) is canonical Viper wiring, and
// before the fix this fell through to json.Marshal and rendered as
// "30000000000" - a bare nanosecond count that time.ParseDuration rejects
// for missing a unit. Both the Go-typed path (SetDefault with a
// time.Duration) and the string path (what a YAML file's `timeout: 30s`
// already gives) must resolve to the same parseable text.
func TestResolveDuration(t *testing.T) {
	t.Run("typed default", func(t *testing.T) {
		v := spf.New()
		v.SetDefault("timeout", 30*time.Second)
		got, err := resolve(t, New(WithViper(v)), "viper://timeout")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if string(got.Bytes) != "30s" {
			t.Errorf("Bytes = %q, want %q", got.Bytes, "30s")
		}
		if _, err := time.ParseDuration(string(got.Bytes)); err != nil {
			t.Errorf("rendered bytes %q do not parse as a duration: %v", got.Bytes, err)
		}
	})

	t.Run("string form from a config file", func(t *testing.T) {
		v := spf.New()
		v.Set("timeout", "30s") // what a YAML file's `timeout: 30s` already gives
		got, err := resolve(t, New(WithViper(v)), "viper://timeout")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if string(got.Bytes) != "30s" {
			t.Errorf("Bytes = %q, want %q", got.Bytes, "30s")
		}
		if _, err := time.ParseDuration(string(got.Bytes)); err != nil {
			t.Errorf("rendered bytes %q do not parse as a duration: %v", got.Bytes, err)
		}
	})
}

// TestResolveTimeFromYAML pins a value-corruption bug found in review.
// gopkg.in/yaml.v3 decodes a bare YAML timestamp into a time.Time when
// unmarshaling into `any`, so an ordinary config file - not a Set call - is
// what actually produces this Go type. Before the fix this fell through to
// json.Marshal and arrived as a quoted RFC 3339 string
// ("\"2026-07-29T00:00:00Z\""), the exact defect the string case in render
// exists to prevent, just entering through a different Go type. This test
// reads real YAML rather than calling Set, so it exercises the path that
// actually produces a time.Time.
func TestResolveTimeFromYAML(t *testing.T) {
	v := spf.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader("expires: 2026-07-29T00:00:00Z\n")); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	got, err := resolve(t, New(WithViper(v)), "viper://expires")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	const want = "2026-07-29T00:00:00Z"
	if string(got.Bytes) != want {
		t.Errorf("Bytes = %q, want %q", got.Bytes, want)
	}
	if _, err := time.Parse(time.RFC3339, string(got.Bytes)); err != nil {
		t.Errorf("rendered bytes %q do not parse as RFC 3339: %v", got.Bytes, err)
	}
}

func TestResolveJSONPointerSelection(t *testing.T) {
	v := spf.New()
	v.Set("db", map[string]any{"creds": map[string]any{"port": 5432}})
	got, err := resolve(t, New(WithViper(v)), "viper://db#/creds/port")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got.Bytes) != "5432" {
		t.Errorf("Bytes = %q, want %q", got.Bytes, "5432")
	}
}

func TestResolveEmptyKey(t *testing.T) {
	_, err := resolve(t, New(WithViper(spf.New())), "viper://")
	if err == nil {
		t.Fatal("Resolve of an empty key returned nil error")
	}
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Errorf("error %v does not satisfy errors.Is(ErrInvalid)", err)
	}
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != Scheme {
		t.Errorf("Scheme() = %q, want %q", got, Scheme)
	}
}

func TestRegisteredScheme(t *testing.T) {
	for _, s := range mamori.RegisteredSchemes() {
		if s == Scheme {
			return
		}
	}
	t.Errorf("scheme %q was not registered by init()", Scheme)
}

func TestConformance(t *testing.T) {
	v := spf.New()
	providertest.Run(t, providertest.Config{
		New:        func() mamori.Provider { return New(WithViper(v)) },
		Ref:        func(key string) string { return Scheme + "://" + key },
		PointerRef: func(key, frag string) string { return Scheme + "://" + key + frag },
		Seed:       func(_ context.Context, key, val string) error { v.Set(key, val); return nil },
		Mutate:     func(_ context.Context, key, val string) error { v.Set(key, val); return nil },
		// Viper's read API has no error return anywhere: Get(key) any and
		// IsSet(key) bool. There is no permission, rate-limit, unavailability,
		// or malformed-response case to inject, because the data is already in
		// memory by the time mamori asks. Any config-load failure happened
		// earlier, inside the application's own ReadInConfig call.
		NoResolveErrors: true,
	})
}
