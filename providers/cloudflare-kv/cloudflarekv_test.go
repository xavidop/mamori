package cloudflarekv

import (
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

func ref(t *testing.T, tag string) mamori.Ref {
	t.Helper()
	r, err := mamori.ParseRef(tag)
	if err != nil {
		t.Fatalf("parsing ref %q: %v", tag, err)
	}
	return r
}

func TestKeyOf(t *testing.T) {
	tests := []struct {
		name    string
		tag     string
		want    string
		wantErr string
	}{
		{name: "simple key", tag: "cloudflare-kv://log-level", want: "log-level"},
		{name: "key containing slashes is one key", tag: "cloudflare-kv://config/prod/log-level", want: "config/prod/log-level"},
		{name: "namespace option is not part of the key", tag: "cloudflare-kv://log-level?namespace=ns123", want: "log-level"},
		{name: "fragment is not part of the key", tag: "cloudflare-kv://api-config#timeout", want: "api-config"},
		{name: "empty key", tag: "cloudflare-kv://", wantErr: "requires a key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := keyOf(ref(t, tc.tag))
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
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSettingsPrecedence(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "env-token")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "env-account")
	t.Setenv("CLOUDFLARE_KV_NAMESPACE_ID", "env-namespace")

	got, err := New().settingsFor(ref(t, "cloudflare-kv://k"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.token != "env-token" || got.account != "env-account" || got.namespace != "env-namespace" {
		t.Fatalf("environment not read: %+v", got)
	}

	p := New(WithAPIToken("opt-token"), WithAccountID("opt-account"), WithNamespaceID("opt-namespace"))
	got, err = p.settingsFor(ref(t, "cloudflare-kv://k"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.token != "opt-token" || got.account != "opt-account" || got.namespace != "opt-namespace" {
		t.Fatalf("options must win over the environment: %+v", got)
	}
}

func TestSettingsRefNamespaceWins(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "t")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "a")
	t.Setenv("CLOUDFLARE_KV_NAMESPACE_ID", "default-ns")

	got, err := New().settingsFor(ref(t, "cloudflare-kv://k?namespace=ref-ns"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.namespace != "ref-ns" {
		t.Fatalf("got namespace %q, want the ref's %q", got.namespace, "ref-ns")
	}
}

// TestSettingsNamespaceAllThreeSourcesRefWins sets the environment, an
// explicit WithNamespaceID, and the ref's ?namespace= to three distinguishable
// values at once. TestSettingsRefNamespaceWins alone only pits the ref
// against the environment, and TestSettingsPrecedence only pits the explicit
// option against the environment, so neither catches a reordering of
// firstNonEmpty's arguments in settingsFor that puts the explicit option
// ahead of the ref's option. This test does.
func TestSettingsNamespaceAllThreeSourcesRefWins(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "t")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "a")
	t.Setenv("CLOUDFLARE_KV_NAMESPACE_ID", "env-ns")

	p := New(WithNamespaceID("opt-ns"))
	got, err := p.settingsFor(ref(t, "cloudflare-kv://k?namespace=ref-ns"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.namespace != "ref-ns" {
		t.Fatalf("got namespace %q, want the ref's %q to win over both WithNamespaceID and the environment", got.namespace, "ref-ns")
	}
}

func TestSettingsMissing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "no token", env: map[string]string{"CLOUDFLARE_ACCOUNT_ID": "a", "CLOUDFLARE_KV_NAMESPACE_ID": "n"}, wantErr: "CLOUDFLARE_API_TOKEN"},
		{name: "no account", env: map[string]string{"CLOUDFLARE_API_TOKEN": "t", "CLOUDFLARE_KV_NAMESPACE_ID": "n"}, wantErr: "CLOUDFLARE_ACCOUNT_ID"},
		{name: "no namespace", env: map[string]string{"CLOUDFLARE_API_TOKEN": "t", "CLOUDFLARE_ACCOUNT_ID": "a"}, wantErr: "CLOUDFLARE_KV_NAMESPACE_ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLOUDFLARE_API_TOKEN", "")
			t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
			t.Setenv("CLOUDFLARE_KV_NAMESPACE_ID", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := New().settingsFor(ref(t, "cloudflare-kv://k"))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got err %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

// A credential must never reach an error message. This pins the regression that
// providers/vercel-gc shipped and had to fix.
func TestSettingsErrorsNeverCarryCredentials(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "SUPER_SECRET_TOKEN")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	t.Setenv("CLOUDFLARE_KV_NAMESPACE_ID", "")

	_, err := New().settingsFor(ref(t, "cloudflare-kv://k"))
	if err == nil {
		t.Fatal("want an error when the account id is missing")
	}
	if strings.Contains(err.Error(), "SUPER_SECRET_TOKEN") {
		t.Fatalf("error leaked the API token: %v", err)
	}
}

// A trailing slash must be trimmed here, not left for Task 2's request
// building to trip over: baseURL + "/accounts/..." must never produce a
// double slash.
func TestWithBaseURLTrimsTrailingSlash(t *testing.T) {
	p := New(WithBaseURL("https://example.com/kv/"))
	if p.baseURL != "https://example.com/kv" {
		t.Fatalf("got baseURL %q, want the trailing slash trimmed", p.baseURL)
	}
}
