package mamori

import (
	"reflect"
	"testing"
)

// specFor builds a minimal fieldSpec for fingerprint tests.
func specFor(path, ref string, sensitive bool) fieldSpec {
	r, err := ParseRef(ref)
	if err != nil {
		panic("bad ref in test: " + ref)
	}
	return fieldSpec{
		Path:      path,
		Refs:      []Ref{r},
		Type:      reflect.TypeOf(""),
		Sensitive: sensitive,
	}
}

func TestSchemaFingerprintIsStable(t *testing.T) {
	a := []fieldSpec{specFor("A", "env:A", false), specFor("B", "env:B", true)}
	b := []fieldSpec{specFor("A", "env:A", false), specFor("B", "env:B", true)}
	if schemaFingerprint(a) != schemaFingerprint(b) {
		t.Fatal("identical specs produced different fingerprints")
	}
}

// TestSchemaFingerprintIgnoresSpecOrder pins that a reordered struct is not
// treated as a different schema. Field order carries no meaning for whether a
// snapshot can satisfy a config, and treating it as drift would throw away a
// usable snapshot after a cosmetic edit.
func TestSchemaFingerprintIgnoresSpecOrder(t *testing.T) {
	a := []fieldSpec{specFor("A", "env:A", false), specFor("B", "env:B", false)}
	b := []fieldSpec{specFor("B", "env:B", false), specFor("A", "env:A", false)}
	if schemaFingerprint(a) != schemaFingerprint(b) {
		t.Fatal("reordering the specs changed the fingerprint")
	}
}

func TestSchemaFingerprintChangesOn(t *testing.T) {
	base := []fieldSpec{specFor("A", "env:A", false)}
	tests := []struct {
		name  string
		specs []fieldSpec
	}{
		{"an added field", []fieldSpec{specFor("A", "env:A", false), specFor("B", "env:B", false)}},
		{"a removed field", nil},
		{"a renamed field", []fieldSpec{specFor("Z", "env:A", false)}},
		{"a changed ref", []fieldSpec{specFor("A", "env:CHANGED", false)}},
		{"a changed sensitivity", []fieldSpec{specFor("A", "env:A", true)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if schemaFingerprint(base) == schemaFingerprint(tt.specs) {
				t.Fatalf("%s did not change the fingerprint", tt.name)
			}
		})
	}
}

// TestSchemaFingerprintChangesOnType pins that a field keeping its name and ref
// but changing type is drift. Restoring bytes into the wrong type would fail
// decoding with a message about the value rather than about the schema.
func TestSchemaFingerprintChangesOnType(t *testing.T) {
	a := []fieldSpec{{Path: "A", Type: reflect.TypeOf("")}}
	b := []fieldSpec{{Path: "A", Type: reflect.TypeOf(0)}}
	if schemaFingerprint(a) == schemaFingerprint(b) {
		t.Fatal("changing a field's type did not change the fingerprint")
	}
}
