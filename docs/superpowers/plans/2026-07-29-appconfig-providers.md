# AppConfig Providers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add AWS AppConfig and Azure App Configuration as mamori providers, inside the `providers/aws` and `providers/azure` modules that already exist.

**Architecture:** `aws-appconfig` wraps a session protocol whose payload is empty when the client already has the current version, so `Resolve` starts a throwaway session per call: a fresh session cannot already be current, which puts that hazard out of reach rather than guarding against it. It implements no `Watch`, so mamori polls it through `pollWatch` like any other backend without native change notification (see the dropped Task 2 below). `azure-appconfig` is a conventional request/response provider shaped like the `azure-kv` provider beside it.

**Tech Stack:** Go 1.26, `aws-sdk-go-v2/service/appconfigdata` v1.26.1, `azure-sdk-for-go/sdk/data/azappconfig` v1.2.0, `providertest` conformance kit.

**Spec:** `docs/superpowers/specs/2026-07-29-appconfig-providers-design.md`

## Global Constraints

- Go 1.26+. Provider modules are tested with `GOWORK=off` so only their own `go.mod` is exercised.
- **Every task must run `golangci-lint run` in the module directory before reporting done.** CI lints each provider module with golangci-lint v2.12.2, and `.golangci.yml` sets `default: standard`, which includes `unused`. `go test`, `go vet`, and `gofmt` all pass on code the linter rejects: an unused test helper is the common case, and it fails the build. Task 1 shipped exactly that defect because its verification steps omitted the linter.
- Never use the em-dash character in any file, code comment, doc, or commit message.
- Both new providers set `Value.Sensitive = false`. They are configuration services, not secret stores, matching `consul`, `etcd`, `configcat`, and `launchdarkly`.
- Wrap SDK errors with mamori sentinels using `%w`, never `%v`, so `errors.Is` survives.
- Missing values return an error satisfying `errors.Is(err, mamori.ErrNotFound)`.
- `Value.Version` is always set: a native revision when one exists, `mamori.VersionHash(data)` otherwise.
- Every provider passes `providertest.Run` with `Seed`, `Mutate`, `Fail`, `Clear`, and `PointerRef` all supplied.
- Docs ship with the feature, per `CONTRIBUTING.md` step 6 and 7: module `README.md`, a docs-site page under `site/`, a row in **both** coverage tables (root `README.md` and `site/src/pages/docs/providers/index.md`), and an `## Error classification` section in both the module README and the site page.
- Commits follow Conventional Commits. Use `feat(aws):` / `feat(azure):` scopes. Do **not** write a `BREAKING CHANGE:` footer.
- Do not run `git commit`; stage work and report it. The controller commits.

---

### Task 1: `aws-appconfig` resolve

**Files:**
- Create: `providers/aws/appconfig.go`
- Create: `providers/aws/appconfig_test.go`
- Modify: `providers/aws/aws.go` (register the provider in `init`; extend `classifyAWS`)
- Modify: `providers/aws/go.mod`, `providers/aws/go.sum`
- Modify: `providers/aws/README.md`
- Modify: `README.md` (root coverage table)
- Create: `site/src/pages/docs/providers/aws-appconfig.md`
- Modify: `site/src/pages/docs/providers/index.md` (coverage table)

**Interfaces:**
- Consumes: `options`, `newOptions`, `loadConfig`, `classifyAWS` from `providers/aws/aws.go`.
- Produces: `AppConfigProvider` struct; `NewAppConfig(opts ...Option) *AppConfigProvider`; `newAppConfigWithClient(c appConfigAPI) *AppConfigProvider`; `const schemeAppConfig = "aws-appconfig"`; the `appConfigAPI` interface; `parseAppConfigPath(ref mamori.Ref) (app, env, profile string, err error)`.

- [ ] **Step 1: Add the SDK dependency**

```bash
cd providers/aws
GOWORK=off go get github.com/aws/aws-sdk-go-v2/service/appconfigdata@v1.26.1
```

Expected: `go.mod` gains `github.com/aws/aws-sdk-go-v2/service/appconfigdata v1.26.1` as a direct dependency.

- [ ] **Step 2: Write the failing path-parsing test**

Add to `providers/aws/appconfig_test.go`:

```go
package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appconfigdata"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

func TestParseAppConfigPath(t *testing.T) {
	tests := []struct {
		name                     string
		raw                      string
		wantApp, wantEnv, wantPr string
		wantErr                  bool
	}{
		{name: "three segments", raw: "aws-appconfig://myapp/prod/flags", wantApp: "myapp", wantEnv: "prod", wantPr: "flags"},
		{name: "with fragment", raw: "aws-appconfig://myapp/prod/flags#a", wantApp: "myapp", wantEnv: "prod", wantPr: "flags"},
		{name: "two segments", raw: "aws-appconfig://myapp/prod", wantErr: true},
		{name: "four segments", raw: "aws-appconfig://myapp/prod/flags/extra", wantErr: true},
		{name: "empty middle", raw: "aws-appconfig://myapp//flags", wantErr: true},
		{name: "trailing slash", raw: "aws-appconfig://myapp/prod/", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := mamori.ParseRef(tt.raw)
			if err != nil {
				t.Fatalf("ParseRef(%q): %v", tt.raw, err)
			}
			app, env, pr, err := parseAppConfigPath(ref)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseAppConfigPath(%q) = (%q,%q,%q), want error", tt.raw, app, env, pr)
				}
				if !errors.Is(err, mamori.ErrInvalid) {
					t.Errorf("error %v does not satisfy errors.Is(ErrInvalid)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAppConfigPath(%q): %v", tt.raw, err)
			}
			if app != tt.wantApp || env != tt.wantEnv || pr != tt.wantPr {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)", app, env, pr, tt.wantApp, tt.wantEnv, tt.wantPr)
			}
		})
	}
}
```

- [ ] **Step 3: Run it to confirm it fails**

```bash
cd providers/aws && GOWORK=off go test -run TestParseAppConfigPath ./...
```

Expected: FAIL, `undefined: parseAppConfigPath`.

- [ ] **Step 4: Write the session-protocol fake**

