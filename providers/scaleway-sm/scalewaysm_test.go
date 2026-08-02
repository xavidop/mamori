package scalewaysm

import (
	"net/http"
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

// TestParseRef pins the split between a secret's path and its name: the LAST
// path segment is the name, everything before it is the path. The
// default-revision cases ("root path", "explicit path", "deep path", and
// "fragment is not part of the name") are the load-bearing ones: several of
// them assert "latest_enabled" explicitly so that a change which silently
// swaps the default for "latest" fails more than one case, not just the one
// that happens to check it.
func TestParseRef(t *testing.T) {
	tests := []struct {
		name         string
		tag          string
		wantPath     string
		wantName     string
		wantRevision string
		wantErr      string
	}{
		{name: "root path", tag: "scaleway-sm://db-password", wantPath: "/", wantName: "db-password", wantRevision: "latest_enabled"},
		{name: "explicit path", tag: "scaleway-sm://prod/db-password", wantPath: "/prod", wantName: "db-password", wantRevision: "latest_enabled"},
		{name: "deep path", tag: "scaleway-sm://a/b/c/secret", wantPath: "/a/b/c", wantName: "secret", wantRevision: "latest_enabled"},
		{name: "explicit revision number", tag: "scaleway-sm://db?revision=7", wantPath: "/", wantName: "db", wantRevision: "7"},
		{name: "explicit revision latest", tag: "scaleway-sm://db?revision=latest", wantPath: "/", wantName: "db", wantRevision: "latest"},
		{name: "fragment is not part of the name", tag: "scaleway-sm://db#user", wantPath: "/", wantName: "db", wantRevision: "latest_enabled"},
		{name: "empty ref has no name", tag: "scaleway-sm://", wantErr: "requires a secret name"},
		{name: "trailing slash leaves no name", tag: "scaleway-sm://prod/", wantErr: "requires a secret name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, name, revision, err := parseRef(ref(t, tc.tag))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got err %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if path != tc.wantPath || name != tc.wantName || revision != tc.wantRevision {
				t.Fatalf("got (path=%q, name=%q, revision=%q), want (path=%q, name=%q, revision=%q)",
					path, name, revision, tc.wantPath, tc.wantName, tc.wantRevision)
			}
		})
	}
}

func TestSettingsPrecedence(t *testing.T) {
	t.Setenv("SCW_SECRET_KEY", "env-key")
	t.Setenv("SCW_DEFAULT_PROJECT_ID", "env-project")
	t.Setenv("SCW_DEFAULT_REGION", "env-region")

	got, err := New().settingsFor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.secretKey != "env-key" || got.projectID != "env-project" || got.region != "env-region" {
		t.Fatalf("environment not read: %+v", got)
	}

	p := New(WithSecretKey("opt-key"), WithProjectID("opt-project"), WithRegion("opt-region"))
	got, err = p.settingsFor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.secretKey != "opt-key" || got.projectID != "opt-project" || got.region != "opt-region" {
		t.Fatalf("options must win over the environment: %+v", got)
	}
}

// TestSettingsRegionDefaultsToFrPar pins the one setting with a fallback:
// unlike the secret key and project id, a missing region is not an error.
func TestSettingsRegionDefaultsToFrPar(t *testing.T) {
	t.Setenv("SCW_SECRET_KEY", "k")
	t.Setenv("SCW_DEFAULT_PROJECT_ID", "p")
	t.Setenv("SCW_DEFAULT_REGION", "")

	got, err := New().settingsFor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.region != "fr-par" {
		t.Fatalf("got region %q, want the default %q", got.region, "fr-par")
	}
}

