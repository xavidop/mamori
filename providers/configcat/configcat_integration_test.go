//go:build integration

// Package configcat live integration test. It is NOT run by the standard
// `go test ./...` pass; it requires a real ConfigCat SDK key and a setting that
// exists in that config.
//
// Run it against a real ConfigCat config, e.g.:
//
//	export CONFIGCAT_SDK_KEY=configcat-sdk-1/xxxx/yyyy   # your SDK key
//	export CONFIGCAT_TEST_SETTING=isPOCFeatureEnabled    # a key that exists
//	go test -tags=integration -run TestLive ./...
package configcat

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/xavidop/mamori"
)

func TestLive(t *testing.T) {
	sdkKey := os.Getenv("CONFIGCAT_SDK_KEY")
	setting := os.Getenv("CONFIGCAT_TEST_SETTING")
	if sdkKey == "" || setting == "" {
		t.Skip("set CONFIGCAT_SDK_KEY and CONFIGCAT_TEST_SETTING to run the live test")
	}

	p := New() // SDK key read from CONFIGCAT_SDK_KEY lazily
	defer p.Close()

	ref, err := mamori.ParseRef("configcat://" + setting)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("live Resolve: %v", err)
	}
	if len(v.Bytes) == 0 {
		t.Error("live setting resolved to empty bytes")
	}
	if v.Version == "" {
		t.Error("live setting has empty Version")
	}
	if v.Sensitive {
		t.Error("feature flag must not be marked Sensitive")
	}
	t.Logf("resolved %s: %q (version=%s)", setting, v.Bytes, v.Version)

	// A non-existent setting must be ErrNotFound, not the SDK default.
	miss, _ := mamori.ParseRef("configcat://___definitely_missing_setting___")
	if _, err := p.Resolve(context.Background(), miss); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("missing setting error = %v, want ErrNotFound", err)
	}
}
