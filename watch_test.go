package mamori

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori/secret"
)

// watchProvider is a fake WatchableProvider driven manually in tests. push sends
// a new value to all active watchers of a ref path.
type watchProvider struct {
	scheme string

	mu       sync.Mutex
	data     map[string]Value
	watchers map[string][]chan Update
}

func newWatchProvider(scheme string) *watchProvider {
	return &watchProvider{
		scheme:   scheme,
		data:     map[string]Value{},
		watchers: map[string][]chan Update{},
	}
}

func key(ref Ref) string {
	if ref.Key != "" {
		return ref.Path + "#" + ref.Key
	}
	return ref.Path
}

func (w *watchProvider) Scheme() string { return w.scheme }

func (w *watchProvider) set(path, val, version string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.data[path] = Value{Bytes: []byte(val), Version: version}
}

func (w *watchProvider) push(path, val, version string) {
	w.mu.Lock()
	v := Value{Bytes: []byte(val), Version: version}
	w.data[path] = v
	chans := append([]chan Update(nil), w.watchers[path]...)
	w.mu.Unlock()
	for _, ch := range chans {
		ch <- Update{Value: v}
	}
}

func (w *watchProvider) Resolve(_ context.Context, ref Ref) (Value, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	v, ok := w.data[key(ref)]
	if !ok {
		return Value{}, ErrNotFound
	}
	return v, nil
}

func (w *watchProvider) Watch(ctx context.Context, ref Ref) (<-chan Update, error) {
	ch := make(chan Update, 8)
	k := key(ref)
	w.mu.Lock()
	w.watchers[k] = append(w.watchers[k], ch)
	w.mu.Unlock()
	go func() {
		<-ctx.Done()
		w.mu.Lock()
		defer w.mu.Unlock()
		cur := w.watchers[k]
		for i, c := range cur {
			if c == ch {
				w.watchers[k] = append(cur[:i], cur[i+1:]...)
				break
			}
		}
		close(ch)
	}()
	return ch, nil
}

type watchConfig struct {
	Password secret.String `source:"w://prod/db#password"`
	Level    string        `source:"w://cfg/level" default:"info" validate:"oneof=debug info warn error"`
}

func TestWatchInitialAndNoStartupEvent(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("w")
	wp.set("prod/db#password", "old", "v1")
	wp.set("cfg/level", "info", "l1")

	var mu sync.Mutex
	changes := 0
	w, err := Watch[watchConfig](context.Background(),
		WithProvider(wp), WithClock(clk),
		OnChange(func(Change[watchConfig]) { mu.Lock(); changes++; mu.Unlock() }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().Password.Reveal(); got != "old" {
		t.Fatalf("initial password = %q, want old", got)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if changes != 0 {
		t.Fatalf("OnChange fired %d times on startup, want 0", changes)
	}
}

func TestWatchAppliesValidUpdate(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("w")
	wp.set("prod/db#password", "old", "v1")
	wp.set("cfg/level", "info", "l1")

	got := make(chan Change[watchConfig], 1)
	w, err := Watch[watchConfig](context.Background(),
		WithProvider(wp), WithClock(clk),
		OnChange(func(ev Change[watchConfig]) { got <- ev }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	wp.push("prod/db#password", "new", "v2")
	// Let the update reach the reconciler, then advance past the debounce window.
	waitPending(clk)
	clk.Advance(defaultDebounce)

	select {
	case ev := <-got:
		if ev.New.Password.Reveal() != "new" {
			t.Errorf("New.Password = %q, want new", ev.New.Password.Reveal())
		}
		if ev.Old.Password.Reveal() != "old" {
			t.Errorf("Old.Password = %q, want old", ev.Old.Password.Reveal())
		}
		if !ev.Changed("Password") {
			t.Errorf("Changed(Password) = false")
		}
		if ev.Changed("Level") {
			t.Errorf("Changed(Level) = true, want false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnChange did not fire for valid update")
	}
	if w.Get().Password.Reveal() != "new" {
		t.Errorf("Get() password = %q, want new", w.Get().Password.Reveal())
	}
}

func TestWatchRejectsInvalidUpdateAtomically(t *testing.T) {
	clk := NewFakeClock(time.Time{})
	wp := newWatchProvider("w")
	wp.set("prod/db#password", "old", "v1")
	wp.set("cfg/level", "info", "l1")

	changed := make(chan Change[watchConfig], 1)
	errs := make(chan error, 1)
	w, err := Watch[watchConfig](context.Background(),
		WithProvider(wp), WithClock(clk),
		OnChange(func(ev Change[watchConfig]) { changed <- ev }),
		OnError(func(e error) { errs <- e }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	wp.push("cfg/level", "BOGUS", "l2") // fails oneof validation
	waitPending(clk)
	clk.Advance(defaultDebounce)

	select {
	case e := <-errs:
		var ve *ValidationError
		if !errors.As(e, &ve) {
			t.Fatalf("error = %T, want *ValidationError", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnError did not fire for invalid update")
	}
	select {
	case <-changed:
		t.Fatal("OnChange fired for invalid update")
	default:
	}
	if w.Get().Level != "info" {
		t.Errorf("Get().Level = %q, want info (last good)", w.Get().Level)
	}
}

// waitPending gives the reconciler goroutine a moment to consume the pushed
// update and arm its debounce timer against the fake clock before we advance.
func waitPending(_ *FakeClock) { time.Sleep(30 * time.Millisecond) }
