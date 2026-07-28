package mamori

import "context"

// WatchRef watches a single ref for changes, choosing a provider's native
// watch when it implements WatchableProvider and falling back to the shared
// polling adapter (pollWatch) otherwise. This is exactly the per-position
// source-selection engine.start performs for every ref in a Watch[T]'s field
// specs (see reconciler.go); it is exported here so a caller that only wants
// to watch one ref directly - the config server, most notably - gets the
// identical selection behavior and options handling as the reconciler,
// rather than a second, independently-maintained copy of the same decision.
//
// opts is turned into an *options the same way Load and Watch build theirs:
// defaultOptions() overlaid with opts in the order given. Of everything an
// *options carries, only clock, pollInterval, jitter, and the WithBackoff
// window matter here - the fields pollWatch itself reads - but the full Option
// surface (WithClock, WithPollInterval, WithJitter, ...) is accepted so a
// caller does not need a second, narrower configuration vocabulary just for
// this entry point. Since backoff lives in the polling adapter, it reaches a
// ref watched through here on exactly the terms WithBackoff documents: polled
// refs and native-watch fallbacks, never a native watch that started cleanly.
//
// The returned channel is closed when ctx is cancelled, matching both
// WatchableProvider's and pollWatch's own contract.
func WatchRef(ctx context.Context, p Provider, ref Ref, opts ...Option) <-chan Update {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}
	return watchRef(ctx, p, ref, o)
}

// watchRef is the extracted native-watch-or-poll selection engine.start's
// per-position loop used to inline directly (see reconciler.go's per-ref
// loop in start): a WatchableProvider is watched natively, falling back to
// pollWatch if the native watch fails to start; every other Provider is
// polled outright. Moving it here, unexported and taking the already-built
// *options start already has (e.o), lets start call it with the exact
// options it always used, with nothing rebuilt or re-derived - the single
// behavior-preservation requirement this extraction exists to satisfy.
// WatchRef (above) is the public, ...Option-taking wrapper for a caller that
// does not already have an *options of its own.
func watchRef(ctx context.Context, p Provider, ref Ref, o *options) <-chan Update {
	if wp, isW := p.(WatchableProvider); isW {
		ch, werr := wp.Watch(ctx, ref)
		if werr != nil {
			// Fall back to polling if native watch cannot start.
			return pollWatch(ctx, p, ref, o)
		}
		return ch
	}
	return pollWatch(ctx, p, ref, o)
}
