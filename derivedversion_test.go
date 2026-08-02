package mamori

import (
	"reflect"
	"testing"
	"time"

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

// TestDerivedVersionRevealsNestedSecret is the top-level secret guard one level
// down, and the one that matters in practice: the derived-fields guide tells
// callers to assemble credentials into a secret.String, and nothing stops them
// declaring the enclosing struct as the write path. Formatting that struct
// hashes "[REDACTED]" for the secret inside it, so every rotation of the
// credential would report an identical, rotation-proof version.
func TestDerivedVersionRevealsNestedSecret(t *testing.T) {
	type Creds struct {
		User string
		Pass secret.String
	}
	old := derivedVersion(reflect.ValueOf(Creds{User: "u", Pass: secret.NewString("old")}))
	fresh := derivedVersion(reflect.ValueOf(Creds{User: "u", Pass: secret.NewString("new")}))
	if old == "" || fresh == "" {
		t.Fatalf("expected non-empty versions, got %q and %q", old, fresh)
	}
	if old == fresh {
		t.Fatalf("a struct holding two different secrets hashed identically to %q: the nested secret was redacted, not revealed", old)
	}
}

// TestDerivedVersionRevealsSecretAnywhere covers the rest of the shapes a
// secret can hide in: a slice element, a map value, and behind a pointer or an
// interface. Each pair differs only in the secret's plaintext.
func TestDerivedVersionRevealsSecretAnywhere(t *testing.T) {
	oldS, newS := secret.NewString("old"), secret.NewString("new")
	cases := []struct {
		name     string
		old, new any
	}{
		{"slice element", []secret.String{oldS}, []secret.String{newS}},
		{"map value", map[string]secret.String{"db": oldS}, map[string]secret.String{"db": newS}},
		{"pointer", &oldS, &newS},
		{"interface field", struct{ V any }{oldS}, struct{ V any }{newS}},
		{"secret bytes in a struct", struct{ B secret.Bytes }{secret.NewBytes([]byte("old"))}, struct{ B secret.Bytes }{secret.NewBytes([]byte("new"))}},
		{"unexported field", newUnexportedSecret("old"), newUnexportedSecret("new")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := derivedVersion(reflect.ValueOf(tc.old))
			b := derivedVersion(reflect.ValueOf(tc.new))
			if a == "" || b == "" {
				t.Fatalf("expected non-empty versions, got %q and %q", a, b)
			}
			if a == b {
				t.Fatalf("two different secrets hashed identically to %q", a)
			}
		})
	}
}

// unexportedSecret holds a secret where reflect refuses to hand it back through
// Interface, so the canonical walk has to reach the bytes structurally.
type unexportedSecret struct {
	pass secret.String
}

func newUnexportedSecret(s string) unexportedSecret {
	return unexportedSecret{pass: secret.NewString(s)}
}

// TestDerivedVersionHashesPointeeNotAddress pins that a pointer field is hashed
// by what it points at. Hashing the address instead reports a fresh Version on
// every rebuild even when nothing changed, while ev.Changed (which compares
// with reflect.DeepEqual) correctly reports no change: the two disagree.
func TestDerivedVersionHashesPointeeNotAddress(t *testing.T) {
	type Holder struct{ P *string }
	p, q, r := "x", "x", "y"
	first := derivedVersion(reflect.ValueOf(Holder{P: &p}))
	second := derivedVersion(reflect.ValueOf(Holder{P: &q}))
	if first != second {
		t.Fatalf("two pointers to equal contents hashed differently (%q vs %q): the address was hashed, not the pointee", first, second)
	}
	if changed := derivedVersion(reflect.ValueOf(Holder{P: &r})); changed == first {
		t.Fatalf("a pointer to different contents hashed identically to %q", changed)
	}
	if nilled := derivedVersion(reflect.ValueOf(Holder{})); nilled == first {
		t.Fatal("a nil pointer hashed the same as a pointer to a value")
	}
}

// TestDerivedVersionDistinguishesFieldBoundaries guards the framing the walk
// adds: without it, two structs whose fields concatenate to the same text would
// share a version, so a rotation that only moved a character between fields
// would report as no change at all.
func TestDerivedVersionDistinguishesFieldBoundaries(t *testing.T) {
	type Pair struct{ A, B string }
	if a, b := derivedVersion(reflect.ValueOf(Pair{"ab", ""})), derivedVersion(reflect.ValueOf(Pair{"a", "b"})); a == b {
		t.Fatalf("Pair{\"ab\", \"\"} and Pair{\"a\", \"b\"} both hashed to %q", a)
	}
}

