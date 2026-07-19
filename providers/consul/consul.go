// Package consul implements a mamori provider for the HashiCorp Consul KV store.
//
// The scheme is "consul" and the ref grammar is:
//
//	consul://<kv-path>[#json-key]
//
// The KV path is looked up with the Consul HTTP API. The raw value bytes become
// Value.Bytes and the entry's ModifyIndex becomes Value.Version, giving cheap,
// native change detection. When a #json-key fragment is present the stored value
// is treated as a JSON object and the named field is selected with
// mamori.SelectKey, identically to every other provider.
//
//	DBPassword string `source:"consul://config/app/db#password"`
//	LogLevel   string `source:"consul://config/app/log_level"`
//
// Consul KV holds configuration, not managed secrets, so resolved values are not
// marked Sensitive. Wrap fields in secret.String if you want redaction anyway.
//
// The provider implements mamori.WatchableProvider using Consul blocking
// queries: it re-issues KV.Get with a WaitIndex set to the last-seen
// ModifyIndex, so the call blocks server-side until the key changes (or the wait
// time elapses) and an Update is emitted the instant the value moves.
package consul

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "consul"

// defaultWaitTime bounds a single blocking query. Consul aborts the in-flight
// request as soon as the context is cancelled, so a long wait costs nothing in
// responsiveness while keeping the number of round-trips low.
const defaultWaitTime = 5 * time.Minute

// watchErrBackoff is how long the watch loop pauses after a transient error
// before retrying, so a persistent failure does not spin.
const watchErrBackoff = 500 * time.Millisecond

// kvAPI is the minimal subset of *api.KV the provider depends on. The real
// *api.KV returned by (*api.Client).KV satisfies it directly, and tests inject
// an in-memory fake implementing the same shape (including blocking-query
// semantics) so the conformance kit runs without a live Consul.
type kvAPI interface {
	Get(key string, q *api.QueryOptions) (*api.KVPair, *api.QueryMeta, error)
}

// Provider resolves consul:// refs against a Consul KV store. It is safe for
// concurrent use. The underlying client is built lazily on first use from the
// ambient Consul environment (CONSUL_HTTP_ADDR, CONSUL_HTTP_TOKEN, ...) unless a
// client is injected via WithClient or explicit options are supplied.
type Provider struct {
	address  string
	token    string
	waitTime time.Duration

	mu sync.Mutex
	kv kvAPI // resolved client (injected or lazily built)
}

// Option configures a Provider.
type Option func(*Provider)

// WithAddress overrides the Consul HTTP address (default: CONSUL_HTTP_ADDR or
// 127.0.0.1:8500).
func WithAddress(address string) Option {
	return func(p *Provider) { p.address = address }
}

// WithToken sets the ACL token used for requests (default: CONSUL_HTTP_TOKEN).
func WithToken(token string) Option {
	return func(p *Provider) { p.token = token }
}

// WithWaitTime overrides the maximum duration a single blocking-query Watch call
// blocks server-side before returning and re-issuing. It does not affect how
// quickly a change is observed (that is immediate) nor how quickly Watch reacts
// to context cancellation.
func WithWaitTime(d time.Duration) Option {
	return func(p *Provider) { p.waitTime = d }
}

// WithClient injects a pre-configured *api.Client, bypassing lazy construction.
// Use it when you build the Consul client yourself (custom TLS, datacenter,
// namespace, ...).
func WithClient(c *api.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.kv = c.KV()
		}
	}
}

// withKV injects a bare kvAPI. Unexported: used by tests to supply an in-memory
// fake.
func withKV(kv kvAPI) Option {
	return func(p *Provider) { p.kv = kv }
}