The fake is the centrepiece of this task. A fake that always returns the payload would pass a provider carrying the exact bug this design exists to prevent, so it models the protocol: single-use tokens, rejection of reused tokens, and an empty payload when the session already holds the current version.

Add to `providers/aws/appconfig_test.go`:

```go
// ---------------------------------------------------------------------------
// In-memory fake for AppConfig Data.
//
// This models the session protocol rather than simply returning a payload,
// because the protocol is where the risk lives. Tokens are single-use, a
// reused or unknown token is rejected the way the service rejects one, and a
// session that already holds the current version receives an empty
// Configuration. A fake without these properties would pass a provider that
// reuses one session across Resolve calls, which is the precise defect the
// per-call-session design exists to prevent.
// ---------------------------------------------------------------------------

// acProfile is one configuration profile's stored state.
type acProfile struct {
	data    string
	version int    // bumped on every set, so a session can detect staleness
	label   string // VersionLabel, empty for non-hosted configurations
}

// acSession is one live configuration session. seenVersion is the version the
// session's client is known to hold; 0 means it holds nothing yet, which is
// why a fresh session's first call always returns data.
type acSession struct {
	key         string // "app/env/profile"
	seenVersion int
}

type fakeAppConfig struct {
	mu       sync.Mutex
	profiles map[string]acProfile
	fails    map[string]error
	sessions map[string]acSession // token -> session
	counter  int

	// forceEmptyFirst makes the first GetLatestConfiguration of every new
	// session return an empty payload, modelling the (contradicted, but
	// defended against) reading of the API in which a fresh session receives
	// no data. It exists so the defensive branch in Resolve is testable.
	forceEmptyFirst bool

	// pollInterval is returned as NextPollIntervalInSeconds. Task 2's watch
	// tests set it low so they do not wait on the 60s service default.
	pollInterval int32
}

func newFakeAppConfig() *fakeAppConfig {
	return &fakeAppConfig{
		profiles:     map[string]acProfile{},
		fails:        map[string]error{},
		sessions:     map[string]acSession{},
		pollInterval: 60,
	}
}

// set stores a profile's payload and bumps its version, so any session
// holding the previous version sees a change on its next poll.
func (f *fakeAppConfig) set(key, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.profiles[key]
	p.data = val
	p.version++
	p.label = fmt.Sprintf("v%d", p.version)
	f.profiles[key] = p
}

// remove deletes a profile, so StartConfigurationSession reports not-found.
func (f *fakeAppConfig) remove(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.profiles, key)
}

// fail makes the next StartConfigurationSession for key return err, until
// clear(key) is called. It powers the providertest ErrorClassification case.
func (f *fakeAppConfig) fail(key string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[key] = err
}

func (f *fakeAppConfig) clear(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fails, key)
}

// mint issues a fresh single-use token bound to a session.
func (f *fakeAppConfig) mint(s acSession) string {
	f.counter++
	tok := fmt.Sprintf("tok-%d", f.counter)
	f.sessions[tok] = s
	return tok
}

func (f *fakeAppConfig) StartConfigurationSession(ctx context.Context, in *appconfigdata.StartConfigurationSessionInput, _ ...func(*appconfigdata.Options)) (*appconfigdata.StartConfigurationSessionOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.Join([]string{
		awssdk.ToString(in.ApplicationIdentifier),
		awssdk.ToString(in.EnvironmentIdentifier),
		awssdk.ToString(in.ConfigurationProfileIdentifier),
	}, "/")
	if err, ok := f.fails[key]; ok {
		return nil, err
	}
	if _, ok := f.profiles[key]; !ok {
		return nil, &smithy.GenericAPIError{Code: "ResourceNotFoundException", Message: "profile " + key + " not found"}
	}
	tok := f.mint(acSession{key: key})
	return &appconfigdata.StartConfigurationSessionOutput{InitialConfigurationToken: awssdk.String(tok)}, nil
}

func (f *fakeAppConfig) GetLatestConfiguration(ctx context.Context, in *appconfigdata.GetLatestConfigurationInput, _ ...func(*appconfigdata.Options)) (*appconfigdata.GetLatestConfigurationOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	tok := awssdk.ToString(in.ConfigurationToken)
	s, ok := f.sessions[tok]
	if !ok {
		// Unknown or already-spent token. The service returns
		// BadRequestException for an expired or reused token.
		return nil, &smithy.GenericAPIError{Code: "BadRequestException", Message: "invalid configuration token"}
	}
	delete(f.sessions, tok) // single use

	if err, ok := f.fails[s.key]; ok {
		return nil, err
	}

	p := f.profiles[s.key]
	out := &appconfigdata.GetLatestConfigurationOutput{
		NextPollIntervalInSeconds: f.pollInterval,
	}

	fresh := s.seenVersion == 0
	unchanged := s.seenVersion == p.version
	if (fresh && f.forceEmptyFirst) || (!fresh && unchanged) {
		// Empty payload: the client already holds the current version.
		// VersionLabel is empty in this case too, per the API docs.
		out.NextPollConfigurationToken = awssdk.String(f.mint(s))
		return out, nil
	}

	s.seenVersion = p.version
	out.Configuration = []byte(p.data)
	out.VersionLabel = awssdk.String(p.label)
	out.ContentType = awssdk.String("application/json")
	out.NextPollConfigurationToken = awssdk.String(f.mint(s))
	return out, nil
}
```

Add `"github.com/aws/smithy-go"` to the test file's imports.

Do **not** define a custom API-error type. `smithy.GenericAPIError` already
implements `smithy.APIError`, which is the interface `classifyAWS` matches on
with `errors.As`, and `providers/aws/errors_test.go` already uses it throughout.

- [ ] **Step 5: Write the failing resolve tests**

These are the two session-semantics tests from the spec, which the conformance
kit cannot express, plus the basics.

