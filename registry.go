package mamori

import (
	"fmt"
	"sort"
	"sync"
)

var (
	registryMu sync.RWMutex
	registry   = map[string]Provider{}
)

// Register makes a provider available under its Scheme(), following the
// database/sql driver pattern. It is normally called from a provider package's
// init function. Register panics if a provider for the same scheme is already
// registered (fail fast at process init) or if p is nil.
func Register(p Provider) {
	if p == nil {
		panic("mamori: Register(nil)")
	}
	scheme := p.Scheme()
	if scheme == "" {
		panic("mamori: Register provider with empty scheme")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[scheme]; dup {
		panic(fmt.Sprintf("mamori: Register called twice for scheme %q", scheme))
	}
	registry[scheme] = p
}

// providerFor returns the registered provider for scheme.
func providerFor(scheme string) (Provider, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[scheme]
	return p, ok
}

// RegisteredSchemes returns the sorted list of registered provider schemes. It
// is useful for diagnostics and for the docs/provider gallery.
func RegisteredSchemes() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for s := range registry {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// unregister removes a scheme. Test-only helper (not exported).
func unregister(scheme string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, scheme)
}
