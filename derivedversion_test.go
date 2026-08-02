package mamori

import (
	"reflect"
	"testing"

	"github.com/xavidop/mamori/secret"
)

// TestDerivedVersionRevealsSecretString is the guard against hashing the
// redacted form. secret.String.String() returns "[REDACTED]", so an
// implementation reaching for %v gives every secret field an identical,
// rotation-proof version. Two different secrets must hash differently.
func TestDerivedVersionRevealsSecretString(t *testing.T) {
	a := derivedVersion(reflect.ValueOf(secret.NewString("postgres://u:old@h/db")))
	b := derivedVersion(reflect.ValueOf(secret.NewString("postgres://u:new@h/db")))
	if a == "" || b == "" {
		t.Fatalf("expected non-empty versions, got %q and %q", a, b)
	}
	if a == b {
		t.Fatalf("two different secrets hashed identically to %q: the redacted form was hashed, not the revealed value", a)
	}
}

// TestDerivedVersionRevealsSecretBytes is TestDerivedVersionRevealsSecretString
// for the secret.Bytes half, which redacts through the same methods.
func TestDerivedVersionRevealsSecretBytes(t *testing.T) {
	a := derivedVersion(reflect.ValueOf(secret.NewBytes([]byte("old"))))
	b := derivedVersion(reflect.ValueOf(secret.NewBytes([]byte("new"))))
	if a == b {
		t.Fatalf("two different secrets hashed identically to %q", a)
	}
}

// TestDerivedVersionMatchesVersionHash pins that a derived version is the same
// helper ~35 providers use, not a second hashing scheme, so an operator
// comparing a derived row against a sourced one is comparing like with like.
func TestDerivedVersionMatchesVersionHash(t *testing.T) {
	got := derivedVersion(reflect.ValueOf("plain"))
	want := VersionHash([]byte("plain"))
	if got != want {
		t.Fatalf("derivedVersion = %q, want VersionHash = %q", got, want)
	}
}

// TestDerivedVersionStableAcrossCalls pins determinism: the same value must
// hash identically every time, or Status would report spurious churn.
func TestDerivedVersionStableAcrossCalls(t *testing.T) {
	v := reflect.ValueOf(map[string]int{"b": 2, "a": 1})
	first := derivedVersion(v)
	second := derivedVersion(v)
	if first != second {
		t.Fatal("derivedVersion is not deterministic for the same value")
	}
}

// TestDerivedVersionUnreadableIsEmpty covers the unexported-field path, which
// report.go already guards with CanInterface: derivedVersion must not panic.
func TestDerivedVersionUnreadableIsEmpty(t *testing.T) {
	if got := derivedVersion(reflect.Value{}); got != "" {
		t.Fatalf("invalid reflect.Value gave %q, want empty", got)
	}
}