```go
func TestAppConfigResolve(t *testing.T) {
	fake := newFakeAppConfig()
	fake.set("myapp/prod/flags", `{"featureX":true,"port":8080}`)
	p := newAppConfigWithClient(fake)

	ref, err := mamori.ParseRef("aws-appconfig://myapp/prod/flags")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, want := string(v.Bytes), `{"featureX":true,"port":8080}`; got != want {
		t.Errorf("Bytes = %q, want %q", got, want)
	}
	if v.Sensitive {
		t.Error("Sensitive = true, want false: AppConfig is a configuration service, not a secret store")
	}
	if v.Version == "" {
		t.Error("Version is empty")
	}
}

// TestAppConfigResolveTwiceReturnsPayloadBothTimes is the regression test for
// the central hazard of this provider. GetLatestConfiguration returns an empty
// payload when the session already holds the current version, so a provider
// that cached one session across calls would return the config once and empty
// bytes forever after, and mamori would apply those empty bytes over a live
// field. Resolve must start a fresh session every call.
func TestAppConfigResolveTwiceReturnsPayloadBothTimes(t *testing.T) {
	fake := newFakeAppConfig()
	fake.set("myapp/prod/flags", `{"a":1}`)
	p := newAppConfigWithClient(fake)

	ref, err := mamori.ParseRef("aws-appconfig://myapp/prod/flags")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	for i := 1; i <= 3; i++ {
		v, err := p.Resolve(context.Background(), ref)
		if err != nil {
			t.Fatalf("Resolve #%d: %v", i, err)
		}
		if string(v.Bytes) != `{"a":1}` {
			t.Fatalf("Resolve #%d returned %q, want %q: the provider is reusing a session across calls", i, v.Bytes, `{"a":1}`)
		}
	}
}

// TestAppConfigResolveEmptyOnFreshSessionErrors covers the defensive branch.
// The API reference and user guide agree that a fresh session's first call
// returns data, so this should be unreachable in production. It is asserted
// anyway: if that reading is ever wrong, the provider must fail loudly rather
// than hand mamori a zero-length value to apply over a live config field.
func TestAppConfigResolveEmptyOnFreshSessionErrors(t *testing.T) {
	fake := newFakeAppConfig()
	fake.set("myapp/prod/flags", `{"a":1}`)
	fake.forceEmptyFirst = true
	p := newAppConfigWithClient(fake)

	ref, err := mamori.ParseRef("aws-appconfig://myapp/prod/flags")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatalf("Resolve returned (%q, nil), want an error: an empty payload from a session created moments ago must never be applied as a value", v.Bytes)
	}
	if len(v.Bytes) != 0 {
		t.Errorf("Resolve returned %d bytes alongside an error, want none", len(v.Bytes))
	}
}

func TestAppConfigResolveKeySelection(t *testing.T) {
	fake := newFakeAppConfig()
	fake.set("myapp/prod/flags", `{"db":{"port":5432}}`)
	p := newAppConfigWithClient(fake)

	ref, err := mamori.ParseRef("aws-appconfig://myapp/prod/flags#/db/port")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := string(v.Bytes); got != "5432" {
		t.Errorf("Bytes = %q, want %q", got, "5432")
	}
}

func TestAppConfigResolveNotFound(t *testing.T) {
	fake := newFakeAppConfig()
	p := newAppConfigWithClient(fake)

	ref, err := mamori.ParseRef("aws-appconfig://myapp/prod/missing")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if _, err := p.Resolve(context.Background(), ref); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("Resolve error = %v, want errors.Is(ErrNotFound)", err)
	}
}
```

- [ ] **Step 6: Run them to confirm they fail**

```bash
cd providers/aws && GOWORK=off go test -run TestAppConfig ./...
```

Expected: FAIL, `undefined: newAppConfigWithClient`.

- [ ] **Step 7: Implement the provider**

Create `providers/aws/appconfig.go`:

