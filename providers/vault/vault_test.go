package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeVault is an in-memory implementation of kvReader for tests. It mimics KV
// v2 semantics: each write bumps the version, and reads carry version metadata
// plus an optional lease on the raw response.
type fakeVault struct {
	mu        sync.Mutex
	entries   map[string]*fakeEntry
	renewCall int // number of times Renew was invoked
}

type fakeEntry struct {
	data          map[string]interface{}
	version       int
	leaseID       string
	leaseDuration int
	renewable     bool
}

func newFakeVault() *fakeVault {
	return &fakeVault{entries: map[string]*fakeEntry{}}
}

func fakeKey(mount, path string) string { return mount + "/" + path }

// setSecret writes/overwrites a secret, bumping its version like KV v2.
func (f *fakeVault) setSecret(mount, path string, data map[string]interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := fakeKey(mount, path)
	e := f.entries[k]
	if e == nil {
		e = &fakeEntry{}
		f.entries[k] = e
	}
	e.data = data
	e.version++
}

// setLease attaches lease info to an existing secret's raw response.
func (f *fakeVault) setLease(mount, path, leaseID string, seconds int, renewable bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.entries[fakeKey(mount, path)]
	if e == nil {
		return
	}
	e.leaseID = leaseID
	e.leaseDuration = seconds
	e.renewable = renewable
}

func (f *fakeVault) Get(ctx context.Context, mount, path string) (*vaultapi.KVSecret, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.entries[fakeKey(mount, path)]
	if e == nil {
		return nil, fmt.Errorf("%w: at %s/data/%s", vaultapi.ErrSecretNotFound, mount, path)
	}
	// Copy the data map so callers cannot mutate our store.
	data := make(map[string]interface{}, len(e.data))
	for k, v := range e.data {
		data[k] = v
	}
	kv := &vaultapi.KVSecret{
		Data:            data,
		VersionMetadata: &vaultapi.KVVersionMetadata{Version: e.version},
	}
	if e.leaseDuration > 0 || e.leaseID != "" {
		kv.Raw = &vaultapi.Secret{
			LeaseID:       e.leaseID,
			LeaseDuration: e.leaseDuration,
			Renewable:     e.renewable,
			Data:          data,
		}
	}
	return kv, nil
}

func (f *fakeVault) Renew(ctx context.Context, leaseID string, increment int) (*vaultapi.Secret, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewCall++
	return &vaultapi.Secret{LeaseID: leaseID, LeaseDuration: increment, Renewable: true}, nil
}

// --- conformance -----------------------------------------------------------

func TestConformance(t *testing.T) {
	fake := newFakeVault()
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return newWithReader(fake) },
		// The conformance kit seeds a single scalar per key; we store it under the
		// "value" field and select it with #value so Bytes is exactly that scalar.
		Ref: func(key string) string { return "vault://secret/" + key + "#value" },
		Seed: func(_ context.Context, key, val string) error {
			fake.setSecret("secret", key, map[string]interface{}{"value": val})
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			fake.setSecret("secret", key, map[string]interface{}{"value": val})
			return nil
		},
	})
}

// --- unit tests ------------------------------------------------------------

func TestResolveWholeDataMapAsJSON(t *testing.T) {
	fake := newFakeVault()
	fake.setSecret("secret", "myapp/config", map[string]interface{}{
		"username": "admin",
		"password": "s3cr3t",
	})
	p := newWithReader(fake)

	ref := mustRef(t, "vault://secret/myapp/config")
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Error("expected Sensitive=true for a Vault secret")
	}
	if v.Version != "1" {
		t.Errorf("Version = %q, want \"1\"", v.Version)
	}
	var got map[string]string
	if err := json.Unmarshal(v.Bytes, &got); err != nil {
		t.Fatalf("payload is not a JSON object: %v (%s)", err, v.Bytes)
	}
	if got["username"] != "admin" || got["password"] != "s3cr3t" {
		t.Errorf("payload = %s, want the full data map", v.Bytes)
	}
	if !v.NotAfter.IsZero() {
		t.Errorf("NotAfter = %v, want zero for a leaseless KV secret", v.NotAfter)
	}
}

