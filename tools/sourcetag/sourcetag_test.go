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