```go
package aws

import (
	"context"
	"fmt"
	"strings"
	"sync"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appconfigdata"
	"github.com/xavidop/mamori"
)

// schemeAppConfig is the URL scheme handled by AppConfigProvider.
const schemeAppConfig = "aws-appconfig"

// appConfigAPI is the minimal subset of the AppConfig Data client this
// provider uses. The real *appconfigdata.Client satisfies it; tests inject an
// in-memory fake that models the session protocol.
type appConfigAPI interface {
	StartConfigurationSession(ctx context.Context, params *appconfigdata.StartConfigurationSessionInput, optFns ...func(*appconfigdata.Options)) (*appconfigdata.StartConfigurationSessionOutput, error)
	GetLatestConfiguration(ctx context.Context, params *appconfigdata.GetLatestConfigurationInput, optFns ...func(*appconfigdata.Options)) (*appconfigdata.GetLatestConfigurationOutput, error)
}

// AppConfigProvider resolves
// aws-appconfig://<application>/<environment>/<profile>[#key] refs against AWS
// AppConfig. It is safe for concurrent use.
//
// AppConfig is a session protocol, not a request/response API, and one of its
// properties shapes this whole type: GetLatestConfiguration returns an empty
// payload when the calling session already holds the current version. A
// provider that opened one session and reused it would therefore return the
// configuration on its first Resolve and empty bytes on every Resolve after,
// and mamori would apply those empty bytes over a live configuration field.
// The failure would be silent and would look like a config wipe.
//
// Resolve therefore starts a session, makes exactly one call, and discards the
// session. A session created moments ago holds no version at all, so it cannot
// be considered current, so the empty-payload case is structurally unreachable
// on this path. The cost is two API calls per resolve, which is the price of a
// stateless Resolve and is worth paying.
type AppConfigProvider struct {
	opts   options
	mu     sync.Mutex
	client appConfigAPI
}

// Compile-time interface check.
var _ mamori.Provider = (*AppConfigProvider)(nil)

// NewAppConfig constructs an AWS AppConfig provider. The underlying client is
// built lazily on first Resolve using the default credential chain, so
// construction never performs I/O and never fails.
func NewAppConfig(opts ...Option) *AppConfigProvider {
	return &AppConfigProvider{opts: newOptions(opts)}
}

// newAppConfigWithClient returns a provider backed by a caller-supplied
// client. It is the injection seam used by tests.
func newAppConfigWithClient(c appConfigAPI) *AppConfigProvider {
	return &AppConfigProvider{client: c}
}

// Scheme returns "aws-appconfig".
func (p *AppConfigProvider) Scheme() string { return schemeAppConfig }

// getClient returns the cached client, building the real one on first use.
func (p *AppConfigProvider) getClient(ctx context.Context) (appConfigAPI, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	cfg, err := loadConfig(ctx, p.opts)
	if err != nil {
		return nil, fmt.Errorf("aws-appconfig: load config: %w", err)
	}
	p.client = appconfigdata.NewFromConfig(cfg)
	return p.client, nil
}

// parseAppConfigPath splits a ref path into its three required identifiers.
// Each may be an ID or a name; the provider passes them through verbatim and
// lets the service resolve them.
func parseAppConfigPath(ref mamori.Ref) (app, env, profile string, err error) {
	parts := strings.Split(ref.Path, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf(
			"aws-appconfig: ref %q must be aws-appconfig://<application>/<environment>/<profile>[#key]: %w",
			ref.Raw, mamori.ErrInvalid)
	}
	return parts[0], parts[1], parts[2], nil
}

// startSession opens a configuration session and returns its initial token.
func (p *AppConfigProvider) startSession(ctx context.Context, client appConfigAPI, ref mamori.Ref) (string, error) {
	app, env, profile, err := parseAppConfigPath(ref)
	if err != nil {
		return "", err
	}
	in := &appconfigdata.StartConfigurationSessionInput{
		ApplicationIdentifier:          awssdk.String(app),
		EnvironmentIdentifier:          awssdk.String(env),
		ConfigurationProfileIdentifier: awssdk.String(profile),
	}
	if secs, ok := appConfigMinPoll(ref); ok {
		in.RequiredMinimumPollIntervalInSeconds = awssdk.Int32(secs)
	}
	out, err := client.StartConfigurationSession(ctx, in)
	if err != nil {
		return "", fmt.Errorf("aws-appconfig: start session for %q: %w", ref.Path, classifyAWS(err))
	}
	return awssdk.ToString(out.InitialConfigurationToken), nil
}

// Resolve fetches the current configuration. It starts a session, makes one
// GetLatestConfiguration call, and discards the session; see the type comment
// for why a session is never reused across calls.
func (p *AppConfigProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	client, err := p.getClient(ctx)
	if err != nil {
		return mamori.Value{}, err
	}
	token, err := p.startSession(ctx, client, ref)
	if err != nil {
		return mamori.Value{}, err
	}
	out, err := client.GetLatestConfiguration(ctx, &appconfigdata.GetLatestConfigurationInput{
		ConfigurationToken: awssdk.String(token),
	})
	if err != nil {
		return mamori.Value{}, fmt.Errorf("aws-appconfig: resolve %q: %w", ref.Path, classifyAWS(err))
	}

	// An empty payload means "the calling session already holds the current
	// version". This session was created one call ago and holds nothing, so
	// reaching here contradicts the documented protocol. Fail rather than
	// return a zero-length value: mamori would apply that value over a live
	// configuration field, and a silent wipe is far worse than a loud error.
	if len(out.Configuration) == 0 {
		return mamori.Value{}, fmt.Errorf(
			"aws-appconfig: %q returned an empty configuration on a newly created session: %w",
			ref.Path, mamori.ErrUnavailable)
	}

	return appConfigValue(ref.Key, out.Configuration, awssdk.ToString(out.VersionLabel))
}

// appConfigValue assembles a mamori.Value from a configuration payload,
// applying #key selection when key is non-empty.
func appConfigValue(key string, data []byte, versionLabel string) (mamori.Value, error) {
	if key != "" {
		sel, err := mamori.SelectKey(data, key)
		if err != nil {
			return mamori.Value{}, err
		}
		data = sel
	}
	// VersionLabel only applies to AppConfig-hosted configuration versions and
	// is empty for every other source, so the hash fallback is the common path
	// rather than a defensive one.
	version := versionLabel
	if version == "" {
		version = mamori.VersionHash(data)
	}
	return mamori.Value{
		Bytes:     data,
		Version:   version,
		Sensitive: false, // a configuration service, not a secret store
	}, nil
}
```

- [ ] **Step 8: Add the `?minPoll=` option parser**

Append to `providers/aws/appconfig.go`:

```go
// appConfigMinPoll reads the optional ?minPoll=<seconds> query option, which
// sets RequiredMinimumPollIntervalInSeconds on the session. It is meaningful
// only on the Watch path, where it raises the floor the service enforces on
// how often this client may poll; Resolve accepts it and it has no effect,
// since that session is discarded before a second call could be rate-limited.
//
// An unparseable or non-positive value is ignored rather than rejected. The
// service supplies its own default, and refusing to resolve a configuration
// field over a malformed tuning hint would be a worse outcome than ignoring
// the hint.
func appConfigMinPoll(ref mamori.Ref) (int32, bool) {
	raw := ref.Opt("minPoll")
	if raw == "" {
		return 0, false
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return 0, false
	}
	return int32(secs), true
}
```

Add `"strconv"` to the file's imports.

- [ ] **Step 9: Register the provider and close the two `classifyAWS` gaps**

In `providers/aws/aws.go`, extend `init`:

```go
func init() {
	mamori.Register(NewSecretsManager())
	mamori.Register(NewParameterStore())
	mamori.Register(NewAppConfig())
}
```

Update the `init` doc comment above it so it names all three registrations rather than two.

Then extend `classifyAWS`. AppConfig documents four errors, and two of them are
currently unclassified: `BadRequestException` has no entry at all, and
`InternalServerException` is spelled differently from the `InternalServerError`
already in the list, so it falls through to the default and reports as
`KindUnknown`. Add both:

```go
	case "InternalServiceError", "InternalServerError", "InternalFailure",
		"InternalServerException",
		"ServiceUnavailable", "ServiceUnavailableException":
		sentinel = mamori.ErrUnavailable
	case "InvalidParameterException", "InvalidRequestException", "ValidationException",
		"InvalidParameterValue", "InvalidKeyId", "BadRequestException":
		sentinel = mamori.ErrInvalid
```

Also update the module doc comment at the top of `aws.go` so its scheme list
includes `aws-appconfig://<app>/<env>/<profile>[#json-key]` alongside the other
two, and note that AppConfig values are not marked Sensitive.

- [ ] **Step 10: Run the tests**

```bash
cd providers/aws && GOWORK=off go test -run 'TestAppConfig|TestParseAppConfigPath' ./...
```

Expected: PASS.

- [ ] **Step 11: Extend the existing error-classification table test**

