package providertest_test

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeBackend is a correct in-memory provider used to self-test the conformance
// kit: it must pass every check.
type fakeBackend struct {
	mu       sync.Mutex
	data     map[string]string
	version  map[string]int
	watchers map[string][]chan mamori.Update
}

func newFake() *fakeBackend {
	return &fakeBackend{
		data:     map[string]string{},
		version:  map[string]int{},
		watchers: map[string][]chan mamori.Update{},
	}
}

func (f *fakeBackend) Scheme() string { return "fake" }

func (f *fakeBackend) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[ref.Path]
	if !ok {
		return mamori.Value{}, mamori.ErrNotFound
	}
	return mamori.Value{Bytes: []byte(v), Version: strconv.Itoa(f.version[ref.Path])}, nil
}

func (f *fakeBackend) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	ch := make(chan mamori.Update, 4)
	f.mu.Lock()
	f.watchers[ref.Path] = append(f.watchers[ref.Path], ch)
	if v, ok := f.data[ref.Path]; ok {
		ch <- mamori.Update{Value: mamori.Value{Bytes: []byte(v), Version: strconv.Itoa(f.version[ref.Path])}}
	}
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		cur := f.watchers[ref.Path]
		for i, c := range cur {
			if c == ch {
				f.watchers[ref.Path] = append(cur[:i], cur[i+1:]...)
				break
			}
		}
		close(ch)
	}()
	return ch, nil
}

func (f *fakeBackend) set(key, val string) {
	f.mu.Lock()
	f.data[key] = val
	f.version[key]++
	chans := append([]chan mamori.Update(nil), f.watchers[key]...)
	ver := strconv.Itoa(f.version[key])
	f.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- mamori.Update{Value: mamori.Value{Bytes: []byte(val), Version: ver}}:
		default:
		}
	}
}

func TestConformanceKitPassesForCorrectProvider(t *testing.T) {
	backend := newFake()
	providertest.Run(t, providertest.Config{
		New:    func() mamori.Provider { return backend },
		Ref:    func(key string) string { return "fake://" + key },
		Seed:   func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
		Mutate: func(_ context.Context, key, val string) error { backend.set(key, val); return nil },
	})
}
