package httpcore

import (
	"context"
	"fmt"
	"time"

	"github.com/xavidop/mamori"
)

// DefaultLongPollHold is how long one round asks the backend to hold the
// connection open when [LongPollConfig.Hold] is not positive. Thirty seconds is
// what Nacos's listener endpoint documents and what Consul's default wait time
// is comfortably inside, so it is a value both families of backend accept
// without tuning.
const DefaultLongPollHold = 30 * time.Second

// DefaultLongPollGrace is the margin added to Hold to bound the client side of
// one round when [LongPollConfig.Grace] is not positive.
//
// The margin is the whole point, and it must never be zero. A long-poll backend
// answers at its own hold deadline, so a client deadline equal to Hold races the
// server's reply and turns a perfectly healthy poll that returned "nothing
// changed" into a context-deadline error on roughly every round. Ten seconds
// covers the server's own scheduling slack (Nacos subtracts 500ms from the
// requested hold before it parks the request, then still has to write the
// response) plus a wide-area round trip.
const DefaultLongPollGrace = 10 * time.Second

// DefaultLongPollErrorPause is the floor between a failed round and the next
// one when [LongPollConfig.ErrorPause] is not positive. See the field's
// documentation for why this is pacing and not retry.
const DefaultLongPollErrorPause = 500 * time.Millisecond

// LongPollResult is what one round of a long poll observed.
//
// Changed and Value are separate rather than a *mamori.Value because the two
// backends this shape has to serve report differently: Nacos's listener returns
// only the identity of what moved, so the provider fetches the value itself
// before returning, while a Consul blocking query returns the new value in the
// same call. Both fill in this one struct.
type LongPollResult struct {
	// Changed reports that the backend signalled a change and that Value holds
	// the new value. False is the ordinary outcome of a round that simply hit
	// the hold deadline with nothing to report, and it emits nothing.
	Changed bool
	// Value is the new value, meaningful only when Changed is true.
	Value mamori.Value
}

// LongPollConfig configures [LongPoll].
type LongPollConfig struct {
	// Round performs exactly one held-open request and reports what it saw. It
	// is required.
	//
	// The ctx it is handed is already bounded at Hold+Grace and is cancelled
	// the moment the watch's own context ends, so an implementation only has to
	// pass it to whatever call it makes. It must not start a goroutine that
	// outlives that ctx.
	//
	// hold is passed in rather than read from the config by the implementation
	// because a long-poll backend is told how long to hold the request open in
	// the request itself (Nacos's Long-Pulling-Timeout header, Consul's `wait`
	// query parameter), and that number and the client-side deadline must come
	// from one place. Two independent declarations drift, and the direction
	// they drift in is silent: a client deadline shorter than the hold the
	// server was asked for fails every idle round with a timeout that looks
	// exactly like an unhealthy backend.
	Round func(ctx context.Context, hold time.Duration) (LongPollResult, error)

	// Baseline, when non-nil, runs once before the first Round and its result
	// is delivered on the same terms as a Round's.
	//
	// Its real job is ordering. A long-poll subscription is keyed on what the
	// client believes the current value to be (a content MD5 for Nacos, a
	// modify index for Consul), and Baseline is where that belief is
	// established. Running it here, inside the loop's own goroutine and
	// strictly before the first Round, is what lets a provider honestly declare
	// providertest.Config.WatchDeliversBaseline: the emitted baseline and the
	// state the first Round subscribes with are read from the same observation,
	// so a write landing after the baseline cannot fall into a gap - the first
	// Round carries a now-stale key and the backend answers it immediately.
	//
	// It is given a ctx bounded at Grace, not Hold+Grace: it is an ordinary
	// request, not a held-open one.
	//
	// Returning Changed false emits nothing, which is how a provider reports
	// "the value does not exist yet" without inventing one. A watch on an
	// absent key is legitimate: mamori's polling adapter is silent for a key
	// that does not exist, and this keeps a native watch consistent with it.
	Baseline func(ctx context.Context) (LongPollResult, error)

	// Hold is how long the backend is asked to hold one round open. Not
	// positive selects DefaultLongPollHold.
	Hold time.Duration

	// Grace is added to Hold to bound the client side of one round, and bounds
	// Baseline on its own. Not positive selects DefaultLongPollGrace.
	Grace time.Duration

	// ErrorPause is the floor between a failed round and the next one. Not
	// positive selects DefaultLongPollErrorPause.
	//
	// This is pacing, not retry, and the difference is worth being exact about
	// because this package promises no retry anywhere. A retry re-attempts a
	// failed operation and reports only the last outcome, which is what would
	// multiply against the reconciler's own backoff. LongPoll does neither: it
	// delivers every failure to the caller as an Update before it pauses, and
	// it never re-attempts the round it already reported. The pause exists for
	// one reason - a backend that rejects a round instantly (a 401 from an
	// expired token, a connection refused during a restart) would otherwise
	// turn the watch into a hot loop hammering that backend thousands of times
	// a second - and it is a fixed floor that never grows, so it cannot behave
	// like a backoff schedule either.
	ErrorPause time.Duration
}

