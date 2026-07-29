package viper

import (
	"context"
	"errors"
	"testing"

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

// TestResolveRespectsPrecedence is the reason this provider exists. An
// explicit Set outranks a default in Viper, and the ref must return the winner
// Viper picked rather than any particular layer.
func TestResolveRespectsPrecedence(t *testing.T) {
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
