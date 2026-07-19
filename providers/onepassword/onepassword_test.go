package onepassword

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// Fixed vault/item that the conformance kit's logical keys map onto: each key
// becomes a field (label == key) on this single item, and Ref(key) produces
// op://<confVaultName>/<confItemTitle>/<key>.
const (
	confVaultID    = "v-conformance"
	confVaultName  = "conf-vault"
	confItemID     = "i-conformance"
	confItemTitle  = "conf-item"
	testToken      = "test-token"
	testHostMarker = "http://connect.test"
)

// --- in-memory 1Password Connect emulation ----------------------------------

type fakeItem struct {
	id      string
	title   string
	version int
	fields  map[string]string // label -> value
}

type fakeVault struct {
	id    string
	name  string
	items map[string]*fakeItem // by item id
}

// fakeConnect emulates the subset of the 1Password Connect REST API this
// provider uses. It implements http.Handler so the same emulation can back both
// an httptest.Server (unit tests) and an in-memory RoundTripper (conformance).
type fakeConnect struct {
	mu     sync.Mutex
	vaults map[string]*fakeVault // by vault id
}

func newFakeConnect() *fakeConnect {
	return &fakeConnect{
		vaults: map[string]*fakeVault{
			confVaultID: {
				id:   confVaultID,
				name: confVaultName,
				items: map[string]*fakeItem{
					confItemID: {
						id:      confItemID,
						title:   confItemTitle,
						version: 0,
						fields:  map[string]string{},
					},
				},
			},
		},
	}
}

// set writes label=val on the fixed conformance item and bumps its version,
// mirroring how 1Password increments an item's version on any edit.
func (f *fakeConnect) set(label, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it := f.vaults[confVaultID].items[confItemID]
	it.fields[label] = val
	it.version++
}

func (f *fakeConnect) vaultByName(name string) *fakeVault {
	for _, v := range f.vaults {
		if v.name == name {
			return v
		}
	}
	return nil
}

func (f *fakeConnect) itemByTitle(v *fakeVault, title string) *fakeItem {
	for _, it := range v.items {
		if it.title == title {
			return it
		}
	}
	return nil
}

func (f *fakeConnect) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// Expected forms (all prefixed with "v1"):
	//   v1/vaults                       (?filter=name eq "X")
	//   v1/vaults/{vid}
	//   v1/vaults/{vid}/items           (?filter=title eq "Y")
	//   v1/vaults/{vid}/items/{iid}
	switch {
	case len(parts) == 2 && parts[0] == "v1" && parts[1] == "vaults":
		name := filterValue(r.URL.Query().Get("filter"))
		var out []vaultSummary
		if v := f.vaultByName(name); v != nil {
			out = append(out, vaultSummary{ID: v.id, Name: v.name})
		}
		writeJSON(w, http.StatusOK, out)

	case len(parts) == 3 && parts[0] == "v1" && parts[1] == "vaults":
		v, ok := f.vaults[parts[2]]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "vault not found"})
			return
		}
		writeJSON(w, http.StatusOK, vaultSummary{ID: v.id, Name: v.name})

	case len(parts) == 4 && parts[0] == "v1" && parts[1] == "vaults" && parts[3] == "items":
		v, ok := f.vaults[parts[2]]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "vault not found"})
			return
		}
		title := filterValue(r.URL.Query().Get("filter"))
		var out []itemSummary
		if it := f.itemByTitle(v, title); it != nil {
			out = append(out, itemSummary{ID: it.id, Title: it.title, Version: it.version})
		}
		writeJSON(w, http.StatusOK, out)

	case len(parts) == 5 && parts[0] == "v1" && parts[1] == "vaults" && parts[3] == "items":
		v, ok := f.vaults[parts[2]]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "vault not found"})
			return
		}
		it, ok := v.items[parts[4]]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "item not found"})
			return
		}
		full := item{ID: it.id, Title: it.title, Version: it.version}
		for label, val := range it.fields {
			full.Fields = append(full.Fields, itemField{ID: "f-" + label, Label: label, Value: val})
		}
		writeJSON(w, http.StatusOK, full)

	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "not found"})
	}
}