// New constructs a Consul KV provider. The client is created lazily on first
// Resolve/Watch, so New never fails and never contacts Consul.
func New(opts ...Option) *Provider {
	p := &Provider{waitTime: defaultWaitTime}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// init registers a lazily-initialized provider so `import _` wiring works from
// ambient Consul configuration. Users who need explicit config call
// mamori.WithProvider(consul.New(consul.WithAddress("..."))).
func init() { mamori.Register(New()) }

// Scheme returns "consul".
func (p *Provider) Scheme() string { return scheme }

// client returns the KV handle, building it lazily from the ambient Consul
// configuration on first use. Concurrent callers share one client.
func (p *Provider) client() (kvAPI, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.kv != nil {
		return p.kv, nil
	}
	cfg := api.DefaultConfig() // reads CONSUL_HTTP_ADDR / CONSUL_HTTP_TOKEN / etc.
	if p.address != "" {
		cfg.Address = p.address
	}
	if p.token != "" {
		cfg.Token = p.token
	}
	c, err := api.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("consul: build client: %w", err)
	}
	p.kv = c.KV()
	return p.kv, nil
}

// Resolve fetches the current value for ref from Consul KV. A missing key yields
// an error satisfying errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	kv, err := p.client()
	if err != nil {
		return mamori.Value{}, err
	}
	q := (&api.QueryOptions{}).WithContext(ctx)
	pair, _, err := kv.Get(ref.Path, q)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("consul: get %q: %w", ref.Path, err)
	}
	return valueFor(pair, ref)
}

// Watch implements mamori.WatchableProvider using Consul blocking queries. It
// emits the current value as a baseline, then blocks on KV.Get with a WaitIndex
// equal to the last-seen ModifyIndex, emitting a fresh Update each time the entry
// changes. The channel is closed when ctx is cancelled; the goroutine never
// leaks because every blocking call is bound to ctx.
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	kv, err := p.client()
	if err != nil {
		return nil, err
	}

	ch := make(chan mamori.Update, 1)
	go func() {
		defer close(ch)

		emit := func(u mamori.Update) bool {
			select {
			case ch <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}

		var lastIndex uint64
		for {
			if ctx.Err() != nil {
				return
			}
			q := (&api.QueryOptions{
				WaitIndex: lastIndex,
				WaitTime:  p.waitTime,
			}).WithContext(ctx)

			pair, meta, err := kv.Get(ref.Path, q)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				if !emit(mamori.Update{Err: fmt.Errorf("consul: watch %q: %w", ref.Path, err)}) {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(watchErrBackoff):
				}
				continue
			}

			newIndex := lastIndex
			if meta != nil {
				newIndex = meta.LastIndex
			}
			// Consul can reset its index (e.g. after a restore); when the index
			// goes backwards, start over so we do not block forever.
			if newIndex < lastIndex {
				lastIndex = 0
				continue
			}
			// A blocking query that timed out returns the same index and no new
			// data; loop without emitting. The first pass (lastIndex == 0) always
			// emits the baseline.
			if lastIndex != 0 && newIndex == lastIndex {
				continue
			}
			lastIndex = newIndex

			if !emit(update(pair, ref)) {
				return
			}
		}
	}()
	return ch, nil
}

// update turns a KV pair into a watch Update, mapping a missing key to a
// not-found error carried on the Update.
func update(pair *api.KVPair, ref mamori.Ref) mamori.Update {
	v, err := valueFor(pair, ref)
	return mamori.Update{Value: v, Err: err}
}

// valueFor converts a Consul KV pair into a mamori.Value, applying #json-key
// selection when requested. A nil pair means the key does not exist.
func valueFor(pair *api.KVPair, ref mamori.Ref) (mamori.Value, error) {
	if pair == nil {
		return mamori.Value{}, fmt.Errorf("consul: key %q: %w", ref.Path, mamori.ErrNotFound)
	}
	b := pair.Value
	if ref.Key != "" {
		sel, err := mamori.SelectKey(b, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		b = sel
	}
	return mamori.Value{
		Bytes:     b,
		Version:   strconv.FormatUint(pair.ModifyIndex, 10),
		Sensitive: false,
	}, nil
}
