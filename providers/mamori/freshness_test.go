package mamoriprov

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/mamori"
)

// watchBase is the instant every resolved_at in this file is expressed
// relative to. A fixed date rather than time.Now() keeps the frames these
// tests serve byte-for-byte reproducible, and none of the guard's behavior
// depends on the timestamps being anywhere near the present.
var watchBase = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

// freshnessFrame renders one SSE "update" frame for name, carrying text as the
// value's bytes.
//
// A ZERO resolvedAt omits the "resolved_at" key entirely, which is exactly how
// a server that does not send the field looks on the wire - the case the guard
// must never withhold a value for. stale adds the "stale":true annotation a
// replica serving last-known-good bytes sets.
func freshnessFrame(name, text string, resolvedAt time.Time, stale bool) string {
	fields := []string{
		fmt.Sprintf(`"name":%q`, name),
		fmt.Sprintf(`"bytes":%q`, base64.StdEncoding.EncodeToString([]byte(text))),
	}
	if !resolvedAt.IsZero() {
		fields = append(fields, fmt.Sprintf(`"resolved_at":%q`, resolvedAt.Format(time.RFC3339Nano)))
	}
	if stale {
		fields = append(fields, `"stale":true`)
	}
	fields = append(fields, `"metadata":{}`)
	return fmt.Sprintf("event: update\ndata: {%s}\n\n", strings.Join(fields, ","))
}

// holdOpenSSE returns a handler that writes frames in order on every
// connection and then holds that connection open until the client goes away.
//
// Holding it open is load-bearing for every test here: a handler that returned
// would have watchLoop reconnect and replay the same frames, and a replayed
// frame arriving on the channel is indistinguishable from a guard that failed
// to drop one. Blocking on the request context means the only thing that ever
// ends these streams is the test cancelling its watch.
func holdOpenSSE(frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
			flusher.Flush()
		}
		<-r.Context().Done()
	}
}

// startFreshnessWatch serves handler and returns the update channel of a watch
// on "db-password" against it, cancelled when the test ends.
func startFreshnessWatch(t *testing.T, handler http.HandlerFunc) <-chan mamori.Update {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	p := newTestProvider(t, ts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, err := p.Watch(ctx, mustParseRef(t, "mamori://db-password"))
	if err != nil {
		t.Fatalf("Watch returned unexpected error: %v", err)
	}
	return ch
}

// recvValue waits for one non-error Update and returns its bytes as a string.
func recvValue(t *testing.T, ch <-chan mamori.Update) string {
	t.Helper()
	u := recvUpdate(t, ch)
	if u.Err != nil {
		t.Fatalf("Update.Err = %v, want nil", u.Err)
	}
	return string(u.Value.Bytes)
}

// expectNoUpdate fails if anything at all arrives on ch within d. It is the
// central assertion of every drop test in this file: the guard's entire job is
// that the caller NEVER sees the older value, so "it arrived late" would be
// just as much of a failure as "it arrived immediately".
func expectNoUpdate(t *testing.T, ch <-chan mamori.Update, d time.Duration) {
	t.Helper()
	select {
	case u, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed unexpectedly")
		}
		t.Fatalf("received an Update that should have been dropped as out of order: %+v (bytes %q)", u, string(u.Value.Bytes))
	case <-time.After(d):
	}
}

func TestWatchDropsUpdateOlderThanWatermark(t *testing.T) {
	ch := startFreshnessWatch(t, holdOpenSSE(
		freshnessFrame("db-password", "fresh", watchBase, false),
		freshnessFrame("db-password", "five-minutes-behind", watchBase.Add(-5*time.Minute), false),
	))

	if got, want := recvValue(t, ch), "fresh"; got != want {
		t.Fatalf("first Bytes = %q, want %q", got, want)
	}
	expectNoUpdate(t, ch, 300*time.Millisecond)
}

