package mamori

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestDoctorEmitsDerivedRow pins the headline behavior: a declared write path
// gets a FieldStatus from Doctor, carrying a real version, where before this
// change Doctor never emitted one at all.
func TestDoctorEmitsDerivedRow(t *testing.T) {
	m := newMem("ddrow")
	m.put("host", "db.example.com")

	type Config struct {
		Host string `source:"ddrow://host"`
		DSN  string
	}
	rep, err := Doctor[Config](context.Background(),
		WithProvider(m),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.Host; return nil }, "DSN"),
	)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	var found *FieldStatus
	for i := range rep.Fields {
		if rep.Fields[i].Path == "DSN" {
			found = &rep.Fields[i]
		}
	}
	if found == nil {
		t.Fatalf("rep.Fields has no DSN entry: %+v", rep.Fields)
	}
	if !found.Derived {
		t.Error("DSN entry has Derived = false, want true")
	}
	if found.Version == "" {
		t.Error("DSN entry has empty Version, want a non-empty derivedVersion of the resolved DSN")
	}
}

// TestDoctorDerivedRowBlockedWhenSourceUnhealthy pins the chosen semantics: a
// hash computed from a zero value is worse than no hash, because it looks real
// and will not match what production computes.
func TestDoctorDerivedRowBlockedWhenSourceUnhealthy(t *testing.T) {
	m := newMem("ddblocked")
	// Host is required but never populated in the provider, so it resolves to
	// ErrNotFound and never reaches a default.

	type Config struct {
		Host string `source:"ddblocked://host"`
		DSN  string
	}
	rep, err := Doctor[Config](context.Background(),
		WithProvider(m),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.Host; return nil }, "DSN"),
	)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	var found *FieldStatus
	for i := range rep.Fields {
		if rep.Fields[i].Path == "DSN" {
			found = &rep.Fields[i]
		}
	}
	if found == nil {
		t.Fatalf("rep.Fields has no DSN entry: %+v", rep.Fields)
	}
	if found.Version != "" {
		t.Errorf("DSN entry has Version %q, want empty: the source field never resolved", found.Version)
	}
	if !strings.Contains(found.LastError, "not evaluated") {
		t.Errorf("DSN entry LastError = %q, want it to mention the row was not evaluated", found.LastError)
	}
	if found.LastKind != "" {
		t.Errorf("DSN entry LastKind = %q, want empty: it must not double-count the already-unhealthy source field", found.LastKind)
	}
	if rep.Healthy {
		t.Error("rep.Healthy = true, want false: the required Host field is unreachable")
	}
}

// TestDoctorDerivedRowUsesDefaultNotZero pins that an absent-but-defaulted
// field, which Doctor already reports healthy, feeds its default into the hook.
// Deriving from the zero value here would publish a version that silently
// disagrees with production.
func TestDoctorDerivedRowUsesDefaultNotZero(t *testing.T) {
	m := newMem("dddefault")
	// Host is never populated, so it resolves via its default "h" rather than
	// ErrNotFound going unhandled.

	type Config struct {
		Host string `source:"dddefault://host" default:"h"`
		DSN  string
	}
	rep, err := Doctor[Config](context.Background(),
		WithProvider(m),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.Host; return nil }, "DSN"),
	)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	var found *FieldStatus
	for i := range rep.Fields {
		if rep.Fields[i].Path == "DSN" {
			found = &rep.Fields[i]
		}
	}
	if found == nil {
		t.Fatalf("rep.Fields has no DSN entry: %+v", rep.Fields)
	}
	want := derivedVersion(reflect.ValueOf("postgres://h"))
	if found.Version != want {
		t.Errorf("DSN entry Version = %q, want %q (derived from the default %q, not the zero value)", found.Version, want, "h")
	}
}

// TestDoctorFailingHookIsUnhealthy pins that a derived field can now fail a
// preflight, which is the whole point of probing it.
func TestDoctorFailingHookIsUnhealthy(t *testing.T) {
	m := newMem("ddfail")
	m.put("host", "db.example.com")
	boom := errors.New("derive boom")

	type Config struct {
		Host string `source:"ddfail://host"`
		DSN  string
	}
	rep, err := Doctor[Config](context.Background(),
		WithProvider(m),
		WithDerive(func(c *Config) error { return boom }, "DSN"),
	)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if rep.Healthy {
		t.Error("rep.Healthy = true, want false: the derive hook returned an error")
	}
	var found *FieldStatus
	for i := range rep.Fields {
		if rep.Fields[i].Path == "DSN" {
			found = &rep.Fields[i]
		}
	}
	if found == nil {
		t.Fatalf("rep.Fields has no DSN entry: %+v", rep.Fields)
	}
	if found.LastKind != KindInvalid {
		t.Errorf("DSN entry LastKind = %q, want %q", found.LastKind, KindInvalid)
	}
}

