package hcpvaultsecrets

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

func mustRef(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

// clearEnv unsets every HCP_* variable for the duration of a test, so a
// developer's own shell cannot decide what these tests exercise. t.Setenv also
// makes the test refuse to run in parallel, which is what keeps that guarantee.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{envClientID, envClientSecret, envOrganization, envProject, envApp} {
		t.Setenv(k, "")
	}
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != scheme {
		t.Fatalf("Scheme() = %q, want %q", got, scheme)
	}
}

// TestSchemeIsNotVault pins the distinction this whole module exists to keep.
// providers/vault owns "vault" for self-hosted HashiCorp Vault, a different
// product with a different API; a collision here would make one of the two
// unregisterable, since mamori.Register panics on a duplicate scheme.
func TestSchemeIsNotVault(t *testing.T) {
	if got := New().Scheme(); got == "vault" {
		t.Fatal("Scheme() collides with providers/vault, which serves self-hosted Vault")
	}
}

// TestProviderIsNotWatchable pins the deliberate absence of a native watch, so
// removing that decision cannot happen silently. The OpenAppSecret endpoint
// exposes no streaming or blocking read, so mamori must poll.
func TestProviderIsNotWatchable(t *testing.T) {
	if _, ok := any(New()).(mamori.WatchableProvider); ok {
		t.Fatal("Provider implements WatchableProvider; the HCP read API has no push channel, so mamori must poll it")
	}
}

// TestResolveBuildsTheDocumentedPath is the single most important test in this
// file: it pins the exact OpenAppSecret path template against the one HCP
// documents, including the ":open" custom-method suffix and the dated API
// version segment.
//
// Nobody here has HCP credentials, so this path came from the vendor's API
// reference rather than from a live call. If it is wrong, every resolve 404s
// and no other test in this package would say why.
func TestResolveBuildsTheDocumentedPath(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("DB_PASSWORD"), "hunter2")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://DB_PASSWORD"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hunter2" {
		t.Fatalf("Bytes = %q, want %q", v.Bytes, "hunter2")
	}

	want := "/secrets/2023-11-28/organizations/" + testOrganization +
		"/projects/" + testProject + "/apps/" + testApp + "/secrets/DB_PASSWORD:open"
	got, auth := f.lastRead()
	if got != want {
		t.Fatalf("request path = %q, want %q", got, want)
	}
	if auth != "Bearer "+testAccessToken {
		t.Fatalf("Authorization = %q, want the bearer token from the exchange", auth)
	}
}

// TestTokenExchangeMatchesTheGrantHCPDocuments pins the other half of the wire
// contract: HCP's token endpoint takes a form-encoded client-credentials grant
// WITH an audience parameter, and a token issued without that audience is
// refused by the control plane.
//
// The audience is the reason this provider can reuse
// httpcore.OAuth2ClientCredentials at all rather than writing its own
// authenticator, so a regression that dropped it would quietly undo that
// decision.
func TestTokenExchangeMatchesTheGrantHCPDocuments(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")

	if _, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	form := f.lastToken()
	for _, tc := range []struct{ field, want string }{
		{"grant_type", "client_credentials"},
		{"client_id", testClientID},
		{"client_secret", testClientSecret},
		{"audience", "https://api.hashicorp.cloud"},
	} {
		if got := form.Get(tc.field); got != tc.want {
			t.Errorf("token form %s = %q, want %q", tc.field, got, tc.want)
		}
	}
}