// TestDerivedVersionStableForNestedValues pins determinism for the shapes the
// walk has to normalize itself: map iteration order is randomized per range,
// and a func value carries an address.
func TestDerivedVersionStableForNestedValues(t *testing.T) {
	type Nested struct {
		M  map[string]int
		Fn func()
		S  []*string
	}
	s1, s2 := "a", "a"
	first := derivedVersion(reflect.ValueOf(Nested{
		M:  map[string]int{"a": 1, "b": 2, "c": 3, "d": 4},
		Fn: func() {},
		S:  []*string{&s1},
	}))
	second := derivedVersion(reflect.ValueOf(Nested{
		M:  map[string]int{"d": 4, "c": 3, "b": 2, "a": 1},
		Fn: func() {},
		S:  []*string{&s2},
	}))
	if first != second {
		t.Fatalf("equal nested values hashed differently: %q vs %q", first, second)
	}
}

// TestDerivedVersionTerminatesOnCycle pins the depth bound: a config struct is
// a tree, but a self-referential pointer would otherwise recurse forever now
// that the walk follows pointers by value.
func TestDerivedVersionTerminatesOnCycle(t *testing.T) {
	type Node struct {
		Name string
		Next *Node
	}
	n := &Node{Name: "a"}
	n.Next = n
	done := make(chan string, 1)
	go func() { done <- derivedVersion(reflect.ValueOf(n)) }()
	select {
	case got := <-done:
		if got == "" {
			t.Fatal("a cyclic value hashed to the empty string, want a real version")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("derivedVersion did not terminate on a cyclic value")
	}
}

// TestDerivedFieldChangeCarriesVersions pins that a derived FieldChange
// reports the same content hash Status does on both sides, rather than the
// blank "" -> "" a caller's log loop cannot read. The two sides must differ:
// derivedFieldChanges only emits an entry when the values already differ.
func TestDerivedFieldChangeCarriesVersions(t *testing.T) {
	type cfg struct {
		Pass secret.String
		DSN  secret.String
	}
	derives := []typedDerive[cfg]{{
		fn:     func(c *cfg) error { c.DSN = secret.NewString("dsn-" + c.Pass.Reveal()); return nil },
		writes: []string{"DSN"},
	}}
	oldCfg := cfg{Pass: secret.NewString("old"), DSN: secret.NewString("dsn-old")}
	newCfg := cfg{Pass: secret.NewString("new"), DSN: secret.NewString("dsn-new")}

	got := derivedFieldChanges(oldCfg, newCfg, derives, nil)
	if len(got) != 1 {
		t.Fatalf("got %d FieldChange(s), want 1", len(got))
	}
	if got[0].OldVersion == "" || got[0].NewVersion == "" {
		t.Fatalf("derived FieldChange has a blank version: old=%q new=%q", got[0].OldVersion, got[0].NewVersion)
	}
	if got[0].OldVersion == got[0].NewVersion {
		t.Fatalf("both versions are %q, but the values differ", got[0].OldVersion)
	}
	if want := derivedVersion(reflect.ValueOf(newCfg.DSN)); got[0].NewVersion != want {
		t.Fatalf("NewVersion = %q, want the same hash Status reports (%q)", got[0].NewVersion, want)
	}
}

// TestDerivedVersionDistinguishesDynamicTypes pins that an interface-typed
// field's dynamic type is part of its version. Without it any(int(1)) and
// any(uint(1)) hash alike, as do any("x") and any(secret.NewString("x")), while
// reflect.DeepEqual (what ev.Changed uses) calls each pair different. The
// version would then sit still for a change ev.Changed reports.
func TestDerivedVersionDistinguishesDynamicTypes(t *testing.T) {
	cases := []struct {
		name string
		a, b any
	}{
		{"int vs uint", int(1), uint(1)},
		{"string vs secret.String", "x", secret.NewString("x")},
		{"int vs float", int(1), float64(1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if reflect.DeepEqual(c.a, c.b) {
				t.Fatalf("fixture is wrong: DeepEqual already calls these equal")
			}
			type holder struct{ V any }
			av := derivedVersion(reflect.ValueOf(holder{V: c.a}))
			bv := derivedVersion(reflect.ValueOf(holder{V: c.b}))
			if av == bv {
				t.Fatalf("both hashed to %q, but DeepEqual calls them different: ev.Changed would fire with no version move", av)
			}
		})
	}
}
