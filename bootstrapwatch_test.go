package mamori

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori/secret"
)

// bootProvider is a fake provider whose outcome a test flips between serving
// values and failing with a chosen classification, which is the whole variable
// the bootstrap cache branches on.
type bootProvider struct {
	mu   sync.Mutex
	vals map[string]Value
	err  error
}

func newBootProvider() *bootProvider {
	return &bootProvider{vals: map[string]Value{
		"token": {Bytes: []byte("s3cr3t"), Version: "v1", Sensitive: true},
		"host":  {Bytes: []byte("db.internal"), Version: "h1"},
	}}
}

func (p *bootProvider) Scheme() string { return "boot" }

func (p *bootProvider) Resolve(_ context.Context, ref Ref) (Value, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return Value{}, p.err
	}
	v, ok := p.vals[ref.Path]
	if !ok {
		return Value{}, ErrNotFound
	}
	return v, nil
}

func (p *bootProvider) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func (p *bootProvider) set(path, val, version string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = nil
	p.vals[path] = Value{Bytes: []byte(val), Version: version, Sensitive: path == "token"}
}

// bootConfig carries a secret deliberately: a snapshot of the decoded struct
// would persist secret.String's redaction, so a round trip that still reveals
// "s3cr3t" is the property that proves the cache stores resolved values.
type bootConfig struct {
	Token secret.String `source:"boot://token"`
	Host  string        `source:"boot://host"`
	Level string        `source:"boot://level" default:"info"`
}

// unavailable builds the transient failure a cold start falls back for.
func unavailable() error {
	return fmt.Errorf("%w: %w", ErrUnavailable, errors.New("dial tcp: connection refused"))
}

// bootOpts returns the Options a bootstrap test shares: the fake provider, a
// snapshot at path, and a manually driven clock.
func bootOpts(p *bootProvider, path string, clk *FakeClock, extra ...Option) []Option {
	opts := []Option{
		WithProvider(p),
		WithClock(clk),
		WithBootstrapCache(path, bootTestKey()),
	}
	return append(opts, extra...)
}

func bootTestKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 7)
	}
	return k
}

// readSnapshotFile opens the snapshot on disk for assertions. Only a test ever
// does this; nothing in the package reads a snapshot back except a restore.
func readSnapshotFile(t *testing.T, path string) snapshot {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	s, err := openSnapshot(b, bootTestKey())
	if err != nil {
		t.Fatalf("openSnapshot: %v", err)
	}
	return s
}

// TestBootstrapRestoresSecretsThroughAColdStartOutage is the feature in one
// test: a process that resolved once, then restarted while the backend was
// unreachable, still boots with its real secret rather than the redaction a
// serialized T would have persisted.
func TestBootstrapRestoresSecretsThroughAColdStartOutage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap")
	clk := NewFakeClock(time.Time{})
	p := newBootProvider()

	if _, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...); err != nil {
		t.Fatalf("first Load: %v", err)
	}

	p.fail(unavailable())
	cfg, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...)
	if err != nil {
		t.Fatalf("Load during the outage: %v", err)
	}
	if got := cfg.Token.Reveal(); got != "s3cr3t" {
		t.Fatalf("Token = %q, want s3cr3t; the snapshot lost the secret", got)
	}
	if cfg.Host != "db.internal" {
		t.Fatalf("Host = %q, want db.internal", cfg.Host)
	}
	if cfg.Level != "info" {
		t.Fatalf("Level = %q, want the default", cfg.Level)
	}
}

// TestBootstrapIsNeverAFastPath pins that a reachable backend always wins. A
// cache that answered first would mask a backend that is up but wrong, and
// nobody would find out until the cached values expired.
func TestBootstrapIsNeverAFastPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap")
	clk := NewFakeClock(time.Time{})
	p := newBootProvider()

	if _, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	p.set("host", "db.rotated", "h2")

	cfg, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Host != "db.rotated" {
		t.Fatalf("Host = %q, want the live value db.rotated", cfg.Host)
	}
}

