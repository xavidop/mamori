package infisical

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// mustRef parses a ref or fails the test, so every call site below reads as one
// line rather than three.
func mustRef(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

// clearEnv unsets every INFISICAL_* variable for the duration of a test, so a
// developer's own shell cannot decide what these tests exercise. t.Setenv also
// makes the test refuse to run in parallel, which is what keeps that guarantee.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{envClientID, envClientSecret, envProjectID, envEnvironment, envSecretPath} {
		t.Setenv(k, "")
	}
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != scheme {
		t.Fatalf("Scheme() = %q, want %q", got, scheme)
	}
}

// TestProviderIsNotWatchable pins the deliberate absence of a native watch, so
// removing that decision cannot happen silently. Infisical's read API exposes
// no streaming or blocking read, so mamori must poll.
func TestProviderIsNotWatchable(t *testing.T) {
	if _, ok := any(New()).(mamori.WatchableProvider); ok {
		t.Fatal("Provider implements WatchableProvider; Infisical's read API has no push channel, so mamori must poll it")
	}
}

// TestResolveReturnsSecretValue pins the documented response shape: the value
// is nested under "secret", not returned as raw bytes.
func TestResolveReturnsSecretValue(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("DB_PASSWORD"), "s3cr3t-value")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://DB_PASSWORD"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "s3cr3t-value" {
		t.Fatalf("Bytes = %q, want s3cr3t-value", v.Bytes)
	}
}

// TestResolveMarksSensitive is its own test rather than an assertion tucked
// into the one above, so that dropping Sensitive fails a test whose NAME says
// what broke. Infisical is a secret manager: there is no configuration-only
// mode of it for a per-ref or per-provider switch to describe.
func TestResolveMarksSensitive(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("DB_PASSWORD"), "s3cr3t-value")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://DB_PASSWORD"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Fatal("Value.Sensitive is false; every value from a secret manager must be marked sensitive so mamori redacts it")
	}
}

// TestResolveUsesBackendVersion proves Version comes from the backend's own
// revision rather than a content hash, which is what makes change detection
// cheap and what distinguishes a rewrite of identical bytes from no write.
func TestResolveUsesBackendVersion(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "one")
	p := f.provider()

	first, err := p.Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if first.Version != "1" {
		t.Fatalf("Version = %q, want the backend revision %q", first.Version, "1")
	}

	f.set(conformanceRef("TOKEN"), "two")
	second, err := p.Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if second.Version != "2" {
		t.Fatalf("Version after rewrite = %q, want %q", second.Version, "2")
	}
}

// TestResolveHashesWhenBackendSendsNoVersion covers versionOf's fallback. A
// version of 0 rendered as "0" would pin Version to a constant and make change
// detection impossible for every ref at once, which is the one failure a poller
// cannot report.
func TestResolveHashesWhenBackendSendsNoVersion(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.setUnversioned(conformanceRef("TOKEN"), "one")
	p := f.provider()

	first, err := p.Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if first.Version == "" || first.Version == "0" {
		t.Fatalf("Version = %q, want a content hash when the backend sent no revision", first.Version)
	}

	f.setUnversioned(conformanceRef("TOKEN"), "two")
	second, err := p.Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if first.Version == second.Version {
		t.Fatalf("Version did not change after a rewrite (both %q); change detection is impossible", first.Version)
	}
}

// TestResolveVersionIgnoresKeySelection pins that two refs selecting different
// #fields of one secret agree on when that secret changed. The version
// describes the whole secret, so deriving it from the selected fragment would
// make two refs on one secret disagree about its revision.
func TestResolveVersionIgnoresKeySelection(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("DB"), `{"user":"app","password":"hunter2"}`)
	p := f.provider()

	user, err := p.Resolve(context.Background(), mustRef(t, "infisical://DB#user"))
	if err != nil {
		t.Fatalf("Resolve #user: %v", err)
	}
	password, err := p.Resolve(context.Background(), mustRef(t, "infisical://DB#password"))
	if err != nil {
		t.Fatalf("Resolve #password: %v", err)
	}
	if user.Version != password.Version {
		t.Fatalf("#user Version %q and #password Version %q disagree; the version describes the whole secret", user.Version, password.Version)
	}
}

func TestResolveSelectsTopLevelKey(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("DB"), `{"user":"app","password":"hunter2"}`)

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://DB#password"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hunter2" {
		t.Fatalf("Bytes = %q, want hunter2", v.Bytes)
	}
}

