package httpcore

import (
	"bytes"
	"container/list"
	"context"
	"sync"
)

// DefaultRevalidatorEntries is the entry ceiling used when NewRevalidator is
// given a non-positive maxEntries.
const DefaultRevalidatorEntries = 512

// Revalidator turns a repeated poll of the same ref into a conditional GET.
//
// mamori polls any provider without a native watch, which means the same value
// is fetched on every tick. Remembering the last ETag and body lets the next
// poll send If-None-Match and take a 304 with an empty body instead of the full
// payload, which is the difference between a poll that costs a megabyte and one
// that costs a few hundred bytes.
//
// Entries are bounded and evicted least-recently-used, so a large config cannot
// grow the cache without limit. Revalidator is safe for concurrent use.
type Revalidator struct {
	client     *Client
	maxEntries int

	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List // front is most recently used
}

// cacheEntry is one remembered response. It is stored by pointer in the LRU
// list, so key must travel with it for eviction.
type cacheEntry struct {
	key          string
	etag         string
	lastModified string
	body         []byte
}

// NewRevalidator returns a Revalidator over c holding at most maxEntries
// entries. A non-positive maxEntries selects DefaultRevalidatorEntries.
func NewRevalidator(c *Client, maxEntries int) *Revalidator {
	if maxEntries <= 0 {
		maxEntries = DefaultRevalidatorEntries
	}
	return &Revalidator{
		client:     c,
		maxEntries: maxEntries,
		entries:    make(map[string]*list.Element, maxEntries),
		lru:        list.New(),
	}
}

// Get performs r as a conditional GET keyed by key, which should be the ref's
// raw string so two fields reading the same ref share one entry.
//
// On a 304 the returned Response carries the cached body with NotModified set,
// so a caller can treat it exactly like a 200 and still know nothing changed.
//
// A failed request drops the entry, so a later success is never answered from a
// validator the backend has not confirmed.
func (rv *Revalidator) Get(ctx context.Context, key string, r Request) (*Response, error) {
	if etag, lastMod, ok := rv.validators(key); ok {
		if r.IfNoneMatch == "" {
			r.IfNoneMatch = etag
		}
		if r.IfModifiedSince == "" {
			r.IfModifiedSince = lastMod
		}
	}

	resp, err := rv.client.Do(ctx, r)
	if err != nil {
		rv.drop(key)
		return nil, err
	}

	if resp.NotModified {
		etag, lastMod, body, ok := rv.cached(key)
		if !ok {
			// The backend answered 304 for a validator we no longer hold, which
			// means the entry was evicted between the two halves of this call.
			// Retry unconditionally rather than returning an empty body.
			r.IfNoneMatch, r.IfModifiedSince = "", ""
			resp, err = rv.client.Do(ctx, r)
			if err != nil {
				rv.drop(key)
				return nil, err
			}
			rv.store(key, resp)
			return resp, nil
		}
		out := *resp
		out.Body = body
		// Report the validators the cache holds, not the ones the 304 carried.
		// RFC 7232 says a 304 should repeat them, but real backends, CDNs and
		// proxies especially, sometimes omit them. Copying an empty ETag makes
		// Version fall back to a body hash, so a genuinely unmodified poll
		// reports a changed Version and mamori runs a spurious update: a
		// needless PreApply, a needless OnChange, and for a rotating credential
		// a needless reconnect.
		out.ETag = etag
		out.LastModified = lastMod
		return &out, nil
	}

	rv.store(key, resp)
	return resp, nil
}

// validators returns the cached validators for key, marking it recently used.
func (rv *Revalidator) validators(key string) (etag, lastModified string, ok bool) {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	el, ok := rv.entries[key]
	if !ok {
		return "", "", false
	}
	rv.lru.MoveToFront(el)
	e := el.Value.(*cacheEntry)
	return e.etag, e.lastModified, true
}

// cached returns the entry's validators and a private copy of its body, marking
// it recently used.
//
// The body is copied because the caller receives it. Without the copy the
// Revalidator and every caller share one backing array, so a caller that decodes
// or trims in place silently changes what the next poll returns.
func (rv *Revalidator) cached(key string) (etag, lastModified string, body []byte, ok bool) {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	el, ok := rv.entries[key]
	if !ok {
		return "", "", nil, false
	}
	rv.lru.MoveToFront(el)
	e := el.Value.(*cacheEntry)
	return e.etag, e.lastModified, bytes.Clone(e.body), true
}

// store records resp under key, evicting the least recently used entry when the
// cache is over its ceiling.
func (rv *Revalidator) store(key string, resp *Response) {
	if resp.ETag == "" && resp.LastModified == "" {
		// Nothing to revalidate with next time; caching the body would only
		// consume memory for a validator that will never be sent.
		rv.drop(key)
		return
	}
	rv.mu.Lock()
	defer rv.mu.Unlock()

	// The body is copied on the way in for the same reason cached copies it on
	// the way out: the cache must own its bytes outright, or the caller that
	// received this same 200 response can write into what the cache will serve.
	if el, ok := rv.entries[key]; ok {
		e := el.Value.(*cacheEntry)
		e.etag, e.lastModified, e.body = resp.ETag, resp.LastModified, bytes.Clone(resp.Body)
		rv.lru.MoveToFront(el)
		return
	}
	el := rv.lru.PushFront(&cacheEntry{
		key:          key,
		etag:         resp.ETag,
		lastModified: resp.LastModified,
		body:         bytes.Clone(resp.Body),
	})
	rv.entries[key] = el

	for rv.lru.Len() > rv.maxEntries {
		oldest := rv.lru.Back()
		if oldest == nil {
			break
		}
		rv.lru.Remove(oldest)
		delete(rv.entries, oldest.Value.(*cacheEntry).key)
	}
}

// drop removes key's entry.
func (rv *Revalidator) drop(key string) {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	if el, ok := rv.entries[key]; ok {
		rv.lru.Remove(el)
		delete(rv.entries, key)
	}
}

// len reports the number of cached entries. It exists for tests.
func (rv *Revalidator) len() int {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	return len(rv.entries)
}