// TestWatchForwardsNewerUpdateAndAdvancesWatermark checks both halves of a
// forwarded update: it reaches the caller, and it becomes the new watermark.
// The third frame is what proves the second half - it is newer than the FIRST
// frame, so it would sail through a guard that never advanced, and older than
// the second, so it must be dropped by one that did.
func TestWatchForwardsNewerUpdateAndAdvancesWatermark(t *testing.T) {
	ch := startFreshnessWatch(t, holdOpenSSE(
		freshnessFrame("db-password", "first", watchBase, false),
		freshnessFrame("db-password", "second", watchBase.Add(time.Minute), false),
		freshnessFrame("db-password", "behind-second", watchBase.Add(30*time.Second), false),
	))

	if got, want := recvValue(t, ch), "first"; got != want {
		t.Fatalf("first Bytes = %q, want %q", got, want)
	}
	if got, want := recvValue(t, ch), "second"; got != want {
		t.Fatalf("second Bytes = %q, want %q", got, want)
	}
	expectNoUpdate(t, ch, 300*time.Millisecond)
}

// TestWatchForwardsEqualResolvedAt pins the equality boundary: an update dated
// at exactly the watermark is the same fetch, not an older one, so
// re-announcing it - which a reconnect onto an equally fresh replica does
// routinely - must still deliver.
func TestWatchForwardsEqualResolvedAt(t *testing.T) {
	ch := startFreshnessWatch(t, holdOpenSSE(
		freshnessFrame("db-password", "first", watchBase, false),
		freshnessFrame("db-password", "same-instant", watchBase, false),
	))

	if got, want := recvValue(t, ch), "first"; got != want {
		t.Fatalf("first Bytes = %q, want %q", got, want)
	}
	if got, want := recvValue(t, ch), "same-instant"; got != want {
		t.Fatalf("second Bytes = %q, want %q", got, want)
	}
}

// TestWatchForwardsUpdateWithoutResolvedAt is the no-regression guard against
// an older server: a frame with no resolved_at at all cannot be ordered, and
// an unorderable value must be delivered, never withheld.
func TestWatchForwardsUpdateWithoutResolvedAt(t *testing.T) {
	ch := startFreshnessWatch(t, holdOpenSSE(
		freshnessFrame("db-password", "dated", watchBase, false),
		freshnessFrame("db-password", "undated", time.Time{}, false),
	))

	if got, want := recvValue(t, ch), "dated"; got != want {
		t.Fatalf("first Bytes = %q, want %q", got, want)
	}
	if got, want := recvValue(t, ch), "undated"; got != want {
		t.Fatalf("second Bytes = %q, want %q", got, want)
	}
}

// TestWatchForwardsWithinSkewToleranceWithoutLoweringWatermark covers
// resolvedAtSkewTolerance from both sides. A value dated just inside the band
// is delivered, because two wall clocks that close together cannot order two
// fetches and the guard abstains rather than dropping a possibly-newer value.
// The third frame then proves that delivering it did not RATCHET the watermark
// down: at 1.2s behind the original watermark it is outside the band and must
// be dropped, where a watermark lowered to the near-tie would have let it
// through at 0.7s behind.
func TestWatchForwardsWithinSkewToleranceWithoutLoweringWatermark(t *testing.T) {
	ch := startFreshnessWatch(t, holdOpenSSE(
		freshnessFrame("db-password", "first", watchBase, false),
		freshnessFrame("db-password", "near-tie", watchBase.Add(-resolvedAtSkewTolerance/2), false),
		freshnessFrame("db-password", "outside-band", watchBase.Add(-resolvedAtSkewTolerance-200*time.Millisecond), false),
	))

	if got, want := recvValue(t, ch), "first"; got != want {
		t.Fatalf("first Bytes = %q, want %q", got, want)
	}
	if got, want := recvValue(t, ch), "near-tie"; got != want {
		t.Fatalf("second Bytes = %q, want %q", got, want)
	}
	expectNoUpdate(t, ch, 300*time.Millisecond)
}

