//go:build integration

// Package heroku live integration tests hit the real Heroku Platform API and
// are excluded from the default build. They exist for one reason: nobody on
// this project has a Heroku account, so the contract in heroku.go and
// resolve.go - the path template, the required Accept version header, the flat
// name-to-value response shape, and the statuses - comes from the vendor's API
// reference and published JSON schema rather than from a live call. These tests
// are how the first person with an API token confirms all of it in one command.
//
//	export HEROKU_API_KEY=$(heroku auth:token)
//	export HEROKU_APP=my-app
//	export HEROKU_TEST_CONFIG_VAR=SOME_EXISTING_VAR
//	GOWORK=off go test -tags integration -run Integration ./...
//
// All three are required and every test here skips if any is unset. Nothing
// below ever logs a token or a resolved value: only the config var NAME (an
// environment variable the operator chose, not a secret), the app name, and a
// byte count.
package heroku

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// envTestVar names an existing config var of the app under test. It is a test
// knob rather than something the provider reads, which is why it is declared
// here and not beside envAPIKey.
const envTestVar = "HEROKU_TEST_CONFIG_VAR"

// liveProvider builds a provider from the environment, skipping the calling
// test when the three required variables are not all set.
//
// The token and app are passed as explicit options rather than left to the
// provider's own lazy environment reading, so that this test exercises a
// known configuration instead of silently picking up a HEROKU_APP_NAME from a
// dyno it happens to be running on.
func liveProvider(t *testing.T) (*Provider, string, string) {
	t.Helper()
	token := os.Getenv(envAPIKey)
	app := os.Getenv(envApp)
	name := os.Getenv(envTestVar)
	if token == "" || app == "" || name == "" {
		t.Skipf("set %s, %s and %s to run the Heroku integration tests", envAPIKey, envApp, envTestVar)
	}

	opts := []Option{WithAPIKey(token), WithApp(app)}
	if base := os.Getenv("HEROKU_BASE_URL"); base != "" {
		opts = append(opts, WithBaseURL(base))
	}
	return New(opts...), app, name
}

// TestIntegrationResolve is the check no fake can perform: that
// GET /apps/{app}/config-vars really answers with a FLAT object of names to
// string values, with no envelope around it, and that the Accept version header
// this package sends is accepted.
func TestIntegrationResolve(t *testing.T) {
	p, app, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("heroku://" + name)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", name, err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", name, err)
	}
	if len(v.Bytes) == 0 {
		t.Fatalf("Resolve(%q) returned an empty value; %s must name a config var with a non-empty value", name, envTestVar)
	}
	if v.Version == "" {
		t.Errorf("Resolve(%q) returned an empty Version", name)
	}
	if !v.Sensitive {
		t.Errorf("Resolve(%q) returned Sensitive=false; every config var must be marked sensitive", name)
	}
	if v.Metadata["app"] != app {
		t.Errorf("Metadata[app] = %q, want %q", v.Metadata["app"], app)
	}
	// The byte count and the version, never the value.
	t.Logf("Resolve(%q): %d byte(s), version %q", name, len(v.Bytes), v.Version)
}

// TestIntegrationRefWithAppInThePath confirms the two-segment grammar against
// the real path template. A one-segment ref could pass while /apps/{app} was
// built wrongly, because the app would have come from the same place either
// way; this is the only test where the app identity travels from the ref itself.
func TestIntegrationRefWithAppInThePath(t *testing.T) {
	_, app, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Constructed with no WithApp, so the ref path is the only source of the
	// app identity. HEROKU_APP is still set in this process, so the option is
	// what is being removed, not the environment.
	opts := []Option{WithAPIKey(os.Getenv(envAPIKey))}
	if base := os.Getenv("HEROKU_BASE_URL"); base != "" {
		opts = append(opts, WithBaseURL(base))
	}
	bare := New(opts...)

	ref, err := mamori.ParseRef("heroku://" + app + "/" + name)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := bare.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve(heroku://%s/%s): %v", app, name, err)
	}
	t.Logf("two-segment ref resolved %d byte(s)", len(v.Bytes))
}

