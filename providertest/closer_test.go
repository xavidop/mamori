// This file is package providertest, not providertest_test like the rest of the
// kit's own tests, because it drives checkCloserContract directly. That is
// deliberate: the checker is a pure function precisely so its verdict can be
// observed here. The testing.T wrapper around it cannot be tested the same way,
// since a t.Fatal against a hand-built &testing.T{} calls runtime.Goexit rather
// than recording a failure anywhere this test could read it.
package providertest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/xavidop/mamori"
)

// closeLeakProvider implements io.Closer but keeps resolving after Close, which
// the CloserContract case must reject.
type closeLeakProvider struct{ closed bool }

func (p *closeLeakProvider) Scheme() string { return "closeleak" }
func (p *closeLeakProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	return mamori.Value{Bytes: []byte("still alive"), Version: "1"}, nil
}
func (p *closeLeakProvider) Close() error { p.closed = true; return nil }

// closeCleanProvider satisfies the contract.
type closeCleanProvider struct {
	mu     sync.Mutex
	closed bool
}

func (p *closeCleanProvider) Scheme() string { return "closeclean" }
func (p *closeCleanProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return mamori.Value{}, fmt.Errorf("%w: closeclean: provider is closed", mamori.ErrUnavailable)
	}
	return mamori.Value{Bytes: []byte("alive"), Version: "1"}, nil
}
func (p *closeCleanProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

func TestCheckCloserContractRejectsResolveAfterClose(t *testing.T) {
	ref, err := mamori.ParseRef("closeleak://some-key")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if err := checkCloserContract(&closeLeakProvider{}, ref); err == nil {
		t.Fatal("checkCloserContract accepted a provider that resolves after Close")
	}
}

func TestCheckCloserContractAcceptsACompliantProvider(t *testing.T) {
	ref, err := mamori.ParseRef("closeclean://some-key")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if err := checkCloserContract(&closeCleanProvider{}, ref); err != nil {
		t.Fatalf("checkCloserContract rejected a compliant provider: %v", err)
	}
}
