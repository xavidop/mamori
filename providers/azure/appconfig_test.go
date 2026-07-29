package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
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

// TestAzureAppConfigResolveNotFoundAfterRemove exercises fakeAppConfig.remove,
// proving a setting that existed and was then deleted is reported not-found on
// the next resolve, distinct from TestAzureAppConfigNotFound (which never had
// a setting at all).
func TestAzureAppConfigResolveNotFoundAfterRemove(t *testing.T) {
	fake := newFakeAppConfig()
	fake.set("db/port", "", "5432")
	p := newAppConfigWithClient(fake)

	ref, err := mamori.ParseRef("azure-appconfig://mystore/db/port")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if _, err := p.Resolve(context.Background(), ref); err != nil {
		t.Fatalf("Resolve before remove: %v", err)
	}

	fake.remove("db/port", "")

	if _, err := p.Resolve(context.Background(), ref); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("Resolve after remove error = %v, want errors.Is(ErrNotFound)", err)
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
