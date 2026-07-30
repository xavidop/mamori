package mamori

import (
	"errors"
	"reflect"
	"testing"
)

func TestSplitArgs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "vault-agent token", []string{"vault-agent", "token"}},
		{"collapses runs of space", "a   b\tc", []string{"a", "b", "c"}},
		{"double quotes group", `mytool --msg "hello world"`, []string{"mytool", "--msg", "hello world"}},
		{"single quotes group", `sh -c 'echo hi'`, []string{"sh", "-c", "echo hi"}},
		{"single quotes are literal", `echo 'a\b$c"d'`, []string{"echo", `a\b$c"d`}},
		{"backslash escapes in double quotes", `echo "a\"b"`, []string{"echo", `a"b`}},
		{"backslash escapes outside quotes", `echo a\ b`, []string{"echo", "a b"}},
		{"adjacent quoted and bare concatenate", `echo pre"fix post"`, []string{"echo", "prefix post"}},
		{"empty quoted argument is preserved", `mytool ""`, []string{"mytool", ""}},
		{"empty input", "", nil},
		{"only whitespace", "   ", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitArgs(tt.in)
			if err != nil {
				t.Fatalf("splitArgs(%q): %v", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitArgs(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

// TestSplitArgsRejectsUnbalanced covers the cases that must not be guessed at.
// Silently closing an open quote would run a command the author never wrote.
func TestSplitArgsRejectsUnbalanced(t *testing.T) {
	for _, in := range []string{`sh -c 'echo hi`, `echo "unterminated`, `echo trailing\`} {
		t.Run(in, func(t *testing.T) {
			got, err := splitArgs(in)
			if err == nil {
				t.Fatalf("splitArgs(%q) = %#v, want an error", in, got)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error %v does not satisfy errors.Is(ErrInvalid)", err)
			}
			if got != nil {
				t.Errorf("returned %#v alongside an error, want nil", got)
			}
		})
	}
}
