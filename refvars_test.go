package mamori

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestExpandRefVars(t *testing.T) {
	vars := map[string]string{"ENV": "prod", "SVC": "checkout", "EMPTY": ""}
	tests := []struct{ in, want string }{
		{"aws-sm://${ENV}/db#password", "aws-sm://prod/db#password"},
		{"${ENV}-sm://x", "prod-sm://x"},
		{"aws-sm://a?tag=${SVC}", "aws-sm://a?tag=checkout"},
		{"aws-sm://a#${SVC}", "aws-sm://a#checkout"},
		{"env:PORT,aws-ps://${SVC}/port", "env:PORT,aws-ps://checkout/port"},
		{"env:NO_VARS_HERE", "env:NO_VARS_HERE"},
		{"exec:echo $HOME", "exec:echo $HOME"},  // bare $ is left alone
		{"exec:echo $$HOME", "exec:echo $HOME"}, // $$ is a literal $
		{"aws-sm://${EMPTY}x", "aws-sm://x"},    // defined-but-empty is legal
	}
	for _, tt := range tests {
		got, err := expandRefVars(tt.in, vars)
		if err != nil {
			t.Fatalf("expandRefVars(%q): %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("expandRefVars(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestExpandRefVarsUndefined(t *testing.T) {
	_, err := expandRefVars("aws-sm://${NOPE}/db", map[string]string{})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "NOPE") {
		t.Errorf("error %q should name the variable", err)
	}
	if !strings.Contains(err.Error(), "WithRefVars") {
		t.Errorf("error %q should say how to fix it", err)
	}
}

func TestExpandRefVarsUnterminated(t *testing.T) {
	if _, err := expandRefVars("aws-sm://${NOPE/db", map[string]string{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestExpandRefVarsEmptyName(t *testing.T) {
	_, err := expandRefVars("aws-sm://${}/db", map[string]string{})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q should call out the empty variable name, not just report it as undefined", err)
	}
}

// TestExpandRefVarsNotRecursive pins that a variable's own value is inserted
// verbatim and never rescanned. This is deliberate (recursive expansion would
// risk an infinite loop from a self-referencing or cyclic variable value), but
// nothing else in the test suite would catch a future refactor that started
// rescanning expanded output, so this test exists to catch exactly that.
func TestExpandRefVarsNotRecursive(t *testing.T) {
	vars := map[string]string{"A": "${B}", "B": "resolved"}
	got, err := expandRefVars("x-${A}-y", vars)
	if err != nil {
		t.Fatalf("expandRefVars: %v", err)
	}
	if want := "x-${B}-y"; got != want {
		t.Errorf("expandRefVars = %q, want %q (expansion must not recurse into an expanded value)", got, want)
	}
}

func TestFieldSpecsExpandsAndReportsField(t *testing.T) {
	type cfg struct {
		Pass string `source:"aws-sm://${ENV}/db#password"`
	}
	specs, err := fieldSpecs(reflect.TypeOf(cfg{}), map[string]string{"ENV": "prod"})
	if err != nil {
		t.Fatalf("fieldSpecs: %v", err)
	}
	if got := specs[0].Refs[0].Path; got != "prod/db" {
		t.Errorf("Path = %q, want prod/db", got)
	}
	if got := specs[0].Refs[0].Raw; !strings.Contains(got, "prod") {
		t.Errorf("Raw = %q, want the expanded form", got)
	}

	_, err = fieldSpecs(reflect.TypeOf(cfg{}), nil)
	if err == nil {
		t.Fatal("want an error for an undefined variable, got nil")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
	// The message must name the field, the ref, and the variable together, so
	// it points at something a user can fix: which field, which source tag,
	// and which name is missing from WithRefVars. Asserting each in isolation
	// would not catch a future change that dropped one of the three while
	// keeping the others.
	for _, want := range []string{"Pass", "ENV", "aws-sm://${ENV}/db#password"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
	}
}

func TestWithRefVarsMerges(t *testing.T) {
	o := defaultOptions()
	WithRefVars(map[string]string{"A": "1", "B": "2"})(o)
	WithRefVars(map[string]string{"B": "override", "C": "3"})(o)
	want := map[string]string{"A": "1", "B": "override", "C": "3"}
	for k, v := range want {
		if o.refVars[k] != v {
			t.Errorf("refVars[%q] = %q, want %q", k, o.refVars[k], v)
		}
	}
}

func TestEnvVarsOmitsUnset(t *testing.T) {
	t.Setenv("MAMORI_TEST_SET", "yes")
	got := EnvVars("MAMORI_TEST_SET", "MAMORI_TEST_DEFINITELY_UNSET")
	if got["MAMORI_TEST_SET"] != "yes" {
		t.Errorf("set var = %q, want yes", got["MAMORI_TEST_SET"])
	}
	// An unset variable must be absent rather than empty, so expansion reports
	// the undefined-variable error rather than silently expanding to "".
	if _, present := got["MAMORI_TEST_DEFINITELY_UNSET"]; present {
		t.Error("unset var must be omitted, not mapped to the empty string")
	}
}