func filterValue(filter string) string {
	i := strings.IndexByte(filter, '"')
	j := strings.LastIndexByte(filter, '"')
	if i < 0 || j <= i {
		return ""
	}
	return filter[i+1 : j]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// recorderRT serves requests against an http.Handler entirely in memory (via
// httptest.NewRecorder), spawning no goroutines and opening no sockets, so the
// conformance kit's goroutine-leak check stays clean.
type recorderRT struct{ h http.Handler }

func (rt recorderRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	rec := httptest.NewRecorder()
	rt.h.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// --- unit tests --------------------------------------------------------------

func newTestProvider(t *testing.T, fake *fakeConnect) *Provider {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return New(WithHost(srv.URL), WithToken(testToken), WithHTTPClient(srv.Client()))
}

func TestResolveField(t *testing.T) {
	fake := newFakeConnect()
	fake.set("password", "s3cr3t")
	p := newTestProvider(t, fake)

	ref, err := mamori.ParseRef("op://" + confVaultName + "/" + confItemTitle + "/password")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "s3cr3t" {
		t.Fatalf("Bytes = %q, want s3cr3t", v.Bytes)
	}
	if !v.Sensitive {
		t.Error("expected Sensitive = true")
	}
	if v.Version == "" {
		t.Error("expected a non-empty Version")
	}
	if v.Metadata["field"] != "password" {
		t.Errorf("Metadata[field] = %q, want password", v.Metadata["field"])
	}
}

func TestResolveByFieldID(t *testing.T) {
	fake := newFakeConnect()
	fake.set("api-key", "abc123")
	p := newTestProvider(t, fake)

	// The fake assigns field id "f-<label>"; resolve by id rather than label.
	ref, err := mamori.ParseRef("op://" + confVaultName + "/" + confItemTitle + "/f-api-key")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve by field id: %v", err)
	}
	if string(v.Bytes) != "abc123" {
		t.Fatalf("Bytes = %q, want abc123", v.Bytes)
	}
}

func TestResolveVaultByID(t *testing.T) {
	fake := newFakeConnect()
	fake.set("token", "xyz")
	p := newTestProvider(t, fake)

	// Reference the vault by its id (name filter misses, direct GET hits).
	ref, err := mamori.ParseRef("op://" + confVaultID + "/" + confItemTitle + "/token")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve with vault id: %v", err)
	}
	if string(v.Bytes) != "xyz" {
		t.Fatalf("Bytes = %q, want xyz", v.Bytes)
	}
}

func TestNotFound(t *testing.T) {
	fake := newFakeConnect()
	fake.set("password", "s3cr3t")
	p := newTestProvider(t, fake)

	cases := map[string]string{
		"missing vault": "op://nope/" + confItemTitle + "/password",
		"missing item":  "op://" + confVaultName + "/nope/password",
		"missing field": "op://" + confVaultName + "/" + confItemTitle + "/nope",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			ref, err := mamori.ParseRef(raw)
			if err != nil {
				t.Fatalf("ParseRef: %v", err)
			}
			_, err = p.Resolve(context.Background(), ref)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, mamori.ErrNotFound) {
				t.Fatalf("error %v is not ErrNotFound", err)
			}
		})
	}
}

func TestBadRef(t *testing.T) {
	fake := newFakeConnect()
	p := newTestProvider(t, fake)

	// Only two path segments - not a valid op:// field reference.
	ref, err := mamori.ParseRef("op://vault/item")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if _, err := p.Resolve(context.Background(), ref); err == nil {
		t.Fatal("expected an error for a malformed op:// ref")
	}
}

func TestMissingHostToken(t *testing.T) {
	// Ensure ambient env does not accidentally satisfy the provider.
	t.Setenv(envHost, "")
	t.Setenv(envToken, "")

	ref, err := mamori.ParseRef("op://" + confVaultName + "/" + confItemTitle + "/password")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

	p := New(WithToken(testToken)) // no host
	if _, err := p.Resolve(context.Background(), ref); err == nil {
		t.Error("expected error when host is unset")
	}

	p = New(WithHost(testHostMarker)) // no token
	if _, err := p.Resolve(context.Background(), ref); err == nil {
		t.Error("expected error when token is unset")
	}
}

// --- conformance -------------------------------------------------------------

func TestConformance(t *testing.T) {
	fake := newFakeConnect()
	client := &http.Client{Transport: recorderRT{h: fake}}

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider {
			return New(WithHost(testHostMarker), WithToken(testToken), WithHTTPClient(client))
		},
		Ref: func(key string) string {
			return "op://" + confVaultName + "/" + confItemTitle + "/" + key
		},
		Seed: func(_ context.Context, key, val string) error {
			fake.set(key, val)
			return nil
		},
		Mutate: func(_ context.Context, key, val string) error {
			fake.set(key, val)
			return nil
		},
		SkipWatch: true,
	})
}
