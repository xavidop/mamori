package aws

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// ---------------------------------------------------------------------------
// In-memory fake for Secrets Manager.
// ---------------------------------------------------------------------------

type fakeSecret struct {
	value     string
	binary    []byte
	versionID string
}

type fakeSM struct {
	mu      sync.Mutex
	secrets map[string]fakeSecret
	counter int
}

func newFakeSM() *fakeSM {
	return &fakeSM{secrets: map[string]fakeSecret{}}
}

// set stores a string secret, assigning a fresh (globally unique) version id.
func (f *fakeSM) set(id, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counter++
	f.secrets[id] = fakeSecret{value: val, versionID: fmt.Sprintf("v%d", f.counter)}
}

// setBinary stores a binary secret (no SecretString).
func (f *fakeSM) setBinary(id string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counter++
	f.secrets[id] = fakeSecret{binary: b, versionID: fmt.Sprintf("v%d", f.counter)}
}

func (f *fakeSM) GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := awssdk.ToString(in.SecretId)
	s, ok := f.secrets[id]
	if !ok {
		return nil, &smtypes.ResourceNotFoundException{Message: awssdk.String("Secrets Manager can't find the specified secret.")}
	}
	out := &secretsmanager.GetSecretValueOutput{
		Name:      awssdk.String(id),
		VersionId: awssdk.String(s.versionID),
	}
	if s.binary != nil {
		out.SecretBinary = s.binary
	} else {
		out.SecretString = awssdk.String(s.value)
	}
	return out, nil
}

