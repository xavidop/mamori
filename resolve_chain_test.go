package mamori_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
)

// TestResolveChainFirstRefWins verifies precedence: with both refs able to
// resolve, the first one in declaration order wins, not the second.
func TestResolveChainFirstRefWins(t *testing.T) {
	a := mamoritest.NewProvider("chn-a1")
	b := mamoritest.NewProvider("chn-b1")
	a.Set("x", "from-a")
	b.Set("y", "from-b")

	type cfg struct {
		V string `source:"chn-a1://x,chn-b1://y"`
	}
	c, err := mamori.Load[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.V != "from-a" {
		t.Errorf("V = %q, want from-a (first ref should win)", c.V)
	}
}

// TestResolveChainFallsThroughOnNotFound verifies that ErrNotFound is the
// only outcome that lets the walk continue to the next ref.
func TestResolveChainFallsThroughOnNotFound(t *testing.T) {
	a := mamoritest.NewProvider("chn-a2")
	b := mamoritest.NewProvider("chn-b2")
	// a never had a value for "x" (equivalent to a Del'd/absent key).
	b.Set("y", "from-b")

	type cfg struct {
		V string `source:"chn-a2://x,chn-b2://y"`
	}
	c, err := mamori.Load[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.V != "from-b" {
		t.Errorf("V = %q, want from-b (fall through past a not-found ref)", c.V)
	}

	// Del makes the fall-through explicit even after a value was once present.
	a.Set("x", "was-here")
	a.Del("x")
	c2, err := mamori.Load[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b))
	if err != nil {
		t.Fatalf("Load after Del: %v", err)
	}
	if c2.V != "from-b" {
		t.Errorf("V after Del = %q, want from-b", c2.V)
	}
}

// TestResolveChainNonNotFoundStopsWalk is the precedence-not-failover
// regression test (spec 10.3 case 3, decision D2): a first ref failing with a
// non-not-found error must stop the walk. The chain must NOT slide down to
// the second ref even though it has a value ready, and applying the field's
// onfail policy (default keeplast, no prior value on an initial Load) must
// fail the Load rather than silently using the second ref.
func TestResolveChainNonNotFoundStopsWalk(t *testing.T) {
	a := mamoritest.NewProvider("chn-a3")
	b := mamoritest.NewProvider("chn-b3")
	a.Fail("x", fmt.Errorf("%w: denied", mamori.ErrPermissionDenied))
	b.Set("y", "from-b")

	type cfg struct {
		V string `source:"chn-a3://x,chn-b3://y"`
	}
	_, err := mamori.Load[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b))
	if err == nil {
		t.Fatal("Load succeeded; a permission-denied first ref must stop the walk, not fail over to the second ref")
	}
	if mamori.ErrorKind(err) != mamori.KindPermissionDenied {
		t.Errorf("ErrorKind(err) = %q, want %q", mamori.ErrorKind(err), mamori.KindPermissionDenied)
	}
}

