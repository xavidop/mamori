//go:build integration

// Package etcd integration test. Runs against a real etcd server.
//
// Start a local etcd and run the suite:
//
//	etcd &
//	export ETCD_ENDPOINTS=127.0.0.1:2379
//	GOWORK=off go test -tags integration -run Integration ./...
//
// The test seeds and mutates keys under a unique prefix and cleans them up.
package etcd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func liveClient(t *testing.T) *clientv3.Client {
	t.Helper()
	raw := os.Getenv("ETCD_ENDPOINTS")
	if raw == "" {
		t.Skip("ETCD_ENDPOINTS not set; skipping live etcd integration test")
	}
	var eps []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			eps = append(eps, s)
		}
	}
	c, err := clientv3.New(clientv3.Config{
		Endpoints:   eps,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("etcd client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestIntegrationConformance(t *testing.T) {
	client := liveClient(t)
	prefix := fmt.Sprintf("mamori-it/%d/", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = client.Delete(ctx, prefix, clientv3.WithPrefix())
	})

	put := func(ctx context.Context, key, val string) error {
		_, err := client.Put(ctx, key, val)
		return err
	}

	providertest.Run(t, providertest.Config{
		New:               func() mamori.Provider { return New(WithClient(client)) },
		Ref:               func(key string) string { return "etcd://" + key },
		Key:               func(name string) string { return prefix + name },
		Seed:              put,
		Mutate:            put,
		EventuallyTimeout: 15 * time.Second,
	})
}

func TestIntegrationResolveAndWatch(t *testing.T) {
	client := liveClient(t)
	key := fmt.Sprintf("mamori-it/%d/app", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = client.Delete(ctx, key)
	})

	if _, err := client.Put(context.Background(), key, `{"level":"info"}`); err != nil {
		t.Fatalf("put: %v", err)
	}

	p := New(WithClient(client))

	ref := mustRef(t, "etcd://"+key+"#level")
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

	if _, err := client.Put(context.Background(), key, `{"level":"debug"}`); err != nil {
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
