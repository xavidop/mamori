package mamori_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/mamoritest"
	"github.com/xavidop/mamori/secret"
)

// benchConfig is representative of a real service config: a couple of secrets,
// a couple of plain scalars, and a nested struct.
type benchConfig struct {
	DBPassword secret.String `source:"mem://db-password"`
	APIKey     secret.String `source:"mem://api-key"`
	LogLevel   string        `source:"mem://log-level" validate:"oneof=debug info warn error"`
	Workers    int           `source:"mem://workers"   validate:"gte=1,lte=256"`
	Redis      benchRedis
}

type benchRedis struct {
	Addr string `source:"mem://redis-addr"`
	DB   int    `source:"mem://redis-db"`
}

func benchProvider() *mamoritest.Provider {
	p := mamoritest.NewProvider("mem")
	p.Set("db-password", "s3cret")
	p.Set("api-key", "sk-live-abc123")
	p.Set("log-level", "info")
	p.Set("workers", "8")
	p.Set("redis-addr", "127.0.0.1:6379")
	p.Set("redis-db", "0")
	return p
}

func benchWatcher(tb testing.TB) *mamori.Watcher[benchConfig] {
	tb.Helper()
	w, err := mamori.Watch[benchConfig](context.Background(), mamori.WithProvider(benchProvider()))
	if err != nil {
		tb.Fatalf("Watch: %v", err)
	}
	tb.Cleanup(func() { _ = w.Close() })
	return w
}

// BenchmarkWatcherGet guards the central performance claim: Get is a lock-free
// read of the last valid snapshot, so it must stay allocation-free and cheap
// enough to call on every request rather than being hoisted and cached by
// callers (which is what reintroduces the staleness mamori exists to remove).
func BenchmarkWatcherGet(b *testing.B) {
	w := benchWatcher(b)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_ = w.Get()
	}
}

// BenchmarkWatcherGetParallel is the one that matters for the lock-free claim:
// with a mutex, throughput per goroutine collapses as GOMAXPROCS rises. It
// should stay roughly flat instead.
func BenchmarkWatcherGetParallel(b *testing.B) {
	w := benchWatcher(b)
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = w.Get()
		}
	})
}

// BenchmarkWatcherGetContended measures Get while the reconciler is publishing
// new snapshots underneath it. A reader must not be slowed by a concurrent
// writer, which is the practical difference between an atomic pointer swap and
// a read-write lock.
func BenchmarkWatcherGetContended(b *testing.B) {
	prov := benchProvider()
	w, err := mamori.Watch[benchConfig](context.Background(), mamori.WithProvider(prov))
	if err != nil {
		b.Fatalf("Watch: %v", err)
	}
	b.Cleanup(func() { _ = w.Close() })

	done := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
				prov.Set("workers", fmt.Sprint(i%256+1))
			}
		}
	}()
	b.Cleanup(func() { close(done) })

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_ = w.Get()
	}
}

// BenchmarkWatcherStatus covers the readiness-probe path: Status builds a
// per-field report on every call, and a Kubernetes probe calls it on a timer
// for the life of the pod.
func BenchmarkWatcherStatus(b *testing.B) {
	w := benchWatcher(b)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_ = w.Status()
	}
}

// BenchmarkWatcherHealth is the same path reduced to the single boolean a
// liveness or readiness endpoint actually needs.
func BenchmarkWatcherHealth(b *testing.B) {
	w := benchWatcher(b)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_ = w.Health()
	}
}

// BenchmarkLoad measures the one-shot path: resolve every field, decode, and
// validate. This is startup cost, paid once, but it gates how fast a process
// becomes ready.
func BenchmarkLoad(b *testing.B) {
	prov := benchProvider()
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if _, err := mamori.Load[benchConfig](context.Background(), mamori.WithProvider(prov)); err != nil {
			b.Fatalf("Load: %v", err)
		}
	}
}

// BenchmarkParseRef and BenchmarkParseRefs cover tag parsing, which runs once
// per field per Load and is the only regexp on that path.
func BenchmarkParseRef(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := mamori.ParseRef("aws-sm://prod/db#password?version=3"); err != nil {
			b.Fatalf("ParseRef: %v", err)
		}
	}
}

func BenchmarkParseRefs(b *testing.B) {
	cases := []struct {
		name string
		tag  string
	}{
		{"single", "env:PORT"},
		{"chain2", "env:PORT,aws-ps://svc/port"},
		{"chain2Spaced", "env:PORT, aws-ps://svc/port"},
		{"chain3", "env:A,env:B,aws-sm://c#k?version=2"},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := mamori.ParseRefs(tc.tag); err != nil {
					b.Fatalf("ParseRefs: %v", err)
				}
			}
		})
	}
}