// TestResolveChainAllNotFoundAppliesDefault covers chain case 4: every ref
// reporting ErrNotFound applies default:, exactly as a single ref would.
func TestResolveChainAllNotFoundAppliesDefault(t *testing.T) {
	a := mamoritest.NewProvider("chn-a4")
	b := mamoritest.NewProvider("chn-b4")

	type cfg struct {
		V string `source:"chn-a4://x,chn-b4://y" default:"fallback"`
	}
	c, err := mamori.Load[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.V != "fallback" {
		t.Errorf("V = %q, want fallback", c.V)
	}
}

// TestResolveChainAllNotFoundOptionalIsZeroValue covers chain case 4 with no
// default: an optional field with every ref absent resolves to the zero
// value rather than failing.
func TestResolveChainAllNotFoundOptionalIsZeroValue(t *testing.T) {
	a := mamoritest.NewProvider("chn-a5")
	b := mamoritest.NewProvider("chn-b5")

	type cfg struct {
		V string `source:"chn-a5://x,chn-b5://y" optional:"true"`
	}
	c, err := mamori.Load[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.V != "" {
		t.Errorf("V = %q, want empty (zero value)", c.V)
	}
}

// TestResolveChainAllNotFoundRequiredFails covers chain case 4 with no
// default and no optional: a required field with every ref absent fails.
func TestResolveChainAllNotFoundRequiredFails(t *testing.T) {
	a := mamoritest.NewProvider("chn-a6")
	b := mamoritest.NewProvider("chn-b6")

	type cfg struct {
		V string `source:"chn-a6://x,chn-b6://y"`
	}
	_, err := mamori.Load[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b))
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// TestResolveChainOnFailDefaultTagNeverMasksError is the corrected-rule
// regression test: a `default:` tag applies ONLY on genuine absence (every
// ref not-found). It must NEVER be silently triggered by a non-not-found
// error, even with a default: present and no onfail tag (the default policy,
// onfail:"keeplast"). On an initial Load there is no prior value for
// keeplast to retain, so it fails instead of masking the error behind the
// default - the same footgun classifying a missing exec binary as
// KindUnknown (see TestExecProviderMissingBinaryDoesNotTriggerDefault) exists
// to prevent.
func TestResolveChainOnFailDefaultTagNeverMasksError(t *testing.T) {
	a := mamoritest.NewProvider("chn-a7")
	b := mamoritest.NewProvider("chn-b7")
	a.Fail("x", fmt.Errorf("%w: denied", mamori.ErrPermissionDenied))
	b.Set("y", "from-b")

	type cfg struct {
		V string `source:"chn-a7://x,chn-b7://y" default:"fallback"`
	}
	_, err := mamori.Load[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b))
	if err == nil {
		t.Fatal("Load succeeded; a non-not-found error must fail Load, not silently apply default:")
	}
	if mamori.ErrorKind(err) != mamori.KindPermissionDenied {
		t.Errorf("ErrorKind(err) = %q, want %q", mamori.ErrorKind(err), mamori.KindPermissionDenied)
	}
}

// TestResolveChainOnFailUseDefaultOptsInExplicitly is the counterpart to
// TestResolveChainOnFailDefaultTagNeverMasksError: onfail:"default" is the
// only way to have an error fall back to default:, and it is an explicit,
// visible opt-in on the tag rather than something a bare default: implies.
func TestResolveChainOnFailUseDefaultOptsInExplicitly(t *testing.T) {
	a := mamoritest.NewProvider("chn-a8")
	b := mamoritest.NewProvider("chn-b8")
	a.Fail("x", fmt.Errorf("%w: denied", mamori.ErrPermissionDenied))
	b.Set("y", "from-b")

	type cfg struct {
		V string `source:"chn-a8://x,chn-b8://y" default:"fallback" onfail:"default"`
	}
	c, err := mamori.Load[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.V != "fallback" {
		t.Errorf("V = %q, want fallback", c.V)
	}
}

// TestResolveChainOnFailFailAlwaysFails covers onfail:"fail": an errored ref
// rejects the Load even when a default: is present.
func TestResolveChainOnFailFailAlwaysFails(t *testing.T) {
	a := mamoritest.NewProvider("chn-a9")
	b := mamoritest.NewProvider("chn-b9")
	a.Fail("x", fmt.Errorf("%w: denied", mamori.ErrPermissionDenied))
	b.Set("y", "from-b")

	type cfg struct {
		V string `source:"chn-a9://x,chn-b9://y" default:"fallback" onfail:"fail"`
	}
	_, err := mamori.Load[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b))
	if err == nil {
		t.Fatal("Load succeeded; onfail:fail must reject the Load")
	}
	if mamori.ErrorKind(err) != mamori.KindPermissionDenied {
		t.Errorf("ErrorKind(err) = %q, want %q", mamori.ErrorKind(err), mamori.KindPermissionDenied)
	}
}

// TestDoctorChainReportsWinningRefOnFailure verifies Doctor walks the chain
// with the same precedence-not-failover rule as Load: a first ref that fails
// with a non-not-found error is reported as the field's outcome even though
// the second ref has a value ready, because the walk never reaches it.
func TestDoctorChainReportsWinningRefOnFailure(t *testing.T) {
	a := mamoritest.NewProvider("chn-a10")
	b := mamoritest.NewProvider("chn-b10")
	a.Fail("x", fmt.Errorf("%w: denied", mamori.ErrPermissionDenied))
	b.Set("y", "from-b")

	type cfg struct {
		V string `source:"chn-a10://x,chn-b10://y"`
	}
	rep, err := mamori.Doctor[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b))
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if len(rep.Fields) != 1 {
		t.Fatalf("Doctor reported %d fields, want 1", len(rep.Fields))
	}
	f := rep.Fields[0]
	if f.LastKind != mamori.KindPermissionDenied {
		t.Errorf("LastKind = %q, want %q", f.LastKind, mamori.KindPermissionDenied)
	}
	if f.Scheme != "chn-a10" {
		t.Errorf("Scheme = %q, want chn-a10 (the ref that stopped the walk, not the second ref)", f.Scheme)
	}
	if rep.Healthy {
		t.Error("Doctor reported healthy despite a chain-stopping error")
	}
}

// TestDoctorChainReportsWinningRefOnSuccess verifies Doctor reports the
// actual winning ref of a chain (not always the first/primary ref) when the
// first ref is absent and the second resolves.
func TestDoctorChainReportsWinningRefOnSuccess(t *testing.T) {
	a := mamoritest.NewProvider("chn-a11")
	b := mamoritest.NewProvider("chn-b11")
	b.Set("y", "from-b")

	type cfg struct {
		V string `source:"chn-a11://x,chn-b11://y"`
	}
	rep, err := mamori.Doctor[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b))
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	f := rep.Fields[0]
	if f.Scheme != "chn-b11" {
		t.Errorf("Scheme = %q, want chn-b11 (the winning ref)", f.Scheme)
	}
	if !rep.Healthy {
		t.Errorf("Doctor reported unhealthy for a resolvable chain: %+v", rep)
	}
}

// TestDoctorChainAbsentWithDefaultIsHealthy covers chain case 4 through
// Doctor: every ref not-found but the field has a default:, so it resolves in
// practice and Doctor must report it healthy.
func TestDoctorChainAbsentWithDefaultIsHealthy(t *testing.T) {
	a := mamoritest.NewProvider("chn-a12")
	b := mamoritest.NewProvider("chn-b12")

	type cfg struct {
		V string `source:"chn-a12://x,chn-b12://y" default:"fallback"`
	}
	rep, err := mamori.Doctor[cfg](context.Background(), mamori.WithProvider(a), mamori.WithProvider(b))
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !rep.Healthy {
		t.Fatalf("Doctor reported unhealthy for a chain covered by default: %+v", rep)
	}
}
