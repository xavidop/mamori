//go:build integration

// Package firestore live integration test. Not run by the default `go test ./...`.
//
// Run against a real Google Cloud Firestore (or the Firestore emulator) with
// Application Default Credentials configured (e.g.
// `gcloud auth application-default login`), then:
//
//	FIRESTORE_TEST_PROJECT=my-project \
//	go test -tags integration -run TestLive ./...
//
// To run against the local emulator instead of a real project:
//
//	gcloud emulators firestore start --host-port=127.0.0.1:8080 &
//	export FIRESTORE_EMULATOR_HOST=127.0.0.1:8080
//	FIRESTORE_TEST_PROJECT=demo-project \
//	go test -tags integration -run TestLive ./...
//
// The test writes and cleans up documents under a unique collection.
package firestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	fs "cloud.google.com/go/firestore"
	"github.com/xavidop/mamori"
)

func liveClient(t *testing.T) *fs.Client {
	t.Helper()
	project := os.Getenv("FIRESTORE_TEST_PROJECT")
	if project == "" {
		t.Skip("set FIRESTORE_TEST_PROJECT to run the live Firestore integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := fs.NewClient(ctx, project)
	if err != nil {
		t.Fatalf("firestore client: %v", err)
	}
	return c
}

func TestLiveResolveAndWatch(t *testing.T) {
	client := liveClient(t)
	t.Cleanup(func() { _ = client.Close() })

	collection := fmt.Sprintf("mamori-it-%d", time.Now().UnixNano())
	doc := "app"
	docRef := client.Collection(collection).Doc(doc)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = docRef.Delete(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := docRef.Set(ctx, map[string]interface{}{"level": "info"}); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	p := New(WithClient(client))

	// Resolve a single field.
	ref := mustRef(t, "firestore://"+collection+"/"+doc+"#level")
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "info" {
		t.Fatalf("level = %q, want info", v.Bytes)
	}
	if v.Version == "" {
		t.Error("Version is empty")
	}
	if v.Sensitive {
		t.Error("Firestore values must not be marked Sensitive")
	}

	// Native snapshot-listener watch: baseline then a pushed change.
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	ch, err := p.Watch(wctx, ref)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	<-ch // baseline

	if _, err := docRef.Set(ctx, map[string]interface{}{"level": "debug"}); err != nil {
		t.Fatalf("update set: %v", err)
	}
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("watch update err: %v", u.Err)
		}
		if string(u.Value.Bytes) != "debug" {
			t.Fatalf("watch level = %q, want debug", u.Value.Bytes)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("watch did not deliver the update")
	}
}

func TestLiveNotFound(t *testing.T) {
	client := liveClient(t)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(WithClient(client))
	ref := mustRef(t, "firestore://mamori-does-not-exist/nope-"+fmt.Sprint(time.Now().UnixNano()))
	_, err := p.Resolve(ctx, ref)
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
