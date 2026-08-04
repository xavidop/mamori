package heroku

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// TestResolveSendsTheRequiredAcceptHeader is the single most consequential test
// in this file, and the reason fake_test.go answers 406 rather than only
// recording what it saw.
//
// The Platform API requires "Accept: application/vnd.heroku+json; version=3" on
// every request and refuses anything else with 406 not_acceptable. A fake that
// merely recorded the header would let a provider that never sent it resolve
// every value in this package's tests and fail against the real backend on the
// first call, which is exactly how hand-rolled Heroku clients die. Both halves
// are asserted: that the resolve succeeds at all (the fake would have 406'd),
// and that the header is byte-for-byte the documented string.
func TestResolveSendsTheRequiredAcceptHeader(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "LOG_LEVEL", "debug")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "heroku://LOG_LEVEL"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("Bytes = %q, want %q", v.Bytes, "debug")
	}

	_, accept, auth := f.observed()
	if accept != "application/vnd.heroku+json; version=3" {
		t.Fatalf("Accept = %q, want the documented version header", accept)
	}
	if auth != "Bearer "+testToken {
		t.Fatalf("Authorization = %q, want the documented bearer form", auth)
	}
}

// TestMissingAcceptHeaderIsRefusedByTheBackend proves the previous test could
// have failed. It sends a request through the same fake without the header and
// asserts the 406, so the fake's enforcement is itself pinned rather than
// assumed.
func TestMissingAcceptHeaderIsRefusedByTheBackend(t *testing.T) {
	f := newFake()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://api.heroku.test"+appsPath+testApp+configVarsSuffix, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := f.transport().RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406: a fake that does not enforce the version header cannot prove the provider sends it", resp.StatusCode)
	}
}

// TestResolveRequestsTheDocumentedPath pins the path template,
// GET /apps/{app_id_or_name}/config-vars. It is the one string in this package
// that cannot be inferred from anything else, and a typo in it fails only
// against the real backend.
func TestResolveRequestsTheDocumentedPath(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "LOG_LEVEL", "debug")

	if _, err := f.provider().Resolve(context.Background(), mustRef(t, "heroku://LOG_LEVEL")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	path, _, _ := f.observed()
	if want := "/apps/" + testApp + "/config-vars"; path != want {
		t.Fatalf("request path = %q, want %q", path, want)
	}
}

// TestRefGrammar covers every shape the two-segment grammar admits and every
// shape it refuses. The refusals matter as much as the acceptances: a grammar
// that silently ignored a third segment would make heroku://a/b/C resolve
// something other than what it says.
func TestRefGrammar(t *testing.T) {
	tests := []struct {
		name    string
		tag     string
		wantApp string
		wantVar string
		wantErr bool
	}{
		{name: "one segment uses the configured app", tag: "heroku://LOG_LEVEL", wantApp: testApp, wantVar: "LOG_LEVEL"},
		{name: "two segments name the app", tag: "heroku://other-app/LOG_LEVEL", wantApp: "other-app", wantVar: "LOG_LEVEL"},
		{name: "a fragment is not part of the path", tag: "heroku://CREDS#password", wantApp: testApp, wantVar: "CREDS"},
		{name: "a leading slash is tolerated", tag: "heroku:///LOG_LEVEL", wantApp: testApp, wantVar: "LOG_LEVEL"},
		{name: "an app id works like a name", tag: "heroku://01234567-89ab-cdef-0123-456789abcdef/LOG_LEVEL",
			wantApp: "01234567-89ab-cdef-0123-456789abcdef", wantVar: "LOG_LEVEL"},
		{name: "three segments are refused", tag: "heroku://app/extra/LOG_LEVEL", wantErr: true},
		{name: "an empty path is refused", tag: "heroku://", wantErr: true},
		{name: "a trailing slash leaves no name", tag: "heroku://app/", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			p := newFake().provider()

			got, err := p.targetFor(mustRef(t, tc.tag))
			if tc.wantErr {
				if !errors.Is(err, mamori.ErrInvalid) {
					t.Fatalf("targetFor(%q) err = %v, want ErrInvalid", tc.tag, err)
				}
				// A malformed ref must never report not-found: that is the one
				// kind that makes mamori silently apply the field's default.
				if errors.Is(err, mamori.ErrNotFound) {
					t.Fatalf("targetFor(%q) reported ErrNotFound; mamori would apply the field's default for a typo", tc.tag)
				}
				return
			}
			if err != nil {
				t.Fatalf("targetFor(%q): %v", tc.tag, err)
			}
			if got.app != tc.wantApp || got.name != tc.wantVar {
				t.Fatalf("targetFor(%q) = {app:%q name:%q}, want {app:%q name:%q}", tc.tag, got.app, got.name, tc.wantApp, tc.wantVar)
			}
		})
	}
}