// countingResolveProvider wraps memProvider and counts how many times Resolve
// was called, so TestDoctorDeriveAddsNoRoundTrips can prove the derive phase
// spends no extra provider round trips beyond the probe loop's own.
type countingResolveProvider struct {
	*memProvider
	calls int
}

func (c *countingResolveProvider) Resolve(ctx context.Context, ref Ref) (Value, error) {
	c.calls++
	return c.memProvider.Resolve(ctx, ref)
}

// TestDoctorDeriveAddsNoRoundTrips pins that the derive phase reuses values the
// probe loop already resolved. A counting provider must see exactly the same
// call count with and without WithDerive.
func TestDoctorDeriveAddsNoRoundTrips(t *testing.T) {
	m1 := newMem("ddcount1")
	m1.put("host", "db.example.com")
	c1 := &countingResolveProvider{memProvider: m1}

	type ConfigNoDerive struct {
		Host string `source:"ddcount1://host"`
	}
	if _, err := Doctor[ConfigNoDerive](context.Background(), WithProvider(c1)); err != nil {
		t.Fatalf("Doctor without WithDerive: %v", err)
	}

	m2 := newMem("ddcount2")
	m2.put("host", "db.example.com")
	c2 := &countingResolveProvider{memProvider: m2}

	type ConfigWithDerive struct {
		Host string `source:"ddcount2://host"`
		DSN  string
	}
	if _, err := Doctor[ConfigWithDerive](context.Background(),
		WithProvider(c2),
		WithDerive(func(c *ConfigWithDerive) error { c.DSN = "postgres://" + c.Host; return nil }, "DSN"),
	); err != nil {
		t.Fatalf("Doctor with WithDerive: %v", err)
	}

	if c1.calls != c2.calls {
		t.Errorf("Resolve called %d times without WithDerive, %d times with WithDerive; want equal, the derive phase must not spend an extra round trip", c1.calls, c2.calls)
	}
}

// deriveErrProvider fails every ref with a fixed error, so a test can choose
// exactly which Kind a source field comes back with.
type deriveErrProvider struct {
	scheme string
	err    error
}

func (p *deriveErrProvider) Scheme() string { return p.scheme }

func (p *deriveErrProvider) Resolve(context.Context, Ref) (Value, error) { return Value{}, p.err }

// TestDoctorDerivedRowBlockedOnSelfHealingSourceFailure is the gate that
// matters: the derive phase must be blocked by a source field that produced no
// value, not by the report's healthy flag. fieldUnhealthy counts only the
// terminal kinds, so a field failing with unavailable, rate-limited, or unknown
// leaves the report healthy while having nothing at all to feed a hook. Gating
// on healthy therefore published a hash of the zero value: a version that looks
// real and does not match what production computes.
func TestDoctorDerivedRowBlockedOnSelfHealingSourceFailure(t *testing.T) {
	type Config struct {
		Host string `source:"ddselfheal://host"`
		DSN  string
	}
	for _, tc := range []struct {
		name string
		err  error
		kind Kind
	}{
		{"unavailable", ErrUnavailable, KindUnavailable},
		{"rate limited", ErrRateLimited, KindRateLimited},
		{"unknown", errors.New("boom"), KindUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep, err := Doctor[Config](context.Background(),
				WithProvider(&deriveErrProvider{scheme: "ddselfheal", err: tc.err}),
				WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.Host; return nil }, "DSN"),
			)
			if err != nil {
				t.Fatalf("Doctor: %v", err)
			}
			host := findField(t, rep, "Host")
			if host.LastKind != tc.kind {
				t.Fatalf("Host LastKind = %q, want %q (the premise of this test)", host.LastKind, tc.kind)
			}
			dsn := findField(t, rep, "DSN")
			if dsn.Version != "" {
				t.Errorf("DSN Version = %q, want empty: Host never resolved, so the hook had only a zero value to read", dsn.Version)
			}
			if !strings.Contains(dsn.LastError, "not evaluated") {
				t.Errorf("DSN LastError = %q, want it to say the row was not evaluated", dsn.LastError)
			}
		})
	}
}

