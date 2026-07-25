// Package mamoritest provides an in-memory, scriptable mamori.Provider for
// testing application code that consumes mamori. Where providertest serves
// provider authors, mamoritest serves application authors: it lets you drive a
// real mamori.Watch through value changes, deletions, and failures
// deterministically, without a real backend.
package mamoritest

import (
	"context"
	"sync"

	"github.com/xavidop/mamori"
)

// watchBuf is the buffer size for channels handed out by Watch. It is sized
// generously so a single Set/Del/Fail/Clear call from a test never has to wait
// on a real Watcher's forwarder goroutine to drain a prior update first.
const watchBuf = 8

// subscription is one outstanding Watch registration. It owns the channel
// handed back to the caller and guards that channel's lifecycle (send vs.
// close) with its own mutex so a send from publish can never race a close from
// the deregistration goroutine spawned by Watch: without that guard, a send
// could be scheduled by the Go runtime after the channel was closed and panic.
// Using a per-subscription mutex (rather than Provider.mu) keeps that guard
// cheap and keeps a slow consumer on one key from blocking unrelated Provider
// calls.
type subscription struct {
	mu     sync.Mutex
	ch     chan mamori.Update
	closed bool
}

// send delivers up unless the subscription has already ended. It never blocks
// on a slow or gone consumer: a full buffer or an ended watch both result in
// the update being dropped rather than the caller stalling. mamoritest sizes
// the buffer generously and a real Watcher drains it continuously, so a drop
// only happens against a raw Watch channel a caller isn't reading.
func (s *subscription) send(up mamori.Update) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- up:
	default:
	}
}

// end marks the subscription over and closes its channel, satisfying
// mamori.WatchableProvider's contract that the channel is closed when the
// watch ends. It is idempotent and safe to race with send: whichever of the
// two acquires mu first wins, and send checks closed before touching ch, so
// ch is never sent to after it is closed.
func (s *subscription) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}

// Provider is an in-memory, scriptable mamori.Provider. It implements
// mamori.WatchableProvider, so changes pushed with Set, Del, and Fail are
// delivered to a Watch natively rather than by polling. It is safe for
// concurrent use.
type Provider struct {
	scheme string

	mu     sync.Mutex
	values map[string][]byte
	fails  map[string]error
	subs   map[string][]*subscription
}

// NewProvider returns a scriptable provider registered for the given scheme.
// Pass it to Load or Watch with mamori.WithProvider.
func NewProvider(scheme string) *Provider {
	return &Provider{
		scheme: scheme,
		values: map[string][]byte{},
		fails:  map[string]error{},
		subs:   map[string][]*subscription{},
	}
}

// Scheme implements mamori.Provider.
func (p *Provider) Scheme() string { return p.scheme }

// refKey derives the lookup key for a ref the same way watchProvider does in
// the parent package's tests: the path, plus a "#key" suffix when the ref
// selects a fragment. Set, SetBytes, Del, Fail, and Clear take this same
// string directly, so a test's key argument must match the ref's Path (or
// Path#Key) exactly.
func refKey(ref mamori.Ref) string {
	if ref.Key != "" {
		return ref.Path + "#" + ref.Key
	}
	return ref.Path
}

// Resolve implements mamori.Provider. It honors ctx cancellation first (so a
// canceled Load or reconcile fails fast, matching every real provider), then
// an injected Fail error, then reports mamori.ErrNotFound for an absent or
// Del'd key, and otherwise returns the stored value with a Version derived
// from mamori.VersionHash so equal values compare equal, as real providers do.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	k := refKey(ref)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err, ok := p.fails[k]; ok {
		return mamori.Value{}, err
	}
	b, ok := p.values[k]
	if !ok {
		return mamori.Value{}, mamori.ErrNotFound
	}
	return valueOf(b), nil
}

// currentUpdateLocked reports the Update that reflects key's present state: an
// injected failure if one is active, else the stored value, else false when
// neither is set. Callers must hold p.mu.
func (p *Provider) currentUpdateLocked(k string) (mamori.Update, bool) {
	if err, ok := p.fails[k]; ok {
		return mamori.Update{Err: err}, true
	}
	if b, ok := p.values[k]; ok {
		return mamori.Update{Value: valueOf(b)}, true
	}
	return mamori.Update{}, false
}

// valueOf builds a mamori.Value from stored bytes, copying b so neither side
// can mutate the other's memory through it, and hashing it for Version so
// unchanged values compare equal across calls.
func valueOf(b []byte) mamori.Value {
	cp := append([]byte(nil), b...)
	return mamori.Value{Bytes: cp, Version: mamori.VersionHash(cp)}
}

