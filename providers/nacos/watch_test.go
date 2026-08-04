package nacos

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// awaitValue takes Updates until one carries bytes, failing on timeout.
func awaitValue(t *testing.T, ch <-chan mamori.Update, timeout time.Duration) mamori.Update {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case u, open := <-ch:
			if !open {
				t.Fatal("watch channel closed before delivering a value")
			}
			if u.Err != nil {
				continue
			}
			return u
		case <-deadline:
			t.Fatalf("no value within %v", timeout)
		}
	}
}

// TestListenerProbeIsExactlyWhatNacosDocuments pins the wire format of one
// round.
//
// This is the test that would catch a wrong separator. Nacos's listener answers
// a malformed probe with an empty body, which is indistinguishable from "nothing
// changed", so a wrong separator produces a watch that never fires, never
// errors, and never logs. Asserting the encoded form directly means the failure
// shows up as a diff on this line rather than as a silent feature.
func TestListenerProbeIsExactlyWhatNacosDocuments(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("ns-1", "prod", "app.yaml", "hello")
	p := fake.provider("ns-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "nacos://prod/app.yaml"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	awaitValue(t, ch, 5*time.Second) // baseline

	// Wait for the first listener round to have been recorded.
	waitFor(t, 5*time.Second, func() bool { return fake.rounds() > 0 })
	body, _, header := fake.snapshot()

	// contentMD5("hello"), the digest the server compares against.
	md5Hello := contentMD5([]byte("hello"))
	want := listeningConfigsParam + "=" +
		url.QueryEscape("app.yaml"+wordSeparator+"prod"+wordSeparator+md5Hello+wordSeparator+"ns-1"+lineSeparator)
	if body != want {
		t.Fatalf("listener body:\n got %q\nwant %q", body, want)
	}
	// Spelled out, so a reader can check it against the vendor doc without
	// running anything: the fields are separated by %02 and the record is
	// terminated by %01.
	if !strings.Contains(body, "%02") || !strings.HasSuffix(body, "%01") {
		t.Fatalf("listener body %q must join fields with %%02 and terminate the record with %%01", body)
	}

	// The header's spelling is Nacos's own; correcting it stops the server
	// parking the request at all.
	if got := header.Get(longPullingTimeoutHeader); got != "2000" {
		t.Fatalf("%s = %q, want 2000 (milliseconds)", longPullingTimeoutHeader, got)
	}
	if got := header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type = %q, want application/x-www-form-urlencoded", got)
	}
}

func TestListenerProbeOmitsTheTenantFieldWhenThereIsNoNamespace(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("", defaultGroup, "app.yaml", "hello")
	p := fake.provider("")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "nacos://app.yaml"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	awaitValue(t, ch, 5*time.Second)
	waitFor(t, 5*time.Second, func() bool { return fake.rounds() > 0 })

	body, _, _ := fake.snapshot()
	want := listeningConfigsParam + "=" +
		url.QueryEscape("app.yaml"+wordSeparator+defaultGroup+wordSeparator+contentMD5([]byte("hello"))+lineSeparator)
	if body != want {
		t.Fatalf("listener body:\n got %q\nwant %q (the three-field form Nacos documents as dataId^2group^2contentMD5^1)", body, want)
	}
}

func TestChangedConfigsReadsTheURLEncodedResponse(t *testing.T) {
	c := coordinates{group: "prod", dataID: "app.yaml"}

	// Exactly what MD5Util.compareMd5ResultString produces: the control
	// characters, then URLEncoder.encode over the whole string. The body on the
	// wire carries the literal characters "%02" and "%01".
	encoded := url.QueryEscape("app.yaml" + wordSeparator + "prod" + wordSeparator + "ns-1" + lineSeparator)
	if !strings.Contains(encoded, "%02") {
		t.Fatalf("test fixture %q is not the encoded form", encoded)
	}
	if !changedConfigs([]byte(encoded), c, "ns-1") {
		t.Fatal("a URL-encoded change response was not recognised; splitting the raw body on the control characters finds nothing and the watch silently never fires")
	}
}

func TestChangedConfigsAlsoReadsRawControlCharacters(t *testing.T) {
	c := coordinates{group: "prod", dataID: "app.yaml"}
	raw := "app.yaml" + wordSeparator + "prod" + lineSeparator
	if !changedConfigs([]byte(raw), c, "") {
		t.Fatal("the un-encoded form must be tolerated: the endpoint sits behind whatever proxy an operator put in front of it")
	}
}

func TestChangedConfigsIgnoresAnEmptyBody(t *testing.T) {
	c := coordinates{group: "prod", dataID: "app.yaml"}
	for _, body := range []string{"", "  ", "\n"} {
		if changedConfigs([]byte(body), c, "") {
			t.Fatalf("body %q reported a change; an empty body is how the hold elapsing is signalled", body)
		}
	}
}