// findField returns the report's entry for path, failing the test if there is
// none.
func findField(t *testing.T, rep Report, path string) FieldStatus {
	t.Helper()
	for _, f := range rep.Fields {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("rep.Fields has no %q entry: %+v", path, rep.Fields)
	return FieldStatus{}
}

// TestDoctorDerivedRowMatchesLoadForOnFailDefault pins that an onfail:"default"
// field feeds the hook the same value in Doctor as it does in Load. Doctor used
// to mirror only applyDefault (absence), not applyOnFail (an error the field
// explicitly opted into tolerating), so the hook derived from the zero value
// while Load derived from the default and the two published versions for the
// same deployable config disagreed.
func TestDoctorDerivedRowMatchesLoadForOnFailDefault(t *testing.T) {
	type Config struct {
		Host string `source:"ddonfail://host" default:"h" onfail:"default"`
		DSN  string
	}
	opts := []Option{
		WithProvider(&deriveErrProvider{scheme: "ddonfail", err: ErrUnavailable}),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.Host; return nil }, "DSN"),
	}

	cfg, err := Load[Config](context.Background(), opts...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Host != "h" {
		t.Fatalf("Load built Host = %q, want the default %q (the premise of this test)", cfg.Host, "h")
	}

	rep, err := Doctor[Config](context.Background(), opts...)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	want := derivedVersion(reflect.ValueOf(cfg.DSN))
	if got := findField(t, rep, "DSN").Version; got != want {
		t.Errorf("DSN Version = %q, want %q: Doctor must derive from the same default Load derives from", got, want)
	}
}

// TestDoctorMismatchedDeriveHookIsUnhealthy pins that a hook typed for another
// config fails the preflight instead of vanishing. Load and Watch reject the
// identical options with ErrInvalid, so a Doctor that returns green on them
// reports a config that cannot start as deployable - the one outcome a
// preflight must never produce.
func TestDoctorMismatchedDeriveHookIsUnhealthy(t *testing.T) {
	m := newMem("ddmismatch")
	m.put("host", "db.example.com")

	type Config struct {
		Host string `source:"ddmismatch://host"`
		DSN  string
	}
	type Other struct {
		DSN string
	}
	opts := []Option{
		WithProvider(m),
		WithDerive(func(o *Other) error { o.DSN = "nope"; return nil }, "DSN"),
	}

	if _, err := Load[Config](context.Background(), opts...); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Load error = %v, want ErrInvalid (the behavior Doctor has to preflight)", err)
	}

	rep, err := Doctor[Config](context.Background(), opts...)
	if err != nil {
		t.Fatalf("Doctor returned %v, want nil: the returned error covers only an unwalkable T", err)
	}
	if rep.Healthy {
		t.Error("rep.Healthy = true, want false: this config fails at startup")
	}
	dsn := findField(t, rep, "DSN")
	if !dsn.Derived {
		t.Error("DSN entry has Derived = false, want true")
	}
	if dsn.LastKind != KindInvalid {
		t.Errorf("DSN LastKind = %q, want %q", dsn.LastKind, KindInvalid)
	}
	if !strings.Contains(dsn.LastError, "WithDerive hook has type") {
		t.Errorf("DSN LastError = %q, want the type-mismatch text", dsn.LastError)
	}
}

// TestDoctorEmptyDeriveWritePathIsUnhealthy covers the other typedDerives
// rejection through the same path: a declared write path that is empty or
// whitespace-only also fails Load and Watch outright.
func TestDoctorEmptyDeriveWritePathIsUnhealthy(t *testing.T) {
	m := newMem("ddempty")
	m.put("host", "db.example.com")

	type Config struct {
		Host string `source:"ddempty://host"`
		DSN  string
	}
	opts := []Option{
		WithProvider(m),
		WithDerive(func(c *Config) error { c.DSN = "postgres://" + c.Host; return nil }, "  "),
	}

	if _, err := Load[Config](context.Background(), opts...); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Load error = %v, want ErrInvalid", err)
	}
	rep, err := Doctor[Config](context.Background(), opts...)
	if err != nil {
		t.Fatalf("Doctor returned %v, want nil", err)
	}
	if rep.Healthy {
		t.Error("rep.Healthy = true, want false: this config fails at startup")
	}
}