// TestWatchFreshnessWatermarkSurvivesReconnect is the actual bug this guard
// exists for, and the one test that would still pass if the watermark lived
// anywhere inside a single connection's scope.
//
// The first replica delivers a fresh value and then drops the connection. The
// reconnect rotates onto a second replica that is five minutes behind (its own
// upstream poll has not caught up yet), and that replica opens its stream by
// announcing its stale snapshot - which, to a client with no memory across
// reconnects, is indistinguishable from a brand new change. Delivering it
// would walk the consumer's config backwards in time. The watermark is created
// by watchLoop OUTSIDE its reconnect loop precisely so it is still there at
// this moment; without that, or without the guard at all, the "five-minutes-
// behind" value reaches the caller and this test fails.
func TestWatchFreshnessWatermarkSurvivesReconnect(t *testing.T) {
	var firstConns atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := firstConns.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		if n == 1 {
			_, _ = io.WriteString(w, freshnessFrame("db-password", "fresh", watchBase, false))
			flusher.Flush()
			return // drop the connection, forcing a reconnect onto the laggy replica
		}
		<-r.Context().Done() // any later rotation back here contributes nothing
	}))
	t.Cleanup(first.Close)

	var laggyConns atomic.Int32
	laggy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, freshnessFrame("db-password", "five-minutes-behind", watchBase.Add(-5*time.Minute), false))
		flusher.Flush()
		laggyConns.Add(1)
		<-r.Context().Done()
	}))
	t.Cleanup(laggy.Close)

	p := New(Config{Endpoints: []string{first.URL, laggy.URL}, InsecureNoTLS: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := p.Watch(ctx, mustParseRef(t, "mamori://db-password"))
	if err != nil {
		t.Fatalf("Watch returned unexpected error: %v", err)
	}

	if got, want := recvValue(t, ch), "fresh"; got != want {
		t.Fatalf("first Bytes = %q, want %q", got, want)
	}

	// Wait until the laggy replica has actually served its stale snapshot, so
	// the assertion below is about a frame the client definitely received and
	// chose to drop, not about one that never arrived.
	deadline := time.After(3 * time.Second)
	for laggyConns.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the watch never reconnected onto the second replica")
		case <-time.After(10 * time.Millisecond):
		}
	}

	expectNoUpdate(t, ch, 500*time.Millisecond)
}

// TestWatchForwardsErrorFrameDespiteActiveGuard proves the guard never touches
// an error. The error frame here even carries a resolved_at ten minutes behind
// the watermark, which would be dropped outright on an update frame: an error
// reports the state of the CURRENT connection, carries no value to be out of
// order, and swallowing it would cost the caller the only signal they get that
// their deployment is degraded.
func TestWatchForwardsErrorFrameDespiteActiveGuard(t *testing.T) {
	errFrame := fmt.Sprintf("event: error\ndata: {\"name\":\"db-password\",\"resolved_at\":%q,\"error\":{\"kind\":\"permission_denied\",\"message\":\"denied\"}}\n\n",
		watchBase.Add(-10*time.Minute).Format(time.RFC3339Nano))

	ch := startFreshnessWatch(t, holdOpenSSE(
		freshnessFrame("db-password", "fresh", watchBase, false),
		errFrame,
	))

	if got, want := recvValue(t, ch), "fresh"; got != want {
		t.Fatalf("first Bytes = %q, want %q", got, want)
	}
	u := recvUpdate(t, ch)
	if !errors.Is(u.Err, mamori.ErrPermissionDenied) {
		t.Fatalf("second Update.Err = %v, want errors.Is(..., mamori.ErrPermissionDenied)", u.Err)
	}
}