func TestChangedConfigsIgnoresAnotherConfiguration(t *testing.T) {
	c := coordinates{group: "prod", dataID: "app.yaml"}
	other := url.QueryEscape("other.yaml" + wordSeparator + "prod" + lineSeparator)
	if changedConfigs([]byte(other), c, "") {
		t.Fatal("a response naming a different dataId reported a change")
	}
	wrongGroup := url.QueryEscape("app.yaml" + wordSeparator + "staging" + lineSeparator)
	if changedConfigs([]byte(wrongGroup), c, "") {
		t.Fatal("a response naming a different group reported a change")
	}
	wrongTenant := url.QueryEscape("app.yaml" + wordSeparator + "prod" + wordSeparator + "other-ns" + lineSeparator)
	if changedConfigs([]byte(wrongTenant), c, "ns-1") {
		t.Fatal("a response naming a different tenant reported a change")
	}
}

func TestChangedConfigsFindsTheEntryAmongSeveral(t *testing.T) {
	c := coordinates{group: "prod", dataID: "app.yaml"}
	body := url.QueryEscape(
		"other.yaml" + wordSeparator + "prod" + lineSeparator +
			"app.yaml" + wordSeparator + "prod" + lineSeparator)
	if !changedConfigs([]byte(body), c, "") {
		t.Fatal("a multi-record response must be split on the 0x01 line separator")
	}
}

func TestContentMD5(t *testing.T) {
	// The digest the Nacos server computes over stored content.
	if got := contentMD5([]byte("hello")); got != "5d41402abc4b2a76b9719d911017c592" {
		t.Fatalf("contentMD5(\"hello\") = %q, want 5d41402abc4b2a76b9719d911017c592", got)
	}
	// Absent content is the empty digest, not MD5(""), because that is what a
	// client holding nothing sends and what the server compares it to.
	if got := contentMD5(nil); got != "" {
		t.Fatalf("contentMD5(nil) = %q, want the empty string", got)
	}
}

func TestWatchEmitsTheBaselineThenEveryChange(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("", defaultGroup, "app.yaml", "one")
	p := fake.provider("")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "nacos://app.yaml"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	if got := string(awaitValue(t, ch, 5*time.Second).Value.Bytes); got != "one" {
		t.Fatalf("baseline = %q, want one", got)
	}
	for _, want := range []string{"two", "three", "four"} {
		fake.set("", defaultGroup, "app.yaml", want)
		if got := string(awaitValue(t, ch, 5*time.Second).Value.Bytes); got != want {
			t.Fatalf("update = %q, want %q", got, want)
		}
	}
}

func TestWatchFiresWhenAnAbsentConfigurationIsCreated(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	p := fake.provider("")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "nacos://later.yaml"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// No baseline Update: the configuration does not exist, and mamori's
	// polling adapter is silent for an absent key too.
	waitFor(t, 5*time.Second, func() bool { return fake.rounds() > 0 })
	select {
	case u := <-ch:
		t.Fatalf("received %+v for a configuration that does not exist", u)
	default:
	}

	fake.set("", defaultGroup, "later.yaml", "born")
	if got := string(awaitValue(t, ch, 5*time.Second).Value.Bytes); got != "born" {
		t.Fatalf("update = %q, want born", got)
	}
}

