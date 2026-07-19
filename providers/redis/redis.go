// Package redis implements a mamori provider for the Redis key/value store.
//
// The scheme is "redis" and the ref grammar is:
//
//	redis://<key>[#json-key]
//
// The key is fetched with a Redis GET. The raw string value becomes
// Value.Bytes. Redis has no per-key revision counter, so the Version is
// synthesized from the value bytes with mamori.VersionHash, giving mamori cheap
// change detection without a byte-by-byte comparison. When a #json-key fragment
// is present the stored value is treated as a JSON object and the named field is
// selected with mamori.SelectKey, identically to every other provider.
//
//	LogLevel   string `source:"redis://config/app/log_level"`
//	DBPassword string `source:"redis://config/app/db#password"`
//
// Redis is typically used for configuration and caches rather than managed
// secrets, so resolved values are not marked Sensitive. Wrap fields in
// secret.String if you want redaction anyway.
//
// # Native watch (keyspace notifications)
//
// The provider implements mamori.WatchableProvider using Redis keyspace
// notifications. It PSUBSCRIBEs to __keyspace@<db>__:<key> and, on every
// notification for the key (set, del, expired, ...), re-runs GET and emits an
// Update. This is the idiomatic Redis push mechanism, not a polling ticker.
//
// Keyspace notifications are OFF by default on a Redis server. The server must
// be configured with, for example:
//
//	CONFIG SET notify-keyspace-events KEA
//
// or the equivalent notify-keyspace-events entry in redis.conf. Without it the
// server never publishes notifications and the watch will only ever deliver the
// baseline value (Resolve and polling still work). "KEA" enables keyspace (K)
// events for all (A) event classes; a narrower mask such as "K$g" (keyspace,
// string and generic commands) is also sufficient.
package redis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	goredis "github.com/redis/go-redis/v9"
	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "redis"

// defaultAddr is used when neither REDIS_URL, WithURL, nor WithAddr is supplied.
const defaultAddr = "127.0.0.1:6379"

// subscription is the minimal pub/sub surface the watcher depends on. The real
// *goredis.PubSub is adapted to it (its Channel method is variadic and so does
// not satisfy this interface directly), and tests inject an in-memory fake that
// implements the same shape.
type subscription interface {
	// Channel returns the stream of messages for the subscribed patterns.
	Channel() <-chan *goredis.Message
	// Close ends the subscription and releases its resources (goroutines).
	Close() error
}

// redisAPI is the minimal subset of Redis operations the provider depends on.
// The real go-redis client is adapted to it via universalAdapter, and tests
// inject an in-memory fake implementing the same shape (GET plus keyspace-style
// pub/sub) so the conformance kit runs without a live Redis.
type redisAPI interface {
	Get(ctx context.Context, key string) *goredis.StringCmd
	PSubscribe(ctx context.Context, patterns ...string) subscription
	Close() error
}

// universalAdapter wraps a goredis.UniversalClient (the interface satisfied by
// *goredis.Client, *goredis.ClusterClient, and *goredis.Ring) so it satisfies
// redisAPI, in particular wrapping *goredis.PubSub in pubsubAdapter.
type universalAdapter struct{ c goredis.UniversalClient }

func (a universalAdapter) Get(ctx context.Context, key string) *goredis.StringCmd {
	return a.c.Get(ctx, key)
}

func (a universalAdapter) PSubscribe(ctx context.Context, patterns ...string) subscription {
	return pubsubAdapter{ps: a.c.PSubscribe(ctx, patterns...)}
}

func (a universalAdapter) Close() error { return a.c.Close() }

// pubsubAdapter adapts *goredis.PubSub (whose Channel method is variadic) to the
// subscription interface.
type pubsubAdapter struct{ ps *goredis.PubSub }

func (p pubsubAdapter) Channel() <-chan *goredis.Message { return p.ps.Channel() }
func (p pubsubAdapter) Close() error                     { return p.ps.Close() }

// Provider resolves redis:// refs against a Redis server. It is safe for
// concurrent use. The underlying client is built lazily on first use from
// REDIS_URL (or the configured address) unless a client is injected via
// WithClient.
type Provider struct {
	url  string
	addr string
	db   int

	mu     sync.Mutex
	client redisAPI // resolved client (injected or lazily built)
}

// Option configures a Provider.
type Option func(*Provider)

// WithURL sets the connection URL (redis://[:password@]host:port/db). It takes
// precedence over the REDIS_URL environment variable and over WithAddr.
func WithURL(url string) Option {
	return func(p *Provider) { p.url = url }
}

// WithAddr sets the Redis host:port to connect to (default 127.0.0.1:6379). It
// is ignored when a URL (WithURL or REDIS_URL) or a client (WithClient) is
// supplied.
func WithAddr(addr string) Option {
	return func(p *Provider) { p.addr = addr }
}

// WithDB selects the Redis logical database number (default 0). It also
// determines the __keyspace@<db>__ channel the watcher subscribes to, so it must
// match the database the watched keys live in. When set it overrides any DB
// encoded in a URL.
func WithDB(db int) Option {
	return func(p *Provider) { p.db = db }
}

