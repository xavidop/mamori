package middleware_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/middleware"
)

// countingProvider records how many times Resolve was called and returns a fixed
// value (or an error).
type countingProvider struct {
	scheme string
	calls  atomic.Int32
	val    string
	err    error
	seen   atomic.Value // last ref path
}

func (p *countingProvider) Scheme() string { return p.scheme }
func (p *countingProvider) Resolve(_ context.Context, ref mamori.Ref) (mamori.Value, error) {
	p.calls.Add(1)
	p.seen.Store(ref.Path)
	if p.err != nil {
		return mamori.Value{}, p.err
	}
	return mamori.Value{Bytes: []byte(p.val), Version: "v"}, nil
}

func ref(t *testing.T, s string) mamori.Ref {
	t.Helper()
	r, err := mamori.ParseRef(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCacheServesWithinTTL(t *testing.T) {
	clk := mamori.NewFakeClock(time.Time{})
	inner := &countingProvider{scheme: "c", val: "data"}
	p := middleware.Cache(time.Minute, inner, middleware.WithClock(clk))

	r := ref(t, "c://key")
	for i := 0; i < 3; i++ {
		if _, err := p.Resolve(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("inner called %d times within TTL, want 1", inner.calls.Load())
	}
	// Expire the cache.
	clk.Advance(2 * time.Minute)
	if _, err := p.Resolve(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if inner.calls.Load() != 2 {
		t.Fatalf("inner called %d times after TTL, want 2", inner.calls.Load())
	}
}

func TestFailoverFallsThrough(t *testing.T) {
	primary := &countingProvider{scheme: "f", err: errors.New("primary down")}
	replica := &countingProvider{scheme: "f", val: "from-replica"}
	p := middleware.Failover(primary, replica)

	v, err := p.Resolve(context.Background(), ref(t, "f://key"))
	if err != nil {
		t.Fatalf("failover: %v", err)
	}
	if string(v.Bytes) != "from-replica" {
		t.Fatalf("value = %q, want from-replica", v.Bytes)
	}
}

func TestFailoverNotFoundAuthoritative(t *testing.T) {
	primary := &countingProvider{scheme: "f", err: mamori.ErrNotFound}
	replica := &countingProvider{scheme: "f", val: "should-not-be-used"}
	p := middleware.Failover(primary, replica)

	_, err := p.Resolve(context.Background(), ref(t, "f://key"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (authoritative)", err)
	}
	if replica.calls.Load() != 0 {
		t.Fatal("replica consulted despite authoritative not-found")
	}
}

func TestPrefixRewrites(t *testing.T) {
	inner := &countingProvider{scheme: "p", val: "x"}
	p := middleware.Prefix("tenant-a", inner)

	if _, err := p.Resolve(context.Background(), ref(t, "p://db/password")); err != nil {
		t.Fatal(err)
	}
	if got := inner.seen.Load().(string); got != "tenant-a/db/password" {
		t.Fatalf("inner saw path %q, want tenant-a/db/password", got)
	}
}

func TestRateLimitSpaces(t *testing.T) {
	clk := mamori.NewFakeClock(time.Time{})
	inner := &countingProvider{scheme: "r", val: "x"}
	p := middleware.RateLimit(100, inner, middleware.WithClock(clk))

	// First call passes immediately.
	done := make(chan error, 1)
	go func() { _, err := p.Resolve(context.Background(), ref(t, "r://k")); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first rate-limited call blocked unexpectedly")
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", inner.calls.Load())
	}
}

func TestAuditDoesNotLogPayload(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	inner := &countingProvider{scheme: "a", val: "TOP-SECRET-VALUE"}
	p := middleware.Audit(logger, inner)

	if _, err := p.Resolve(context.Background(), ref(t, "a://key")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "TOP-SECRET-VALUE") {
		t.Fatalf("audit log leaked the payload: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "a://key") {
		t.Fatalf("audit log missing the ref: %s", buf.String())
	}
}

func TestMiddlewarePreservesWatch(t *testing.T) {
	// A watchable inner should remain watchable through middleware.
	wp := &watchableCounting{countingProvider: countingProvider{scheme: "w", val: "x"}}
	p := middleware.Cache(time.Minute, wp)
	if _, ok := p.(mamori.WatchableProvider); !ok {
		t.Fatal("cache wrapper is not watchable")
	}
}

type watchableCounting struct {
	countingProvider
}

func (w *watchableCounting) Watch(ctx context.Context, _ mamori.Ref) (<-chan mamori.Update, error) {
	ch := make(chan mamori.Update)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}
