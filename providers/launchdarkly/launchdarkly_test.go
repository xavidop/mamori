package launchdarkly

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/launchdarkly/go-sdk-common/v3/ldcontext"
	"github.com/launchdarkly/go-sdk-common/v3/ldreason"
	"github.com/launchdarkly/go-sdk-common/v3/ldvalue"
	"github.com/launchdarkly/go-server-sdk/v7/interfaces"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// fakeClient is an in-memory implementation of ldEvaluator. It holds a set of
// flag values, evaluates them with a not-found reason for unseeded flags, and
// supports native value-change subscriptions: a change to a flag's value pushes
// a FlagValueChangeEvent to every active listener of that flag. A listener's
// channel is closed on RemoveFlagValueChangeListener (mirroring the SDK's flag
// tracker), and the fake itself starts no goroutines, so the conformance kit's
// goroutine-leak check exercises only the provider.
type fakeClient struct {
	mu        sync.Mutex
	flags     map[string]ldvalue.Value
	listeners map[*fakeListener]struct{}
}

type fakeListener struct {
	flagKey string
	ch      chan interfaces.FlagValueChangeEvent
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		flags:     map[string]ldvalue.Value{},
		listeners: map[*fakeListener]struct{}{},
	}
}

// set stores val for flagKey and, if the value actually changed, pushes a
// value-change event to each active listener of that flag.
func (f *fakeClient) set(flagKey string, val ldvalue.Value) {
	f.mu.Lock()
	defer f.mu.Unlock()
	old, existed := f.flags[flagKey]
	f.flags[flagKey] = val
	if existed && old.Equal(val) {
		return
	}
	oldVal := ldvalue.Null()
	if existed {
		oldVal = old
	}
	for l := range f.listeners {
		if l.flagKey != flagKey {
			continue
		}
		// The channel is buffered; a non-blocking send keeps set() from blocking
		// on a slow or absent consumer.
		select {
		case l.ch <- interfaces.FlagValueChangeEvent{Key: flagKey, OldValue: oldVal, NewValue: val}:
		default:
		}
	}
}

// setString is a convenience used by the conformance kit, which seeds string
// values.
func (f *fakeClient) setString(flagKey, val string) { f.set(flagKey, ldvalue.String(val)) }

// JSONVariationDetail implements ldEvaluator. An unseeded flag yields an ERROR
// reason of kind FLAG_NOT_FOUND (and a non-nil error) so the provider's
// reason-based not-found detection is exercised.
func (f *fakeClient) JSONVariationDetail(flagKey string, _ ldcontext.Context, defaultVal ldvalue.Value) (ldvalue.Value, ldreason.EvaluationDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	val, ok := f.flags[flagKey]
	if !ok {
		detail := ldreason.NewEvaluationDetailForError(ldreason.EvalErrorFlagNotFound, defaultVal)
		return defaultVal, detail, errors.New("feature flag not found")
	}
	detail := ldreason.NewEvaluationDetail(val, 0, ldreason.NewEvalReasonFallthrough())
	return val, detail, nil
}

// AddFlagValueChangeListener implements ldEvaluator.
func (f *fakeClient) AddFlagValueChangeListener(flagKey string, _ ldcontext.Context, _ ldvalue.Value) <-chan interfaces.FlagValueChangeEvent {
	l := &fakeListener{flagKey: flagKey, ch: make(chan interfaces.FlagValueChangeEvent, 16)}
	f.mu.Lock()
	f.listeners[l] = struct{}{}
	f.mu.Unlock()
	return l.ch
}

// RemoveFlagValueChangeListener implements ldEvaluator. It unregisters and
// closes the matching listener channel.
func (f *fakeClient) RemoveFlagValueChangeListener(listener <-chan interfaces.FlagValueChangeEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for l := range f.listeners {
		if (<-chan interfaces.FlagValueChangeEvent)(l.ch) == listener {
			delete(f.listeners, l)
			close(l.ch)
			return
		}
	}
}

// Close implements ldEvaluator, closing any remaining listener channels.
func (f *fakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for l := range f.listeners {
		delete(f.listeners, l)
		close(l.ch)
	}
	return nil
}