// TestAccessTokenIsCachedAcrossResolves pins that the token exchange happens
// once rather than on every poll. mamori polls this provider, so a token bought
// per resolve would double every request and would rate-limit the identity
// provider on a busy configuration.
func TestAccessTokenIsCachedAcrossResolves(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")
	p := f.provider()

	for range 3 {
		if _, err := p.Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN")); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}

	tokens, reads := f.counts()
	if tokens != 1 {
		t.Errorf("token exchanges = %d, want 1; the access token is not being cached", tokens)
	}
	if reads != 3 {
		t.Errorf("reads = %d, want 3; the secret value must not be cached", reads)
	}
}

// TestSecretNameWithSlashStaysOneSegment pins the reason the ENTIRE ref path is
// the secret name. url.PathEscape keeps a slash inside the name as %2F, one
// segment, so a name containing one addresses the secret it names rather than a
// deeper, non-existent resource.
func TestSecretNameWithSlashStaysOneSegment(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("a/b"), "sliced")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://a%2Fb"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "sliced" {
		t.Fatalf("Bytes = %q, want %q", v.Bytes, "sliced")
	}
	got, _ := f.lastRead()
	if !strings.HasSuffix(got, "/secrets/a%2Fb:open") {
		t.Fatalf("request path = %q, want the name escaped as one segment", got)
	}
}

// TestRefOptionsOverrideProviderOptions pins the precedence rule shared with
// providers/cloudflare-kv and providers/infisical: the ref option wins, so one
// provider can serve refs pointing at different applications.
func TestRefOptionsOverrideProviderOptions(t *testing.T) {
	clearEnv(t)
	f := newFake()
	other := secretRef{organization: "org-from-ref", project: "proj-from-ref", app: "app-from-ref", name: "TOKEN"}
	f.set(other, "from-the-ref")

	v, err := f.provider().Resolve(context.Background(),
		mustRef(t, "hcp-vs://TOKEN?org=org-from-ref&project=proj-from-ref&app=app-from-ref"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "from-the-ref" {
		t.Fatalf("Bytes = %q, want the value scoped by the ref options", v.Bytes)
	}
}

// TestEnvironmentSuppliesScopeWhenNoOptionDoes pins the bottom of the
// precedence chain, and that it is read at RESOLVE time rather than in New: a
// process that learns its configuration after start must still work.
func TestEnvironmentSuppliesScopeWhenNoOptionDoes(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "from-the-env")

	t.Setenv(envOrganization, testOrganization)
	t.Setenv(envProject, testProject)
	t.Setenv(envApp, testApp)

	p := New(
		WithClientID(testClientID),
		WithClientSecret(testClientSecret),
		WithBaseURL(testBaseURL),
		WithTokenURL(testTokenURL),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
	)
	v, err := p.Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "from-the-env" {
		t.Fatalf("Bytes = %q, want the value the environment scoped", v.Bytes)
	}
}

// TestMissingScopeIsInvalidNotNotFound pins that every missing piece of scope
// is reported as a misconfiguration.
//
// ErrNotFound is the one kind that makes mamori apply a field's default, so a
// missing organization reported as "not found" would turn a deployment mistake
// into a silently defaulted secret.
func TestMissingScopeIsInvalidNotNotFound(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want string
	}{
		{"no organization", []Option{WithOrganizationID("")}, envOrganization},
		{"no project", []Option{WithProjectID("")}, envProject},
		{"no application", []Option{WithAppName("")}, envApp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			f := newFake()

			// Build a provider with every scope EXCEPT the one under test.
			opts := []Option{
				WithClientID(testClientID),
				WithClientSecret(testClientSecret),
				WithBaseURL(testBaseURL),
				WithTokenURL(testTokenURL),
				WithHTTPClient(&http.Client{Transport: f.transport()}),
			}
			switch tt.want {
			case envOrganization:
				opts = append(opts, WithProjectID(testProject), WithAppName(testApp))
			case envProject:
				opts = append(opts, WithOrganizationID(testOrganization), WithAppName(testApp))
			case envApp:
				opts = append(opts, WithOrganizationID(testOrganization), WithProjectID(testProject))
			}

			_, err := New(opts...).Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if errors.Is(err, mamori.ErrNotFound) {
				t.Fatal("a missing scope reported ErrNotFound; mamori would silently apply the field's default")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to name %s", err, tt.want)
			}
		})
	}
}

