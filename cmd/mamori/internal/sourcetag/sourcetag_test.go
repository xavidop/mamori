package sourcetag

import (
	"reflect"
	"testing"
)

func TestSplitChain(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		want []string
	}{
		{
			name: "plain ref",
			tag:  "env:X",
			want: []string{"env:X"},
		},
		{
			name: "two-ref chain",
			tag:  "env:X,aws-sm://s",
			want: []string{"env:X", "aws-sm://s"},
		},
		{
			name: "comma inside opaque path stays in one part",
			tag:  "exec:echo a,b",
			want: []string{"exec:echo a,b"},
		},
		{
			name: "comma inside query stays in one part",
			tag:  "file:///x?a=1,2",
			want: []string{"file:///x?a=1,2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitChain(tt.tag)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SplitChain(%q) = %#v, want %#v", tt.tag, got, tt.want)
			}
		})
	}
}

func TestSchemeOf(t *testing.T) {
	tests := []struct {
		name       string
		ref        string
		wantScheme string
		wantOK     bool
	}{
		{
			name:       "scheme with authority",
			ref:        "aws-sm://s",
			wantScheme: "aws-sm",
			wantOK:     true,
		},
		{
			name:       "no colon",
			ref:        "noscheme",
			wantScheme: "",
			wantOK:     false,
		},
		{
			name:       "empty",
			ref:        "",
			wantScheme: "",
			wantOK:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotScheme, gotOK := SchemeOf(tt.ref)
			if gotScheme != tt.wantScheme || gotOK != tt.wantOK {
				t.Errorf("SchemeOf(%q) = (%q, %v), want (%q, %v)", tt.ref, gotScheme, gotOK, tt.wantScheme, tt.wantOK)
			}
		})
	}
}

func TestFirstSensitiveScheme(t *testing.T) {
	tests := []struct {
		name       string
		tag        string
		wantScheme string
		wantOK     bool
	}{
		{
			name:       "sensitive ref later in chain",
			tag:        "env:X,vault://kv",
			wantScheme: "vault",
			wantOK:     true,
		},
		{
			name:       "no sensitive ref",
			tag:        "env:X",
			wantScheme: "",
			wantOK:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotScheme, gotOK := FirstSensitiveScheme(tt.tag)
			if gotScheme != tt.wantScheme || gotOK != tt.wantOK {
				t.Errorf("FirstSensitiveScheme(%q) = (%q, %v), want (%q, %v)", tt.tag, gotScheme, gotOK, tt.wantScheme, tt.wantOK)
			}
		})
	}
}

func TestIsSecretBearingScheme(t *testing.T) {
	if !IsSecretBearingScheme("aws-sm") {
		t.Error("IsSecretBearingScheme(\"aws-sm\") = false, want true")
	}
	if IsSecretBearingScheme("env") {
		t.Error("IsSecretBearingScheme(\"env\") = true, want false")
	}
}

// TestDefaultSecretSchemesIsolated checks that each call returns a fresh set,
// so one caller extending its set cannot leak schemes into another's (or into
// the package-level helpers).
func TestDefaultSecretSchemesIsolated(t *testing.T) {
	a := DefaultSecretSchemes()
	a.Add("mysecrets")

	b := DefaultSecretSchemes()
	if b.Contains("mysecrets") {
		t.Error("extending one set leaked into a later DefaultSecretSchemes()")
	}
	if IsSecretBearingScheme("mysecrets") {
		t.Error("extending one set leaked into the package-level default set")
	}
	if !b.Contains("aws-sm") {
		t.Error("a fresh set is missing the built-in aws-sm scheme")
	}
}

// TestSchemeSetExtends checks that an added scheme is reported by the set's
// own chain walk, and that the built-ins still are.
func TestSchemeSetExtends(t *testing.T) {
	set := DefaultSecretSchemes()
	set.Add("mysecrets")

	if scheme, ok := set.FirstSensitiveScheme("mysecrets://prod/token"); !ok || scheme != "mysecrets" {
		t.Errorf("FirstSensitiveScheme(custom) = (%q, %v), want (\"mysecrets\", true)", scheme, ok)
	}
	// Added anywhere in a chain, not just first, matching the built-in rule.
	if scheme, ok := set.FirstSensitiveScheme("env:TOKEN,mysecrets://prod/token"); !ok || scheme != "mysecrets" {
		t.Errorf("FirstSensitiveScheme(chained custom) = (%q, %v), want (\"mysecrets\", true)", scheme, ok)
	}
	if scheme, ok := set.FirstSensitiveScheme("vault://kv/db"); !ok || scheme != "vault" {
		t.Errorf("FirstSensitiveScheme(builtin) = (%q, %v), want (\"vault\", true)", scheme, ok)
	}
	if _, ok := set.FirstSensitiveScheme("env:LOG_LEVEL"); ok {
		t.Error("FirstSensitiveScheme(env:LOG_LEVEL) = true, want false")
	}
}

func TestSortedIsStable(t *testing.T) {
	set := SchemeSet{}
	set.Add("vault", "aws-sm", "op")
	got := set.Sorted()
	want := []string{"aws-sm", "op", "vault"}
	if len(got) != len(want) {
		t.Fatalf("Sorted() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Sorted() = %v, want %v", got, want)
		}
	}
}

func TestParseSchemeList(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"single", "mysecrets", []string{"mysecrets"}, false},
		{"multiple", "mysecrets,corp-kv", []string{"mysecrets", "corp-kv"}, false},
		{"spaces trimmed", " mysecrets , corp-kv ", []string{"mysecrets", "corp-kv"}, false},
		{"empty entries dropped", "mysecrets,,corp-kv,", []string{"mysecrets", "corp-kv"}, false},
		{"dots and plus allowed", "corp.kv,a+b", []string{"corp.kv", "a+b"}, false},
		{"full ref rejected", "mysecrets://prod", nil, true},
		{"trailing colon rejected", "mysecrets:", nil, true},
		{"leading digit rejected", "1secrets", nil, true},
		{"space inside rejected", "my secrets", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSchemeList(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseSchemeList(%q) = %v, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSchemeList(%q) error: %v", tt.in, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ParseSchemeList(%q) = %v, want %v", tt.in, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("ParseSchemeList(%q) = %v, want %v", tt.in, got, tt.want)
				}
			}
		})
	}
}