func TestResolveSelectsWithPointer(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("DB"), `{"creds":{"password":"hunter2"}}`)

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://DB#/creds/password"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hunter2" {
		t.Fatalf("Bytes = %q, want hunter2", v.Bytes)
	}
}

// TestResolveSendsProjectEnvironmentAndPath pins the request the provider
// actually builds. A response-only assertion would pass against a provider that
// sent no scope at all, since the fake would then simply miss and 404.
func TestResolveSendsProjectEnvironmentAndPath(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")

	if _, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://TOKEN")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	f.mu.Lock()
	q := f.lastReadQuery
	path := f.lastReadPath
	auth := f.lastReadAuth
	f.mu.Unlock()

	if got := q.Get("projectId"); got != testProject {
		t.Errorf("projectId = %q, want %q", got, testProject)
	}
	if got := q.Get("environment"); got != testEnvironment {
		t.Errorf("environment = %q, want %q", got, testEnvironment)
	}
	if got := q.Get("secretPath"); got != testSecretPath {
		t.Errorf("secretPath = %q, want %q", got, testSecretPath)
	}
	if want := secretsPath + "TOKEN"; path != want {
		t.Errorf("request path = %q, want %q", path, want)
	}
	if want := "Bearer " + testAccessToken; auth != want {
		t.Errorf("Authorization = %q, want %q", auth, want)
	}
}

// TestResolveOmitsEmptyEnvironment pins that an unconfigured environment is
// omitted from the query rather than sent empty. The API treats the parameter
// as optional, and "absent" is not the same request as "present and empty".
func TestResolveOmitsEmptyEnvironment(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(secretRef{project: testProject, path: testSecretPath, name: "TOKEN"}, "value")

	// Built from scratch rather than through f.provider, whose defaults set an
	// environment: WithEnvironment("") could not clear one already applied.
	p := New(
		WithClientID(testClientID),
		WithClientSecret(testClientSecret),
		WithProjectID(testProject),
		WithBaseURL("https://infisical.test"),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
	)
	if _, err := p.Resolve(context.Background(), mustRef(t, "infisical://TOKEN")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	f.mu.Lock()
	q := f.lastReadQuery
	f.mu.Unlock()

	if _, present := q["environment"]; present {
		t.Fatalf("environment was sent as %q; an unconfigured environment must be omitted, not sent empty", q.Get("environment"))
	}
}

// TestResolveDefaultsSecretPathToRoot pins the documented default. A ref that
// names no folder must address "/", which is what the API itself defaults to.
func TestResolveDefaultsSecretPathToRoot(t *testing.T) {
	clearEnv(t)
	f := newFake()
	p := New(
		WithClientID(testClientID),
		WithClientSecret(testClientSecret),
		WithProjectID(testProject),
		WithBaseURL("https://infisical.test"),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
	)
	f.set(secretRef{project: testProject, path: defaultSecretPath, name: "TOKEN"}, "value")

	if _, err := p.Resolve(context.Background(), mustRef(t, "infisical://TOKEN")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	f.mu.Lock()
	got := f.lastReadQuery.Get("secretPath")
	f.mu.Unlock()
	if got != defaultSecretPath {
		t.Fatalf("secretPath = %q, want %q", got, defaultSecretPath)
	}
}

// TestSettingsPrecedence pins the whole precedence chain in one place: a ref
// option beats a provider option, which beats an environment variable. This is
// the same rule providers/cloudflare-kv uses for ?namespace=, so an operator who
// knows one knows the other, and a silent reordering here would send every
// resolve to the wrong project without failing anything else.
func TestSettingsPrecedence(t *testing.T) {
	tests := []struct {
		name string
		// what each layer supplies, "" meaning "not supplied at that layer"
		refOpt, providerOpt, envVar string
		want                        string
	}{
		{name: "ref option wins over both", refOpt: "from-ref", providerOpt: "from-option", envVar: "from-env", want: "from-ref"},
		{name: "provider option wins over the environment", providerOpt: "from-option", envVar: "from-env", want: "from-option"},
		{name: "environment is the last resort", envVar: "from-env", want: "from-env"},
	}

	for _, layer := range []struct {
		what    string
		refName string
		env     string
		opt     func(string) Option
		query   string
	}{
		{what: "project", refName: "project", env: envProjectID, opt: WithProjectID, query: "projectId"},
		{what: "environment", refName: "env", env: envEnvironment, opt: WithEnvironment, query: "environment"},
		{what: "secret path", refName: "path", env: envSecretPath, opt: WithSecretPath, query: "secretPath"},
	} {
		for _, tt := range tests {
			t.Run(layer.what+"/"+tt.name, func(t *testing.T) {
				clearEnv(t)
				if tt.envVar != "" {
					t.Setenv(layer.env, tt.envVar)
				}
				f := newFake()

				opts := []Option{
					WithClientID(testClientID),
					WithClientSecret(testClientSecret),
					WithBaseURL("https://infisical.test"),
					WithHTTPClient(&http.Client{Transport: f.transport()}),
				}
				// Every layer under test needs the OTHER two settings supplied
				// from somewhere, and a project id is mandatory.
				if layer.what != "project" {
					opts = append(opts, WithProjectID(testProject))
				}
				if tt.providerOpt != "" {
					opts = append(opts, layer.opt(tt.providerOpt))
				}

				raw := "infisical://TOKEN"
				if tt.refOpt != "" {
					raw += "?" + layer.refName + "=" + tt.refOpt
				}
				// The secret is never seeded: this test reads the request the
				// provider built, and a 404 is the expected outcome.
				_, _ = New(opts...).Resolve(context.Background(), mustRef(t, raw))

				f.mu.Lock()
				got := f.lastReadQuery.Get(layer.query)
				f.mu.Unlock()
				if got != tt.want {
					t.Fatalf("%s = %q, want %q", layer.query, got, tt.want)
				}
			})
		}
	}
}

// TestCredentialPrecedence pins the same rule for the credentials, which have
// no ref layer: an explicit option wins over the environment variable.
func TestCredentialPrecedence(t *testing.T) {
	clearEnv(t)
	t.Setenv(envClientID, "env-client-id")
	t.Setenv(envClientSecret, "env-client-secret")

	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")

	// The fake only issues a token for testClientID/testClientSecret, so a
	// successful resolve is itself proof the option beat the environment.
	if _, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://TOKEN")); err != nil {
		t.Fatalf("Resolve with explicit credentials: %v", err)
	}
}

// TestCredentialsReadFromEnvironment pins the other half: with no option set,
// the environment supplies both halves of the machine identity, read lazily at
// resolve time so registering from init needs no credentials at process start.
func TestCredentialsReadFromEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv(envClientID, testClientID)
	t.Setenv(envClientSecret, testClientSecret)

	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")
	p := New(
		WithProjectID(testProject),
		WithEnvironment(testEnvironment),
		WithSecretPath(testSecretPath),
		WithBaseURL("https://infisical.test"),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
	)

	if _, err := p.Resolve(context.Background(), mustRef(t, "infisical://TOKEN")); err != nil {
		t.Fatalf("Resolve with environment credentials: %v", err)
	}
}