// TestMissingCredentialsAreInvalidAndNameTheirSource pins that a provider with
// no key pair fails legibly, naming both the option and the environment
// variable that would supply it, and echoing neither.
func TestMissingCredentialsAreInvalidAndNameTheirSource(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want string
	}{
		{"no client id", []Option{WithClientSecret(testClientSecret)}, envClientID},
		{"no client secret", []Option{WithClientID(testClientID)}, envClientSecret},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			opts := append([]Option{
				WithOrganizationID(testOrganization),
				WithProjectID(testProject),
				WithAppName(testApp),
			}, tt.opts...)

			_, err := New(opts...).Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to name %s", err, tt.want)
			}
			if strings.Contains(err.Error(), testClientSecret) {
				t.Errorf("err = %q leaked the client secret", err)
			}
		})
	}
}

// TestEmptyOptionsFallThroughToTheEnvironment pins that an empty option is
// IGNORED rather than stored. Storing it would pin an unusable empty credential
// and defeat the environment fallback, which is how a container that injects
// credentials at start is expected to work.
func TestEmptyOptionsFallThroughToTheEnvironment(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")

	t.Setenv(envClientID, testClientID)
	t.Setenv(envClientSecret, testClientSecret)

	p := New(
		WithClientID(""),
		WithClientSecret(""),
		WithOrganizationID(testOrganization),
		WithProjectID(testProject),
		WithAppName(testApp),
		WithBaseURL(testBaseURL),
		WithTokenURL(testTokenURL),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
	)
	if _, err := p.Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

// TestRefWithoutASecretNameIsInvalid pins that an empty path is a
// misconfiguration rather than a lookup of the empty name.
func TestRefWithoutASecretNameIsInvalid(t *testing.T) {
	clearEnv(t)
	f := newFake()

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// TestURLSchemesAreCheckedAgainstAClosedSet pins that a URL this package will
// not talk to is refused at the first resolve, naming which of the two it was.
//
// httpcore.New requires only a scheme and a host, so an ftp:// typo constructs
// cleanly and then fails on every resolve with net/http's "unsupported protocol
// scheme". A cleartext URL is refused for a stronger reason: the grant POSTs
// the client secret.
func TestURLSchemesAreCheckedAgainstAClosedSet(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want string
	}{
		{"http base URL", []Option{WithBaseURL("http://hcp-api.test")}, "base URL"},
		{"ftp base URL", []Option{WithBaseURL("ftp://hcp-api.test")}, "base URL"},
		{"http token URL", []Option{WithTokenURL("http://hcp-auth.test/oauth2/token")}, "TokenURL"},
		{"ftp token URL", []Option{WithTokenURL("ftp://hcp-auth.test/oauth2/token")}, "TokenURL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			f := newFake()

			_, err := f.provider(tt.opts...).Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to name the %s", err, tt.want)
			}
		})
	}
}

// TestAllowInsecurePermitsHTTPAndNothingElse pins that the opt-in relaxes
// exactly one scheme. An operator who accepts cleartext for a local proxy must
// not thereby accept an ftp:// typo.
func TestAllowInsecurePermitsHTTPAndNothingElse(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")

	p := f.provider(
		WithBaseURL("http://hcp-api.test"),
		WithTokenURL("http://hcp-auth.test/oauth2/token"),
		WithAllowInsecure(),
	)
	if _, err := p.Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN")); err != nil {
		t.Fatalf("Resolve over http:// with WithAllowInsecure: %v", err)
	}

	_, err := f.provider(WithBaseURL("ftp://hcp-api.test"), WithAllowInsecure()).
		Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("WithAllowInsecure accepted an ftp:// base URL: %v", err)
	}
}

// TestVersionComesFromTheBackendRevision pins that Value.Version is the
// backend's own static_version.version, which is what lets mamori's poller tell
// a rotation from a no-op without comparing secret bytes.
func TestVersionComesFromTheBackendRevision(t *testing.T) {
	clearEnv(t)
	f := newFake()
	r := conformanceRef("TOKEN")
	f.set(r, "first")

	p := f.provider()
	v1, err := p.Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v1.Version != "1" {
		t.Fatalf("Version = %q, want %q", v1.Version, "1")
	}

	f.set(r, "second")
	v2, err := p.Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v2.Version != "2" {
		t.Fatalf("Version = %q, want %q after a rewrite", v2.Version, "2")
	}
}