// TestBootstrapRefusesANonTransientFailure is the revocation guard. A backend
// that answered and said no has said something about the value, and serving a
// cached copy of it would undo exactly the change the operator made.
func TestBootstrapRefusesANonTransientFailure(t *testing.T) {
	tests := []struct {
		name     string
		sentinel error
	}{
		{"a deleted secret", ErrNotFound},
		{"a revoked policy", ErrPermissionDenied},
		{"an expired credential", ErrUnauthenticated},
		{"a malformed payload", ErrInvalid},
		{"an unclassifiable failure", errors.New("something went wrong")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snap")
			clk := NewFakeClock(time.Time{})
			p := newBootProvider()
			if _, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...); err != nil {
				t.Fatalf("first Load: %v", err)
			}

			p.fail(fmt.Errorf("%w: %w", tt.sentinel, errors.New("backend said no")))
			cfg, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...)
			if err == nil {
				t.Fatalf("Load succeeded from the cache for %v; the backend answered and refused", tt.sentinel)
			}
			if cfg.Token.Reveal() != "" {
				t.Fatal("a cached secret was returned alongside the error")
			}
			if !errors.Is(err, tt.sentinel) {
				t.Fatalf("err = %v, want it to still carry %v", err, tt.sentinel)
			}
		})
	}
}

// TestBootstrapRefusesAnExpiredRecord pins that a lease the backend has already
// invalidated is not restorable. Restoring it would hand the application a
// credential guaranteed not to work while reporting a healthy boot.
func TestBootstrapRefusesAnExpiredRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap")
	clk := NewFakeClock(time.Time{})
	p := newBootProvider()
	p.mu.Lock()
	p.vals["token"] = Value{
		Bytes:     []byte("s3cr3t"),
		Version:   "v1",
		Sensitive: true,
		NotAfter:  clk.Now().Add(time.Hour),
	}
	p.mu.Unlock()

	if _, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...); err != nil {
		t.Fatalf("first Load: %v", err)
	}

	clk.Advance(2 * time.Hour)
	p.fail(unavailable())
	_, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...)
	if err == nil {
		t.Fatal("Load restored a record whose lease had already expired")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want it to still name the outage that made the restore necessary", err)
	}
	if want := "Token"; !contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to name the field %s", err, want)
	}
}

// TestBootstrapRefusesADriftedSchema pins that a snapshot written for a
// different config struct is reported as drift rather than failing downstream
// with a message about a value.
func TestBootstrapRefusesADriftedSchema(t *testing.T) {
	type otherConfig struct {
		Host string `source:"boot://host"`
	}
	path := filepath.Join(t.TempDir(), "snap")
	clk := NewFakeClock(time.Time{})
	p := newBootProvider()
	if _, err := Load[otherConfig](context.Background(), bootOpts(p, path, clk)...); err != nil {
		t.Fatalf("first Load: %v", err)
	}

	p.fail(unavailable())
	_, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...)
	if err == nil {
		t.Fatal("a snapshot written for another struct was restored")
	}
	if !contains(err.Error(), "different config struct") {
		t.Fatalf("err = %v, want it to name schema drift", err)
	}
}

// TestBootstrapRestoredValuesAreStillValidated pins that a restore replays
// through the ordinary path rather than around it. This is what makes the cache
// a fallback instead of a way past the gate.
func TestBootstrapRestoredValuesAreStillValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap")
	clk := NewFakeClock(time.Time{})
	p := newBootProvider()
	if _, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...); err != nil {
		t.Fatalf("first Load: %v", err)
	}

	p.fail(unavailable())
	rejected := errors.New("Host must be an allowed host")
	_, err := Load[bootConfig](context.Background(),
		append(bootOpts(p, path, clk), WithValidator(validatorFunc(func(any) error { return rejected })))...)
	if err == nil {
		t.Fatal("a restored configuration bypassed validation")
	}
	if !errors.Is(err, rejected) {
		t.Fatalf("err = %v, want the validation failure", err)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want it to also name the outage", err)
	}
}

// validatorFunc adapts a function to the Validator interface.
type validatorFunc func(any) error

func (f validatorFunc) Validate(v any) error { return f(v) }