func TestResolveWithKey(t *testing.T) {
	fake := newFakeVault()
	fake.setSecret("secret", "myapp/config", map[string]interface{}{
		"username": "admin",
		"password": "s3cr3t",
	})
	p := newWithReader(fake)

	v, err := p.Resolve(context.Background(), mustRef(t, "vault://secret/myapp/config#password"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "s3cr3t" {
		t.Errorf("Bytes = %q, want s3cr3t", v.Bytes)
	}
}

func TestResolveMissingKeyIsNotFound(t *testing.T) {
	fake := newFakeVault()
	fake.setSecret("secret", "myapp/config", map[string]interface{}{"username": "admin"})
	p := newWithReader(fake)

	_, err := p.Resolve(context.Background(), mustRef(t, "vault://secret/myapp/config#nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for absent key", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	p := newWithReader(newFakeVault())
	_, err := p.Resolve(context.Background(), mustRef(t, "vault://secret/does/not/exist"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveLeaseSetsNotAfter(t *testing.T) {
	fake := newFakeVault()
	fake.setSecret("database", "creds/readonly", map[string]interface{}{
		"username": "v-token-abcd",
		"password": "pw",
	})
	fake.setLease("database", "creds/readonly", "database/creds/readonly/lease-id", 3600, true)
	p := newWithReader(fake)

	before := time.Now()
	v, err := p.Resolve(context.Background(), mustRef(t, "vault://database/creds/readonly"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.NotAfter.IsZero() {
		t.Fatal("NotAfter is zero; expected lease-derived expiry")
	}
	// NotAfter should be roughly now + 3600s.
	wantMin := before.Add(3600 * time.Second)
	wantMax := time.Now().Add(3601 * time.Second)
	if v.NotAfter.Before(wantMin) || v.NotAfter.After(wantMax) {
		t.Errorf("NotAfter = %v, want ~%v", v.NotAfter, wantMin)
	}
	if fake.renewCall != 0 {
		t.Errorf("Renew called %d times without ?renew=true", fake.renewCall)
	}
}

func TestResolveRenewLease(t *testing.T) {
	fake := newFakeVault()
	fake.setSecret("database", "creds/readonly", map[string]interface{}{"password": "pw"})
	fake.setLease("database", "creds/readonly", "database/creds/readonly/lease-id", 1800, true)
	p := newWithReader(fake)

	v, err := p.Resolve(context.Background(), mustRef(t, "vault://database/creds/readonly?renew=true"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if fake.renewCall != 1 {
		t.Errorf("Renew called %d times, want 1 with ?renew=true", fake.renewCall)
	}
	if v.Metadata["vault.renewed"] != "true" {
		t.Errorf("Metadata[vault.renewed] = %q, want true", v.Metadata["vault.renewed"])
	}
	if v.NotAfter.IsZero() {
		t.Error("NotAfter is zero after renew")
	}
}

func TestResolveKeyAndRenewCombined(t *testing.T) {
	fake := newFakeVault()
	fake.setSecret("database", "creds/readonly", map[string]interface{}{"password": "pw", "username": "u"})
	fake.setLease("database", "creds/readonly", "lease-id", 900, true)
	p := newWithReader(fake)

	// mamori grammar puts #key before ?opts.
	ref := mustRef(t, "vault://database/creds/readonly#password?renew=true")
	if ref.Key != "password" || ref.Opt("renew") != "true" {
		t.Fatalf("ref parsed unexpectedly: key=%q renew=%q", ref.Key, ref.Opt("renew"))
	}
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "pw" {
		t.Errorf("Bytes = %q, want pw", v.Bytes)
	}
	if fake.renewCall != 1 {
		t.Errorf("Renew called %d times, want 1", fake.renewCall)
	}
	if v.NotAfter.IsZero() {
		t.Error("NotAfter is zero; expected lease-derived expiry")
	}
}

func TestSplitMountPath(t *testing.T) {
	cases := []struct {
		in         string
		mount      string
		path       string
		wantErr    bool
	}{
		{in: "secret/myapp", mount: "secret", path: "myapp"},
		{in: "secret/a/b/c", mount: "secret", path: "a/b/c"},
		{in: "secret/data/myapp", mount: "secret", path: "myapp"}, // /data/ tolerated
		{in: "/secret/myapp/", mount: "secret", path: "myapp"},
		{in: "secret/data/", mount: "secret", path: "data"}, // trailing slash trimmed -> secret named "data"
		{in: "secret", wantErr: true},
		{in: "", wantErr: true},
		{in: "/", wantErr: true},
	}
	for _, tc := range cases {
		m, pth, err := splitMountPath(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("splitMountPath(%q) = (%q,%q,nil), want error", tc.in, m, pth)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitMountPath(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if m != tc.mount || pth != tc.path {
			t.Errorf("splitMountPath(%q) = (%q,%q), want (%q,%q)", tc.in, m, pth, tc.mount, tc.path)
		}
	}
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != "vault" {
		t.Errorf("Scheme() = %q, want vault", got)
	}
}

func mustRef(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}