// TestResolveRequiresProjectID pins that a missing project reports ErrInvalid,
// NEVER ErrNotFound. ErrNotFound is the one kind that makes mamori apply a
// field's default, so classifying a misconfiguration as one would turn a typo
// into a silently defaulted value.
func TestResolveRequiresProjectID(t *testing.T) {
	clearEnv(t)
	f := newFake()
	p := New(
		WithClientID(testClientID),
		WithClientSecret(testClientSecret),
		WithBaseURL("https://infisical.test"),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
	)

	_, err := p.Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("a missing project id reported ErrNotFound; mamori would silently apply the field's default")
	}
}

// TestResolveRequiresCredentials covers both halves of the machine identity,
// and pins that neither message echoes a credential that IS set.
func TestResolveRequiresCredentials(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want string
	}{
		{name: "no client id", opts: []Option{WithClientSecret(testClientSecret)}, want: envClientID},
		{name: "no client secret", opts: []Option{WithClientID(testClientID)}, want: envClientSecret},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			f := newFake()
			opts := append([]Option{
				WithProjectID(testProject),
				WithBaseURL("https://infisical.test"),
				WithHTTPClient(&http.Client{Transport: f.transport()}),
			}, tt.opts...)

			_, err := New(opts...).Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to name %s so an operator knows what to set", err, tt.want)
			}
			if strings.Contains(err.Error(), testClientSecret) {
				t.Fatalf("the client secret leaked into %q", err)
			}
		})
	}
}

