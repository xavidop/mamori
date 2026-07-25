package mamori

import "testing"

func TestParseRef(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		scheme  string
		path    string
		key     string
		opts    map[string]string
		wantErr bool
	}{
		{name: "aws-sm with key", in: "aws-sm://prod/db#password", scheme: "aws-sm", path: "prod/db", key: "password"},
		{name: "vault with opt", in: "vault://kv/data/api#key?renew=true", scheme: "vault", path: "kv/data/api", key: "key", opts: map[string]string{"renew": "true"}},
		{name: "env opaque", in: "env:LOG_LEVEL", scheme: "env", path: "LOG_LEVEL"},
		{name: "file abs", in: "file:///etc/tls/tls.crt", scheme: "file", path: "/etc/tls/tls.crt"},
		{name: "exec opaque with args", in: "exec:echo hi", scheme: "exec", path: "echo hi"},
		{name: "env with debounce opt", in: "env:CERT?debounce=0", scheme: "env", path: "CERT", opts: map[string]string{"debounce": "0"}},
		{name: "gcp with version opt", in: "gcp-sm://proj/secret#k?version=3", scheme: "gcp-sm", path: "proj/secret", key: "k", opts: map[string]string{"version": "3"}},
		{name: "empty", in: "", wantErr: true},
		{name: "no scheme", in: "nonsense", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := ParseRef(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseRef(%q) = %+v, want error", tt.in, ref)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRef(%q) unexpected error: %v", tt.in, err)
			}
			if ref.Scheme != tt.scheme {
				t.Errorf("Scheme = %q, want %q", ref.Scheme, tt.scheme)
			}
			if ref.Path != tt.path {
				t.Errorf("Path = %q, want %q", ref.Path, tt.path)
			}
			if ref.Key != tt.key {
				t.Errorf("Key = %q, want %q", ref.Key, tt.key)
			}
			for k, want := range tt.opts {
				if got := ref.Opt(k); got != want {
					t.Errorf("Opt(%q) = %q, want %q", k, got, want)
				}
			}
			if ref.Raw != tt.in {
				t.Errorf("Raw = %q, want %q", ref.Raw, tt.in)
			}
		})
	}
}

// TestParseRefs covers the comma-ambiguity corpus: a comma is a chain
// separator only when what follows it looks like a new scheme, so commas
// inside a query string or an opaque exec: path must NOT be split.
func TestParseRefs(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantSchemes []string
		wantErr     bool
	}{
		{
			name:        "two refs",
			in:          "env:PORT,aws-ps://svc/port",
			wantSchemes: []string{"env", "aws-ps"},
		},
		{
			name:        "single ref still works",
			in:          "env:PORT",
			wantSchemes: []string{"env"},
		},
		{
			name:        "comma inside query is not a separator",
			in:          "vault://kv?tags=a,b",
			wantSchemes: []string{"vault"},
		},
		{
			name:        "comma inside opaque exec path is not a separator",
			in:          "exec:echo a,b",
			wantSchemes: []string{"exec"},
		},
		{
			name:        "percent-encoded comma forces a literal comma",
			in:          "exec:echo foo%2Cbar",
			wantSchemes: []string{"exec"},
		},
		{
			name:        "three refs",
			in:          "env:A,env:B,aws-sm://c",
			wantSchemes: []string{"env", "env", "aws-sm"},
		},
		{
			name:    "empty",
			in:      "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := ParseRefs(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseRefs(%q) = %+v, want error", tt.in, refs)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRefs(%q) unexpected error: %v", tt.in, err)
			}
			if len(refs) != len(tt.wantSchemes) {
				t.Fatalf("ParseRefs(%q) = %d refs %+v, want %d", tt.in, len(refs), refs, len(tt.wantSchemes))
			}
			for i, scheme := range tt.wantSchemes {
				if refs[i].Scheme != scheme {
					t.Errorf("refs[%d].Scheme = %q, want %q", i, refs[i].Scheme, scheme)
				}
			}
		})
	}

	t.Run("query comma preserved in tags opt", func(t *testing.T) {
		refs, err := ParseRefs("vault://kv?tags=a,b")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(refs) != 1 {
			t.Fatalf("got %d refs, want 1", len(refs))
		}
		if got := refs[0].Opt("tags"); got != "a,b" {
			t.Errorf("Opt(tags) = %q, want %q", got, "a,b")
		}
	})

	t.Run("exec path keeps whole comma-bearing argument as one ref", func(t *testing.T) {
		refs, err := ParseRefs("exec:echo a,b")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(refs) != 1 {
			t.Fatalf("got %d refs, want 1", len(refs))
		}
		if refs[0].Path != "echo a,b" {
			t.Errorf("Path = %q, want %q", refs[0].Path, "echo a,b")
		}
	})

	t.Run("percent-encoded comma is NOT decoded by ParseRef", func(t *testing.T) {
		// ParseRef does not percent-decode Path, so %2C is preserved verbatim
		// rather than becoming a literal ",". It still achieves its purpose here:
		// the %2C sequence contains no bare comma, so splitChain has nothing to
		// split on and the whole exec path stays one ref.
		refs, err := ParseRefs("exec:echo foo%2Cbar")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(refs) != 1 {
			t.Fatalf("got %d refs, want 1", len(refs))
		}
		if refs[0].Path != "echo foo%2Cbar" {
			t.Errorf("Path = %q, want %q (ParseRef does not percent-decode)", refs[0].Path, "echo foo%2Cbar")
		}
	})

	// The following pin the doubled/trailing comma behavior described in
	// splitChain's doc comment as DELIBERATE, not a bug: a comma is only a
	// split point when a scheme-like token immediately follows it, so a
	// doubled or trailing comma has nothing after it that looks like a scheme
	// and is kept as part of the adjacent ref's value instead. That ref then
	// simply fails to resolve at lookup time and the chain falls through.

	t.Run("doubled comma leaves stray comma on the preceding ref, DELIBERATE", func(t *testing.T) {
		refs, err := ParseRefs("env:A,,env:B")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(refs) != 2 {
			t.Fatalf("got %d refs %+v, want 2", len(refs), refs)
		}
		if refs[0].Scheme != "env" || refs[0].Path != "A," {
			t.Errorf("refs[0] = scheme %q path %q, want scheme %q path %q", refs[0].Scheme, refs[0].Path, "env", "A,")
		}
		if refs[1].Scheme != "env" || refs[1].Path != "B" {
			t.Errorf("refs[1] = scheme %q path %q, want scheme %q path %q", refs[1].Scheme, refs[1].Path, "env", "B")
		}
	})

	t.Run("trailing comma is kept as part of the ref's path, DELIBERATE", func(t *testing.T) {
		refs, err := ParseRefs("env:A,")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(refs) != 1 {
			t.Fatalf("got %d refs %+v, want 1", len(refs), refs)
		}
		if refs[0].Scheme != "env" || refs[0].Path != "A," {
			t.Errorf("refs[0] = scheme %q path %q, want scheme %q path %q", refs[0].Scheme, refs[0].Path, "env", "A,")
		}
	})

	t.Run("genuine trailing comma in a value is preserved, not trimmed", func(t *testing.T) {
		// exec:echo a, is a single valid ref whose path legitimately ends in a
		// comma. This is exactly why splitChain must not special-case or trim
		// trailing commas: doing so would corrupt this value.
		refs, err := ParseRefs("exec:echo a,")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(refs) != 1 {
			t.Fatalf("got %d refs %+v, want 1", len(refs), refs)
		}
		if refs[0].Path != "echo a," {
			t.Errorf("Path = %q, want %q", refs[0].Path, "echo a,")
		}
	})
}
