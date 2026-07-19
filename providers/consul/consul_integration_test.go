//go:build integration

// Package consul integration test. Runs against a real Consul agent.
//
// Start a local agent and run the suite:
//
//	consul agent -dev &
//	export CONSUL_HTTP_ADDR=127.0.0.1:8500
//	GOWORK=off go test -tags integration -run Integration ./...
//
// The test seeds and mutates keys under a unique prefix and cleans them up.
package consul

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

func liveClient(t *testing.T) *api.Client {
	t.Helper()
	if os.Getenv("CONSUL_HTTP_ADDR") == "" {
		t.Skip("CONSUL_HTTP_ADDR not set; skipping live Consul integration test")
	}
	c, err := api.NewClient(api.DefaultConfig())
	if err != nil {
		t.Fatalf("consul client: %v", err)
	}
	return c
}

func TestIntegrationConformance(t *testing.T) {
	client := liveClient(t)
	kv := client.KV()
	prefix := fmt.Sprintf("mamori-it/%d/", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = kv.DeleteTree(prefix, nil)
	})

	put := func(_ context.Context, key, val string) error {
		_, err := kv.Put(&api.KVPair{Key: key, Value: []byte(val)}, nil)
		return err
	}

	providertest.Run(t, providertest.Config{
		New:               func() mamori.Provider { return New(WithClient(client)) },
		Ref:               func(key string) string { return "consul://" + key },
		Key:               func(name string) string { return prefix + name },
		Seed:              put,
		Mutate:            put,
		EventuallyTimeout: 15 * time.Second,
	})
}

func TestIntegrationResolveAndWatch(t *testing.T) {
	client := liveClient(t)
	kv := client.KV()
	key := fmt.Sprintf("mamori-it/%d/app", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = kv.Delete(key, nil) })

	if _, err := kv.Put(&api.KVPair{Key: key, Value: []byte(`{"level":"info"}`)}, nil); err != nil {
		t.Fatalf("put: %v", err)
	}

	p := New(WithClient(client), WithWaitTime(10*time.Second))

	ref := mustRef(t, "consul://"+key+"#level")
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "info" {
		t.Fatalf("level = %q, want info", v.Bytes)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, ref)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	<-ch // baseline

	if _, err := kv.Put(&api.KVPair{Key: key, Value: []byte(`{"level":"debug"}`)}, nil); err != nil {
		t.Fatalf("put update: %v", err)
	}
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("watch update err: %v", u.Err)
		}
		if string(u.Value.Bytes) != "debug" {
			t.Fatalf("watch level = %q, want debug", u.Value.Bytes)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("watch did not deliver the update")
	}
}