// TestBootstrapWriteFailureDoesNotFailTheUpdate pins the inversion the feature
// must not commit: a good configuration refused because a cache file could not
// be written would turn a fallback meant to survive an outage into a new way to
// fail during one.
func TestBootstrapWriteFailureDoesNotFailTheUpdate(t *testing.T) {
	// A path under a directory that does not exist: the temporary file cannot
	// be created, so the write fails without needing to fake a filesystem.
	path := filepath.Join(t.TempDir(), "no-such-dir", "snap")
	clk := NewFakeClock(time.Time{})
	p := newBootProvider()
	m := &recordingMeter{}
	var got []error

	cfg, err := Load[bootConfig](context.Background(),
		append(bootOpts(p, path, clk), WithMeter(m), OnError(func(e error) { got = append(got, e) }))...)
	if err != nil {
		t.Fatalf("Load failed because the snapshot could not be written: %v", err)
	}
	if cfg.Token.Reveal() != "s3cr3t" {
		t.Fatalf("Token = %q, want the resolved value", cfg.Token.Reveal())
	}
	if m.bootstrapWriteFailures() != 1 {
		t.Fatalf("RecordBootstrapWriteFailed called %d times, want 1", m.bootstrapWriteFailures())
	}
	if len(got) != 1 {
		t.Fatalf("OnError received %d errors, want 1: %v", len(got), got)
	}
	if !errors.Is(got[0], ErrUnavailable) {
		t.Fatalf("OnError got %v, want it to wrap ErrUnavailable", got[0])
	}
}

// TestBootstrapWritesOnEveryAppliedUpdate pins that the snapshot tracks the
// configuration actually being served, not only the one the process started
// with.
func TestBootstrapWritesOnEveryAppliedUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap")
	p := newBootProvider()

	// A real clock with a tight poll interval, not the FakeClock the other tests
	// use: this one needs the poller to actually tick, and driving a fake clock
	// on behalf of a goroutine that has not armed its timer yet is the race
	// FakeClock.BlockUntil exists for.
	w, err := Watch[bootConfig](context.Background(),
		WithProvider(p), WithBootstrapCache(path, bootTestKey()),
		WithDebounce(0), WithPollInterval(20*time.Millisecond), WithJitter(0))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	p.set("host", "db.rotated", "h2")
	waitFor(t, "the rotated value to be applied", func() bool { return w.Get().Host == "db.rotated" })
	waitFor(t, "the snapshot to carry the rotated value", func() bool {
		for _, rec := range readSnapshotFile(t, path).Records {
			if rec.Path == "Host" && string(rec.Bytes) == "db.rotated" {
				return true
			}
		}
		return false
	})
}

// TestBootstrapRestoreDoesNotRewriteTheSnapshot pins that a restore leaves the
// file's age alone. Rewriting it with what was just read would reset the clock
// BootstrapMaxAge measures and quietly hand back the staleness bound.
func TestBootstrapRestoreDoesNotRewriteTheSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap")
	clk := NewFakeClock(time.Time{})
	p := newBootProvider()
	if _, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	written := readSnapshotFile(t, path).WrittenAt

	clk.Advance(3 * time.Hour)
	p.fail(unavailable())
	if _, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...); err != nil {
		t.Fatalf("Load during the outage: %v", err)
	}
	if got := readSnapshotFile(t, path).WrittenAt; !got.Equal(written) {
		t.Fatalf("WrittenAt = %v, want it unchanged at %v; a restore must not reset the age", got, written)
	}
}

// TestBootstrapReportsItsSourceAndAge pins that a process serving a restored
// configuration says so from the first second, rather than only once it turns
// unhealthy.
func TestBootstrapReportsItsSourceAndAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap")
	clk := NewFakeClock(time.Time{})
	p := newBootProvider()
	if _, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...); err != nil {
		t.Fatalf("first Load: %v", err)
	}

	clk.Advance(2 * time.Hour)
	p.fail(unavailable())
	w, err := Watch[bootConfig](context.Background(), bootOpts(p, path, clk)...)
	if err != nil {
		t.Fatalf("Watch during the outage: %v", err)
	}
	defer func() { _ = w.Close() }()

	rep := w.Status()
	if rep.Source != SourceBootstrapCache {
		t.Fatalf("Source = %q, want %q", rep.Source, SourceBootstrapCache)
	}
	if rep.Bootstrap == nil {
		t.Fatal("Bootstrap is nil while serving from the snapshot")
	}
	if !rep.Bootstrap.Restored {
		t.Fatal("Bootstrap.Restored is false while serving from the snapshot")
	}
	if rep.Bootstrap.Age != 2*time.Hour {
		t.Fatalf("Bootstrap.Age = %v, want 2h", rep.Bootstrap.Age)
	}
	// Each restored field's own Age is the age of the value, not zero: dating a
	// restored value to now would claim a successful resolve that never
	// happened, and would hide the config from WithStale as well.
	for _, f := range rep.Fields {
		if f.Age != 2*time.Hour {
			t.Fatalf("%s Age = %v, want 2h; a restored value was dated to now", f.Path, f.Age)
		}
	}
}

