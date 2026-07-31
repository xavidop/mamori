package mamori

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type deriveCfg struct {
	A string
	B string
}

func TestWithDeriveRegistersInOrder(t *testing.T) {
	o := &options{}
	WithDerive(func(c *deriveCfg) error { c.A = "first"; return nil })(o)
	WithDerive(func(c *deriveCfg) error { c.A += "-second"; return nil })(o)

	fns, err := typedDerives[deriveCfg](o)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fns) != 2 {
		t.Fatalf("got %d derives, want 2", len(fns))
	}

	var cfg deriveCfg
	for _, fn := range fns {
		if err := fn(&cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if cfg.A != "first-second" {
		t.Fatalf("got %q, want %q: derives must run in registration order", cfg.A, "first-second")
	}
}

func TestTypedDerivesNoneIsNil(t *testing.T) {
	fns, err := typedDerives[deriveCfg](&options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fns != nil {
		t.Fatalf("got %v, want nil when no derive is registered", fns)
	}
}

// A mismatched type parameter must be a loud error, not a silent no-op. This is
// the property that distinguishes WithDerive from OnChange, which discards the
// failed assertion and then never fires. See the DeriveError doc comment.
func TestTypedDerivesTypeMismatchIsLoud(t *testing.T) {
	type otherCfg struct{ Z string }

	o := &options{}
	WithDerive(func(c *otherCfg) error { return nil })(o)

	_, err := typedDerives[deriveCfg](o)
	if err == nil {
		t.Fatal("want an error when the derive's type parameter does not match")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error must satisfy ErrInvalid, got %v", err)
	}
	for _, want := range []string{"func(*mamori.otherCfg) error", "func(*mamori.deriveCfg) error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q so the mistake is findable, got: %v", want, err)
		}
	}
}

func TestDeriveErrorUnwraps(t *testing.T) {
	base := errors.New("boom")
	de := &DeriveError{Err: base}

	if !errors.Is(de, base) {
		t.Error("DeriveError must unwrap to its cause")
	}
	if !strings.Contains(de.Error(), "boom") {
		t.Errorf("DeriveError message must carry the cause, got %q", de.Error())
	}
}

type dsnCfg struct {
	User string `source:"env:DERIVE_USER"`
	Pass string `source:"env:DERIVE_PASS"`
	DSN  string
}

func TestDeriveRunsOnLoad(t *testing.T) {
	t.Setenv("DERIVE_USER", "alice")
	t.Setenv("DERIVE_PASS", "s3cret")

	cfg, err := Load[dsnCfg](context.Background(),
		WithDerive(func(c *dsnCfg) error {
			c.DSN = c.User + ":" + c.Pass
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DSN != "alice:s3cret" {
		t.Fatalf("got %q, want %q", cfg.DSN, "alice:s3cret")
	}
}

// The load-bearing ordering test: a derived field carrying a validate tag must
// be validated on its DERIVED value, not on the zero value it held a moment
// earlier. If derive ran after validation this would fail.
type validatedDeriveCfg struct {
	User string `source:"env:DERIVE_USER"`
	DSN  string `validate:"required"`
}

func TestDeriveRunsBeforeValidation(t *testing.T) {
	t.Setenv("DERIVE_USER", "alice")

	if _, err := Load[validatedDeriveCfg](context.Background(),
		WithDerive(func(c *validatedDeriveCfg) error { c.DSN = "postgres://" + c.User; return nil }),
	); err != nil {
		t.Fatalf("a derive that fills a required field must satisfy validation, got %v", err)
	}

	_, err := Load[validatedDeriveCfg](context.Background(),
		WithDerive(func(c *validatedDeriveCfg) error { return nil }),
	)
	if err == nil {
		t.Fatal("a derive that leaves a required field empty must fail validation")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("got %T, want *ValidationError", err)
	}
}

func TestDeriveErrorFailsLoad(t *testing.T) {
	t.Setenv("DERIVE_USER", "alice")
	t.Setenv("DERIVE_PASS", "s3cret")
	boom := errors.New("boom")

	_, err := Load[dsnCfg](context.Background(),
		WithDerive(func(c *dsnCfg) error { return boom }),
	)
	if err == nil {
		t.Fatal("a derive returning an error must fail the Load")
	}
	var de *DeriveError
	if !errors.As(err, &de) {
		t.Fatalf("got %T, want *DeriveError", err)
	}
	if !errors.Is(err, boom) {
		t.Error("the cause must survive in the chain")
	}
}

func TestDeriveTypeMismatchFailsLoad(t *testing.T) {
	t.Setenv("DERIVE_USER", "alice")
	t.Setenv("DERIVE_PASS", "s3cret")
	type otherCfg struct{ Z string }

	_, err := Load[dsnCfg](context.Background(),
		WithDerive(func(c *otherCfg) error { return nil }),
	)
	if err == nil {
		t.Fatal("a mismatched derive type must fail Load, not be silently skipped")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("got %v, want an error satisfying ErrInvalid", err)
	}
}
