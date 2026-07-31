package vercelgc

import (
	"strings"
	"testing"
)

func TestParseConnectionString(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    connection
		wantErr string
	}{
		{
			name: "global config host",
			in:   "https://global-config.vercel.com/ecfg_abc123?token=tok_xyz",
			want: connection{host: "https://global-config.vercel.com", storeID: "ecfg_abc123", token: "tok_xyz"},
		},
		{
			name: "legacy edge config host is preserved",
			in:   "https://edge-config.vercel.com/ecfg_old?token=tok_old",
			want: connection{host: "https://edge-config.vercel.com", storeID: "ecfg_old", token: "tok_old"},
		},
		{name: "empty", in: "", wantErr: "empty"},
		{name: "no token", in: "https://global-config.vercel.com/ecfg_abc", wantErr: "token"},
		{name: "no store id", in: "https://global-config.vercel.com?token=t", wantErr: "store"},
		{name: "not a url", in: "://nope", wantErr: "parsing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseConnectionString(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got err %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A trailing newline is the realistic trigger for a malformed connection
// string - GLOBAL_CONFIG read from a file, or a kubectl create secret
// --from-file, commonly carries one - and url.Parse rejects it as an invalid
// control character. Its *url.Error.Error() text is `parse "<the whole raw
// URL>": <reason>`, and the raw URL is the connection string, token and all.
// parseConnectionString must reach past that for just the reason.
func TestParseConnectionStringNeverLeaksToken(t *testing.T) {
	const token = "SUPER_SECRET_SENTINEL"
	in := "https://global-config.vercel.com/ecfg_abc?token=" + token + "\n"

	_, err := parseConnectionString(in)
	if err == nil {
		t.Fatal("want an error for a connection string containing a control character")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked the token: %v", err)
	}
	if !strings.Contains(err.Error(), "parsing") {
		t.Fatalf("error must still name what went wrong, got: %v", err)
	}
}

func TestParsePath(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		defaultStore string
		wantStore    string
		wantKey      string
		wantErr      string
	}{
		{name: "one segment uses default store", path: "log-level", defaultStore: "ecfg_def", wantStore: "ecfg_def", wantKey: "log-level"},
		{name: "two segments name the store", path: "ecfg_abc/log-level", defaultStore: "ecfg_def", wantStore: "ecfg_abc", wantKey: "log-level"},
		{name: "leading slash tolerated", path: "/log-level", defaultStore: "ecfg_def", wantStore: "ecfg_def", wantKey: "log-level"},
		{name: "one segment with no default store", path: "log-level", defaultStore: "", wantErr: "no store"},
		{name: "empty path", path: "", defaultStore: "ecfg_def", wantErr: "requires a key"},
		{name: "three segments", path: "a/b/c", defaultStore: "ecfg_def", wantErr: "at most"},
		{name: "empty key in two-segment form", path: "ecfg_abc/", defaultStore: "", wantErr: "requires a key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, key, err := parsePath(tc.path, tc.defaultStore)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got err %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if store != tc.wantStore || key != tc.wantKey {
				t.Fatalf("got (%q, %q), want (%q, %q)", store, key, tc.wantStore, tc.wantKey)
			}
		})
	}
}

func TestConnectionPrecedence(t *testing.T) {
	t.Setenv("GLOBAL_CONFIG", "https://global-config.vercel.com/ecfg_global?token=t_global")
	t.Setenv("EDGE_CONFIG", "https://edge-config.vercel.com/ecfg_edge?token=t_edge")

	got, err := New().connection()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.storeID != "ecfg_global" {
		t.Fatalf("GLOBAL_CONFIG must win over EDGE_CONFIG, got store %q", got.storeID)
	}

	got, err = New(WithStoreID("ecfg_explicit"), WithToken("t_explicit")).connection()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.storeID != "ecfg_explicit" || got.token != "t_explicit" {
		t.Fatalf("explicit options must win over the environment, got %+v", got)
	}
	if got.host != defaultHost {
		t.Fatalf("explicit options must default the host to %q, got %q", defaultHost, got.host)
	}
}

// WithBaseURL redirects a connection-string-derived host rather than being
// ignored when one is present - the opposite of what its doc comment used to
// say. This is the option's only exported-symbol test: it had zero coverage
// before this fix.
func TestWithBaseURLRedirectsConnectionStringHost(t *testing.T) {
	p := New(
		WithConnectionString("https://global-config.vercel.com/ecfg_abc?token=tok"),
		WithBaseURL("https://proxy.example.com"),
	)

	got, err := p.connection()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.host != "https://proxy.example.com" {
		t.Fatalf("WithBaseURL must redirect the connection-string host, got %q", got.host)
	}
	if got.storeID != "ecfg_abc" || got.token != "tok" {
		t.Fatalf("store and token must still come from the connection string, got %+v", got)
	}
}

func TestConnectionEdgeConfigFallback(t *testing.T) {
	t.Setenv("GLOBAL_CONFIG", "")
	t.Setenv("EDGE_CONFIG", "https://edge-config.vercel.com/ecfg_edge?token=t_edge")

	got, err := New().connection()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.storeID != "ecfg_edge" || got.host != "https://edge-config.vercel.com" {
		t.Fatalf("EDGE_CONFIG fallback failed, got %+v", got)
	}
}

func TestConnectionMissing(t *testing.T) {
	t.Setenv("GLOBAL_CONFIG", "")
	t.Setenv("EDGE_CONFIG", "")

	_, err := New().connection()
	if err == nil {
		t.Fatal("want an error when no connection is configured")
	}
	for _, want := range []string{"GLOBAL_CONFIG", "EDGE_CONFIG", "WithConnectionString"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q, got: %v", want, err)
		}
	}
}
