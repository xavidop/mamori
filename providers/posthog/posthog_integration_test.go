//go:build integration

// Package posthog live integration tests evaluate a real feature flag against a
// real PostHog project and are excluded from the default build. Run them with:
//
//	export MAMORI_POSTHOG_PROJECT_API_KEY=phc_...
//	export MAMORI_POSTHOG_FLAG=new-checkout
//	export MAMORI_POSTHOG_HOST=https://eu.i.posthog.com  # optional, defaults to US Cloud
//	export MAMORI_POSTHOG_DISTINCT_ID=svc-billing        # optional
//	GOWORK=off go test -tags integration -run Integration ./...
//
// The project API key and the flag key are required; the tests skip if either
// is unset. Nothing here ever logs the key or a resolved flag value, only a
// byte count and the facet name.
package posthog

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// liveConfig reads the environment the integration tests require, skipping the
// calling test when the two required variables are unset.
func liveConfig(t *testing.T) (apiKey, flag, host, distinctID string) {
	t.Helper()
	apiKey = os.Getenv("MAMORI_POSTHOG_PROJECT_API_KEY")
	flag = os.Getenv("MAMORI_POSTHOG_FLAG")
	if apiKey == "" || flag == "" {
		t.Skip("set MAMORI_POSTHOG_PROJECT_API_KEY and MAMORI_POSTHOG_FLAG to run the live tests")
	}
	return apiKey, flag, os.Getenv("MAMORI_POSTHOG_HOST"), os.Getenv("MAMORI_POSTHOG_DISTINCT_ID")
}

// liveProvider builds a Provider against the nominated PostHog project.
func liveProvider(t *testing.T, apiKey, host, distinctID string) *Provider {
	t.Helper()
	opts := []Option{WithProjectAPIKey(apiKey)}
	if host != "" {
		opts = append(opts, WithHost(host))
	}
	if distinctID != "" {
		opts = append(opts, WithDistinctID(distinctID))
	}
	return New(opts...)
}

// TestIntegrationResolve proves a real evaluation reaches a real flag. It
// asserts a Version is present rather than a particular value, because a flag's
// value is whatever the project's release conditions say today.
func TestIntegrationResolve(t *testing.T) {
	apiKey, flag, host, distinctID := liveConfig(t)
	p := liveProvider(t, apiKey, host, distinctID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("posthog://" + flag)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Version == "" {
		t.Fatal("Resolve returned no Version")
	}
	if v.Sensitive {
		t.Fatal("a feature flag must not be marked Sensitive")
	}
	t.Logf("resolved %d bytes, kind %s", len(v.Bytes), v.Metadata["kind"])
}

// TestIntegrationFacets exercises all three fragments against the live flag, so
// a real response shape that the fake models wrongly would surface here.
func TestIntegrationFacets(t *testing.T) {
	apiKey, flag, host, distinctID := liveConfig(t)
	p := liveProvider(t, apiKey, host, distinctID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, frag := range []string{fragEnabled, fragVariant, fragPayload} {
		ref, err := mamori.ParseRef("posthog://" + flag + "#" + frag)
		if err != nil {
			t.Fatalf("ParseRef(#%s): %v", frag, err)
		}
		v, err := p.Resolve(ctx, ref)
		if err != nil {
			t.Fatalf("Resolve(#%s): %v", frag, err)
		}
		t.Logf("#%s resolved %d bytes", frag, len(v.Bytes))
	}
}

// TestIntegrationNotFound proves a flag key the project does not have is
// reported as not-found rather than as an empty value, which is the difference
// between mamori applying your default and applying "".
func TestIntegrationNotFound(t *testing.T) {
	apiKey, _, host, distinctID := liveConfig(t)
	p := liveProvider(t, apiKey, host, distinctID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("posthog://mamori-integration-flag-that-does-not-exist")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if _, err := p.Resolve(ctx, ref); !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("Resolve of an absent flag: err = %v, want errors.Is(err, mamori.ErrNotFound)", err)
	}
}