var _ ldEvaluator = (*fakeClient)(nil)

func TestConformance(t *testing.T) {
	fake := newFakeClient()
	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider {
			return New(withClient(fake))
		},
		Ref:    func(key string) string { return "launchdarkly://" + key },
		Seed:   func(_ context.Context, key, val string) error { fake.setString(key, val); return nil },
		Mutate: func(_ context.Context, key, val string) error { fake.setString(key, val); return nil },

		EventuallyTimeout: 3 * time.Second,
	})
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != scheme {
		t.Fatalf("Scheme() = %q, want %q", got, scheme)
	}
}

func TestDefaultContextKey(t *testing.T) {
	if got := New().contextKey; got != defaultContextKey {
		t.Fatalf("default context key = %q, want %q", got, defaultContextKey)
	}
	if got := New(WithContextKey("tenant-a")).contextKey; got != "tenant-a" {
		t.Fatalf("context key = %q, want tenant-a", got)
	}
	// An empty WithContextKey keeps the default.
	if got := New(WithContextKey("")).contextKey; got != defaultContextKey {
		t.Fatalf("context key = %q, want default %q", got, defaultContextKey)
	}
	if got := New(WithContextKey("x")).evalContext().Key(); got != "x" {
		t.Fatalf("evalContext key = %q, want x", got)
	}
}

func TestResolveBool(t *testing.T) {
	fake := newFakeClient()
	fake.set("kill-switch", ldvalue.Bool(true))
	fake.set("legacy-off", ldvalue.Bool(false))
	p := New(withClient(fake))

	if got := resolveStr(t, p, "launchdarkly://kill-switch"); got != "true" {
		t.Fatalf("bool true = %q, want true", got)
	}
	if got := resolveStr(t, p, "launchdarkly://legacy-off"); got != "false" {
		t.Fatalf("bool false = %q, want false", got)
	}
}

func TestResolveString(t *testing.T) {
	fake := newFakeClient()
	fake.set("log-level", ldvalue.String("debug"))
	p := New(withClient(fake))
	if got := resolveStr(t, p, "launchdarkly://log-level"); got != "debug" {
		t.Fatalf("string = %q, want debug", got)
	}
}

func TestResolveNumber(t *testing.T) {
	fake := newFakeClient()
	fake.set("max-retries", ldvalue.Int(5432))
	fake.set("sample-rate", ldvalue.Float64(0.25))
	p := New(withClient(fake))

	if got := resolveStr(t, p, "launchdarkly://max-retries"); got != "5432" {
		t.Fatalf("int = %q, want 5432", got)
	}
	if got := resolveStr(t, p, "launchdarkly://sample-rate"); got != "0.25" {
		t.Fatalf("float = %q, want 0.25", got)
	}
}