// TestWatchSurvivesADeletionAndFiresOnTheRepublish covers the branch where a
// watched configuration disappears.
//
// A deletion emits nothing, matching mamori's polling adapter, which is silent
// for a key that does not exist - a field on a deleted configuration keeps its
// last good value rather than being handed an empty one. What must still work is
// the resubscription: the watch resets its remembered MD5 to the empty digest,
// which is a live subscription to the configuration coming back, so a republish
// fires. Getting that reset wrong leaves the watch holding the digest of content
// that no longer exists and the republished value never arrives.
func TestWatchSurvivesADeletionAndFiresOnTheRepublish(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("", defaultGroup, "app.yaml", "before")
	p := fake.provider("", WithLongPollTimeout(150*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "nacos://app.yaml"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if got := string(awaitValue(t, ch, 5*time.Second).Value.Bytes); got != "before" {
		t.Fatalf("baseline = %q, want before", got)
	}

	before := fake.rounds()
	fake.del("", defaultGroup, "app.yaml")
	waitFor(t, 5*time.Second, func() bool { return fake.rounds() > before+1 })
	select {
	case u := <-ch:
		t.Fatalf("a deletion delivered %+v; an absent configuration must emit nothing, as the polling adapter does", u)
	default:
	}

	// The watch must still be PARKING, not spinning. If the remembered digest
	// is not reset to the empty one, every probe claims content the server no
	// longer holds, the server answers "changed" instantly on every round, the
	// read that follows finds nothing, and the loop runs flat out against the
	// backend for as long as the configuration stays deleted - while still
	// firing correctly on the republish below, so nothing else here notices.
	settled := fake.rounds()
	time.Sleep(600 * time.Millisecond)
	spun := fake.rounds() - settled
	if spun > 15 {
		t.Fatalf("%d listener rounds in 600ms against a 150ms hold; the watch is spinning rather than parking on an absent configuration", spun)
	}

	fake.set("", defaultGroup, "app.yaml", "after")
	if got := string(awaitValue(t, ch, 5*time.Second).Value.Bytes); got != "after" {
		t.Fatalf("republish delivered %q, want after", got)
	}
}

func TestWatchEmitsNothingWhileNothingChanges(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("", defaultGroup, "app.yaml", "steady")
	// A 150ms hold, so several rounds complete inside the window below.
	p := fake.provider("", WithLongPollTimeout(150*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "nacos://app.yaml"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	awaitValue(t, ch, 5*time.Second) // baseline

	waitFor(t, 5*time.Second, func() bool { return fake.rounds() >= 3 })
	select {
	case u := <-ch:
		t.Fatalf("received %+v after three rounds that timed out; an elapsed hold must emit nothing", u)
	default:
	}
}

func TestWatchDeliversATransientFailureAndKeepsWatching(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("", defaultGroup, "app.yaml", "before")
	p := fake.provider("")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.Watch(ctx, mustRef(t, "nacos://app.yaml"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	awaitValue(t, ch, 5*time.Second) // baseline

	fake.fail(http.StatusServiceUnavailable)
	fake.set("", defaultGroup, "app.yaml", "after") // wakes the parked round

	var sawErr bool
	deadline := time.After(5 * time.Second)
loop:
	for !sawErr {
		select {
		case u, open := <-ch:
			if !open {
				t.Fatal("watch closed on a transient failure; mamori never resubscribes a closed watch")
			}
			if u.Err != nil {
				if !errors.Is(u.Err, mamori.ErrUnavailable) {
					t.Fatalf("Err = %v, want mamori.ErrUnavailable", u.Err)
				}
				sawErr = true
			}
		case <-deadline:
			break loop
		}
	}
	if !sawErr {
		t.Fatal("a 503 from the backend never reached the watch as an Update")
	}

	fake.clearFail()
	if got := string(awaitValue(t, ch, 10*time.Second).Value.Bytes); got != "after" {
		t.Fatalf("after recovery got %q, want after", got)
	}
}

func TestWatchClosesOnCancellationWithoutEmittingTheCancellation(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("", defaultGroup, "app.yaml", "x")
	p := fake.provider("")

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Watch(ctx, mustRef(t, "nacos://app.yaml"))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	awaitValue(t, ch, 5*time.Second)
	waitFor(t, 5*time.Second, func() bool { return fake.rounds() > 0 })
	cancel()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case u, open := <-ch:
			if !open {
				return
			}
			if u.Err != nil {
				t.Fatalf("cancellation surfaced as a watch failure: %v", u.Err)
			}
		case <-deadline:
			t.Fatal("watch channel not closed after cancellation")
		}
	}
}

func TestWatchRejectsAMalformedRefBeforeStartingAnything(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	p := fake.provider("")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := p.Watch(ctx, mustRef(t, "nacos://a/b/c"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want mamori.ErrInvalid", err)
	}
	if ch != nil {
		t.Fatal("a rejected Watch must return no channel, so mamori falls back to polling")
	}
}

// TestWatchTestsWouldCatchAWatchThatNeverFires is the anti-vacuity proof for
// every watch test above.
//
// The failure mode a wrong Listening-Configs separator produces is a listener
// that answers an empty body on every round: no error, no log, no Update. A
// watch test that "passes" against that backend proves nothing. This test builds
// exactly that backend - a listener that always reports no change - and asserts
// the harness the other tests use FAILS against it. If this test ever passes
// while the assertion inside it holds, the watch assertions are vacuous.
func TestWatchTestsWouldCatchAWatchThatNeverFires(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("", defaultGroup, "app.yaml", "one")

	// A transport identical to the fake's except that the listener never
	// reports a change - which is what the real server sends for a probe it
	// could not parse.
	deaf := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		if strings.HasSuffix(req.URL.Path, "/"+listenerPath) {
			return textResponse(http.StatusOK, ""), nil
		}
		return fake.transport().RoundTrip(req)
	})
	client, err := httpcore.New(httpcore.Config{
		BaseURL:    "http://nacos.test/nacos",
		HTTPClient: &http.Client{Transport: deaf},
	})
	if err != nil {
		t.Fatalf("httpcore.New: %v", err)
	}
	p := New(withClient(client), WithGroup(defaultGroup), WithLongPollTimeout(100*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, werr := p.Watch(ctx, mustRef(t, "nacos://app.yaml"))
	if werr != nil {
		t.Fatalf("Watch: %v", werr)
	}
	// The baseline still arrives: it is an ordinary read, not a listener round.
	if got := string(awaitValue(t, ch, 5*time.Second).Value.Bytes); got != "one" {
		t.Fatalf("baseline = %q, want one", got)
	}

	fake.set("", defaultGroup, "app.yaml", "two")

	// Now the assertion the real watch tests make. It must NOT hold here.
	select {
	case u, open := <-ch:
		if !open {
			t.Fatal("channel closed; expected a silent, never-firing watch")
		}
		if u.Err == nil {
			t.Fatalf("a listener that reports no change still delivered %q; the watch assertions above cannot distinguish a working watch from a dead one",
				u.Value.Bytes)
		}
	case <-time.After(1500 * time.Millisecond):
		// Correct: nothing was delivered, so the assertions in the other tests
		// are the thing that makes them pass.
	}
}

// waitFor polls cond until it holds or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}