// TestAppPrecedence pins the chain the package doc promises: the ref beats
// WithApp, which beats HEROKU_APP, which beats HEROKU_APP_NAME. Each row
// removes exactly one source, so a chain that collapsed two levels (or read
// them in the wrong order) fails a specific row rather than all of them.
func TestAppPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		tag     string
		option  string
		envApp  string
		envName string
		want    string
	}{
		{
			name: "the ref wins over everything", tag: "heroku://from-ref/V",
			option: "from-option", envApp: "from-env", envName: "from-dyno", want: "from-ref",
		},
		{
			name: "the option wins over both environment variables", tag: "heroku://V",
			option: "from-option", envApp: "from-env", envName: "from-dyno", want: "from-option",
		},
		{
			name: "HEROKU_APP wins over the dyno metadata name", tag: "heroku://V",
			envApp: "from-env", envName: "from-dyno", want: "from-env",
		},
		{
			name: "HEROKU_APP_NAME is the last resort", tag: "heroku://V",
			envName: "from-dyno", want: "from-dyno",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(envApp, tc.envApp)
			t.Setenv(envAppName, tc.envName)

			p := New(WithApp(tc.option))
			got, err := p.targetFor(mustRef(t, tc.tag))
			if err != nil {
				t.Fatalf("targetFor: %v", err)
			}
			if got.app != tc.want {
				t.Fatalf("app = %q, want %q", got.app, tc.want)
			}
		})
	}
}

// TestNoAppIsInvalidNotNotFound pins the classification of a forgotten app.
// ErrNotFound is the one kind that makes mamori apply a field's default, so a
// misconfiguration reported as one becomes a silently defaulted value instead
// of a startup failure.
func TestNoAppIsInvalidNotNotFound(t *testing.T) {
	clearEnv(t)

	_, err := New().Resolve(context.Background(), mustRef(t, "heroku://LOG_LEVEL"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("a missing app reported ErrNotFound; mamori would silently apply the field's default")
	}
	// The message must name both ways to fix it.
	if !strings.Contains(err.Error(), envApp) || !strings.Contains(err.Error(), "WithApp") {
		t.Fatalf("err = %q, want it to name both %s and WithApp", err, envApp)
	}
}

// TestNoTokenIsInvalidAndNamesBothSources pins the same rule for the credential,
// and that the message never echoes a token (there is none to echo here, which
// is precisely why the assertion is about naming the SOURCES instead).
func TestNoTokenIsInvalidAndNamesBothSources(t *testing.T) {
	clearEnv(t)

	_, err := New(WithApp(testApp)).Resolve(context.Background(), mustRef(t, "heroku://LOG_LEVEL"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), envAPIKey) || !strings.Contains(err.Error(), "WithAPIKey") {
		t.Fatalf("err = %q, want it to name both %s and WithAPIKey", err, envAPIKey)
	}
}

// TestTokenFromEnvironmentIsRead pins the lazy environment read that makes a
// blank import usable: New() takes no token, and the value is picked up at
// resolve time so registering from init before the environment is populated
// still works.
func TestTokenFromEnvironmentIsRead(t *testing.T) {
	clearEnv(t)
	t.Setenv(envAPIKey, testToken)
	t.Setenv(envApp, testApp)

	f := newFake()
	f.set(testApp, "LOG_LEVEL", "debug")
	p := New(WithBaseURL("https://api.heroku.test"), WithHTTPClient(&http.Client{Transport: f.transport()}))

	v, err := p.Resolve(context.Background(), mustRef(t, "heroku://LOG_LEVEL"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("Bytes = %q, want %q", v.Bytes, "debug")
	}
}

// TestValueIsAlwaysSensitive pins the decision the module README argues for. A
// config var namespace holds LOG_LEVEL and DATABASE_URL side by side, with no
// per-var classification in the response to tell them apart, and Heroku add-ons
// write live credentials into it without the operator typing them. A plain-
// looking value must therefore still be marked sensitive.
func TestValueIsAlwaysSensitive(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "LOG_LEVEL", "debug")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "heroku://LOG_LEVEL"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Fatal("Sensitive = false; a config var namespace holds add-on credentials and cannot be classified per var")
	}
	if v.Metadata["app"] != testApp {
		t.Fatalf("Metadata[app] = %q, want %q", v.Metadata["app"], testApp)
	}
}