func TestResolveJSONObjectAndKey(t *testing.T) {
	fake := newFakeClient()
	fake.set("api-config", ldvalue.Parse([]byte(`{"host":"db.internal","port":5432,"tls":true}`)))
	p := New(withClient(fake))

	// The whole object comes back as JSON.
	whole := resolveStr(t, p, "launchdarkly://api-config")
	if whole == "" || whole[0] != '{' {
		t.Fatalf("object = %q, want JSON encoding", whole)
	}

	if got := resolveStr(t, p, "launchdarkly://api-config#host"); got != "db.internal" {
		t.Fatalf("host = %q, want db.internal", got)
	}
	if got := resolveStr(t, p, "launchdarkly://api-config#port"); got != "5432" {
		t.Fatalf("port = %q, want 5432", got)
	}
	if got := resolveStr(t, p, "launchdarkly://api-config#tls"); got != "true" {
		t.Fatalf("tls = %q, want true", got)
	}

	// A missing json key is a typed not-found.
	_, err := p.Resolve(context.Background(), mustRef(t, "launchdarkly://api-config#nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("missing json key err = %v, want ErrNotFound", err)
	}
}

func TestResolveVersionChanges(t *testing.T) {
	fake := newFakeClient()
	fake.set("flag", ldvalue.String("one"))
	p := New(withClient(fake))

	v1, err := p.Resolve(context.Background(), mustRef(t, "launchdarkly://flag"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v1.Version == "" {
		t.Fatal("Version is empty")
	}
	if v1.Sensitive {
		t.Fatal("LaunchDarkly values must not be marked Sensitive")
	}

	fake.set("flag", ldvalue.String("two"))
	v2, err := p.Resolve(context.Background(), mustRef(t, "launchdarkly://flag"))
	if err != nil {
		t.Fatalf("Resolve after mutate: %v", err)
	}
	if v1.Version == v2.Version {
		t.Fatalf("Version did not change after mutate (both %q)", v1.Version)
	}
	if string(v2.Bytes) != "two" {
		t.Fatalf("Bytes = %q, want two", v2.Bytes)
	}
}

func TestResolveNotFound(t *testing.T) {
	p := New(withClient(newFakeClient()))
	_, err := p.Resolve(context.Background(), mustRef(t, "launchdarkly://absent"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveContextCancelled(t *testing.T) {
	fake := newFakeClient()
	fake.set("flag", ldvalue.String("v"))
	p := New(withClient(fake))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Resolve(ctx, mustRef(t, "launchdarkly://flag")); err == nil {
		t.Fatal("Resolve with cancelled context returned nil error")
	}
}

func TestConnNoSDKKey(t *testing.T) {
	t.Setenv("LAUNCHDARKLY_SDK_KEY", "")
	p := New()
	_, err := p.Resolve(context.Background(), mustRef(t, "launchdarkly://flag"))
	if err == nil {
		t.Fatal("Resolve without an SDK key returned nil error")
	}
}

func TestWatchEmitsChange(t *testing.T) {
	fake := newFakeClient()
	fake.set("watched", ldvalue.String("v1"))
	p := New(withClient(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "launchdarkly://watched"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	fake.set("watched", ldvalue.String("v2"))
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("change err: %v", u.Err)
		}
		if string(u.Value.Bytes) != "v2" {
			t.Fatalf("change = %q, want v2", u.Value.Bytes)
		}
		if u.Value.Version == "" {
			t.Fatal("change version is empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mutation not delivered")
	}
}

func TestWatchJSONKey(t *testing.T) {
	fake := newFakeClient()
	fake.set("cfg", ldvalue.Parse([]byte(`{"level":"info"}`)))
	p := New(withClient(fake))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "launchdarkly://cfg#level"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	fake.set("cfg", ldvalue.Parse([]byte(`{"level":"debug"}`)))
	select {
	case u := <-ch:
		if u.Err != nil {
			t.Fatalf("change err: %v", u.Err)
		}
		if string(u.Value.Bytes) != "debug" {
			t.Fatalf("change = %q, want debug", u.Value.Bytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mutation not delivered")
	}
}

func TestWatchClosesOnCancel(t *testing.T) {
	fake := newFakeClient()
	fake.set("k", ldvalue.String("v"))
	p := New(withClient(fake))

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Watch(ctx, mustRef(t, "launchdarkly://k"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed after cancel")
		}
	}
}

func TestFlagValueToBytes(t *testing.T) {
	cases := []struct {
		name string
		val  ldvalue.Value
		want string
	}{
		{"bool-true", ldvalue.Bool(true), "true"},
		{"bool-false", ldvalue.Bool(false), "false"},
		{"string", ldvalue.String("hello"), "hello"},
		{"int", ldvalue.Int(42), "42"},
		{"float", ldvalue.Float64(3.5), "3.5"},
		{"array", ldvalue.Parse([]byte(`[1,2,3]`)), "[1,2,3]"},
		{"null", ldvalue.Null(), "null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(flagValueToBytes(tc.val)); got != tc.want {
				t.Fatalf("flagValueToBytes(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func resolveStr(t *testing.T, p *Provider, ref string) string {
	t.Helper()
	v, err := p.Resolve(context.Background(), mustRef(t, ref))
	if err != nil {
		t.Fatalf("Resolve(%q): %v", ref, err)
	}
	return string(v.Bytes)
}

func mustRef(t *testing.T, s string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return ref
}
