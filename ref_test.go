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
