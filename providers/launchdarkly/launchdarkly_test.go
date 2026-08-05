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
	ldclient "github.com/launchdarkly/go-server-sdk/v7"
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
	fails     map[string]fakeFailure
	listeners map[*fakeListener]struct{}
	closed    bool
}

// fakeFailure is an injected per-flag evaluation failure: the error
// JSONVariationDetail returns, and the evaluation-reason error kind it is
// reported under. reasonKind defaults to EvalErrorException (LaunchDarkly's
// generic "something went wrong" reason) via fail, so an injected error does
// not accidentally collide with a reason kind classifyReason maps; failReason
// lets a test pick a specific reason kind instead.
type fakeFailure struct {
	err        error
	reasonKind ldreason.EvalErrorKind
}

type fakeListener struct {
	flagKey string
	ch      chan interfaces.FlagValueChangeEvent
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		flags:     map[string]ldvalue.Value{},
		fails:     map[string]fakeFailure{},
		listeners: map[*fakeListener]struct{}{},
	}
}

// fail makes the next JSONVariationDetail for flagKey return err with a
// generic EXCEPTION evaluation reason, until clear(flagKey) is called. It
// powers the providertest ErrorClassification case and
// TestResolveClassifiesNonNotFoundError.
func (f *fakeClient) fail(flagKey string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[flagKey] = fakeFailure{err: err, reasonKind: ldreason.EvalErrorException}
}

// failReason is like fail but reports a specific evaluation-reason error kind,
// letting a test exercise classifyReason's CLIENT_NOT_READY mapping through
// Resolve with a realistic (non-sentinel) SDK error, the same shape a live
// backend would return.
func (f *fakeClient) failReason(flagKey string, kind ldreason.EvalErrorKind, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[flagKey] = fakeFailure{err: err, reasonKind: kind}
}

// clear cancels a previously injected fail/failReason for flagKey.
func (f *fakeClient) clear(flagKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fails, flagKey)
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
	if fl, ok := f.fails[flagKey]; ok {
		detail := ldreason.NewEvaluationDetailForError(fl.reasonKind, defaultVal)
		return defaultVal, detail, fl.err
	}
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