// TestVersionIsPerVarNotTheDocumentETag is the assertion that keeps change
// detection honest, and it is the one a reasonable implementation gets wrong.
//
// This endpoint sends an ETag, httpcore.Version prefers an ETag over a body
// hash, and the obvious code (Version: httpcore.Version(resp, body)) therefore
// compiles, reads well, and is wrong: the ETag covers the WHOLE document, so
// editing any var changes the Version of every ref pointing at that app.
// mamori compares Version instead of bytes (value.go), so every field would
// report changed - a spurious PreApply and OnChange each, and for a rotating
// credential a spurious reconnect.
func TestVersionIsPerVarNotTheDocumentETag(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "STABLE", "unchanged")
	f.set(testApp, "MOVING", "before")
	p := f.provider()

	stableBefore, err := p.Resolve(context.Background(), mustRef(t, "heroku://STABLE"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Change a DIFFERENT var. The document, and so its ETag, changes.
	f.set(testApp, "MOVING", "after")

	stableAfter, err := p.Resolve(context.Background(), mustRef(t, "heroku://STABLE"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if stableBefore.Version != stableAfter.Version {
		t.Fatalf("Version of an untouched var moved (%q then %q) because another var in the same app changed; mamori would report every field changed",
			stableBefore.Version, stableAfter.Version)
	}

	// And it must still move when the var itself moves, or the test above would
	// pass against a constant.
	movingBefore, err := p.Resolve(context.Background(), mustRef(t, "heroku://MOVING"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	f.set(testApp, "MOVING", "later")
	movingAfter, err := p.Resolve(context.Background(), mustRef(t, "heroku://MOVING"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if movingBefore.Version == movingAfter.Version {
		t.Fatal("Version did not move when the var's own value changed; change detection is impossible")
	}
}

// TestVersionIsOfTheWholeVarNotTheSelectedFragment pins the sibling rule: two
// refs selecting different #fields of one config var must agree on when that
// var changed, so the hash covers the whole value.
func TestVersionIsOfTheWholeVarNotTheSelectedFragment(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "CREDS", `{"user":"app","password":"p1"}`)
	p := f.provider()

	user, err := p.Resolve(context.Background(), mustRef(t, "heroku://CREDS#user"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	password, err := p.Resolve(context.Background(), mustRef(t, "heroku://CREDS#password"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if user.Version != password.Version {
		t.Fatalf("two selectors of one var disagree on Version (%q vs %q)", user.Version, password.Version)
	}
	if string(user.Bytes) != "app" || string(password.Bytes) != "p1" {
		t.Fatalf("selection wrong: user=%q password=%q", user.Bytes, password.Bytes)
	}
}

// TestAbsentVarIsNotFound pins the shape that makes a field's `default:` and
// `optional` handling work. There is no per-config-var 404 on this endpoint: an
// absent name is simply absent from a successful 200 document, so the provider
// itself has to produce the not-found.
func TestAbsentVarIsNotFound(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "PRESENT", "yes")
	f.del(testApp, "ABSENT")

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "heroku://ABSENT"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestNullValueIsNotFoundNotTheStringNull pins the vendor schema's
// ["string","null"] value type. Decoding a config var document into
// map[string]string instead of map[string]*string compiles, passes every other
// test in this package, and resolves a null var to the empty string - silently
// applying "" where the field's default belonged.
func TestNullValueIsNotFoundNotTheStringNull(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.setNull(testApp, "REMOVED")

	_, err := f.provider().Resolve(context.Background(), mustRef(t, "heroku://REMOVED"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for a null config var", err)
	}
}

// TestEmptyValueIsNotNotFound pins the other side of that distinction: a config
// var whose value is legitimately the empty string resolves, rather than being
// mistaken for an absent or deleted one. This is what the pointer in configVars
// exists for.
func TestEmptyValueIsNotNotFound(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "EMPTY", "")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "heroku://EMPTY"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(v.Bytes) != 0 {
		t.Fatalf("Bytes = %q, want empty", v.Bytes)
	}
	if v.Version == "" {
		t.Error("Version is empty; an empty value still has a content hash")
	}
}

// TestAppIdentityIsEscapedAsOneSegment pins that the app identity travels as
// exactly one path segment.
//
// mamori does not percent-decode a ref path (see ParseRef), so "%2F" written in
// a ref is a literal three-character sequence in the identity, not a slash. The
// requirement is that it stay that way: written into the URL raw it would reach
// the wire as a real separator and address /apps/weird/app/config-vars, a
// different resource. url.PathEscape re-escapes the percent sign, so the wire
// carries "%252F" - one segment - and the backend sees back the literal
// identity the ref named.
func TestAppIdentityIsEscapedAsOneSegment(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("weird%2Fapp", "LOG_LEVEL", "debug")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "heroku://weird%2Fapp/LOG_LEVEL"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("Bytes = %q, want %q", v.Bytes, "debug")
	}
	path, _, _ := f.observed()
	if want := "/apps/weird%252Fapp/config-vars"; path != want {
		t.Fatalf("request path = %q, want %q: a raw %%2F would have become a real path separator", path, want)
	}
}

// TestTraversingAppIdentityCannotLeaveTheAppsPrefix pins both halves of the
// containment story for the first path segment, which is the one interpolated
// straight into the request URL and the one ${VAR} substitution can fill at
// runtime.
//
// A literal ".." is refused by httpcore.Client.Do before anything is sent, so
// no request escapes /apps/ into another Platform API resource. A
// percent-encoded "%2e%2e" is NOT refused, and must not be: mamori does not
// decode a ref path, so it is an ordinary identity whose name happens to
// contain percent signs, and url.PathEscape doubles them so the wire carries
// "%252e%252e" - a literal segment the backend cannot read as a traversal
// either. Asserting the wire path is what distinguishes "neutralised" from
// "waved through".
func TestTraversingAppIdentityCannotLeaveTheAppsPrefix(t *testing.T) {
	clearEnv(t)

	f := newFake()
	_, err := f.provider().Resolve(context.Background(), mustRef(t, "heroku://../LOG_LEVEL"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Errorf(`Resolve("heroku://../LOG_LEVEL") err = %v, want ErrInvalid`, err)
	}
	if total, _ := f.counts(); total != 0 {
		t.Fatalf("a literal .. app identity reached the wire (%d request(s))", total)
	}

	f = newFake()
	_, err = f.provider().Resolve(context.Background(), mustRef(t, "heroku://%2e%2e/LOG_LEVEL"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf(`Resolve("heroku://%%2e%%2e/LOG_LEVEL") err = %v, want ErrNotFound (an app of that literal name)`, err)
	}
	path, _, _ := f.observed()
	if want := "/apps/%252e%252e/config-vars"; path != want {
		t.Fatalf("request path = %q, want %q: an encoded traversal must be doubled, not decoded", path, want)
	}
}

// TestBaseURLScheme pins the closed set of schemes this package will send an
// API token to, and that the refusal happens at resolve time rather than
// silently at every request.
func TestBaseURLScheme(t *testing.T) {
	tests := []struct {
		name          string
		base          string
		allowInsecure bool
		wantErr       bool
	}{
		{name: "https is accepted", base: "https://api.heroku.test"},
		{name: "http is refused by default", base: "http://api.heroku.test", wantErr: true},
		{name: "http is accepted with the opt-in", base: "http://api.heroku.test", allowInsecure: true},
		{name: "another scheme is refused even with the opt-in", base: "ftp://api.heroku.test", allowInsecure: true, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			f := newFake()
			f.set(testApp, "LOG_LEVEL", "debug")

			opts := []Option{WithBaseURL(tc.base)}
			if tc.allowInsecure {
				opts = append(opts, WithAllowInsecure())
			}
			_, err := f.provider(opts...).Resolve(context.Background(), mustRef(t, "heroku://LOG_LEVEL"))
			if tc.wantErr {
				if !errors.Is(err, mamori.ErrInvalid) {
					t.Fatalf("err = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
		})
	}
}

// TestEmptyOptionsFallThroughToTheEnvironment pins that an option given an
// empty string is ignored rather than pinning an unusable empty credential. A
// caller writing WithAPIKey(os.Getenv("SOME_OTHER_VAR")) on an unset variable
// must still fall back to HEROKU_API_KEY.
func TestEmptyOptionsFallThroughToTheEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv(envAPIKey, testToken)

	f := newFake()
	f.set(testApp, "LOG_LEVEL", "debug")
	p := New(
		WithAPIKey(""),
		WithApp(testApp),
		WithBaseURL(""),
		WithBaseURL("https://api.heroku.test"),
		WithHTTPClient(nil),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
	)

	v, err := p.Resolve(context.Background(), mustRef(t, "heroku://LOG_LEVEL"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("Bytes = %q, want %q", v.Bytes, "debug")
	}
}

// TestClientConstructionFailureIsNotCached pins the deliberate absence of
// caching around a failed clientFor. A process whose token arrives after start
// - a mounted secret, a sidecar-populated environment - must succeed on a later
// poll rather than being poisoned by the first attempt.
func TestClientConstructionFailureIsNotCached(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set(testApp, "LOG_LEVEL", "debug")
	p := New(
		WithApp(testApp),
		WithBaseURL("https://api.heroku.test"),
		WithHTTPClient(&http.Client{Transport: f.transport()}),
	)

	if _, err := p.Resolve(context.Background(), mustRef(t, "heroku://LOG_LEVEL")); !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("first Resolve err = %v, want ErrInvalid with no token set", err)
	}

	t.Setenv(envAPIKey, testToken)
	v, err := p.Resolve(context.Background(), mustRef(t, "heroku://LOG_LEVEL"))
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("Bytes = %q, want %q", v.Bytes, "debug")
	}
}

// TestScheme pins the registered scheme string.
func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != "heroku" {
		t.Fatalf("Scheme() = %q, want %q", got, "heroku")
	}
}