`CONTRIBUTING.md` step 3 requires a table test mapping real SDK error values to
kinds, in addition to the conformance case, which only proves a mapping
survives transit rather than that one exists.

`providers/aws/errors_test.go` already has that table as `TestClassifyAWS`. Add
the two codes Step 9 newly classifies to its `cases` slice rather than writing a
second test function:

```go
		{"BadRequest", &smithy.GenericAPIError{Code: "BadRequestException"}, mamori.KindInvalid},
		{"InternalServerException", &smithy.GenericAPIError{Code: "InternalServerException"}, mamori.KindUnavailable},
```

Note the surrounding conventions in that file: the kind type is `mamori.Kind`
and the classifier is `mamori.ErrorKind(err)`. There is no `ClassifyError`.

Then add one test proving the classification actually reaches a caller through
this provider's own error path, mirroring the existing
`TestClassifyAWSPreservesSdkError` style. Wrapping is easy to get wrong
independently of the mapping being right:

```go
func TestAppConfigResolvePreservesClassification(t *testing.T) {
	fake := newFakeAppConfig()
	fake.set("myapp/prod/flags", `{"a":1}`)
	fake.fail("myapp/prod/flags", &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"})
	p := newAppConfigWithClient(fake)

	ref, err := mamori.ParseRef("aws-appconfig://myapp/prod/flags")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	_, err = p.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("Resolve returned nil error")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyAWS may not be wired into the AppConfig resolve path", got, mamori.KindPermissionDenied)
	}
}
```

- [ ] **Step 12: Add the conformance test**

The conformance kit maps a single logical key onto a ref, so the key becomes
the profile segment with a fixed application and environment.

```go
func TestConformanceAppConfig(t *testing.T) {
	fake := newFakeAppConfig()
	const prefix = "myapp/prod/"
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return newAppConfigWithClient(fake) },
		Ref: func(key string) string { return schemeAppConfig + "://" + prefix + key },
		PointerRef: func(key, frag string) string {
			return schemeAppConfig + "://" + prefix + key + frag
		},
		Seed:   func(_ context.Context, key, val string) error { fake.set(prefix+key, val); return nil },
		Mutate: func(_ context.Context, key, val string) error { fake.set(prefix+key, val); return nil },
		Fail:   func(_ context.Context, key string, err error) error { fake.fail(prefix+key, err); return nil },
		Clear:  func(_ context.Context, key string) error { fake.clear(prefix+key); return nil },
	})
}
```

Also extend `TestRegisteredSchemes` and `TestConstructorSchemes` in
`aws_test.go` to cover `schemeAppConfig`.

- [ ] **Step 13: Run the full module suite**

```bash
cd providers/aws && GOWORK=off go mod tidy && GOWORK=off go test ./... && GOWORK=off go vet ./...
```

Expected: PASS. If the conformance kit's not-found case fails, check that the
fake's `StartConfigurationSession` reports `ResourceNotFoundException` for an
unknown profile, which is where not-found surfaces for this provider (the
service resolves identifiers at session start, not at fetch time).

- [ ] **Step 14: Write the docs**

Four documentation surfaces, per the global constraints:

1. `providers/aws/README.md`: add an `aws-appconfig` section covering the ref grammar, the three required path segments, `#key` selection, `?minPoll=`, auth (the same default credential chain as the other two), that values are **not** marked Sensitive and why, and an `## Error classification` section covering the five codes from Step 11. Include a short note that `Resolve` costs two API calls because each one uses a fresh session, and why.
2. `site/src/pages/docs/providers/aws-appconfig.md`: the docs-site page, mirroring the README including its error-classification table. Match the front matter and structure of a sibling page such as `site/src/pages/docs/providers/aws.md` exactly; read it first.
3. Root `README.md`: add the `aws-appconfig` row to the provider coverage table.
4. `site/src/pages/docs/providers/index.md`: add the same row to that table.

- [ ] **Step 15: Verify docs and stage**

```bash
cd providers/aws && GOWORK=off go test ./... && cd ../.. && go build ./... && git add -A && git status --short
```

Report the staged file list. Do not commit.

---

### Task 2: `aws-appconfig` watch - DROPPED, do not implement

This task was written, implemented, and then cut. Its steps have been removed
so no one works from them. It is recorded here rather than deleted because the
reasoning is the useful part.

It specified a `WatchableProvider` implementation owning a long-lived session,
threading `NextPollConfigurationToken` across calls and sleeping
`NextPollIntervalInSeconds`. `provider.go` forbids exactly that, on the
interface itself: "Providers without native watch support are wrapped by mamori
in a polling adapter - provider authors must never fake a Watch with an
internal ticker." AppConfig has no push mechanism, so a session is still
polling. The two providers that do implement `Watch` (`k8s` informers, `sops`
fsnotify) are genuinely event-driven.

The rule is load-bearing: `pollWatch` supplies jitter, change deduplication,
`ErrNotFound` suppression, and a fakeable `Clock`, and a provider-side loop
forfeits all four. Jitter matters most, because AppConfig gives every session
the same cadence and replicas starting together would poll in lockstep against
a priced API.

The argument that justified the session also failed on inspection:
`RequiredMinimumPollIntervalInSeconds` constrains calls within one session, and
`Resolve` creates a fresh session per call, so the service floor is never
approached.

mamori therefore polls `aws-appconfig` through `pollWatch` like any other
non-native provider. If the extra API call per poll ever justifies action, the
fix belongs in core: an optional interface for a provider to suggest a poll
interval, honored by `pollWatch`.

See `docs/superpowers/specs/2026-07-29-appconfig-providers-design.md`.

---

### Task 3: `azure-appconfig`

**Files:**
- Create: `providers/azure/appconfig.go`
- Create: `providers/azure/appconfig_test.go`
- Modify: `providers/azure/azure.go` (register in `init`)
- Modify: `providers/azure/go.mod`, `providers/azure/go.sum`
- Modify: `providers/azure/README.md`
- Modify: `README.md` (root coverage table)
- Create: `site/src/pages/docs/providers/azure-appconfig.md`
- Modify: `site/src/pages/docs/providers/index.md`

