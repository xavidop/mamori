//go:build integration

// Package mongodb integration test. Runs against a real MongoDB deployment.
//
// Change streams require a replica set (or sharded cluster). Start a single-node
// replica set and run the suite:
//
//	docker run -d --name mongo -p 27017:27017 mongo:7 --replSet rs0
//	docker exec mongo mongosh --eval 'rs.initiate()'
//	export MONGODB_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0'
//	export MONGODB_DATABASE=mamori_it
//	GOWORK=off go test -tags integration -run Integration ./...
//
// The test seeds and mutates documents in a unique collection and drops it on
// cleanup.
package mongodb

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

func liveClient(t *testing.T) (*mongo.Client, string) {
	t.Helper()
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		t.Skip("MONGODB_URI not set; skipping live MongoDB integration test")
	}
	db := os.Getenv("MONGODB_DATABASE")
	if db == "" {
		db = "mamori_it"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("mongo ping: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Disconnect(context.Background())
	})
	return client, db
}

func TestIntegrationConformance(t *testing.T) {
	client, db := liveClient(t)
	collName := fmt.Sprintf("mamori_it_%d", time.Now().UnixNano())
	coll := client.Database(db).Collection(collName)
	t.Cleanup(func() {
		_ = coll.Drop(context.Background())
	})

	upsert := func(ctx context.Context, key, val string) error {
		_, err := coll.ReplaceOne(
			ctx,
			bson.M{"_id": key},
			bson.M{"_id": key, "value": val},
			options.Replace().SetUpsert(true),
		)
		return err
	}

	providertest.Run(t, providertest.Config{
		New:               func() mamori.Provider { return New(WithClient(client), WithDatabase(db)) },
		Ref:               func(key string) string { return "mongodb://" + collName + "/" + key + "#value" },
		Seed:              upsert,
		Mutate:            upsert,
		EventuallyTimeout: 15 * time.Second,
	})
}

func TestIntegrationResolveAndWatch(t *testing.T) {
	client, db := liveClient(t)
	collName := fmt.Sprintf("mamori_it_%d", time.Now().UnixNano())
	coll := client.Database(db).Collection(collName)
	t.Cleanup(func() { _ = coll.Drop(context.Background()) })

	if _, err := coll.ReplaceOne(
		context.Background(),
		bson.M{"_id": "app"},
		bson.M{"_id": "app", "level": "info"},
		options.Replace().SetUpsert(true),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p := New(WithClient(client), WithDatabase(db))
	ref := mustRef(t, "mongodb://"+collName+"/app#level")

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
		t.Fatalf("Watch (needs a replica set): %v", err)
	}
	<-ch // baseline

	if _, err := coll.ReplaceOne(
		context.Background(),
		bson.M{"_id": "app"},
		bson.M{"_id": "app", "level": "debug"},
	); err != nil {
		t.Fatalf("update: %v", err)
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