// TestBootstrapSourceIsBackendWithoutARestore pins that an ordinary process
// never reports serving from disk, even with the cache configured and a
// snapshot on it.
func TestBootstrapSourceIsBackendWithoutARestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap")
	clk := NewFakeClock(time.Time{})
	p := newBootProvider()

	w, err := Watch[bootConfig](context.Background(), bootOpts(p, path, clk)...)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	rep := w.Status()
	if rep.Source != SourceBackend {
		t.Fatalf("Source = %q, want %q", rep.Source, SourceBackend)
	}
	if rep.Bootstrap == nil || !rep.Bootstrap.Present || rep.Bootstrap.Restored {
		t.Fatalf("Bootstrap = %+v, want a present, un-restored snapshot", rep.Bootstrap)
	}
	if err := w.Health(); err != nil {
		t.Fatalf("Health = %v, want nil", err)
	}
}

// TestBootstrapHealthIsBounded pins both halves of the health rule: a pod
// serving a restored configuration joins the load balancer inside
// BootstrapMaxAge, and leaves it past that bound.
func TestBootstrapHealthIsBounded(t *testing.T) {
	tests := []struct {
		name     string
		age      time.Duration
		maxAge   time.Duration
		wantFail bool
	}{
		{"inside the bound", time.Hour, 6 * time.Hour, false},
		{"past the bound", 8 * time.Hour, 6 * time.Hour, true},
		{"unbounded by explicit request", 900 * time.Hour, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snap")
			clk := NewFakeClock(time.Time{})
			p := newBootProvider()
			if _, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...); err != nil {
				t.Fatalf("first Load: %v", err)
			}

			clk.Advance(tt.age)
			p.fail(unavailable())
			w, err := Watch[bootConfig](context.Background(),
				WithProvider(p), WithClock(clk),
				WithBootstrapCache(path, bootTestKey(), BootstrapMaxAge(tt.maxAge)))
			if err != nil {
				t.Fatalf("Watch during the outage: %v", err)
			}
			defer func() { _ = w.Close() }()

			err = w.Health()
			var stale *BootstrapStaleError
			switch {
			case tt.wantFail && !errors.As(err, &stale):
				t.Fatalf("Health = %v, want a *BootstrapStaleError for a %v snapshot past a %v bound", err, tt.age, tt.maxAge)
			case !tt.wantFail && err != nil:
				t.Fatalf("Health = %v, want nil for a %v snapshot under a %v bound", err, tt.age, tt.maxAge)
			}
			if tt.wantFail && !w.Status().Healthy {
				return // Report agrees with Health, which is the point
			}
			if tt.wantFail {
				t.Fatal("Health failed but Report.Healthy is still true")
			}
		})
	}
}

// TestBootstrapRestoredLabelClearsOnceEveryFieldResolvesLive pins that a pod
// which booted during a brief outage stops being judged against the snapshot's
// age once nothing it serves comes from the snapshot any more. Without this it
// would eventually fail its own readiness probe over a file it no longer
// depends on.
func TestBootstrapRestoredLabelClearsOnceEveryFieldResolvesLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap")
	clk := NewFakeClock(time.Time{})
	p := newBootProvider()
	if _, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...); err != nil {
		t.Fatalf("first Load: %v", err)
	}

	clk.Advance(10 * time.Hour)
	p.fail(unavailable())
	w, err := Watch[bootConfig](context.Background(),
		WithProvider(p), WithClock(clk), WithDebounce(0), WithJitter(0),
		WithBootstrapCache(path, bootTestKey(), BootstrapMaxAge(time.Hour)))
	if err != nil {
		t.Fatalf("Watch during the outage: %v", err)
	}
	defer func() { _ = w.Close() }()

	var stale *BootstrapStaleError
	if err := w.Health(); !errors.As(err, &stale) {
		t.Fatalf("Health = %v, want a *BootstrapStaleError while restored past the bound", err)
	}

	// The backend comes back; the poller picks every field up.
	p.set("token", "s3cr3t", "v1")
	p.set("host", "db.internal", "h1")
	for i := 0; i < 200 && w.Status().Source == SourceBootstrapCache; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = clk.BlockUntil(ctx, 1)
		cancel()
		clk.Advance(time.Minute)
		time.Sleep(5 * time.Millisecond)
	}
	if got := w.Status().Source; got != SourceBackend {
		t.Fatalf("Source = %q after every field resolved live, want %q", got, SourceBackend)
	}
	if err := w.Health(); err != nil {
		t.Fatalf("Health = %v after the backend recovered, want nil", err)
	}
}