**Interfaces:**
- Consumes: `classifyAzure` and `isNotFound` from `providers/azure/azure.go`. The existing `Provider` type in that file is Key Vault's; this is a separate type in a separate file, sharing only the error helpers.
- Produces: `AppConfigProvider` struct; `NewAppConfig(opts ...AppConfigOption) *AppConfigProvider`; `const SchemeAppConfig = "azure-appconfig"`; the `appConfigClient` interface.

Note the naming collision to avoid: `providers/azure/azure.go` already exports
`Option`, `New`, and `Scheme` for the Key Vault provider. This task must not
redefine any of them. Use `AppConfigOption`, `NewAppConfig`, and
`SchemeAppConfig`. Read `providers/azure/azure.go` first and confirm the exact
set of existing exported names before adding any.

- [ ] **Step 1: Add the SDK dependency**

```bash
cd providers/azure
GOWORK=off go get github.com/Azure/azure-sdk-for-go/sdk/data/azappconfig@v1.2.0
```

- [ ] **Step 2: Write the fake and the failing tests**

Create `providers/azure/appconfig_test.go`:

```go
package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azappconfig"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// settingKey identifies a setting by key and label. An absent label is
// Azure's null label, which is a distinct setting from any labelled one, so
// the empty string is a meaningful map key here rather than a missing value.
type settingKey struct {
	key   string
	label string
}

type fakeSetting struct {
	value       string
	etag        string
	contentType string
}

type fakeAppConfig struct {
	mu       sync.Mutex
	settings map[settingKey]fakeSetting
	fails    map[string]error
	counter  int
}

func newFakeAppConfig() *fakeAppConfig {
	return &fakeAppConfig{
		settings: map[settingKey]fakeSetting{},
		fails:    map[string]error{},
	}
}

func (f *fakeAppConfig) set(key, label, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counter++
	f.settings[settingKey{key, label}] = fakeSetting{value: val, etag: fmt.Sprintf("e%d", f.counter)}
}

// setTyped stores a setting with an explicit content type, used to model a
// Key Vault reference.
func (f *fakeAppConfig) setTyped(key, label, val, contentType string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counter++
	f.settings[settingKey{key, label}] = fakeSetting{value: val, etag: fmt.Sprintf("e%d", f.counter), contentType: contentType}
}

func (f *fakeAppConfig) remove(key, label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.settings, settingKey{key, label})
}

func (f *fakeAppConfig) fail(key string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[key] = err
}

func (f *fakeAppConfig) clear(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fails, key)
}

func (f *fakeAppConfig) GetSetting(ctx context.Context, key string, opts *azappconfig.GetSettingOptions) (azappconfig.GetSettingResponse, error) {
	if err := ctx.Err(); err != nil {
		return azappconfig.GetSettingResponse{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.fails[key]; ok {
		return azappconfig.GetSettingResponse{}, err
	}

	var label string
	if opts != nil && opts.Label != nil {
		label = *opts.Label
	}
	s, ok := f.settings[settingKey{key, label}]
	if !ok {
		return azappconfig.GetSettingResponse{}, &azcore.ResponseError{StatusCode: http.StatusNotFound}
	}

	etag := azcore.ETag(s.etag)
	resp := azappconfig.GetSettingResponse{}
	resp.Key = &key
	resp.Value = &s.value
	resp.ETag = &etag
	if s.contentType != "" {
		ct := s.contentType
		resp.ContentType = &ct
	}
	return resp, nil
}

func TestAzureAppConfigResolve(t *testing.T) {
	fake := newFakeAppConfig()
	fake.set("db/port", "", "5432")
	p := newAppConfigWithClient(fake)

	ref, err := mamori.ParseRef("azure-appconfig://mystore/db/port")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := string(v.Bytes); got != "5432" {
		t.Errorf("Bytes = %q, want %q", got, "5432")
	}
	if v.Sensitive {
		t.Error("Sensitive = true, want false: App Configuration is not a secret store")
	}
	if v.Version == "" {
		t.Error("Version is empty, want the setting's ETag")
	}
}

// TestAzureAppConfigLabelIsNotWildcard asserts that an absent ?label= selects
// the null label rather than matching any label. The two are distinct settings
// in the service, and conflating them would silently return the wrong value.
func TestAzureAppConfigLabelIsNotWildcard(t *testing.T) {
	fake := newFakeAppConfig()
	fake.set("db/port", "prod", "5432")
	p := newAppConfigWithClient(fake)

	ref, err := mamori.ParseRef("azure-appconfig://mystore/db/port")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if _, err := p.Resolve(context.Background(), ref); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("Resolve without a label found the 'prod'-labelled setting; error = %v, want errors.Is(ErrNotFound)", err)
	}

	labelled, err := mamori.ParseRef("azure-appconfig://mystore/db/port?label=prod")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(context.Background(), labelled)
	if err != nil {
		t.Fatalf("Resolve with ?label=prod: %v", err)
	}
	if got := string(v.Bytes); got != "5432" {
		t.Errorf("Bytes = %q, want %q", got, "5432")
	}
}

// TestAzureAppConfigKeyVaultReferenceRejected covers the deliberate choice to
// fail rather than return the reference JSON. A caller whose field is a
// password would otherwise receive the literal text {"uri":"..."}, which
// validates as a non-empty string and reaches a database driver, surfacing as
// an auth failure far from its cause.
func TestAzureAppConfigKeyVaultReferenceRejected(t *testing.T) {
	fake := newFakeAppConfig()
	fake.setTyped("db/password", "", `{"uri":"https://myvault.vault.azure.net/secrets/dbpass"}`,
		"application/vnd.microsoft.appconfig.keyvaultref+json")
	p := newAppConfigWithClient(fake)

	ref, err := mamori.ParseRef("azure-appconfig://mystore/db/password")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatalf("Resolve returned %q, want an error: a Key Vault reference must never be applied as a value", v.Bytes)
	}
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Errorf("error %v does not satisfy errors.Is(ErrInvalid)", err)
	}
	if !strings.Contains(err.Error(), "azure-kv://") {
		t.Errorf("error %q does not name the azure-kv:// ref the user should write instead", err)
	}
}

func TestAzureAppConfigNotFound(t *testing.T) {
	fake := newFakeAppConfig()
	p := newAppConfigWithClient(fake)

	ref, err := mamori.ParseRef("azure-appconfig://mystore/nope")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if _, err := p.Resolve(context.Background(), ref); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("Resolve error = %v, want errors.Is(ErrNotFound)", err)
	}
}

func TestConformanceAzureAppConfig(t *testing.T) {
	fake := newFakeAppConfig()
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return newAppConfigWithClient(fake) },
		Ref: func(key string) string { return SchemeAppConfig + "://mystore/" + key },
		PointerRef: func(key, frag string) string {
			return SchemeAppConfig + "://mystore/" + key + frag
		},
		Seed:   func(_ context.Context, key, val string) error { fake.set(key, "", val); return nil },
		Mutate: func(_ context.Context, key, val string) error { fake.set(key, "", val); return nil },
		Fail:   func(_ context.Context, key string, err error) error { fake.fail(key, err); return nil },
		Clear:  func(_ context.Context, key string) error { fake.clear(key); return nil },
	})
}
```