// Watch implements mamori.WatchableProvider. It registers a buffered channel
// for ref's key, delivers the key's current state as an immediate baseline
// (mirroring how a real native-watch backend replays current state to a new
// subscriber), and deregisters and closes the channel when ctx is done, with
// no goroutine left running afterward. The registration and deregistration
// mechanics mirror the watchProvider fake in the parent package's
// watch_test.go, which this kit's own goleak-checked tests were modeled on.
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	k := refKey(ref)
	sub := &subscription{ch: make(chan mamori.Update, watchBuf)}

	p.mu.Lock()
	baseline, hasBaseline := p.currentUpdateLocked(k)
	if hasBaseline {
		// The channel was just created with a full empty buffer, so this send
		// cannot block. Sending the baseline before sub is appended to p.subs
		// (below, still under p.mu) means no concurrent Set/Del/Fail/Clear can
		// publish to it yet: sub is not reachable through p.subs, so this
		// goroutine is the only one that can ever touch sub.ch at this point.
		// That guarantees the baseline is strictly the first update a consumer
		// observes and makes the direct send safe without going through
		// subscription.send's guard, since nothing else can be racing it.
		sub.ch <- baseline
	}
	p.subs[k] = append(p.subs[k], sub)
	p.mu.Unlock()

	go func() {
		<-ctx.Done()
		p.mu.Lock()
		cur := p.subs[k]
		for i, s := range cur {
			if s == sub {
				p.subs[k] = append(cur[:i], cur[i+1:]...)
				break
			}
		}
		p.mu.Unlock()
		sub.end()
	}()

	return sub.ch, nil
}

// snapshotSubs returns the active subscriptions for key under the lock, so
// callers can publish to them without holding p.mu during the (potentially
// slow) sends.
func (p *Provider) snapshotSubs(key string) []*subscription {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*subscription(nil), p.subs[key]...)
}

// publish delivers up to every active watcher of key.
func (p *Provider) publish(key string, up mamori.Update) {
	for _, s := range p.snapshotSubs(key) {
		s.send(up)
	}
}

// Set stores val as the value for key and pushes it to every active watcher of
// key. key must match the Path (or Path#Key) of the refs that name it.
func (p *Provider) Set(key, val string) {
	p.SetBytes(key, []byte(val))
}

// SetBytes stores b as the value for key and pushes it to every active watcher
// of key. It is the byte-oriented counterpart to Set, for binary payloads.
func (p *Provider) SetBytes(key string, b []byte) {
	cp := append([]byte(nil), b...)
	p.mu.Lock()
	p.values[key] = cp
	p.mu.Unlock()
	p.publish(key, mamori.Update{Value: valueOf(cp)})
}

// Del removes key's value, so a subsequent Resolve reports mamori.ErrNotFound,
// and pushes an Update carrying that same error to every active watcher of
// key. This mirrors how a real native-watch backend (Kubernetes informers,
// Firestore, Redis keyspace notifications) delivers a delete: as
// ErrNotFound on the watch channel, not as a channel close. The engine's
// handleErr (reconciler.go) tolerates that ErrNotFound for a field with a
// `default:` or `optional` tag exactly as it tolerates an absent key at Load
// time, so deleting a defaulted/optional field's key falls back to its
// default instead of failing a consumer's test; a required field's key with
// no default surfaces the not-found as an unhealthy, erroring field, again
// matching Load's behavior for a required-but-missing key.
func (p *Provider) Del(key string) {
	p.mu.Lock()
	delete(p.values, key)
	p.mu.Unlock()
	p.publish(key, mamori.Update{Err: mamori.ErrNotFound})
}

// Fail makes every future Resolve of key return err, and pushes an Update
// carrying err to every active watcher of key, until Clear is called for key.
// It lets a test exercise how application code (or mamori's own reconciler)
// reacts to a provider-side failure - permission denied, rate limited,
// unavailable - deterministically.
func (p *Provider) Fail(key string, err error) {
	p.mu.Lock()
	p.fails[key] = err
	p.mu.Unlock()
	p.publish(key, mamori.Update{Err: err})
}

// Clear removes the injected error for key, so a subsequent Resolve succeeds
// (if a value is stored) or reports mamori.ErrNotFound (if not), and pushes an
// Update reflecting that restored state to every active watcher of key,
// mirroring how a real backend's transient failure recovers and re-delivers.
func (p *Provider) Clear(key string) {
	p.mu.Lock()
	delete(p.fails, key)
	up, ok := p.currentUpdateLocked(key)
	p.mu.Unlock()
	if !ok {
		up = mamori.Update{Err: mamori.ErrNotFound}
	}
	p.publish(key, up)
}