// TestWatchKeepsPerNameWatermarks covers a stream carrying more than one
// binding (the server's /v1/watch accepts repeated name parameters). "beta" is
// dated ten minutes before "alpha" simply because the two bindings resolve on
// unrelated schedules, and a single shared watermark would silently swallow
// every beta update behind the newest alpha one.
//
// The final alpha frame is the ordering trick that makes the drop assertable:
// receiving alpha-newer proves the stream stayed healthy AND that the
// out-of-order beta frame ahead of it in the stream was skipped, not merely
// delayed.
func TestWatchKeepsPerNameWatermarks(t *testing.T) {
	ts := httptest.NewServer(holdOpenSSE(
		freshnessFrame("alpha", "alpha-new", watchBase.Add(10*time.Minute), false),
		freshnessFrame("beta", "beta-current", watchBase, false),
		freshnessFrame("beta", "beta-behind", watchBase.Add(-10*time.Minute), false),
		freshnessFrame("alpha", "alpha-newer", watchBase.Add(20*time.Minute), false),
	))
	t.Cleanup(ts.Close)

	p := newTestProvider(t, ts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := p.Watch(ctx, mustParseRef(t, "mamori://alpha"))
	if err != nil {
		t.Fatalf("Watch returned unexpected error: %v", err)
	}

	if got, want := recvValue(t, ch), "alpha-new"; got != want {
		t.Fatalf("first Bytes = %q, want %q", got, want)
	}
	// Dated before alpha's watermark, yet delivered: beta is ordered against
	// beta alone.
	if got, want := recvValue(t, ch), "beta-current"; got != want {
		t.Fatalf("second Bytes = %q, want %q", got, want)
	}
	if got, want := recvValue(t, ch), "alpha-newer"; got != want {
		t.Fatalf("third Bytes = %q, want %q (beta-behind must have been dropped by beta's own watermark)", got, want)
	}
}

// TestWatchDeliversStaleValue pins that "stale" is an annotation, not a
// suppression signal: bytes served as last-known-good while upstream is
// failing are still the best answer anyone has, and withholding them would
// leave the caller with nothing at all.
func TestWatchDeliversStaleValue(t *testing.T) {
	ch := startFreshnessWatch(t, holdOpenSSE(
		freshnessFrame("db-password", "last-known-good", watchBase, true),
	))

	if got, want := recvValue(t, ch), "last-known-good"; got != want {
		t.Fatalf("Bytes = %q, want %q", got, want)
	}
}

// TestValueBodyDecodesFreshnessFields pins the two wire tags against literal
// JSON, so a typo in them cannot hide behind tests that both encode and decode
// through the same struct. The absent case matters just as much as the present
// one: a nil ResolvedAt is what tells the guard "this server does not date its
// values", which is a different statement from "resolved at the zero time".
func TestValueBodyDecodesFreshnessFields(t *testing.T) {
	var present valueBody
	body := `{"name":"db-password","bytes":"aGVsbG8=","metadata":{},"resolved_at":"2026-07-26T12:00:00Z","stale":true}`
	if err := json.Unmarshal([]byte(body), &present); err != nil {
		t.Fatalf("decoding a value body with freshness fields: %v", err)
	}
	if present.ResolvedAt == nil {
		t.Fatal("ResolvedAt = nil, want the decoded resolved_at timestamp")
	}
	if !present.ResolvedAt.Equal(watchBase) {
		t.Errorf("ResolvedAt = %v, want %v", present.ResolvedAt, watchBase)
	}
	if !present.Stale {
		t.Error("Stale = false, want true")
	}

	var absent valueBody
	if err := json.Unmarshal([]byte(`{"name":"db-password","bytes":"aGVsbG8=","metadata":{}}`), &absent); err != nil {
		t.Fatalf("decoding a value body without freshness fields: %v", err)
	}
	if absent.ResolvedAt != nil {
		t.Errorf("ResolvedAt = %v, want nil when the server omits resolved_at", absent.ResolvedAt)
	}
	if absent.Stale {
		t.Error("Stale = true, want false when the server omits stale")
	}
}