// TestDoctorInspectsTheSnapshot pins that a preflight tells an operator whether
// the fallback they believe they have is real, while the backends are up and
// there is still time to fix it.
func TestDoctorInspectsTheSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap")
	clk := NewFakeClock(time.Time{})
	p := newBootProvider()

	missing, err := Doctor[bootConfig](context.Background(), bootOpts(p, path, clk)...)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if missing.Bootstrap == nil || missing.Bootstrap.Present {
		t.Fatalf("Bootstrap = %+v, want absent before anything was written", missing.Bootstrap)
	}
	if missing.Source != SourceBackend {
		t.Fatalf("Source = %q, want %q", missing.Source, SourceBackend)
	}

	if _, err := Load[bootConfig](context.Background(), bootOpts(p, path, clk)...); err != nil {
		t.Fatalf("Load: %v", err)
	}
	clk.Advance(90 * time.Minute)

	rep, err := Doctor[bootConfig](context.Background(), bootOpts(p, path, clk)...)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if rep.Bootstrap == nil || !rep.Bootstrap.Present {
		t.Fatalf("Bootstrap = %+v, want a present snapshot", rep.Bootstrap)
	}
	if !rep.Bootstrap.FingerprintMatch {
		t.Fatal("FingerprintMatch is false for a snapshot this same struct just wrote")
	}
	if rep.Bootstrap.Age != 90*time.Minute {
		t.Fatalf("Age = %v, want 90m", rep.Bootstrap.Age)
	}
	if rep.Bootstrap.Restored {
		t.Fatal("Doctor reported Restored; it starts no watcher and restores nothing")
	}

	// A snapshot written for a different struct is reported as drift rather
	// than left to fail during the outage it was meant to cover.
	type otherConfig struct {
		Host string `source:"boot://host"`
	}
	drifted, err := Doctor[otherConfig](context.Background(), bootOpts(p, path, clk)...)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if drifted.Bootstrap.FingerprintMatch {
		t.Fatal("FingerprintMatch is true for a snapshot written for another struct")
	}
	if drifted.Bootstrap.Problem == "" {
		t.Fatal("a drifted snapshot reported no problem")
	}
}

// TestBootstrapStatusNeverCarriesTheSnapshot pins the hard rule that a snapshot
// reaches no Report and no admin endpoint. The status block is metadata only.
func TestBootstrapStatusNeverCarriesTheSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap")
	clk := NewFakeClock(time.Time{})
	p := newBootProvider()
	w, err := Watch[bootConfig](context.Background(), bootOpts(p, path, clk)...)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	rendered := fmt.Sprintf("%+v", w.Status())
	for _, forbidden := range []string{"s3cr3t", path} {
		if contains(rendered, forbidden) {
			t.Fatalf("Status() rendered %q, which must never leave the process", forbidden)
		}
	}
}

// TestBootstrapConstructionErrorFailsBeforeAnyRoundTrip pins that a key of the
// wrong size takes the process down at startup rather than at the moment the
// fallback is finally needed.
func TestBootstrapConstructionErrorFailsBeforeAnyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap")
	clk := NewFakeClock(time.Time{})
	p := newBootProvider()

	_, err := Load[bootConfig](context.Background(),
		WithProvider(p), WithClock(clk), WithBootstrapCache(path, make([]byte, 16)))
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid for a 16-byte key", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("a snapshot was written despite the construction error")
	}
}

// contains is strings.Contains, spelled locally so the assertions above read as
// assertions rather than as string manipulation.
func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// waitFor polls cond until it holds or the test's patience runs out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