// LongPoll drives a long-poll watch loop and returns the mamori.Update channel a
// [mamori.WatchableProvider] hands back from Watch.
//
// It exists because a long-poll watch is almost entirely loop-and-lifecycle
// code that has nothing to do with any particular backend: bound each round to
// the caller's context, tell the backend how long to hold and give the client a
// deadline strictly longer than that, deliver a change, deliver a failure
// without letting a fast-failing backend spin, and close the channel exactly
// once when the context ends. The vendor-specific part - what a round actually
// sends and how it decides a change happened - is the Round hook and nothing
// else.
//
// The contract it guarantees:
//
//   - The returned channel is closed when ctx is cancelled, and only then or on
//     a Round that reports the loop cannot continue. Closure means the watch has
//     ended; mamori does not resubscribe, so LongPoll never closes the channel
//     on a transient failure.
//   - Exactly one goroutine is started, and it has returned by the time the
//     channel is closed. Nothing is left running after ctx ends, which is what
//     providertest's NoGoroutineLeak case checks with goleak.
//   - No round is ever re-attempted. Every outcome, success or failure, reaches
//     the caller.
//
// A nil Round is a programming error and is reported as mamori.ErrInvalid
// rather than deferred to a nil dereference inside the goroutine.
func LongPoll(ctx context.Context, cfg LongPollConfig) (<-chan mamori.Update, error) {
	if cfg.Round == nil {
		return nil, fmt.Errorf("httpcore: LongPollConfig.Round is required: %w", mamori.ErrInvalid)
	}
	hold := cfg.Hold
	if hold <= 0 {
		hold = DefaultLongPollHold
	}
	grace := cfg.Grace
	if grace <= 0 {
		grace = DefaultLongPollGrace
	}
	pause := cfg.ErrorPause
	if pause <= 0 {
		pause = DefaultLongPollErrorPause
	}

	// Buffered by one so a single pending Update does not pin the loop
	// goroutine while a consumer is between receives. Anything beyond one is
	// buffering stale values: for a config watch only the newest matters.
	ch := make(chan mamori.Update, 1)

	go func() {
		defer close(ch)

		// emit blocks until the Update is taken or ctx ends, and reports
		// whether the loop may continue. Blocking rather than dropping is
		// deliberate: a dropped change is a value the application never sees,
		// and mamori's consumer always drains.
		emit := func(u mamori.Update) bool {
			select {
			case ch <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}

		// deliver applies one round's outcome and reports whether the loop may
		// continue.
		deliver := func(res LongPollResult, err error) bool {
			// Checked before anything is emitted. On cancellation the in-flight
			// request fails with a context error, and reporting that as a watch
			// failure would surface "context canceled" as a backend fault on
			// every clean shutdown.
			if ctx.Err() != nil {
				return false
			}
			if err != nil {
				if !emit(mamori.Update{Err: err}) {
					return false
				}
				select {
				case <-ctx.Done():
					return false
				case <-time.After(pause):
					return true
				}
			}
			if !res.Changed {
				return true
			}
			return emit(mamori.Update{Value: res.Value})
		}

		if cfg.Baseline != nil {
			bctx, cancel := context.WithTimeout(ctx, grace)
			res, err := cfg.Baseline(bctx)
			cancel()
			if !deliver(res, err) {
				return
			}
		}

		for {
			if ctx.Err() != nil {
				return
			}
			// A fresh deadline per round, cancelled before the next one is
			// built. Deriving it once outside the loop would give the whole
			// watch a single deadline and end it after one hold.
			rctx, cancel := context.WithTimeout(ctx, hold+grace)
			res, err := cfg.Round(rctx, hold)
			cancel()
			if !deliver(res, err) {
				return
			}
		}
	}()

	return ch, nil
}
