//go:build integration

// Package firebasertdb live integration test. Not run by the default
// `go test ./...`.
//
// Run against a real Firebase Realtime Database with Application Default
// Credentials configured (e.g. `gcloud auth application-default login` or a
// service-account key in GOOGLE_APPLICATION_CREDENTIALS), then:
//
//	FIREBASE_DATABASE_URL=https://my-project-default-rtdb.firebaseio.com \
//	FIREBASE_TEST_PATH=config/service/log_level \
//	FIREBASE_TEST_EXPECT=info \
//	go test -tags integration -run TestLive ./...
//
// The path named by FIREBASE_TEST_PATH must already hold a value; if
// FIREBASE_TEST_EXPECT is set, the resolved value must equal it. To exercise the
// native SSE watch, also set FIREBASE_TEST_WATCH=1 and mutate the value in the
// Firebase console within the timeout.
package firebasertdb

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

func liveProvider(t *testing.T) *Provider {
	t.Helper()
	if os.Getenv("FIREBASE_DATABASE_URL") == "" {
		t.Skip("set FIREBASE_DATABASE_URL to run the live test")
	}
	return New()
}

func TestLiveResolve(t *testing.T) {
	path := os.Getenv("FIREBASE_TEST_PATH")
	if path == "" {
		t.Skip("set FIREBASE_TEST_PATH to run the live resolve test")
	}
	p := liveProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, err := mamori.ParseRef("firebase-rtdb://" + path)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Version == "" {
		t.Error("Version is empty")
	}
	if want := os.Getenv("FIREBASE_TEST_EXPECT"); want != "" && string(v.Bytes) != want {
		t.Fatalf("value = %q, want %q", v.Bytes, want)
	}
	t.Logf("resolved %q version=%s (%d bytes)", path, v.Version, len(v.Bytes))
}

func TestLiveNotFound(t *testing.T) {
	p := liveProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, _ := mamori.ParseRef("firebase-rtdb://mamori/does-not-exist-xyz")
	_, err := p.Resolve(ctx, ref)
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLiveWatch(t *testing.T) {
	path := os.Getenv("FIREBASE_TEST_PATH")
	if path == "" || os.Getenv("FIREBASE_TEST_WATCH") == "" {
		t.Skip("set FIREBASE_TEST_PATH and FIREBASE_TEST_WATCH=1 to run the live watch test")
	}
	p := liveProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ref, _ := mamori.ParseRef("firebase-rtdb://" + path)
	ch, err := p.Watch(ctx, ref)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	// Baseline.
	select {
	case u := <-ch:
		t.Logf("baseline: %q err=%v", u.Value.Bytes, u.Err)
	case <-time.After(10 * time.Second):
		t.Fatal("no baseline within 10s")
	}
	t.Logf("mutate %q in the Firebase console within the timeout to observe an update", path)
	for {
		select {
		case u, open := <-ch:
			if !open {
				t.Fatal("watch channel closed")
			}
			if u.Err != nil {
				t.Logf("update err: %v", u.Err)
				continue
			}
			t.Logf("observed change: %q (version %s)", u.Value.Bytes, u.Value.Version)
			return
		case <-ctx.Done():
			t.Skip("no change observed before timeout (mutate the value to exercise this test)")
		}
	}
}