Add `"strings"` to the imports.

- [ ] **Step 3: Run to confirm failure**

```bash
cd providers/azure && GOWORK=off go test -run 'TestAzureAppConfig|TestConformanceAzureAppConfig' ./...
```

Expected: FAIL, `undefined: newAppConfigWithClient`.

- [ ] **Step 4: Implement the provider**

Create `providers/azure/appconfig.go`:

```go
package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azappconfig"
	"github.com/xavidop/mamori"
)

// SchemeAppConfig is the URL scheme handled by AppConfigProvider.
const SchemeAppConfig = "azure-appconfig"

// keyVaultRefContentType marks a setting whose value is a reference to a Key
// Vault secret rather than the value itself.
const keyVaultRefContentType = "application/vnd.microsoft.appconfig.keyvaultref+json"

// appConfigClient is the minimal subset of *azappconfig.Client this provider
// needs. The real SDK client satisfies it; tests inject an in-memory fake.
type appConfigClient interface {
	GetSetting(ctx context.Context, key string, opts *azappconfig.GetSettingOptions) (azappconfig.GetSettingResponse, error)
}

// AppConfigProvider resolves
// azure-appconfig://<store>/<key>[#json-key][?label=<label>] refs against
// Azure App Configuration. One provider serves every store named in a ref,
// lazily building and caching a client per store name, so construction never
// performs I/O and init-time registration is safe without Azure credentials
// present.
//
// AppConfigProvider is safe for concurrent use.
type AppConfigProvider struct {
	mu sync.Mutex

	clients map[string]appConfigClient
	fixed   appConfigClient

	cred         azcore.TokenCredential
	credErr      error
	credResolved bool

	newCredential func() (azcore.TokenCredential, error)
	newClient     func(endpoint string, cred azcore.TokenCredential) (appConfigClient, error)
}

// AppConfigOption configures an AppConfigProvider. It is distinct from Option,
// which configures the Key Vault provider in this same package.
type AppConfigOption func(*AppConfigProvider)

// WithAppConfigCredential injects an explicit token credential instead of the
// default Azure credential chain.
func WithAppConfigCredential(cred azcore.TokenCredential) AppConfigOption {
	return func(p *AppConfigProvider) {
		p.newCredential = func() (azcore.TokenCredential, error) { return cred, nil }
	}
}

// WithAppConfigClient injects a client used for every store, bypassing
// credential and client construction. It is primarily intended for tests.
func WithAppConfigClient(c appConfigClient) AppConfigOption {
	return func(p *AppConfigProvider) { p.fixed = c }
}

// NewAppConfig constructs an Azure App Configuration provider. With no options
// it uses the Azure default credential chain and builds a real azappconfig
// client per store lazily on first Resolve.
func NewAppConfig(opts ...AppConfigOption) *AppConfigProvider {
	p := &AppConfigProvider{
		clients: map[string]appConfigClient{},
		newCredential: func() (azcore.TokenCredential, error) {
			return azidentity.NewDefaultAzureCredential(nil)
		},
	}
	p.newClient = func(endpoint string, cred azcore.TokenCredential) (appConfigClient, error) {
		return azappconfig.NewClient(endpoint, cred, nil)
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// newAppConfigWithClient returns a provider backed by a caller-supplied
// client. It is the injection seam used by tests.
func newAppConfigWithClient(c appConfigClient) *AppConfigProvider {
	return NewAppConfig(WithAppConfigClient(c))
}

// Compile-time interface check.
var _ mamori.Provider = (*AppConfigProvider)(nil)

// Scheme returns "azure-appconfig".
func (p *AppConfigProvider) Scheme() string { return SchemeAppConfig }

// Resolve fetches a setting from Azure App Configuration. The ref path is
// "<store>/<key>", where the key may itself contain slashes. A #json-key
// fragment selects a field from a JSON payload, and ?label=<label> selects a
// labelled setting.
func (p *AppConfigProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	store, key, ok := strings.Cut(ref.Path, "/")
	if !ok || store == "" || key == "" {
		return mamori.Value{}, fmt.Errorf(
			"azure-appconfig: ref %q must be azure-appconfig://<store>/<key>[#json-key][?label=l]: %w",
			ref.Raw, mamori.ErrInvalid)
	}

	client, err := p.clientFor(store)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("azure-appconfig: building client for store %q: %w", store, err)
	}

	// An absent ?label= means Azure's null label, which is a distinct setting
	// from any labelled one, not a wildcard. Passing the empty string
	// explicitly requests that null label.
	label := ref.Opt("label")
	opts := &azappconfig.GetSettingOptions{Label: &label}

	resp, err := client.GetSetting(ctx, key, opts)
	if err != nil {
		if isNotFound(err) {
			return mamori.Value{}, fmt.Errorf(
				"azure-appconfig: setting %q in store %q not found: %w: %w",
				key, store, mamori.ErrNotFound, err)
		}
		return mamori.Value{}, fmt.Errorf("azure-appconfig: resolve %q: %w", ref.Path, classifyAzure(err))
	}
	if resp.Value == nil {
		return mamori.Value{}, fmt.Errorf(
			"azure-appconfig: setting %q in store %q has no value: %w", key, store, mamori.ErrNotFound)
	}

	// A Key Vault reference is a pointer to a secret, not the secret. Returning
	// its JSON would hand the caller {"uri":"..."} as their value, which passes
	// a non-empty-string validation and fails much later inside whatever
	// consumes it. Fail here instead, naming the ref they should have written.
	if resp.ContentType != nil && strings.HasPrefix(*resp.ContentType, keyVaultRefContentType) {
		return mamori.Value{}, fmt.Errorf(
			"azure-appconfig: setting %q in store %q is a Key Vault reference, which this provider does not resolve; %s: %w",
			key, store, keyVaultHint(*resp.Value), mamori.ErrInvalid)
	}

	data := []byte(*resp.Value)
	if ref.Key != "" {
		data, err = mamori.SelectKey(data, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}

	version := ""
	if resp.ETag != nil {
		version = string(*resp.ETag)
	}
	if version == "" {
		version = mamori.VersionHash(data)
	}

	meta := map[string]string{"store": store}
	if label != "" {
		meta["label"] = label
	}

	return mamori.Value{
		Bytes:     data,
		Version:   version,
		Sensitive: false, // a configuration service, not a secret store
		Metadata:  meta,
	}, nil
}

// keyVaultHint turns a Key Vault reference payload into the azure-kv:// ref the
// user should write instead. It degrades to generic advice when the payload is
// not the shape the service documents, since this only ever builds an error
// message and must never itself fail.
func keyVaultHint(value string) string {
	var ref struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal([]byte(value), &ref); err != nil || ref.URI == "" {
		return "use an azure-kv:// ref for the referenced secret"
	}
	// https://<vault>.vault.azure.net/secrets/<name>
	rest, ok := strings.CutPrefix(ref.URI, "https://")
	if !ok {
		return "use an azure-kv:// ref for the referenced secret"
	}
	host, path, ok := strings.Cut(rest, "/")
	if !ok {
		return "use an azure-kv:// ref for the referenced secret"
	}
	vault, _, _ := strings.Cut(host, ".")
	name := strings.TrimPrefix(path, "secrets/")
	if vault == "" || name == "" {
		return "use an azure-kv:// ref for the referenced secret"
	}
	return fmt.Sprintf("use azure-kv://%s/%s instead", vault, name)
}

// clientFor returns the client for the named store, creating and caching it on
// first use. When a fixed client was injected it is returned for every store.
func (p *AppConfigProvider) clientFor(store string) (appConfigClient, error) {
	if p.fixed != nil {
		return p.fixed, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if c, ok := p.clients[store]; ok {
		return c, nil
	}

	cred, err := p.credentialLocked()
	if err != nil {
		return nil, err
	}
	endpoint := "https://" + store + ".azconfig.io"
	c, err := p.newClient(endpoint, cred)
	if err != nil {
		return nil, err
	}
	p.clients[store] = c
	return c, nil
}

// credentialLocked resolves the token credential at most once. Callers must
// hold p.mu.
func (p *AppConfigProvider) credentialLocked() (azcore.TokenCredential, error) {
	if !p.credResolved {
		p.cred, p.credErr = p.newCredential()
		p.credResolved = true
	}
	return p.cred, p.credErr
}
```

