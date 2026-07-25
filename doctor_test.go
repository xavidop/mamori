package mamori

import (
	"context"
	"testing"
)

func TestDoctorReportsHealthyResolution(t *testing.T) {
	t.Setenv("MAMORI_DOC_A", "alpha")
	type Config struct {
		A string `source:"env:MAMORI_DOC_A"`
		B string `source:"env:MAMORI_DOC_MISSING" default:"fallback"`
	}
	rep, err := Doctor[Config](context.Background())
	if err != nil {
		t.Fatalf("Doctor returned a walk error: %v", err)
	}
	if !rep.Healthy {
		t.Fatalf("Doctor reported unhealthy for a resolvable config: %+v", rep)
	}
	if len(rep.Fields) != 2 {
		t.Fatalf("Doctor reported %d fields, want 2", len(rep.Fields))
	}
}

func TestDoctorReportsPerFieldFailureWithoutAborting(t *testing.T) {
	// A required field with no source value must show as unhealthy in the report,
	// while a sibling field still resolves. Doctor must not stop at the first
	// failure.
	t.Setenv("MAMORI_DOC_OK", "here")
	type Config struct {
		OK      string `source:"env:MAMORI_DOC_OK"`
		Missing string `source:"env:MAMORI_DOC_ABSENT_REQUIRED"`
	}
	rep, err := Doctor[Config](context.Background())
	if err != nil {
		t.Fatalf("Doctor returned a walk error for a per-field failure: %v", err)
	}
	byPath := map[string]FieldStatus{}
	for _, f := range rep.Fields {
		byPath[f.Path] = f
	}
	if byPath["OK"].LastKind != "" {
		t.Errorf("OK field wrongly carries kind %q", byPath["OK"].LastKind)
	}
	if byPath["Missing"].LastKind != KindNotFound {
		t.Errorf("Missing field kind = %q, want not_found", byPath["Missing"].LastKind)
	}
	if rep.Healthy {
		t.Errorf("Doctor reported healthy despite a required field being absent")
	}
}

func TestDoctorWalkErrorForNonStruct(t *testing.T) {
	_, err := Doctor[int](context.Background())
	if err == nil {
		t.Fatal("Doctor over a non-struct type returned nil error")
	}
}

func TestDoctorSnapshotAndLiveAreZero(t *testing.T) {
	// Snapshot and Live distinguish a one-shot Doctor probe from a running
	// watcher's report, whose version starts at 1 (see TestStatusSnapshotVersionAdvances).
	t.Setenv("MAMORI_DOC_ZERO", "x")
	type Config struct {
		A string `source:"env:MAMORI_DOC_ZERO"`
	}
	rep, err := Doctor[Config](context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Snapshot != 0 {
		t.Errorf("Snapshot = %d, want 0", rep.Snapshot)
	}
	if rep.Live != 0 {
		t.Errorf("Live = %d, want 0", rep.Live)
	}
}

func TestDoctorUnregisteredSchemeIsInvalid(t *testing.T) {
	// A ref naming a scheme not wired into this process can never resolve; it is
	// a malformed configuration for this process, so Doctor classifies it as
	// KindInvalid rather than KindNotFound.
	type Config struct {
		A string `source:"mamori-doc-unregistered://x"`
	}
	rep, err := Doctor[Config](context.Background())
	if err != nil {
		t.Fatalf("Doctor returned a walk error: %v", err)
	}
	if len(rep.Fields) != 1 {
		t.Fatalf("Doctor reported %d fields, want 1", len(rep.Fields))
	}
	f := rep.Fields[0]
	if f.LastKind != KindInvalid {
		t.Errorf("LastKind = %q, want %q", f.LastKind, KindInvalid)
	}
	if rep.Healthy {
		t.Errorf("Doctor reported healthy with an unregistered scheme")
	}
}

func TestDoctorOptionalAbsentFieldIsHealthy(t *testing.T) {
	// A field that is optional and absent resolves in practice (it is left at
	// its zero value), so Doctor must report it healthy, not as a failure.
	type Config struct {
		A string `source:"env:MAMORI_DOC_OPTIONAL_ABSENT" optional:"true"`
	}
	rep, err := Doctor[Config](context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Healthy {
		t.Fatalf("Doctor reported unhealthy for an absent optional field: %+v", rep)
	}
	if rep.Fields[0].LastKind != "" {
		t.Errorf("optional absent field carries kind %q, want none", rep.Fields[0].LastKind)
	}
}
