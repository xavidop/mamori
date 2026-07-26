package mamoriprov

import "time"

// This file is Watch's monotonic freshness guard: the small amount of state
// that stops a reconnecting watch from travelling backwards in time. It reads
// exactly one wire field, valueBody.ResolvedAt (see resolve.go), and is used
// from exactly one place, dispatchSSEFrame's "update" branch (see watch.go).

// resolvedAtSkewTolerance is how much OLDER than the newest already-delivered
// value an incoming update may be dated and still be forwarded. Only an update
// dated more than this before the watermark is dropped.
//
// The alternative, a strictly-older comparison, is wrong in one specific and
// entirely reachable case. Two replicas straddling an upstream change - A
// polls just before it, B just after - fetch their two values MILLISECONDS
// apart in real time, not the tens of seconds their poll intervals suggest. A
// few milliseconds of clock skew in the wrong direction is then enough to make
// B's genuinely newer value look older than A's, and a strict guard would drop
// it. Inside a band that narrow the two wall clocks simply cannot order the
// two fetches at all, so the guard abstains and delivers.
//
// The two mistakes are not symmetric, which is what settles the tie. An update
// wrongly DROPPED is lost until the binding changes again: nothing re-sends a
// frame the client discarded, so the consumer sits on the older value
// indefinitely. An update wrongly FORWARDED from inside the band is at most
// one second "backwards", i.e. bytes fetched at essentially the same moment as
// the ones already delivered - a difference with no practical consequence for
// a reconciler. Delivering is the cheaper mistake, so ties go to delivering.
//
// One second is chosen to sit in the wide gap between the two quantities this
// guard has to tell apart: it is roughly a thousand times the millisecond-scale
// skew of NTP-synced hosts, and roughly a thirtieth of the ~30s upstream poll
// interval that produces the replica lag the guard exists to catch. That makes
// it wide enough to swallow any skew a functioning fleet exhibits, and far too
// narrow to swallow a replica that is a whole poll cycle behind.
const resolvedAtSkewTolerance = time.Second

// freshnessGuard remembers, per binding name, the newest resolved_at a watch
// has actually DELIVERED to its caller, so an update dated meaningfully before
// it can be dropped instead of forwarded.
//
// # Why a watch needs this at all
//
// A config server run as N replicas behind one address has every replica
// watching upstream on its OWN independent schedule (~30s poll, jittered), so
// two replicas routinely hold the same binding at different ages. Watch
// reconnects automatically and ROTATES to the next configured endpoint as it
// does (see watchLoop), which means a reconnect can land on a laggier replica.
// That replica opens the stream by sending its current snapshot, and without
// this guard the client would forward that older value as though it were a
// brand new change; the consumer's reconciler applies it and the program
// silently goes back in time. Nothing else on this wire can catch that:
// Version is provider-supplied and frequently absent, and the bytes alone
// carry no ordering.
//
// # What it deliberately does NOT do
//
// The guard only ever engages when BOTH the watermark and the incoming update
// carry a resolved_at. A server that does not send the field (an older server,
// or one that simply omitted it for this value) gets exactly the behavior it
// had before the guard existed: every update forwarded, unconditionally.
// Withholding a value from a caller because it could not be dated would be a
// far worse failure than delivering one out of order, so an undatable update
// is never the guard's business. Error updates are likewise never routed
// through here at all (see dispatchSSEFrame): an error describes the current
// connection, not a value, and cannot be out of order.
//
// # The clock-skew assumption
//
// resolved_at is stamped by each replica's own wall clock, so comparing
// timestamps ACROSS replicas assumes those clocks are roughly in step. That
// assumption is real, and stating it plainly matters more than pretending it
// away: there is no shared logical clock or fleet-wide sequence number on this
// wire, so a replica whose clock is badly wrong will be believed.
//
// It holds in practice because the two quantities involved are orders of
// magnitude apart. The lag this guard catches is a replica's upstream poll
// interval - tens of seconds - while the skew between NTP-synced hosts is
// milliseconds. Lag dwarfs skew by roughly three orders of magnitude, so a
// value from a replica a poll cycle behind is still unambiguously older even
// after the worst realistic skew is subtracted from it. The narrow band where
// skew COULD change the answer is exactly what resolvedAtSkewTolerance carves
// out and hands back to the caller. A fleet with genuinely broken clocks -
// tens of seconds out - gets no ordering guarantee here, but it also has much
// larger problems than one stale watch update.
//
// # Concurrency
//
// One guard belongs to one watch: watchLoop creates it and it is only ever
// touched from that single goroutine (watchLoop and everything it calls
// synchronously below it), so it needs no locking. It must not be shared
// between watches, which is why nothing hangs it off Provider.
type freshnessGuard struct {
	// delivered maps binding name to the newest resolved_at forwarded for that
	// name. Per name, not per watch, because one connection can carry frames
	// for several bindings (the server's /v1/watch accepts repeated name
	// parameters) and one lagging binding must not hold back another.
	delivered map[string]time.Time
}

// newFreshnessGuard returns a guard with no watermarks yet, i.e. one that
// forwards the first update it sees for every name.
func newFreshnessGuard() *freshnessGuard {
	return &freshnessGuard{delivered: make(map[string]time.Time)}
}

// allows reports whether an update for binding name, dated resolvedAt, may be
// forwarded to the caller.
//
// It answers true in every case except the single one it exists for: a
// watermark and an incoming timestamp that BOTH exist, with the incoming one
// dated more than resolvedAtSkewTolerance before the watermark. A nil
// resolvedAt, or a name with no watermark yet, is always allowed - see the
// type's doc comment for why the guard must never be the reason a value is
// withheld from a server that does not speak this dialect.
func (g *freshnessGuard) allows(name string, resolvedAt *time.Time) bool {
	if resolvedAt == nil {
		return true
	}
	watermark, seen := g.delivered[name]
	if !seen {
		return true
	}
	return !resolvedAt.Before(watermark.Add(-resolvedAtSkewTolerance))
}

// record advances name's watermark to resolvedAt. It is called only after an
// update has actually been forwarded to the caller, so a dropped update (or
// one whose send lost the race with context cancellation) never moves the
// watermark.
//
// The watermark only ever ratchets forward. An update delivered from inside
// the tolerance band (see resolvedAtSkewTolerance) is a real delivery, but
// letting its older timestamp become the new watermark would lower the bar for
// the next comparison, and a run of near-ties could then walk the watermark
// backwards a tolerance at a time until the guard was watching a timestamp
// from minutes ago. Keeping the maximum costs nothing and removes that drift
// entirely.
func (g *freshnessGuard) record(name string, resolvedAt *time.Time) {
	if resolvedAt == nil {
		return
	}
	if current, seen := g.delivered[name]; seen && !resolvedAt.After(current) {
		return
	}
	g.delivered[name] = *resolvedAt
}