// Close implements ldEvaluator, closing any remaining listener channels and
// recording that Close was called, so tests can assert whether a
// caller-injected fake was (or was not) released.
func (f *fakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
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
		Ref:        func(key string) string { return "launchdarkly://" + key },
		PointerRef: func(key, frag string) string { return "launchdarkly://" + key + frag },
		Seed:       func(_ context.Context, key, val string) error { fake.setString(key, val); return nil },
		Mutate:     func(_ context.Context, key, val string) error { fake.setString(key, val); return nil },
		Fail: func(_ context.Context, key string, err error) error {
			fake.fail(key, err)
			return nil
		},
		Clear: func(_ context.Context, key string) error {
			fake.clear(key)
			return nil
		},

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

// TestResolveClassifiesNonNotFoundError exercises the fake's fail/clear seam
// through Resolve itself, not just as a unit test of the fake. It injects a
// mamori sentinel directly (the same thing the providertest
// ErrorClassification case does) and proves Resolve's %w wrapping keeps it
// intact end to end: reformat the evalErr branch with %v instead of %w, or
// drop the fake's fails-map check from JSONVariationDetail, and this test
// fails.
func TestResolveClassifiesNonNotFoundError(t *testing.T) {
	fake := newFakeClient()
	fake.set("flag", ldvalue.String("v"))
	fake.fail("flag", mamori.ErrPermissionDenied)
	p := New(withClient(fake))

	_, err := p.Resolve(context.Background(), mustRef(t, "launchdarkly://flag"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; the fail seam or Resolve's %%w wrapping may be broken", got, mamori.KindPermissionDenied)
	}
}

// TestResolveClassifiesClientNotReady proves classifyReason's one mapping
// (CLIENT_NOT_READY -> ErrUnavailable) is actually reached from Resolve, not
// just correct in isolation (see TestClassifyReason). It injects a realistic
// SDK-shaped failure - a generic error paired with the CLIENT_NOT_READY
// reason, not a mamori sentinel - so it fails if the classifyReason call were
// removed from Resolve, unlike a test that injects mamori.ErrUnavailable
// directly.
func TestResolveClassifiesClientNotReady(t *testing.T) {
	fake := newFakeClient()
	fake.set("flag", ldvalue.String("v"))
	fake.failReason("flag", ldreason.EvalErrorClientNotReady, errors.New("client not initialized"))
	p := New(withClient(fake))

	_, err := p.Resolve(context.Background(), mustRef(t, "launchdarkly://flag"))
	if err == nil {
		t.Fatal("Resolve returned a nil error while the client was not ready")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindUnavailable {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyReason may not be wired into Resolve", got, mamori.KindUnavailable)
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

// --- Close ---

// TestCloseWithoutUseIsSafe pins the "safe with no prior use" half of the
// Close contract: Close on a provider that never resolved must not connect to
// LaunchDarkly and must not panic, and a second Close must stay clean.
func TestCloseWithoutUseIsSafe(t *testing.T) {
	p := New()
	if err := p.Close(); err != nil {
		t.Fatalf("Close on an unused provider: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if p.ownClient || p.cli != nil {
		t.Error("Close built a client on a provider that never resolved")
	}
}

// TestResolveAfterCloseIsUnavailable pins the terminal half of the contract:
// once Close has run, Resolve must refuse locally (via the p.closed check in
// conn) rather than reaching into the fake client it was told to stop using.
func TestResolveAfterCloseIsUnavailable(t *testing.T) {
	fake := newFakeClient()
	fake.set("flag", ldvalue.String("v"))
	p := New(withClient(fake))

	if _, err := p.Resolve(context.Background(), mustRef(t, "launchdarkly://flag")); err != nil {
		t.Fatalf("Resolve before Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := p.Resolve(context.Background(), mustRef(t, "launchdarkly://flag")); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("Resolve after Close = %v; want ErrUnavailable", err)
	}
}

// TestCloseDoesNotCloseInjectedClient is the ownership half of rule 5: an
// *ldclient.LDClient handed in with WithClient belongs to the caller, so
// Close must stop using it without closing it.
//
// The two assertions below split rule 5 across two different mechanisms
// because no single one covers both:
//
//  1. WithClient must never set p.ownClient. This is checked against the real
//     public API with a real (offline, non-network) *ldclient.LDClient, since
//     that is the only way to prove WithClient's Option func itself does not
//     flip the flag - the internal seam below bypasses WithClient entirely
//     and cannot see a regression there.
//  2. Close must not call Close on the client once ownClient is (correctly)
//     false. mongodb/etcd/redis prove this against their real clients, because
//     each SDK's second Close/Disconnect returns a distinguishable
//     already-closed error. *ldclient.LDClient offers no such signal: its
//     Close is unconditionally idempotent and returns nil every time, even
//     against the same offline client used in (1) (verified directly against
//     the SDK). A real client would give this half of the test nothing to
//     assert against, so it uses the internal ldEvaluator seam (p.cli = fake)
//     and the fake's explicit closed flag instead.
//
// It also proves the other half of rule 5's ordering requirement - that Close
// still marks the provider closed on the not-owned path - since a Close that
// returned early here (skipping p.closed = true to avoid touching the
// injected client) would pass the "not closed" assertion below while silently
// breaking Resolve's terminality for every WithClient caller.
func TestCloseDoesNotCloseInjectedClient(t *testing.T) {
	// (1) WithClient itself must not claim ownership. ldclient.MakeCustomClient
	// with Offline: true returns instantly with no network access (verified
	// directly against the SDK), so this needs no reachable LaunchDarkly
	// service.
	real, err := ldclient.MakeCustomClient("fake-sdk-key", ldclient.Config{Offline: true}, 0)
	if err != nil {
		t.Fatalf("MakeCustomClient: %v", err)
	}
	defer func() { _ = real.Close() }()
	if p := New(WithClient(real)); p.ownClient {
		t.Fatal("WithClient claimed ownership of the injected client")
	}

	// (2) Given ownClient is false, Close must leave the client alone.
	fake := newFakeClient()
	fake.set("flag", ldvalue.String("v"))
	p := New()
	p.cli = fake // stands in for WithClient's *ldclient.LDClient: the caller owns this

	if _, err := p.Resolve(context.Background(), mustRef(t, "launchdarkly://flag")); err != nil {
		t.Fatalf("Resolve before Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fake.closed {
		t.Error("Close closed a caller-injected client; the caller owns it")
	}
	if _, err := p.Resolve(context.Background(), mustRef(t, "launchdarkly://flag")); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("Resolve after Close = %v; want ErrUnavailable (closed flag not set on the injected path)", err)
	}
}

// TestCloseReleasesSelfBuiltClient, the positive half of rule 5's ownership
// tracking (the mirror image of the WithClient half above), is deliberately
// NOT implemented here. Reaching conn()'s dial branch means calling
// ldclient.MakeClient with a real SDK key against LaunchDarkly's real
// streaming endpoint - MakeClient takes no custom endpoint override - so
// exercising it needs either live LaunchDarkly credentials or up to
// initTimeout (10s) blocked on an unreachable network per run. Unlike
// mongodb/etcd/redis, whose lazy constructors (mongo.Connect, clientv3.New,
// goredis.NewClient) never dial synchronously, there is no way to force this
// provider's owned-build site without a live backend. That path is exercised
// by launchdarkly_integration_test.go (//go:build integration) instead.
func TestCloseReleasesSelfBuiltClient(t *testing.T) {
	t.Skip("conn()'s dial branch (ldclient.MakeClient) cannot be reached without live " +
		"LaunchDarkly credentials or a real network wait; see launchdarkly_integration_test.go")
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