// WithClient injects a pre-configured go-redis client, bypassing lazy
// construction. Any type satisfying goredis.UniversalClient is accepted
// (*goredis.Client, *goredis.ClusterClient, *goredis.Ring), so callers can
// supply custom TLS, auth, pooling, or cluster configuration. Set WithDB to
// match the client's database so watch subscribes to the correct keyspace
// channel.
func WithClient(c goredis.UniversalClient) Option {
	return func(p *Provider) {
		if c != nil {
			p.client = universalAdapter{c: c}
		}
	}
}

// withRedisAPI injects a bare redisAPI. Unexported: used by tests to supply an
// in-memory fake.
func withRedisAPI(c redisAPI) Option {
	return func(p *Provider) { p.client = c }
}

// New constructs a Redis provider. The client is created lazily on first
// Resolve/Watch, so New never fails and never contacts Redis.
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// init registers a lazily-initialized provider so `import _` wiring works from
// the ambient REDIS_URL. Users who need explicit config call
// mamori.WithProvider(redis.New(redis.WithAddr("..."))).
func init() { mamori.Register(New()) }

// Scheme returns "redis".
func (p *Provider) Scheme() string { return scheme }

// getClient returns the Redis client, building it lazily from REDIS_URL (or the
// configured address) on first use. Concurrent callers share one client. It also
// records the effective database number so Watch subscribes to the matching
// __keyspace@<db>__ channel.
func (p *Provider) getClient() (redisAPI, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}

	rawURL := p.url
	if rawURL == "" {
		rawURL = os.Getenv("REDIS_URL")
	}

	var opts *goredis.Options
	if rawURL != "" {
		o, err := goredis.ParseURL(rawURL)
		if err != nil {
			return nil, fmt.Errorf("redis: parse url: %w", err)
		}
		opts = o
	} else {
		addr := p.addr
		if addr == "" {
			addr = defaultAddr
		}
		opts = &goredis.Options{Addr: addr}
	}
	// An explicit WithDB overrides any DB encoded in the URL; otherwise adopt the
	// URL's DB so the watcher's keyspace channel matches.
	if p.db != 0 {
		opts.DB = p.db
	}
	p.db = opts.DB

	p.client = universalAdapter{c: goredis.NewClient(opts)}
	return p.client, nil
}

// database returns the effective Redis database number under lock.
func (p *Provider) database() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.db
}

// Resolve fetches the current value for ref from Redis. A missing key yields an
// error satisfying errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	c, err := p.getClient()
	if err != nil {
		return mamori.Value{}, err
	}
	return resolveWith(ctx, c, ref)
}

// resolveWith performs a GET against c and maps it to a mamori.Value, applying
// #json-key selection when requested.
func resolveWith(ctx context.Context, c redisAPI, ref mamori.Ref) (mamori.Value, error) {
	raw, err := c.Get(ctx, ref.Path).Bytes()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			return mamori.Value{}, fmt.Errorf("redis: key %q: %w", ref.Path, mamori.ErrNotFound)
		}
		return mamori.Value{}, fmt.Errorf("redis: get %q: %w", ref.Path, err)
	}
	// The Version identifies the Redis key's revision, so hash the full stored
	// value (before any #key selection); a change to any field changes it.
	version := mamori.VersionHash(raw)

	out := raw
	if ref.Key != "" {
		sel, err := mamori.SelectKey(raw, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		out = sel
	}
	return mamori.Value{
		Bytes:     out,
		Version:   version,
		Sensitive: false,
	}, nil
}

// Watch implements mamori.WatchableProvider using Redis keyspace notifications.
// It PSUBSCRIBEs to __keyspace@<db>__:<key>, emits the current value as a
// baseline, then re-runs GET and emits a fresh Update on every notification for
// the key. The channel is closed when ctx is cancelled; the goroutine never
// leaks because the subscription is always closed on exit.
//
// The Redis server must have keyspace notifications enabled (e.g. CONFIG SET
// notify-keyspace-events KEA). Without it the baseline is delivered but no
// change notifications ever arrive.
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	c, err := p.getClient()
	if err != nil {
		return nil, err
	}
	channel := fmt.Sprintf("__keyspace@%d__:%s", p.database(), ref.Path)

	// Subscribe before emitting the baseline so no notification is missed between
	// the baseline GET and the start of the read loop.
	sub := c.PSubscribe(ctx, channel)
	msgs := sub.Channel()

	out := make(chan mamori.Update, 1)
	go func() {
		defer close(out)
		defer func() { _ = sub.Close() }()

		emit := func(u mamori.Update) bool {
			select {
			case out <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}

		// Baseline: emit the current value (or its not-found) immediately.
		v, rerr := resolveWith(ctx, c, ref)
		if !emit(mamori.Update{Value: v, Err: rerr}) {
			return
		}

		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-msgs:
				if !ok {
					return
				}
				// The message payload is the event name (set/del/expired/...); we
				// re-GET the key regardless to obtain the authoritative value.
				v, rerr := resolveWith(ctx, c, ref)
				if !emit(mamori.Update{Value: v, Err: rerr}) {
					return
				}
			}
		}
	}()
	return out, nil
}
