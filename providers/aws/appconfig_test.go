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
	"github.com/aws/smithy-go"
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

// ---------------------------------------------------------------------------
// Conformance kit.
// ---------------------------------------------------------------------------

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
		Clear:  func(_ context.Context, key string) error { fake.clear(prefix + key); return nil },
	})
}