// TestIntegrationBatchCostsOneRequest is the claim this provider exists to
// make, checked where it matters. A fake can count requests; only the live API
// can confirm that one document really carries every config var, so that asking
// for several names needs no second call.
func TestIntegrationBatchCostsOneRequest(t *testing.T) {
	p, _, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	present, err := mamori.ParseRef("heroku://" + name)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	absent, err := mamori.ParseRef("heroku://" + name + "_MAMORI_INTEGRATION_ABSENT")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

	got, err := p.ResolveBatch(ctx, []mamori.Ref{present, absent})
	if err != nil {
		t.Fatalf("ResolveBatch: %v", err)
	}
	if _, ok := got[absent.Raw]; ok {
		t.Error("an absent config var must be omitted from the result map, not resolved")
	}
	v, ok := got[present.Raw]
	if !ok {
		t.Fatalf("the present config var %q is missing from the batch result", name)
	}

	single, err := p.Resolve(ctx, present)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if single.Version != v.Version {
		t.Errorf("Resolve and ResolveBatch disagree on Version (%q vs %q)", single.Version, v.Version)
	}
	if len(single.Bytes) != len(v.Bytes) {
		t.Errorf("Resolve and ResolveBatch disagree on length (%d vs %d)", len(single.Bytes), len(v.Bytes))
	}
	t.Logf("batch of 2 refs (1 present, 1 absent) agreed with Resolve on %d byte(s)", len(v.Bytes))
}

// TestIntegrationAbsentAppIsNotFound confirms the live 404 really is a 404,
// which is what makes ResolveBatch's "omit this app's refs" branch correct
// rather than a guess. A backend that answered 403 for an app the token cannot
// see would change that decision, and no fake could tell us.
func TestIntegrationAbsentAppIsNotFound(t *testing.T) {
	p, app, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	absent := app + "-mamori-integration-absent"
	ref, err := mamori.ParseRef("heroku://" + absent + "/" + name)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if _, err := p.Resolve(ctx, ref); !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("Resolve of app %q err = %v, want ErrNotFound; if an app of this derived name genuinely exists, rename it", absent, err)
	}
	t.Logf("absent app %q correctly reported not-found", absent)
}

// TestIntegrationBadTokenIsUnauthenticated confirms the live API answers a
// wrong token with a status that classifies as unauthenticated rather than, say,
// 403. mamori treats unauthenticated as terminal, and only that kind names the
// actual problem in `mamori doctor`.
func TestIntegrationBadTokenIsUnauthenticated(t *testing.T) {
	_, app, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opts := []Option{WithAPIKey("mamori-integration-deliberately-wrong-token"), WithApp(app)}
	if base := os.Getenv("HEROKU_BASE_URL"); base != "" {
		opts = append(opts, WithBaseURL(base))
	}
	ref, err := mamori.ParseRef("heroku://" + name)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

	_, err = New(opts...).Resolve(ctx, ref)
	if err == nil {
		t.Fatal("a deliberately wrong API token resolved successfully")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindUnauthenticated {
		t.Errorf("kind = %s, want unauthenticated", got)
	}
	// Only the kind is logged: the error is the reply to a request that carried
	// a credential.
	t.Logf("a wrong API token was classified as %s", mamori.ErrorKind(err))
}

// TestIntegrationMissingAcceptHeaderIs406 is the one claim in this package that
// nothing else can verify and that the whole provider depends on: that the
// Platform API really does refuse a request without the version header. It
// bypasses the provider and issues the same GET without that header, so a
// future Heroku change making the header optional (or making a bare request
// answer something other than 406) surfaces here rather than as a mystery.
func TestIntegrationMissingAcceptHeaderIs406(t *testing.T) {
	_, app, _ := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	base := os.Getenv("HEROKU_BASE_URL")
	if base == "" {
		base = defaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+appsPath+url.PathEscape(app)+configVarsSuffix, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv(envAPIKey))

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The body is deliberately not read: on a 200, which is the failure this
	// test is looking for, it would be the app's entire config var document.
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Errorf("a request without the Accept version header returned %d, want 406; the vendor contract this provider is built on has changed", resp.StatusCode)
	}
	t.Logf("a request without the version header was answered %d", resp.StatusCode)
}