- [ ] **Step 5: Register the provider**

In `providers/azure/azure.go`, change `init` to register both:

```go
func init() {
	mamori.Register(New())
	mamori.Register(NewAppConfig())
}
```

Update the package doc comment at the top of `azure.go` so it documents both
schemes rather than only `azure-kv://`, and note that App Configuration values
are not marked Sensitive while Key Vault values are.

- [ ] **Step 6: Run the tests**

```bash
cd providers/azure && GOWORK=off go test ./...
```

Expected: PASS.

- [ ] **Step 7: Prove the classification is wired into this resolve path**

Unlike Task 1, `classifyAzure` needs **no changes**: App Configuration returns
the same HTTP statuses as Key Vault, and `TestClassifyAzure` in
`providers/azure/errors_test.go` already covers all of them. Do not duplicate
that table.

What is unproven is whether this new provider's error path actually calls it.
Add one test, mirroring the existing `TestClassifyAzurePreservesResponseError`
in that file:

```go
func TestAzureAppConfigResolvePreservesClassification(t *testing.T) {
	fake := newFakeAppConfig()
	fake.set("k", "", "v")
	fake.fail("k", &azcore.ResponseError{StatusCode: http.StatusForbidden, ErrorCode: "Forbidden"})
	p := newAppConfigWithClient(fake)

	ref, err := mamori.ParseRef("azure-appconfig://mystore/k")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	_, err = p.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("Resolve returned nil error")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyAzure may not be wired into the App Configuration resolve path", got, mamori.KindPermissionDenied)
	}
}
```

Note the conventions: the kind type is `mamori.Kind` and the classifier is
`mamori.ErrorKind(err)`. There is no `ClassifyError`.

- [ ] **Step 8: Extend the registration tests**

`providers/azure/azure_test.go` has tests asserting the registered scheme.
Extend them to cover `SchemeAppConfig`, following the existing shape in that
file.

- [ ] **Step 9: Run the full module**

```bash
cd providers/azure && GOWORK=off go mod tidy && GOWORK=off go test ./... && GOWORK=off go test -race ./... && GOWORK=off go vet ./... && golangci-lint run
```

Expected: PASS all five. `golangci-lint run` is not optional: it is what CI
gates on, and it rejects unused test helpers that `go test` and `go vet`
accept. If you write a fake method you do not end up calling (`remove` is the
usual one), either delete it or give it a real test.

- [ ] **Step 10: Write the docs**

1. `providers/azure/README.md`: add an `azure-appconfig` section with the ref grammar, the null-label rule stated explicitly (an absent `?label=` is not a wildcard), `#json-key` selection, ETag versioning, auth, that values are not marked Sensitive, the Key Vault reference rejection and its rationale, and an `## Error classification` section.
2. `site/src/pages/docs/providers/azure-appconfig.md`: mirror it, matching the front matter and structure of the sibling `azure` page exactly; read it first.
3. Root `README.md`: add the row.
4. `site/src/pages/docs/providers/index.md`: add the row.

- [ ] **Step 11: Stage**

```bash
cd providers/azure && GOWORK=off go test ./... && cd ../.. && go build ./... && git add -A && git status --short
```

Report the staged file list. Do not commit.
