package consul

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeKV is an in-memory implementation of kvAPI supporting the parts of the
// Consul KV contract the provider relies on: a monotonically increasing
// ModifyIndex bumped on every write, and blocking-query semantics (a Get with a
// non-zero WaitIndex blocks until the global index moves past WaitIndex, the
// WaitTime elapses, or the request context is cancelled).
type fakeKV struct {
	mu      sync.Mutex
	data    map[string]*api.KVPair
	index   uint64
	waiters chan struct{} // closed (and replaced) on every write to wake blocked Gets
}

func newFakeKV() *fakeKV {
	return &fakeKV{
		data:    map[string]*api.KVPair{},
		waiters: make(chan struct{}),
	}
}

// set writes val for key, bumping the global index and the key's ModifyIndex,
// then wakes any blocked blocking-query Gets.
func (f *fakeKV) set(key, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.index++
	f.data[key] = &api.KVPair{Key: key, Value: []byte(val), ModifyIndex: f.index, CreateIndex: f.index}
	close(f.waiters)
	f.waiters = make(chan struct{})
}

func (f *fakeKV) snapshot(key string) (*api.KVPair, uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copyPair(f.data[key]), f.index
}

func copyPair(p *api.KVPair) *api.KVPair {
	if p == nil {
		return nil
	}
	cp := *p
	if p.Value != nil {
		cp.Value = append([]byte(nil), p.Value...)
	}
	return &cp
}

// Get implements kvAPI, including blocking-query behavior.
func (f *fakeKV) Get(key string, q *api.QueryOptions) (*api.KVPair, *api.QueryMeta, error) {
	ctx := context.Background()
	var waitIndex uint64
	var waitTime time.Duration
	if q != nil {
		if c := q.Context(); c != nil {
			ctx = c
		}
		waitIndex = q.WaitIndex
		waitTime = q.WaitTime
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	for {
		f.mu.Lock()
		cur := f.index
		pair := copyPair(f.data[key])
		wake := f.waiters
		f.mu.Unlock()

		// Non-blocking (WaitIndex == 0), or the index has moved relative to the
		// caller's WaitIndex: return immediately.
		if waitIndex == 0 || cur != waitIndex {
			return pair, &api.QueryMeta{LastIndex: cur}, nil
		}

		var timer *time.Timer
		var timeoutC <-chan time.Time
		if waitTime > 0 {
			timer = time.NewTimer(waitTime)
			timeoutC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil, nil, ctx.Err()
		case <-wake:
			if timer != nil {
				timer.Stop()
			}
			// Loop and re-read the (now-changed) state.
		case <-timeoutC:
			pair, cur := f.snapshot(key)
			return pair, &api.QueryMeta{LastIndex: cur}, nil
		}
	}
}

// compile-time check that the real *api.KV satisfies kvAPI.
var _ kvAPI = (*api.KV)(nil)

func TestConformance(t *testing.T) {
	fake := newFakeKV()
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider {
			return New(withKV(fake), WithWaitTime(500*time.Millisecond))
		},
		Ref:  func(key string) string { return "consul://" + key },
		Seed: func(_ context.Context, key, val string) error { fake.set(key, val); return nil },
		Mutate: func(_ context.Context, key, val string) error {
			fake.set(key, val)
			return nil
		},
		EventuallyTimeout: 3 * time.Second,
	})
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != "consul" {
		t.Fatalf("Scheme() = %q, want consul", got)
	}
}

func TestResolveValueAndVersion(t *testing.T) {
	fake := newFakeKV()
	fake.set("config/app", "hello")
	p := New(withKV(fake))

	ref := mustRef(t, "consul://config/app")
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hello" {
		t.Fatalf("Bytes = %q, want hello", v.Bytes)
	}
	if v.Version != "1" {
		t.Fatalf("Version = %q, want 1 (ModifyIndex)", v.Version)
	}
	if v.Sensitive {
		t.Fatal("Consul KV values must not be marked Sensitive")
	}

	// A write bumps ModifyIndex, so the version must change.
	fake.set("config/app", "world")
	v2, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve after mutate: %v", err)
	}
	if v2.Version == v.Version {
		t.Fatalf("Version did not change after write (both %q)", v.Version)
	}
	if string(v2.Bytes) != "world" {
		t.Fatalf("Bytes = %q, want world", v2.Bytes)
	}
}

func TestResolveJSONKey(t *testing.T) {
	fake := newFakeKV()
	fake.set("config/db", `{"host":"db.internal","port":5432,"password":"s3cr3t"}`)
	p := New(withKV(fake))

	got := func(key string) string {
		t.Helper()
		v, err := p.Resolve(context.Background(), mustRef(t, "consul://config/db#"+key))
		if err != nil {
			t.Fatalf("Resolve #%s: %v", key, err)
		}
		return string(v.Bytes)
	}
	if got("host") != "db.internal" {
		t.Fatalf("host = %q", got("host"))
	}
	if got("port") != "5432" {
		t.Fatalf("port = %q, want 5432", got("port"))
	}
	if got("password") != "s3cr3t" {
		t.Fatalf("password = %q", got("password"))
	}

	// A missing key is a typed not-found.
	_, err := p.Resolve(context.Background(), mustRef(t, "consul://config/db#nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing json key err = %v, want ErrNotFound", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(withKV(newFakeKV()))
	_, err := p.Resolve(context.Background(), mustRef(t, "consul://absent"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveContextCancelled(t *testing.T) {
	fake := newFakeKV()
	fake.set("k", "v")
	p := New(withKV(fake))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustRef(t, "consul://k")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestWatchEmitsBaselineAndChange(t *testing.T) {
	fake := newFakeKV()
	fake.set("watched", "v1")
	p := New(withKV(fake), WithWaitTime(500*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "consul://watched"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Baseline.
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("baseline err: %v", u.Err)
		}
		if string(u.Value.Bytes) != "v1" {
			t.Fatalf("baseline = %q, want v1", u.Value.Bytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no baseline emitted")
	}

	// Mutation must produce an Update via the blocking query.
	fake.set("watched", "v2")
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("change err: %v", u.Err)
		}
		if string(u.Value.Bytes) != "v2" {
			t.Fatalf("change = %q, want v2", u.Value.Bytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mutation not delivered")
	}
}

func TestWatchClosesOnCancel(t *testing.T) {
	fake := newFakeKV()
	fake.set("k", "v")
	p := New(withKV(fake), WithWaitTime(time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Watch(ctx, mustRef(t, "consul://k"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed after cancel")
		}
	}
}

func mustRef(t *testing.T, s string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return ref
}