// TestResolveRejectsEmptySecretName pins that a ref with no path fails with
// ErrInvalid rather than requesting the collection endpoint, which would answer
// a list where a value was expected.
func TestResolveRejectsEmptySecretName(t *testing.T) {
	clearEnv(t)
	f := newFake()

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if logins, reads := f.counts(); logins != 0 || reads != 0 {
		t.Fatalf("an empty secret name reached the backend (%d logins, %d reads); it must be rejected before anything is sent", logins, reads)
	}
}

// TestResolveSecretNameWithSlashIsEscapedNotSplit pins that the whole ref path
// is one secret name. Without url.PathEscape the name would travel as extra
// path segments and address a different resource entirely, and the fake would
// answer a 404 that looks exactly like an absent secret.
func TestResolveSecretNameWithSlashIsEscapedNotSplit(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("app/prod/DB_PASSWORD"), "value")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://app/prod/DB_PASSWORD"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "value" {
		t.Fatalf("Bytes = %q, want value", v.Bytes)
	}

	f.mu.Lock()
	got := f.lastReadPath
	f.mu.Unlock()
	if want := secretsPath + "app%2Fprod%2FDB_PASSWORD"; got != want {
		t.Fatalf("request path = %q, want %q: the name must travel as ONE escaped segment", got, want)
	}
}

// TestResolveRejectsDotSegments pins that a traversing ref cannot escape the
// API prefix the base URL declares. The check itself lives in httpcore, so this
// asserts the provider does not defeat it by pre-decoding or re-joining the
// path, and that nothing is sent.
//
// The backslash case is not decoration: splitting on '/' alone would leave
// `a\..\..\secrets` as one segment matching neither "." nor "..", and IIS and
// ASP.NET decode the resulting %5C as a directory separator. A self-hosted
// Infisical can sit behind any reverse proxy, so this package cannot assume
// otherwise.
//
// A secret genuinely NAMED "../x" is therefore unaddressable through a ref,
// even though url.PathEscape would have kept it inside one segment. That is a
// deliberate cost of inheriting httpcore's blanket check, which runs on the
// decoded path so that no provider has to remember it.
func TestResolveRejectsDotSegments(t *testing.T) {
	clearEnv(t)
	for _, name := range []string{"../../admin/secrets", `a\..\..\secrets`} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			_, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://"+name))
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if _, reads := f.counts(); reads != 0 {
				t.Fatalf("a traversing name reached the backend (%d reads)", reads)
			}
		})
	}
}

// TestResolvePercentEncodedTraversalIsALiteralName pins the case that behaves
// differently here than in providers/https, and says why that is right rather
// than an oversight.
//
// providers/https hands ref.Path to httpcore unescaped, so a ref of "%2e%2e/x"
// decodes to "../x" and is refused. This provider escapes the name first,
// because the whole ref path is ONE secret name, so the same ref becomes the
// literal name "%2e%2e/secrets" and travels as "%252e%252e%2Fsecrets": a single
// segment that cannot reach outside the /api/v4/secrets/ prefix no matter how
// the backend decodes it once. Refusing it would make a legal Infisical secret
// name unaddressable for no security gain.
func TestResolvePercentEncodedTraversalIsALiteralName(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("%2e%2e/secrets"), "value")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://%2e%2e/secrets"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "value" {
		t.Fatalf("Bytes = %q, want value", v.Bytes)
	}

	f.mu.Lock()
	got := f.lastReadPath
	f.mu.Unlock()
	if want := secretsPath + "%252e%252e%2Fsecrets"; got != want {
		t.Fatalf("request path = %q, want %q: the percent sign must be escaped so the name stays one segment", got, want)
	}
}