// TestVersionFallsBackToAContentHash pins the fallback for a backend that
// reported no revision. Rendering an absent version as "0" would pin Version to
// a constant and make change detection impossible for every ref at once, the
// failure mode a poller cannot report because nothing ever looks changed.
func TestVersionFallsBackToAContentHash(t *testing.T) {
	clearEnv(t)
	f := newFake()
	r := conformanceRef("TOKEN")
	f.setUnversioned(r, "first")

	p := f.provider()
	v1, err := p.Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v1.Version == "" || v1.Version == "0" {
		t.Fatalf("Version = %q, want a content hash", v1.Version)
	}

	f.setUnversioned(r, "second")
	v2, err := p.Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v1.Version == v2.Version {
		t.Fatal("Version did not change when the value did; change detection would never fire")
	}
}

// TestVersionDescribesTheWholeSecretNotTheFragment pins that two refs selecting
// different #fields of one JSON secret agree on when it changed. A version
// derived from the selected fragment would make them disagree.
func TestVersionDescribesTheWholeSecretNotTheFragment(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.setUnversioned(conformanceRef("CREDS"), `{"user":"alice","pass":"s3cret"}`)

	p := f.provider()
	user, err := p.Resolve(context.Background(), mustRef(t, "hcp-vs://CREDS#user"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	pass, err := p.Resolve(context.Background(), mustRef(t, "hcp-vs://CREDS#pass"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(user.Bytes) != "alice" || string(pass.Bytes) != "s3cret" {
		t.Fatalf("selection returned %q and %q", user.Bytes, pass.Bytes)
	}
	if user.Version != pass.Version {
		t.Fatalf("two #fields of one secret disagree on version: %q vs %q", user.Version, pass.Version)
	}
}

// TestResolvedValueIsAlwaysSensitive pins the rule for a secret manager: there
// is no configuration-only mode of HCP Vault Secrets, so there is no per-ref or
// per-provider switch for this.
func TestResolvedValueIsAlwaysSensitive(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://TOKEN"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Fatal("Sensitive = false; every value from a secret manager must be marked sensitive")
	}
}

// TestEmptySecretValueIsNotNotFound pins the distinction the pointers in
// secretEnvelope exist for: a secret whose value is legitimately the empty
// string resolves, rather than being mistaken for an absent one.
func TestEmptySecretValueIsNotNotFound(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("EMPTY"), "")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://EMPTY"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(v.Bytes) != 0 {
		t.Fatalf("Bytes = %q, want empty", v.Bytes)
	}
	if v.Version == "" {
		t.Error("Version is empty; an empty value still has a revision")
	}
}

// TestAbsentSecretIsNotFound pins the live 404 path through the fake, which is
// what makes a field's default: and optional handling apply.
func TestAbsentSecretIsNotFound(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("PRESENT"), "value")
	f.del(conformanceRef("PRESENT"))

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "hcp-vs://PRESENT"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestDotSegmentsAreRejected pins that httpcore's traversal check is reached
// through this provider's path building. A ref's path can come from ${VAR}
// substitution at run time, so this is reachable rather than theoretical: it
// would otherwise address another organization's secret without leaving the
// declared host.
func TestDotSegmentsAreRejected(t *testing.T) {
	clearEnv(t)
	f := newFake()

	for _, raw := range []string{"hcp-vs://..%2F..%2Fother", "hcp-vs://a/../../b"} {
		ref, err := mamori.ParseRef(raw)
		if err != nil {
			continue // a ref the parser itself refuses is equally safe
		}
		if _, err := f.provider().Resolve(context.Background(), ref); !errors.Is(err, mamori.ErrInvalid) {
			t.Errorf("Resolve(%q) err = %v, want ErrInvalid", raw, err)
		}
	}
}
