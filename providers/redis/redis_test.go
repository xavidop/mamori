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
	subs   map[string][]chan *goredis.Message
	closed bool
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{data: map[string]string{}, subs: map[string][]chan *goredis.Message{}}
}

func (f *fakeRedis) Get(_ context.Context, key string) *goredis.StringCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[key]
	if !ok {
		return goredis.NewStringResult("", goredis.Nil)
	}
	return goredis.NewStringResult(v, nil)
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

func TestConformance(t *testing.T) {
	f := newFakeRedis()
	providertest.Run(t, providertest.Config{
		New:    func() mamori.Provider { return New(withRedisAPI(f)) },
		Ref:    func(key string) string { return "redis://" + key },
		Seed:   func(_ context.Context, key, val string) error { f.set(key, val); return nil },
		Mutate: func(_ context.Context, key, val string) error { f.set(key, val); return nil },
	})
}