// TestResolveRejectsNonHTTPBaseURL pins that the base URL scheme is checked
// against a closed set. httpcore.New accepts any scheme with a host, so an
// ftp:// typo or a ws:// paste would otherwise construct cleanly and then fail
// on every single resolve with net/http's "unsupported protocol scheme".
func TestResolveRejectsNonHTTPBaseURL(t *testing.T) {
	clearEnv(t)
	for _, base := range []string{"ftp://infisical.test", "ws://infisical.test", "file:///etc/secrets"} {
		t.Run(base, func(t *testing.T) {
			f := newFake()
			_, err := f.provider(WithBaseURL(base)).Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestResolveRejectsInsecureBaseURL pins that http:// needs an explicit opt-in.
// Universal Auth POSTs the client secret in the request body, so a cleartext
// base URL hands it to anything on the path.
func TestResolveRejectsInsecureBaseURL(t *testing.T) {
	clearEnv(t)
	f := newFake()

	_, err := f.provider(WithBaseURL("http://infisical.test")).Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// TestAllowInsecurePermitsHTTPAndNothingElse pins the scope of the opt-in: it
// permits cleartext http, and nothing else. Reading it as a general "skip the
// scheme check" switch would reopen exactly the hole the test above closes.
func TestAllowInsecurePermitsHTTPAndNothingElse(t *testing.T) {
	clearEnv(t)

	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")
	if _, err := f.provider(WithBaseURL("http://infisical.test"), WithAllowInsecure()).
		Resolve(context.Background(), mustRef(t, "infisical://TOKEN")); err != nil {
		t.Fatalf("Resolve over http:// with WithAllowInsecure: %v", err)
	}

	g := newFake()
	_, err := g.provider(WithBaseURL("ftp://infisical.test"), WithAllowInsecure()).
		Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid; WithAllowInsecure rescued a non-http scheme", err)
	}
}

// TestProviderNeverPrintsTheClientSecret is the reason Provider holds the
// secret in a closure rather than a string field. fmt walks unexported fields
// by reflection and cannot call a String method on what it reaches that way, so
// a plain field would print in cleartext from any debug dump or panic trace -
// and a Provider is exactly the value an application passes to
// mamori.WithProvider, so it is a plausible thing to log.
func TestProviderNeverPrintsTheClientSecret(t *testing.T) {
	clearEnv(t)
	p := New(WithClientID(testClientID), WithClientSecret(testClientSecret))

	// The pointer is printed rather than the dereferenced value, and that loses
	// nothing: %v, %+v and %#v all follow a struct pointer one level and walk
	// the fields behind it, which is exactly the reflection this guards
	// against. Printing *p would additionally copy a sync.Mutex, which `go vet`
	// refuses - so no caller can print the dereferenced form either without CI
	// telling them.
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		rendered := fmt.Sprintf(verb, p)
		if strings.Contains(rendered, testClientSecret) {
			t.Errorf("fmt.Sprintf(%q, provider) leaked the client secret: %s", verb, rendered)
		}
	}
}

// TestResolveReusesTheAccessToken pins that the token is cached rather than
// re-bought on every poll. mamori polls this provider, so a login per read
// would double every request and burn Infisical's identity rate limit for no
// gain.
func TestResolveReusesTheAccessToken(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")
	p := f.provider()

	for i := range 3 {
		if _, err := p.Resolve(context.Background(), mustRef(t, "infisical://TOKEN")); err != nil {
			t.Fatalf("Resolve %d: %v", i, err)
		}
	}

	logins, reads := f.counts()
	if logins != 1 {
		t.Fatalf("logins = %d, want 1: the access token must be cached across resolves", logins)
	}
	if reads != 3 {
		t.Fatalf("reads = %d, want 3: there is no value cache, only a token cache", reads)
	}
}

// TestResolveIsNotCached pins the other half of the line above: the VALUE is
// never held. mamori.Refresh and mamori.Doctor both call Resolve directly and
// must see the current value, and Infisical exposes no ETag or digest to gate a
// snapshot on.
func TestResolveIsNotCached(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "one")
	p := f.provider()

	if _, err := p.Resolve(context.Background(), mustRef(t, "infisical://TOKEN")); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	f.set(conformanceRef("TOKEN"), "two")
	v, err := p.Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if string(v.Bytes) != "two" {
		t.Fatalf("Bytes = %q, want two: the provider served a cached value", v.Bytes)
	}
}

// TestResolveAbsentSecretIsNotFound pins the 404 path end to end, so a field's
// default: and optional handling apply to a genuinely missing secret.
func TestResolveAbsentSecretIsNotFound(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("TOKEN"), "value")
	f.del(conformanceRef("TOKEN"))

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://TOKEN"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestResolveAbsentSelectedFieldIsNotFound pins that an absent #field is
// reported as not-found too, so an optional field selected out of a present
// JSON secret still gets its default rather than failing the snapshot.
func TestResolveAbsentSelectedFieldIsNotFound(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(conformanceRef("DB"), `{"user":"app"}`)

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "infisical://DB#password"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