func (f *fakeSM) BatchGetSecretValue(ctx context.Context, in *secretsmanager.BatchGetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.BatchGetSecretValueOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &secretsmanager.BatchGetSecretValueOutput{}
	for _, id := range in.SecretIdList {
		s, ok := f.secrets[id]
		if !ok {
			out.Errors = append(out.Errors, smtypes.APIErrorType{
				SecretId:  awssdk.String(id),
				ErrorCode: awssdk.String("ResourceNotFoundException"),
				Message:   awssdk.String("secret not found"),
			})
			continue
		}
		entry := smtypes.SecretValueEntry{
			Name:      awssdk.String(id),
			VersionId: awssdk.String(s.versionID),
		}
		if s.binary != nil {
			entry.SecretBinary = s.binary
		} else {
			entry.SecretString = awssdk.String(s.value)
		}
		out.SecretValues = append(out.SecretValues, entry)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// In-memory fake for SSM Parameter Store.
// ---------------------------------------------------------------------------

type fakeParam struct {
	value   string
	version int64
	secure  bool
}

type fakeSSM struct {
	mu     sync.Mutex
	params map[string]fakeParam
}

func newFakeSSM() *fakeSSM {
	return &fakeSSM{params: map[string]fakeParam{}}
}

// set stores/overwrites a String parameter, incrementing its version.
func (f *fakeSSM) set(name, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.params[name]
	p.value = val
	p.version++
	f.params[name] = p
}

// setSecure stores/overwrites a SecureString parameter, incrementing its version.
func (f *fakeSSM) setSecure(name, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.params[name]
	p.value = val
	p.secure = true
	p.version++
	f.params[name] = p
}

func (f *fakeSSM) paramType(secure bool) ssmtypes.ParameterType {
	if secure {
		return ssmtypes.ParameterTypeSecureString
	}
	return ssmtypes.ParameterTypeString
}

func (f *fakeSSM) GetParameter(ctx context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := awssdk.ToString(in.Name)
	p, ok := f.params[name]
	if !ok {
		return nil, &ssmtypes.ParameterNotFound{Message: awssdk.String("parameter not found")}
	}
	return &ssm.GetParameterOutput{
		Parameter: &ssmtypes.Parameter{
			Name:    awssdk.String(name),
			Value:   awssdk.String(p.value),
			Version: p.version,
			Type:    f.paramType(p.secure),
		},
	}, nil
}

func (f *fakeSSM) GetParameters(ctx context.Context, in *ssm.GetParametersInput, _ ...func(*ssm.Options)) (*ssm.GetParametersOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &ssm.GetParametersOutput{}
	for _, name := range in.Names {
		p, ok := f.params[name]
		if !ok {
			out.InvalidParameters = append(out.InvalidParameters, name)
			continue
		}
		out.Parameters = append(out.Parameters, ssmtypes.Parameter{
			Name:    awssdk.String(name),
			Value:   awssdk.String(p.value),
			Version: p.version,
			Type:    f.paramType(p.secure),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

func mustParse(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

// ---------------------------------------------------------------------------
// Conformance kit - one run per scheme against the in-memory fakes.
// ---------------------------------------------------------------------------

func TestConformanceSecretsManager(t *testing.T) {
	fake := newFakeSM()
	providertest.Run(t, providertest.Config{
		New:    func() mamori.Provider { return newSMWithClient(fake) },
		Ref:    func(key string) string { return schemeSM + "://" + key },
		Seed:   func(_ context.Context, key, val string) error { fake.set(key, val); return nil },
		Mutate: func(_ context.Context, key, val string) error { fake.set(key, val); return nil },
	})
}

func TestConformanceParameterStore(t *testing.T) {
	fake := newFakeSSM()
	providertest.Run(t, providertest.Config{
		New:    func() mamori.Provider { return newPSWithClient(fake) },
		Ref:    func(key string) string { return schemePS + "://" + key },
		Seed:   func(_ context.Context, key, val string) error { fake.set(key, val); return nil },
		Mutate: func(_ context.Context, key, val string) error { fake.set(key, val); return nil },
	})
}

// ---------------------------------------------------------------------------
// Registration.
// ---------------------------------------------------------------------------

func TestRegisteredSchemes(t *testing.T) {
	got := map[string]bool{}
	for _, s := range mamori.RegisteredSchemes() {
		got[s] = true
	}
	for _, want := range []string{schemeSM, schemePS} {
		if !got[want] {
			t.Errorf("scheme %q was not registered by init()", want)
		}
	}
}

func TestConstructorSchemes(t *testing.T) {
	if s := NewSecretsManager().Scheme(); s != schemeSM {
		t.Errorf("SMProvider.Scheme() = %q, want %q", s, schemeSM)
	}
	if s := NewParameterStore(WithRegion("eu-west-1")).Scheme(); s != schemePS {
		t.Errorf("PSProvider.Scheme() = %q, want %q", s, schemePS)
	}
}

// ---------------------------------------------------------------------------
// Secrets Manager unit tests.
// ---------------------------------------------------------------------------

func TestSMResolveString(t *testing.T) {
	fake := newFakeSM()
	fake.set("prod/db", "s3cr3t")
	p := newSMWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "aws-sm://prod/db"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "s3cr3t" {
		t.Errorf("Bytes = %q, want s3cr3t", v.Bytes)
	}
	if !v.Sensitive {
		t.Error("Secrets Manager value must be Sensitive")
	}
	if v.Version == "" {
		t.Error("Version must be set (the VersionId)")
	}
}

func TestSMResolveJSONKey(t *testing.T) {
	fake := newFakeSM()
	fake.set("prod/db", `{"username":"neo","password":"trinity"}`)
	p := newSMWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "aws-sm://prod/db#password"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "trinity" {
		t.Errorf("Bytes = %q, want trinity", v.Bytes)
	}
}

func TestSMResolveBinaryFallback(t *testing.T) {
	fake := newFakeSM()
	fake.setBinary("prod/cert", []byte{0x00, 0x01, 0x02, 0xff})
	p := newSMWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "aws-sm://prod/cert"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != string([]byte{0x00, 0x01, 0x02, 0xff}) {
		t.Errorf("Bytes = %v, want binary payload", v.Bytes)
	}
}

func TestSMNotFound(t *testing.T) {
	p := newSMWithClient(newFakeSM())
	_, err := p.Resolve(context.Background(), mustParse(t, "aws-sm://missing"))
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

func TestSMBatch(t *testing.T) {
	fake := newFakeSM()
	fake.set("a", "AAA")
	fake.set("b", `{"k":"BBB"}`)
	p := newSMWithClient(fake)

	refs := []mamori.Ref{
		mustParse(t, "aws-sm://a"),
		mustParse(t, "aws-sm://b#k"),
		mustParse(t, "aws-sm://missing"), // not found -> omitted
	}
	got, err := p.ResolveBatch(context.Background(), refs)
	if err != nil {
		t.Fatalf("ResolveBatch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (missing omitted): %v", len(got), keys(got))
	}
	if string(got["aws-sm://a"].Bytes) != "AAA" {
		t.Errorf("a = %q, want AAA", got["aws-sm://a"].Bytes)
	}
	if string(got["aws-sm://b#k"].Bytes) != "BBB" {
		t.Errorf("b#k = %q, want BBB", got["aws-sm://b#k"].Bytes)
	}
	if _, ok := got["aws-sm://missing"]; ok {
		t.Error("missing secret must be omitted from the batch result")
	}
}

// ---------------------------------------------------------------------------
// Parameter Store unit tests.
// ---------------------------------------------------------------------------

func TestPSResolveString(t *testing.T) {
	fake := newFakeSSM()
	fake.set("/app/log-level", "debug")
	p := newPSWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "aws-ps:///app/log-level"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Errorf("Bytes = %q, want debug", v.Bytes)
	}
	if v.Sensitive {
		t.Error("String parameter must not be Sensitive")
	}
	if v.Version != "1" {
		t.Errorf("Version = %q, want 1", v.Version)
	}
}

func TestPSResolveSecureString(t *testing.T) {
	fake := newFakeSSM()
	fake.setSecure("/app/api-key", "topsecret")
	p := newPSWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "aws-ps:///app/api-key"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "topsecret" {
		t.Errorf("Bytes = %q, want topsecret", v.Bytes)
	}
	if !v.Sensitive {
		t.Error("SecureString parameter must be Sensitive")
	}
}

func TestPSResolveJSONKey(t *testing.T) {
	fake := newFakeSSM()
	fake.set("/app/creds", `{"host":"db.internal","port":"5432"}`)
	p := newPSWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "aws-ps:///app/creds#host"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "db.internal" {
		t.Errorf("Bytes = %q, want db.internal", v.Bytes)
	}
}

func TestPSNotFound(t *testing.T) {
	p := newPSWithClient(newFakeSSM())
	_, err := p.Resolve(context.Background(), mustParse(t, "aws-ps:///missing"))
	if err == nil {
		t.Fatal("expected error for missing parameter")
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

func TestPSBatch(t *testing.T) {
	fake := newFakeSSM()
	fake.set("/a", "AAA")
	fake.setSecure("/b", "BBB")
	p := newPSWithClient(fake)

	refs := []mamori.Ref{
		mustParse(t, "aws-ps:///a"),
		mustParse(t, "aws-ps:///b"),
		mustParse(t, "aws-ps:///missing"), // invalid -> omitted
	}
	got, err := p.ResolveBatch(context.Background(), refs)
	if err != nil {
		t.Fatalf("ResolveBatch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (missing omitted): %v", len(got), keys(got))
	}
	if string(got["aws-ps:///a"].Bytes) != "AAA" {
		t.Errorf("a = %q, want AAA", got["aws-ps:///a"].Bytes)
	}
	if !got["aws-ps:///b"].Sensitive {
		t.Error("SecureString parameter /b must be Sensitive in batch result")
	}
	if _, ok := got["aws-ps:///missing"]; ok {
		t.Error("missing parameter must be omitted from the batch result")
	}
}

// ---------------------------------------------------------------------------
// small test-local utilities (kept here to avoid extra imports in the file).
// ---------------------------------------------------------------------------

func keys(m map[string]mamori.Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
