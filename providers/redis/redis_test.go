package redis

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeRedis is an in-memory redisAPI + keyspace pub/sub, so the conformance kit
// (including the native watch checks) runs without a live Redis.
type fakeRedis struct {
	mu     sync.Mutex
	data   map[string]string
	fails  map[string]error
	subs   map[string][]chan *goredis.Message
	closed bool
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{
		data:  map[string]string{},
		fails: map[string]error{},
		subs:  map[string][]chan *goredis.Message{},
	}
}

func (f *fakeRedis) Get(_ context.Context, key string) *goredis.StringCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.fails[key]; ok {
		return goredis.NewStringResult("", err)
	}
	v, ok := f.data[key]
	if !ok {
		return goredis.NewStringResult("", goredis.Nil)
	}
	return goredis.NewStringResult(v, nil)
}

// fail makes the next Get for key return err, until clear(key) is called. It
// powers the providertest ErrorClassification case. key is the same raw
// string Seed/set stores under, since redis keys need no path-building the
// way, say, a GCP "projects/P/secrets/S" resource name does.
func (f *fakeRedis) fail(key string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[key] = err
}

// clear cancels a previously injected fail(key, err).
func (f *fakeRedis) clear(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fails, key)
}

func (f *fakeRedis) PSubscribe(_ context.Context, patterns ...string) subscription {
	ch := make(chan *goredis.Message, 8)
	f.mu.Lock()
	for _, p := range patterns {
		f.subs[p] = append(f.subs[p], ch)
	}
	f.mu.Unlock()
	return &fakeSub{f: f, ch: ch, patterns: patterns}
}

func (f *fakeRedis) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

// set stores a value and publishes a keyspace "set" notification (db 0).
func (f *fakeRedis) set(key, val string) {
	f.mu.Lock()
	f.data[key] = val
	channel := fmt.Sprintf("__keyspace@0__:%s", key)
	chans := append([]chan *goredis.Message(nil), f.subs[channel]...)
	f.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- &goredis.Message{Channel: channel, Payload: "set"}:
		default:
		}
	}
}

type fakeSub struct {
	f        *fakeRedis
	ch       chan *goredis.Message
	patterns []string
	once     sync.Once
}

func (s *fakeSub) Channel() <-chan *goredis.Message { return s.ch }

func (s *fakeSub) Close() error {
	s.once.Do(func() {
		s.f.mu.Lock()
		for _, p := range s.patterns {
			cur := s.f.subs[p]
			for i, c := range cur {
				if c == s.ch {
					s.f.subs[p] = append(cur[:i], cur[i+1:]...)
					break
				}
			}
		}
		s.f.mu.Unlock()
		close(s.ch)
	})
	return nil
}

func TestResolve(t *testing.T) {
	f := newFakeRedis()
	f.set("app/level", "debug")
	p := New(withRedisAPI(f))

	ref, _ := mamori.ParseRef("redis://app/level")
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Bytes) != "debug" {
		t.Errorf("value = %q, want debug", v.Bytes)
	}
	if v.Version == "" {
		t.Error("expected a non-empty version")
	}
	if v.Sensitive {
		t.Error("redis values must not be Sensitive by default")
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(withRedisAPI(newFakeRedis()))
	ref, _ := mamori.ParseRef("redis://missing")
	_, err := p.Resolve(context.Background(), ref)
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveJSONKey(t *testing.T) {
	f := newFakeRedis()
	f.set("app/db", `{"password":"s3cr3t","port":5432}`)
	p := New(withRedisAPI(f))

	ref, _ := mamori.ParseRef("redis://app/db#password")
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Bytes) != "s3cr3t" {
		t.Errorf("value = %q, want s3cr3t", v.Bytes)
	}
}