// TestSettingsMissing pins one half of each missing-setting error: that it
// names the environment variable a caller must set. Its table and setup are
// identical to TestSettingsMissingErrorsNameTheOption below, which pins the
// other half (the WithXxx option name), and that duplication is deliberate,
// not an oversight to tidy later: merging the two into one test would couple
// two independent regressions, a dropped env var name and a dropped option
// name, into a single case, so a future break in either one would no longer
// fail on its own.
func TestSettingsMissing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "no secret key", env: map[string]string{"SCW_DEFAULT_PROJECT_ID": "p"}, wantErr: "SCW_SECRET_KEY"},
		{name: "no project id", env: map[string]string{"SCW_SECRET_KEY": "k"}, wantErr: "SCW_DEFAULT_PROJECT_ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCW_SECRET_KEY", "")
			t.Setenv("SCW_DEFAULT_PROJECT_ID", "")
			t.Setenv("SCW_DEFAULT_REGION", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := New().settingsFor()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got err %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

// TestSettingsMissingErrorsNameTheOption pins the other half of each missing-
// setting error: it must name the WithXxx option, not just the environment
// variable, since an error that only ever mentions the env var would leave a
// caller who never touches the environment with no idea how to fix it.
func TestSettingsMissingErrorsNameTheOption(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "no secret key", env: map[string]string{"SCW_DEFAULT_PROJECT_ID": "p"}, wantErr: "WithSecretKey"},
		{name: "no project id", env: map[string]string{"SCW_SECRET_KEY": "k"}, wantErr: "WithProjectID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCW_SECRET_KEY", "")
			t.Setenv("SCW_DEFAULT_PROJECT_ID", "")
			t.Setenv("SCW_DEFAULT_REGION", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := New().settingsFor()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got err %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

// A credential must never reach an error message, even when it is the value
// sitting in the environment at the moment an unrelated setting's error is
// produced. This pins the regression class providers/vercel-gc actually
// shipped and had to fix: parseConnectionString ran the connection string,
// token included, through url.Parse, which renders the whole input into a
// *url.Error's message, and the fix moved the token out of the URL entirely
// and into the Authorization header. providers/cloudflare-kv did not ship
// this: its sanitizeTransportError guard was present from the commit that
// introduced its resolve.go; what it shipped and fixed in review was a
// narrower test gap, only 1 of its 4 sanitize call sites was pinned by a
// test.
func TestSettingsErrorsNeverCarrySecretKey(t *testing.T) {
	t.Setenv("SCW_SECRET_KEY", "SUPER_SECRET_KEY")
	t.Setenv("SCW_DEFAULT_PROJECT_ID", "")

	_, err := New().settingsFor()
	if err == nil {
		t.Fatal("want an error when the project id is missing")
	}
	if strings.Contains(err.Error(), "SUPER_SECRET_KEY") {
		t.Fatalf("error leaked the secret key: %v", err)
	}
}

// The project id is not conventionally "secret", but the brief treats it as
// credential-adjacent and forbids it from an error message all the same, so
// this pins the symmetric case to TestSettingsErrorsNeverCarrySecretKey.
func TestSettingsErrorsNeverCarryProjectID(t *testing.T) {
	t.Setenv("SCW_SECRET_KEY", "")
	t.Setenv("SCW_DEFAULT_PROJECT_ID", "SUPER_SECRET_PROJECT")

	_, err := New().settingsFor()
	if err == nil {
		t.Fatal("want an error when the secret key is missing")
	}
	if strings.Contains(err.Error(), "SUPER_SECRET_PROJECT") {
		t.Fatalf("error leaked the project id: %v", err)
	}
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != "scaleway-sm" {
		t.Fatalf("got scheme %q, want %q", got, "scaleway-sm")
	}
}

// A trailing slash must be trimmed here, not left for Task 2's request
// building to trip over: baseURL + "/regions/..." must never produce a
// double slash.
func TestWithBaseURLTrimsTrailingSlash(t *testing.T) {
	p := New(WithBaseURL("https://example.com/sm/"))
	if p.baseURL != "https://example.com/sm" {
		t.Fatalf("got baseURL %q, want the trailing slash trimmed", p.baseURL)
	}
}

// An empty WithBaseURL must not clobber the default: New() already applies
// defaultBaseURL, and an Option that runs after it should be a no-op when
// given "".
func TestWithBaseURLEmptyIsNoOp(t *testing.T) {
	p := New(WithBaseURL(""))
	if p.baseURL != defaultBaseURL {
		t.Fatalf("got baseURL %q, want the default %q unchanged", p.baseURL, defaultBaseURL)
	}
}

func TestWithHTTPClientOverridesDefault(t *testing.T) {
	custom := &http.Client{}
	p := New(WithHTTPClient(custom))
	if p.httpClient != custom {
		t.Fatal("WithHTTPClient did not install the custom client")
	}
}

// A nil client must be a no-op, not a way to null out the default and crash
// every future request.
func TestWithHTTPClientNilIsNoOp(t *testing.T) {
	p := New(WithHTTPClient(nil))
	if p.httpClient == nil {
		t.Fatal("WithHTTPClient(nil) must not clear the default client")
	}
}
