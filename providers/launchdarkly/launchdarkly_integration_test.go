//go:build integration

// Package launchdarkly integration test. Runs against a real LaunchDarkly
// environment using a server-side SDK key.
//
// It cannot create or mutate flags (that requires the LaunchDarkly management
// REST API), so it verifies the read path against a flag you have already
// created in the target environment:
//
//	export LAUNCHDARKLY_SDK_KEY=sdk-xxxxxxxx
//	export LAUNCHDARKLY_TEST_FLAG=my-existing-flag-key   # any existing flag
//	GOWORK=off go test -tags integration -run Integration ./...
//
// To exercise the native streaming watch, toggle LAUNCHDARKLY_TEST_FLAG in the
// LaunchDarkly dashboard while TestIntegrationWatch is waiting.
package launchdarkly

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

func liveProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	key := os.Getenv("LAUNCHDARKLY_SDK_KEY")
	if key == "" {
		t.Skip("LAUNCHDARKLY_SDK_KEY not set; skipping live LaunchDarkly integration test")
	}
	flag := os.Getenv("LAUNCHDARKLY_TEST_FLAG")
	if flag == "" {
		t.Skip("LAUNCHDARKLY_TEST_FLAG not set; skipping live LaunchDarkly integration test")
	}
	return New(WithSDKKey(key)), flag
}

func TestIntegrationResolve(t *testing.T) {
	p, flag := liveProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	v, err := p.Resolve(ctx, mustRef(t, "launchdarkly://"+flag))
	if err != nil {
		t.Fatalf("Resolve(%q): %v", flag, err)
	}
	if v.Version == "" {
		t.Fatal("resolved value has an empty Version")
	}
	if v.Sensitive {
		t.Fatal("LaunchDarkly values must not be marked Sensitive")
	}
	t.Logf("flag %q resolved to %q (version %s)", flag, v.Bytes, v.Version)
}

func TestIntegrationNotFound(t *testing.T) {
	p, _ := liveProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := p.Resolve(ctx, mustRef(t, "launchdarkly://mamori-does-not-exist-"+time.Now().Format("150405")))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing flag err = %v, want ErrNotFound", err)
	}
}

func TestIntegrationWatch(t *testing.T) {
	p, flag := liveProvider(t)

	// Prime the client connection with a resolve first.
	primeCtx, primeCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer primeCancel()
	if _, err := p.Resolve(primeCtx, mustRef(t, "launchdarkly://"+flag)); err != nil {
		t.Fatalf("prime Resolve: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "launchdarkly://"+flag))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Best-effort: wait a short while for a change (toggle the flag in the
	// dashboard to observe an Update). Absence of a change is not a failure here.
	select {
	case u, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed unexpectedly")
		}
		if u.Err != nil {
			t.Logf("watch update error: %v", u.Err)
		} else {
			t.Logf("watch delivered change: %q (version %s)", u.Value.Bytes, u.Value.Version)
		}
	case <-time.After(3 * time.Second):
		t.Log("no flag change observed within 3s (toggle the flag to see an Update)")
	}

	// Cancellation must close the channel with no goroutine leak.
	cancel()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("watch channel not closed after cancellation")
		}
	}
}