func TestWatchEmitsOnSet(t *testing.T) {
	f := newFakeRedis()
	f.set("app/flag", "off")
	p := New(withRedisAPI(f))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ref, _ := mamori.ParseRef("redis://app/flag")
	ch, err := p.Watch(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}

	// baseline
	select {
	case u := <-ch:
		if string(u.Value.Bytes) != "off" {
			t.Fatalf("baseline = %q, want off", u.Value.Bytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no baseline")
	}

	f.set("app/flag", "on")
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("watch error: %v", u.Err)
		}
		if string(u.Value.Bytes) != "on" {
			t.Fatalf("update = %q, want on", u.Value.Bytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no update after set")
	}
}

// --- Close ---

// TestCloseWithoutUseIsSafe pins the "safe with no prior use" half of the
// Close contract: Close on a provider that never resolved must not dial and
// must not panic, and a second Close must stay clean.
func TestCloseWithoutUseIsSafe(t *testing.T) {
	p := New()
	if err := p.Close(); err != nil {
		t.Fatalf("Close on an unused provider: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if p.ownClient || p.client != nil {
		t.Error("Close built a client on a provider that never resolved")
	}
}

// TestResolveAfterCloseIsUnavailable pins the terminal half of the contract:
// once Close has run, Resolve must refuse locally (via the p.closed check in
// getClient) rather than reaching into the fake client it was told to stop
// using.
func TestResolveAfterCloseIsUnavailable(t *testing.T) {
	f := newFakeRedis()
	f.set("app/level", "debug")
	p := New(withRedisAPI(f))

	ref, _ := mamori.ParseRef("redis://app/level")
	if _, err := p.Resolve(context.Background(), ref); err != nil {
		t.Fatalf("Resolve before Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := p.Resolve(context.Background(), ref); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("Resolve after Close = %v; want ErrUnavailable", err)
	}
}

// TestCloseDoesNotCloseInjectedClient is the ownership half of rule 5: a
// go-redis client handed in with WithClient belongs to the caller, so Close
// must stop using it without closing it. go-redis returns "redis: client is
// closed" from a second Close, so calling the real Close ourselves after
// provider.Close and getting nil back proves this was the first real close -
// i.e. Provider.Close left the injected client alone.
//
// It also proves the other half of rule 5's ordering requirement - that Close
// still marks the provider closed on the not-owned path - since a Close that
// returned early here (skipping p.closed = true to avoid touching the
// injected client) would pass the "still usable" assertion below while
// silently breaking Resolve's terminality for every WithClient caller.
func TestCloseDoesNotCloseInjectedClient(t *testing.T) {
	// goredis.NewClient never dials synchronously (the connection pool
	// connects lazily on first command), so this construction is instant and
	// needs no reachable server.
	real := goredis.NewClient(&goredis.Options{Addr: "203.0.113.1:6379"})
	p := New(WithClient(real))

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := real.Close(); err != nil {
		t.Fatalf("injected client Close after provider Close: %v; want nil "+
			"(a non-nil error here means Provider.Close already closed it)", err)
	}

	ref, _ := mamori.ParseRef("redis://app/level")
	if _, err := p.Resolve(context.Background(), ref); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("Resolve after Close = %v; want ErrUnavailable (closed flag not set on the injected path)", err)
	}
}

// TestCloseReleasesSelfBuiltClient is the positive half of rule 5's ownership
// tracking, the mirror image of TestCloseDoesNotCloseInjectedClient above: a
// client this provider built itself must actually be released, not merely
// left alone. Without this, deleting "p.ownClient = true" from getClient's
// build branch (redis.go) leaves every other test in this file green, since
// none of them ever force that branch and then check the client came back
// closed.
func TestCloseReleasesSelfBuiltClient(t *testing.T) {
	p := New(WithAddr("203.0.113.1:6379"))

	// getClient's build branch calls goredis.NewClient, which never dials
	// synchronously, so this reaches the p.ownClient = true build site
	// without a reachable server.
	client, err := p.getClient()
	if err != nil {
		t.Fatalf("getClient: %v", err)
	}
	if !p.ownClient {
		t.Fatal("getClient did not claim ownership of the client it built")
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The provider-built client must actually have been closed: go-redis
	// returns "redis: client is closed" from a second Close, so calling it
	// ourselves now must return a non-nil error, not nil.
	if err := client.Close(); err == nil {
		t.Fatal("self-built client Close after provider Close returned nil; " +
			"want an already-closed error (Provider.Close did not actually close it)")
	}
}

func TestConformance(t *testing.T) {
	f := newFakeRedis()
	providertest.Run(t, providertest.Config{
		New:        func() mamori.Provider { return New(withRedisAPI(f)) },
		Ref:        func(key string) string { return "redis://" + key },
		PointerRef: func(key, frag string) string { return "redis://" + key + frag },
		Seed:       func(_ context.Context, key, val string) error { f.set(key, val); return nil },
		Mutate:     func(_ context.Context, key, val string) error { f.set(key, val); return nil },
		// redis.go: "Subscribe before emitting the baseline so no notification
		// is missed between the baseline GET and the start of the read loop."
		WatchDeliversBaseline: true,
		Fail: func(_ context.Context, key string, err error) error {
			f.fail(key, err)
			return nil
		},
		Clear: func(_ context.Context, key string) error {
			f.clear(key)
			return nil
		},
	})
}
