//go:build integration

// Package firebaserc live integration test. Not run by the default
// `go test ./...`.
//
// Run against a real Firebase project with Application Default Credentials
// configured (e.g. `gcloud auth application-default login` or a service-account
// key in GOOGLE_APPLICATION_CREDENTIALS whose principal can read Remote Config),
// then:
//
//	FIREBASE_RC_PROJECT=my-project \
//	FIREBASE_RC_PARAM=welcome_message \
//	FIREBASE_RC_EXPECT=Hello \
//	go test -tags integration -run TestLive ./...
//
// The parameter named by FIREBASE_RC_PARAM must exist in the project's server
// Remote Config template with a concrete (non in-app) default value equal to
// FIREBASE_RC_EXPECT (when set).
package firebaserc

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

func TestLiveResolve(t *testing.T) {
	project := os.Getenv("FIREBASE_RC_PROJECT")
	param := os.Getenv("FIREBASE_RC_PARAM")
	if project == "" || param == "" {
		t.Skip("set FIREBASE_RC_PROJECT and FIREBASE_RC_PARAM to run the live test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(WithProjectID(project))
	t.Cleanup(func() { _ = p.Close() })

	ref, err := mamori.ParseRef("firebase-rc://" + param)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Sensitive {
		t.Error("Sensitive = true, want false")
	}
	if v.Version == "" {
		t.Error("Version is empty")
	}
	if want := os.Getenv("FIREBASE_RC_EXPECT"); want != "" && string(v.Bytes) != want {
		t.Fatalf("value = %q, want %q", v.Bytes, want)
	}
	t.Logf("resolved parameter %q at template version %s (%d bytes)", param, v.Version, len(v.Bytes))
}

func TestLiveNotFound(t *testing.T) {
	project := os.Getenv("FIREBASE_RC_PROJECT")
	if project == "" {
		t.Skip("set FIREBASE_RC_PROJECT to run the live test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(WithProjectID(project))
	t.Cleanup(func() { _ = p.Close() })

	ref, _ := mamori.ParseRef("firebase-rc://mamori_does_not_exist_xyz")
	_, err := p.Resolve(ctx, ref)
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
