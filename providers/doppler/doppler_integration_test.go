//go:build integration

// Package doppler live integration test. Not run by the standard `go test ./...`
// pass; it requires a real Doppler token and a pre-created secret.
//
// Run it against a real Doppler config, e.g.:
//
//	export DOPPLER_TOKEN=dp.st.xxxxxxxx           # service token
//	export DOPPLER_TEST_PROJECT=my-project
//	export DOPPLER_TEST_CONFIG=dev
//	export DOPPLER_TEST_SECRET=MY_SECRET_NAME     # must exist in that config
//	go test -tags=integration -run TestLive ./...
package doppler

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/xavidop/mamori"
)

func TestLive(t *testing.T) {
	token := os.Getenv("DOPPLER_TOKEN")
	project := os.Getenv("DOPPLER_TEST_PROJECT")
	config := os.Getenv("DOPPLER_TEST_CONFIG")
	secret := os.Getenv("DOPPLER_TEST_SECRET")
	if token == "" || project == "" || config == "" || secret == "" {
		t.Skip("set DOPPLER_TOKEN, DOPPLER_TEST_PROJECT, DOPPLER_TEST_CONFIG, DOPPLER_TEST_SECRET to run the live test")
	}

	p := New() // token read from DOPPLER_TOKEN lazily
	ref, err := mamori.ParseRef("doppler://" + project + "/" + config + "#" + secret)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("live Resolve: %v", err)
	}
	if len(v.Bytes) == 0 {
		t.Error("live secret resolved to empty bytes (is the secret set?)")
	}
	if v.Version == "" {
		t.Error("live secret has empty Version")
	}
	if !v.Sensitive {
		t.Error("live secret not marked Sensitive")
	}
	t.Logf("resolved %s/%s#%s: %d bytes, version=%s", project, config, secret, len(v.Bytes), v.Version)

	// A non-existent secret must be ErrNotFound.
	miss, _ := mamori.ParseRef("doppler://" + project + "/" + config + "#___definitely_missing___")
	if _, err := p.Resolve(context.Background(), miss); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("missing secret error = %v, want ErrNotFound", err)
	}
}
