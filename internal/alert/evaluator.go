package alert

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/slavehealth"
)

// State tracks where a (target, alert) pair is in its lifecycle.
type State string

const (
	StateOK      State = "ok"
	StatePending State = "pending"
	StateFiring  State = "firing"
)

// Event is emitted by the evaluator when an alert changes state. Dispatchers
// turn it into a webhook call / log line / exec invocation.
type Event struct {
	Time      time.Time
	Target    config.TargetRef
	AlertName string
	Alert     config.Alert
	Prev      State
	Next      State
	Cycle     scheduler.Cycle
	// Firing and Live describe the quorum at dispatch time: how many sources
	// were simultaneously firing, out of how many were reporting. Both are 0
	// for alerts without quorum. Available in action templates as {{.Firing}}
	// and {{.Live}}.
	Firing int
	Live   int
	// FiringSources names every non-stale source in StateFiring at dispatch
	// time, sorted. Cycle.Source alone is just whichever source's cycle drove
	// this transition, which under quorum understates the outage to one name;
	// this is the full set. Populated for non-quorum alerts too, where it
	// answers "who else is seeing this" for a per-source dispatch. On a resolve
	// it is whoever is *still* firing — under quorum the aggregate clears as
	// soon as it drops below threshold, so a minority can remain. Empty on a
	// standalone node, whose cycles carry no source name. Available in
	// templates as {{range .FiringSources}}.
	FiringSources []string
}

// Dispatcher delivers alert events. Must be safe for concurrent use.
type Dispatcher interface {
	Dispatch(ctx context.Context, e Event)
}

// DispatchFilter lets a Dispatcher decline an event before the evaluator
// queues it. Which transitions are worth notifying about is the dispatcher's
// policy, not the evaluator's, but the queue is a bounded resource the
// evaluator owns — so the policy is asked rather than duplicated. A Dispatcher
// that does not implement this receives every transition.
type DispatchFilter interface {
	Wants(e Event) bool
}

// aggKey identifies one (target, alert) pair. Sources live one level below, so
// the evaluator can both keep independent per-source counters and compute a
// cross-source aggregate without scanning unrelated state.
type aggKey struct {
	target string
	alert  string
}

type alertState struct {
	state      State
	consecHits int // consecutive cycles the condition has been true
	// lastSeen is the master's receive time, not the cycle's own timestamp:
	// the latter is slave-supplied, and ingest accepts one up to
	// config.MaxFutureSkew ahead, which was enough for one hostile slave to
	// age every honest source out of tally and become a majority of itself.
	lastSeen time.Time
	// seenCycle spells "a cycle has been accepted" explicitly rather than
	// letting lastCycle's zero value mean it: the two states are otherwise
	// indistinguishable, and the zero value would admit rather than deny.
	seenCycle bool
	// lastCycle is the newest timestamp accepted for this source, and the
	// identity a replay is recognised by: (target, source, timestamp)
	// identifies one measurement in storage too, so the alert path reads the
	// same tuple rather than inventing a second one. Ordering is the concern
	// master.cycleDedup's set does not cover: an older healthy batch
	// delivered late is a distinct measurement that must be stored and
	// must not clear a newer firing state. A requeue after a lost ack
	// redelivers the same measurement, which incremented consecHits twice and
	// fired a sustained:2 alert off one bad cycle.
	lastCycle time.Time
	// pastCycle is the same high-water mark over the cycles that were not
	// ahead of the master's clock when they arrived. Ordering a genuine cycle
	// against this one rather than lastCycle is what keeps a forward-dated
	// stamp from barring the real cycles behind it.
	pastCycle time.Time
	// ahead holds, oldest first, the timestamps accepted while they were ahead
	// of the master's clock. Once the clock passes one, no ordering mark
	// separates its redelivery from a genuine cycle of the same age, so the
	// stamps are matched exactly until pastCycle rises past them. Ordering the
	// ahead arm against lastCycle costs every genuine cycle stamped between the
	// master's clock and an accepted forward-dated one: those are skipped while
	// they are still ahead, whatever their distance from it.
	ahead []int64
}

// admits reports whether this timestamp is a measurement this source has not
// had applied yet.
func (st *alertState) admits(t, now time.Time) bool {
	if !st.seenCycle {
		return true
	}
	if t.After(now) {
		return t.After(st.lastCycle)
	}
	return t.After(st.pastCycle) && !slices.Contains(st.ahead, t.UnixNano())
}

// accept records a timestamp as applied. Eviction past limit drops the oldest,
// which fails open to the pre-guard double-apply rather than to silence.
func (st *alertState) accept(t, now time.Time, limit int) {
	st.seenCycle = true
	if t.After(st.lastCycle) {
		st.lastCycle = t
	}
	if t.After(now) {
		st.ahead = append(st.ahead, t.UnixNano())
		if len(st.ahead) > limit {
			st.ahead = append(st.ahead[:0], st.ahead[len(st.ahead)-limit:]...)
		}
		return
	}
	st.pastCycle = t
	nano := t.UnixNano()
	// pastCycle only rises, so a stamp at or below it is already barred by the
	// ordering arm and needs no exact match.
	st.ahead = slices.DeleteFunc(st.ahead, func(v int64) bool { return v <= nano })
}

// aggWarmup tracks how much cross-source consensus a quorum aggregate has
// accumulated since its key was first observed. It is kept separate from
// tally's live/stale bookkeeping in bySource because tally deletes stale
// sources outright — sourcesSeen must survive that deletion, otherwise a
// slave that reported once and then died before a peer ever reported would
// look identical to a slave that never reported at all, defeating the
// "2 distinct sources" half of warm-up (see TestQuorumPrunesStaleSources).
type aggWarmup struct {
	firstSeen   time.Time
	sourcesSeen map[string]struct{}
}

// ready reports whether a quorum aggregate has accumulated enough consensus
// to trust a new FIRING transition. Guards against the majority-of-1 flap on
// restart: with an empty e.states, the first source to report makes
// Threshold(1) == 1, so a lone firing source looks like a "majority" until a
// peer's first cycle arrives and immediately un-fires it. Two independently
// reporting sources is real consensus; short of that, degrade to today's
// single-observer behaviour once the staleness window has elapsed so a
// genuinely single-source deployment still alerts.
func (w *aggWarmup) ready(now time.Time, window time.Duration) bool {
	return len(w.sourcesSeen) >= 2 || now.Sub(w.firstSeen) >= window
}

// Evaluator is a scheduler.Sink that evaluates each cycle against the alerts
// configured on the target and emits state transitions to a Dispatcher.
// State is kept in-memory per (target, alert, source); on restart all alerts
// start in StateOK, which avoids spurious "RESOLVED" events after a crash.
type Evaluator struct {
	log        *slog.Logger
	store      *config.Store
	dispatcher Dispatcher
	conds      map[string]Condition // alert name → parsed condition
	// quorumEnabled snapshots each alert's Quorum.Enabled() as of the last
	// refresh, so Refresh can detect an on/off flip across a config reload
	// and drop the now-stale aggregate for that alert (see pruneStaleAggregates).
	quorumEnabled map[string]bool

	// nowFn is the master's receive clock, injected so tests can drive
	// staleness without sleeping. Never the cycle's own timestamp: that is
	// slave-supplied.
	nowFn func() time.Time

	mu     sync.Mutex
	states map[aggKey]map[string]*alertState // target+alert → source → per-source state
	agg    map[aggKey]State                  // target+alert → last dispatched aggregate state (quorum alerts only)
	warmup map[aggKey]*aggWarmup             // target+alert → warm-up bookkeeping (quorum alerts only)

	// pending carries committed transitions to the delivery workers, one
	// queue per shard. Dispatch is deliberately off the caller's goroutine:
	// it ran inline under the ingest handler, where a batch of transitions
	// against an unresponsive endpoint pinned that handler for hours — each
	// cycle bounded, their sum not. A (target, alert) pair always hashes to
	// the same shard, so a firing still precedes its own resolve, and a
	// transition a shard refuses is reverted rather than delivered out of
	// order.
	pending [dispatchShards]chan queuedEvent
	// queuedBytes is the payload each shard's queue currently retains. Per
	// shard, not global: one counter let a single stalled shard's backlog
	// refuse transitions on all eight, which is the fleet-wide blast radius
	// the sharding exists to remove. Written by the producer under mu and by
	// that shard's worker on release, so it is atomic rather than mu-guarded.
	queuedBytes      [dispatchShards]atomic.Int64
	quit             chan struct{}
	closeOnce        sync.Once
	inflight         sync.WaitGroup
	dispatchRefusals atomic.Uint64

	// excludedMu guards excluded, which rate-limits the warning below. It is
	// separate from mu because the freshness check runs before the state lock
	// is taken.
	excludedMu sync.Mutex
	excluded   map[exclusionKey]exclusionRecord
}

// exclusionKey identifies one reason one source is contributing nothing to
// alerting. Source names are bounded by the master's registry, which refuses a
// cycle under a name it does not hold, so the map is bounded by that ceiling
// times the reasons below.
type exclusionKey struct{ source, reason string }

type exclusionRecord struct {
	at         time.Time
	suppressed int
}

const (
	// reasonClockSkew is a cycle that arrived older than alertFreshness. A
	// slave whose clock lags stably by more than that window has every cycle
	// refused while its data keeps being stored.
	reasonClockSkew = "clock_skew"
	// reasonDuplicate is a cycle whose timestamp did not advance: a replay, or
	// a producer whose clock stepped backwards.
	reasonDuplicate = "duplicate_cycle"
)

func NewEvaluator(log *slog.Logger, store *config.Store, dispatcher Dispatcher) (*Evaluator, error) {
	e := &Evaluator{
		log:        log,
		store:      store,
		dispatcher: dispatcher,
		nowFn:      time.Now,
		conds:      make(map[string]Condition),
		states:     make(map[aggKey]map[string]*alertState),
		agg:        make(map[aggKey]State),
		warmup:     make(map[aggKey]*aggWarmup),
		excluded:   make(map[exclusionKey]exclusionRecord),
		quit:       make(chan struct{}),
	}
	if err := e.refreshConditions(); err != nil {
		return nil, err
	}
	for i := range e.pending {
		e.pending[i] = make(chan queuedEvent, dispatchShardDepth)
		go e.deliverLoop(e.pending[i])
	}
	return e, nil
}

// dispatchQueueDepth is the burst this absorbs, not the producer's maximum:
// evaluate emits one Event per alert a target names and config bounds that
// count nowhere, so a batch can produce a multiple of it. It does not need to
// be the maximum — a refused transition is reverted and re-detected on the
// next cycle rather than lost.
const dispatchQueueDepth = cluster.MaxCyclesPerBatch

// dispatchShards is how many delivery workers run, and so the blast radius of
// one unresponsive endpoint: Dispatch bounds itself at actionTimeout per
// action, so a dead webhook does not hang a worker forever — it caps that
// worker's delivery rate at one event per budget, which on a single worker is
// the whole fleet's rate. Sharding by (target, alert) leaves the stall on the
// keys that own the bad endpoint and lets every other key keep paging. It is a
// fanout width rather than a derived bound: each shard costs one goroutine
// parked in a delivery's own budget, never CPU, and the queued total stays
// dispatchQueueDepth because the depth is split rather than multiplied.
const dispatchShards = 8

// dispatchShardDepth is one worker's queue. Ordering is per shard, which is
// why the hash covers the whole aggKey: a firing and its own resolve carry the
// same target and alert, so both land in this FIFO and cannot overtake.
const dispatchShardDepth = dispatchQueueDepth / dispatchShards

// dispatchQueueBytes bounds the payload the queues retain, which the depth
// alone does not: an Event embeds its whole Cycle, and the largest one ingest
// accepts holds config.MaxPingsPerCycle RTTs beside cluster.MaxHopsPerCycle
// hop rows of cluster.MaxRTTsPerHop each — about 1.18 MB, so the depth by
// itself admits 1.2 GB of queued notifications. The producer's own maximum is
// smaller (a 10×30 MTR walk is ~47 KB, the deployed 20-ping shape a few KB),
// so at any shape a probe emits the depth is reached first; only a cycle
// pushed at the ingest bound reaches this. Refusal is the same
// revert-and-retry as a full shard, so the cost of reaching it is a page
// delayed by one interval, not a page lost.
const dispatchQueueBytes = 64 << 20

// dispatchShardBytes is one shard's share. Split for the same reason the depth
// is: a global counter meant one stalled shard's backlog refused every other
// shard's transitions, putting back the fleet-wide coupling the shards remove.
const dispatchShardBytes = dispatchQueueBytes / dispatchShards

// The shards split the depth rather than multiplying it, so an uneven split
// would quietly change the queued total. Negative here fails the build, the
// same way probe pins its echo window to the walk's.
const _ uint = 0 - dispatchQueueDepth%dispatchShards

// queuedEvent pairs an Event with the payload bytes it reserved, so a worker
// releases exactly what the producer charged rather than re-measuring a value
// the scrub may have changed.
type queuedEvent struct {
	ev    Event
	bytes int64
	shard int
}

// shardFor maps a (target, alert) pair onto a delivery worker. FNV-1a over the
// two strings with a separator, so "a"+"bc" and "ab"+"c" do not collide, and
// allocation-free because it runs under e.mu on every transition.
func shardFor(target, alert string) int {
	const (
		fnvOffset = uint64(14695981039346656037)
		fnvPrime  = uint64(1099511628211)
	)
	h := fnvOffset
	for i := 0; i < len(target); i++ {
		h = (h ^ uint64(target[i])) * fnvPrime
	}
	h *= fnvPrime
	for i := 0; i < len(alert); i++ {
		h = (h ^ uint64(alert[i])) * fnvPrime
	}
	return int(h % dispatchShards)
}

// eventBytes is the variable-length payload one queued Event retains. The
// fixed part of the struct is what dispatchQueueDepth bounds; this covers the
// slices behind it, which are the only fields a single cycle can make large.
func eventBytes(ev Event) int64 {
	const durSize = int64(8)
	n := int64(len(ev.Cycle.RTTs)) * durSize
	for _, h := range ev.Cycle.Hops {
		n += int64(len(h.RTTs))*durSize + int64(len(h.IP)) + int64(len(h.Unreach))
	}
	for _, s := range ev.Cycle.HTTPSamples {
		n += int64(len(s.Err))
	}
	for _, src := range ev.FiringSources {
		n += int64(len(src))
	}
	return n
}

// Close signals the delivery workers to stop and returns without waiting for
// them: a worker may be inside a delivery to an endpoint that never answers,
// which is the wait this queue exists to keep off every other goroutine. Each
// exits after that delivery's own budget, abandoning whatever is queued.
func (e *Evaluator) Close() {
	e.closeOnce.Do(func() {
		queued := 0
		for _, ch := range e.pending {
			queued += len(ch)
		}
		if queued > 0 {
			e.log.Error("alert notifications undelivered at shutdown", "queued", queued)
		}
		close(e.quit)
	})
}

func (e *Evaluator) deliverLoop(pending <-chan queuedEvent) {
	for {
		select {
		case q := <-pending:
			e.deliver(q)
		case <-e.quit:
			// Balance inflight and the byte reservation for whatever is still
			// queued so a caller waiting on either cannot hang past shutdown.
			for {
				select {
				case q := <-pending:
					e.queuedBytes[q.shard].Add(-q.bytes)
					e.inflight.Done()
				default:
					return
				}
			}
		}
	}
}

// deliver bounds one event by its own action count: Dispatch runs an event's
// actions in sequence and each bounds itself at actionTimeout.
func (e *Evaluator) deliver(q queuedEvent) {
	ev := q.ev
	defer e.inflight.Done()
	defer e.queuedBytes[q.shard].Add(-q.bytes)
	// Dispatch ran under scheduler.Fanout's recover() while it was inline in
	// OnCycle; on its own goroutine a Dispatcher panic would take the process
	// down instead, so the perimeter moves with it.
	defer func() {
		if v := recover(); v != nil {
			e.log.Error("alert dispatch panicked",
				"target", ev.Target.ID(), "alert", ev.AlertName, "panic", v)
		}
	}()
	budget := time.Duration(max(len(ev.Alert.Actions), 1)) * actionTimeout
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	e.dispatcher.Dispatch(ctx, ev)
}

// flush blocks until every queued notification has been delivered or
// abandoned by Close. It is the synchronisation point tests take instead of
// sleeping on the worker.
func (e *Evaluator) flush() { e.inflight.Wait() }

// Refresh re-parses conditions from the current config — call after a config
// reload to pick up new or changed alerts.
func (e *Evaluator) Refresh() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	oldEnabled := e.quorumEnabled
	if err := e.refreshConditions(); err != nil {
		return err
	}
	e.pruneStaleAggregates(oldEnabled)
	e.pruneDepartedTargets()
	return nil
}

// pruneDepartedTargets drops per-source state for a target or alert the
// reloaded config no longer names. tally prunes only the key a cycle is
// arriving for, so a target renamed or removed leaves an entry — its ahead
// slice included — that nothing revisits for the process's life. Retention is
// otherwise load-bearing: a silent source's StateFiring is what dispatches its
// eventual resolve, so this drops only targets that can no longer produce a
// cycle at all. Health targets are exempt because they live outside the stored
// config. Must be called with e.mu held.
func (e *Evaluator) pruneDepartedTargets() {
	cfg := e.store.Current()
	// Keyed by the pair, not by target and alert independently: an alert that
	// still exists for other targets but is detached from this one can produce
	// no further cycle for this key either, since evaluate only ever iterates
	// the target's own Alerts list.
	live := make(map[aggKey]struct{})
	for _, g := range cfg.Targets {
		for _, t := range g.Targets {
			id := config.TargetRef{Group: g.Group, Target: t}.ID()
			for _, name := range t.Alerts {
				live[aggKey{target: id, alert: name}] = struct{}{}
			}
		}
	}
	for key := range e.states {
		if _, ok := cfg.Alerts[key.alert]; ok {
			if _, ok := live[key]; ok {
				continue
			}
			if group, _, ok := strings.Cut(key.target, "/"); ok && slavehealth.IsHealthGroup(group) {
				continue
			}
		}
		delete(e.states, key)
		delete(e.agg, key)
		delete(e.warmup, key)
	}
}

func (e *Evaluator) refreshConditions() error {
	cfg := e.store.Current()
	conds := make(map[string]Condition, len(cfg.Alerts))
	enabled := make(map[string]bool, len(cfg.Alerts))
	for name, a := range cfg.Alerts {
		c, err := ParseCondition(a.Condition)
		if err != nil {
			return fmt.Errorf("alert %q: %w", name, err)
		}
		conds[name] = c
		enabled[name] = a.Quorum.Enabled()
	}
	e.conds = conds
	e.quorumEnabled = enabled
	return nil
}

// pruneStaleAggregates drops the aggregate + warm-up state for any alert
// whose Quorum.Enabled() flipped across this reload (or that disappeared
// from config entirely). Must be called with e.mu held.
//
// Without this, e.agg keeps whatever State it last dispatched under the old
// mode. Toggling quorum off then back on would then compare a freshly
// computed tally against that stale prevAgg and manufacture a phantom
// transition — e.g. sources recover individually while quorum is off (and
// are already reported per-source), then re-enabling quorum diffs the
// now-OK tally against a leftover StateFiring and dispatches a duplicate,
// stale "resolve" nobody asked for.
//
// Per-source states in e.states are deliberately left untouched: resetting
// them to StateOK would be actively harmful, forcing a source that's still
// genuinely firing back through Sustained before it could report firing
// again. Per-source dispatch already recomputes prev-vs-next from that state
// every cycle regardless of quorum mode, so a real recovery still dispatches
// a correct resolve without any reset — only the cross-source aggregate
// needs a clean slate.
//
// The warm-up map is swept by the same rule, and independently: a quorum
// alert that never dispatched has a warmup entry but no agg entry, so
// sweeping only agg's keys both leaked those entries for alerts that left
// the config and kept a stale firstSeen across a disable/re-enable — making
// the 3×-interval window look long-elapsed, so the first partial-data
// evaluation paged immediately, the exact flap warm-up exists to prevent.
func (e *Evaluator) pruneStaleAggregates(oldEnabled map[string]bool) {
	stale := func(alert string) bool {
		was, existed := oldEnabled[alert]
		now, stillExists := e.quorumEnabled[alert]
		return !existed || !stillExists || was != now
	}
	for key := range e.agg {
		if stale(key.alert) {
			delete(e.agg, key)
			delete(e.warmup, key)
		}
	}
	for key := range e.warmup {
		if stale(key.alert) {
			delete(e.warmup, key)
		}
	}
}

func (e *Evaluator) OnCycle(ctx context.Context, cy scheduler.Cycle) {
	// A cycle that sent nothing measured nothing: every condition field reads
	// zero on it, which is indistinguishable from a perfect cycle and would
	// clear a sustained counter and resolve a live alert on a gap. Returning
	// before lastSeen is touched also lets the source age out of the quorum
	// denominator, which is what a source reporting no data should do.
	if cy.Sent == 0 {
		return
	}
	cfg := e.store.Current()
	alerts := cy.Target.Target.Alerts
	if len(alerts) == 0 {
		return
	}
	// One read for the whole pass, and the only clock any state below is keyed
	// on — never cy.Time, which the pushing slave chose.
	now := e.nowFn()
	liveness := livenessWindow(cfg.Interval)
	aheadLimit := aheadCap(cfg.Interval)
	// Skipped whole like a cycle that sent nothing, so the source ages out
	// rather than voting on data it replayed out of its own history.
	if age := now.Sub(cy.Time); age > alertFreshness(cfg.Interval) {
		e.warnExcluded(now, reasonClockSkew, cy, "age", age, "limit", alertFreshness(cfg.Interval))
		return
	}

	toDispatch, skipped, refused := e.evaluate(cfg, cy, now, liveness, aheadLimit)

	// Logged after evaluate released e.mu: slog writes synchronously, so a line
	// emitted under the evaluator's only lock stalls OnCycle for every other
	// target — on the one path that runs when delivery is already backed up.
	// One line per pass rather than per alert, for the same reason warnExcluded
	// rate-limits: a stalled shard re-refuses the same reverted transition on
	// every cycle.
	if n := len(refused); n > 0 {
		r := refused[0]
		e.log.Error("alert notification refused, delivery queue full; transition will be retried",
			"example_target", r.target, "example_alert", r.alert,
			"prev", r.prev, "next", r.next, "reason", r.reason,
			"held", r.held, "limit", r.limit,
			"refused_this_cycle", n, "refused_total", r.total)
	}

	if len(skipped) > 0 {
		e.warnExcluded(now, reasonDuplicate, cy, "alerts", len(skipped), "behind", slices.Max(skipped))
	}

	// Only transitions the queue accepted reach here: evaluate reverts the
	// ones it refused, so the log names what an operator will actually be
	// paged about.
	for _, ev := range toDispatch {
		e.log.Info("alert state change",
			"target", ev.Target.ID(), "alert", ev.AlertName, "source", cy.Source,
			"prev", ev.Prev, "next", ev.Next, "hits", ev.Cycle.Sent,
			"firing", ev.Firing, "live", ev.Live)
	}
}

// enqueueDispatch offers one transition to its shard's delivery worker,
// reporting whether the queue took it. It never blocks — blocking here is the
// pinned caller this queue exists to prevent — so a full shard or an exhausted
// byte budget refuses, and the caller undoes the transition rather than
// keeping a state nobody was told about. Called with e.mu held, which is what
// makes the commit and the refusal one step and what makes the byte
// reservation single-producer; the workers never take that lock.
func (e *Evaluator) enqueueDispatch(ev Event) (bool, *dispatchRefusal) {
	if f, ok := e.dispatcher.(DispatchFilter); ok && !f.Wants(ev) {
		// The dispatcher will discard this event on arrival, so queueing it
		// spends shard depth and byte budget that exist to carry the pages
		// that do notify. Reported as accepted: the transition is committed
		// and there is nothing left to deliver.
		return true, nil
	}
	ev = scrubHealthAddresses(ev)
	size := eventBytes(ev)
	shard := shardFor(ev.Target.ID(), ev.AlertName)
	if held := e.queuedBytes[shard].Add(size); held > dispatchShardBytes {
		e.queuedBytes[shard].Add(-size)
		return false, e.refuseDispatch(ev, "bytes", held, dispatchShardBytes)
	}
	e.inflight.Add(1)
	select {
	case e.pending[shard] <- queuedEvent{ev: ev, bytes: size, shard: shard}:
		return true, nil
	default:
		e.queuedBytes[shard].Add(-size)
		e.inflight.Done()
		return false, e.refuseDispatch(ev, "depth", int64(dispatchShardDepth), dispatchShardDepth)
	}
}

// dispatchRefusal is one refused transition, returned rather than logged so
// the line is emitted after e.mu is released: slog writes synchronously, and
// holding the evaluator's only lock across a journald write blocked OnCycle
// for every other target on the exact path that runs when delivery is already
// backed up.
type dispatchRefusal struct {
	target, alert, reason string
	prev, next            State
	held                  int64
	limit                 int
	total                 uint64
}

// refuseDispatch counts one refused transition and describes it. reason names
// which of the two ceilings was reached, because they call for different
// remedies: "depth" is a slow endpoint on this shard's keys, "bytes" is one
// cycle carrying far more measurement than the fleet's shape.
func (e *Evaluator) refuseDispatch(ev Event, reason string, held int64, limit int) *dispatchRefusal {
	return &dispatchRefusal{
		target: ev.Target.ID(), alert: ev.AlertName, reason: reason,
		prev: ev.Prev, next: ev.Next,
		held: held, limit: limit, total: e.dispatchRefusals.Add(1),
	}
}

// DispatchRefusals reports transitions the delivery queue could not hold.
// They are reverted and re-detected on the next cycle rather than lost, so a
// non-zero value is a delivery backlog, not missing pages.
func (e *Evaluator) DispatchRefusals() uint64 { return e.dispatchRefusals.Load() }

// evaluate runs the per-alert state machine under e.mu, held via defer because
// scheduler.Fanout recovers sink panics — a panic here must not leave the
// evaluator locked forever inside a process that keeps reporting healthy.
func (e *Evaluator) evaluate(cfg *config.Config, cy scheduler.Cycle, now time.Time, window time.Duration, aheadLimit int) (toDispatch []Event, skipped []time.Duration, refused []*dispatchRefusal) {
	warmupWindow := stalenessWindow(cfg.Interval)
	freshness := alertFreshness(cfg.Interval)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, name := range cy.Target.Target.Alerts {
		alertCfg, ok := cfg.Alerts[name]
		if !ok {
			continue
		}
		cond, ok := e.conds[name]
		if !ok {
			continue
		}

		key := aggKey{target: cy.Target.ID(), alert: name}
		bySource, ok := e.states[key]
		if !ok {
			bySource = make(map[string]*alertState, 4)
			e.states[key] = bySource
		}
		st, ok := bySource[cy.Source]
		if !ok {
			st = &alertState{state: StateOK}
			bySource[cy.Source] = st
		}
		// Before any mutation, and before lastSeen: a source that only ever
		// replays must age out of the quorum denominator rather than vote.
		if !st.admits(cy.Time, now) {
			skipped = append(skipped, st.lastCycle.Sub(cy.Time))
			continue
		}
		st.accept(cy.Time, now, aheadLimit)
		st.lastSeen = now

		triggered := cond.Eval(cy)
		prev := st.state

		if triggered {
			st.consecHits++
			switch st.state {
			case StateOK:
				// Check threshold immediately so sustained=1 fires on the first bad cycle.
				if st.consecHits >= alertCfg.Sustained {
					st.state = StateFiring
				} else {
					st.state = StatePending
				}
			case StatePending:
				if st.consecHits >= alertCfg.Sustained {
					st.state = StateFiring
				}
			}
		} else {
			st.consecHits = 0
			st.state = StateOK
		}

		if !alertCfg.Quorum.Enabled() {
			pruneQuietSources(bySource, now, freshness)
			if prev != st.state {
				ev := Event{
					Time:      cy.Time,
					Target:    cy.Target,
					AlertName: name,
					Alert:     alertCfg,
					Prev:      prev,
					Next:      st.state,
					Cycle:     cy,
					// Read-only: unlike the quorum path this must not prune
					// stale sources. A per-source alert dispatches its own
					// resolve from the state kept here, so evicting a source
					// that went quiet while firing would drop that resolve.
					FiringSources: firingSources(bySource, now, window),
				}
				ok, refusal := e.enqueueDispatch(ev)
				if ok {
					toDispatch = append(toDispatch, ev)
				} else {
					refused = append(refused, refusal)
					// Undo the transition rather than keep a state the
					// operator was never told about: dispatch is change-gated
					// with no renotify, so a committed-but-undelivered
					// transition is a page that never happens. Reverted, the
					// next cycle re-detects it and dispatches again.
					st.state = prev
				}
			}
			continue
		}

		firing, live := e.tally(bySource, now, window, freshness)
		next := StateOK
		if live > 0 && firing >= alertCfg.Quorum.Threshold(live) {
			next = StateFiring
		}
		prevAgg, seen := e.agg[key]
		if !seen {
			prevAgg = StateOK
		}

		w, ok := e.warmup[key]
		if !ok {
			w = &aggWarmup{firstSeen: now, sourcesSeen: make(map[string]struct{}, 4)}
			e.warmup[key] = w
		}
		w.sourcesSeen[cy.Source] = struct{}{}

		// Only a *new* FIRING transition is gated — an already-firing
		// aggregate staying firing, and any transition to OK, pass through
		// untouched so a real resolve can never get stuck behind warm-up.
		if next == StateFiring && prevAgg != StateFiring && !w.ready(now, warmupWindow) {
			next = prevAgg
		}

		if prevAgg != next {
			e.agg[key] = next
			ev := Event{
				Time:      cy.Time,
				Target:    cy.Target,
				AlertName: name,
				Alert:     alertCfg,
				Prev:      prevAgg,
				Next:      next,
				Cycle:     cy,
				Firing:    firing,
				Live:      live,
				// tally has already evicted the stale sources, so this sees
				// exactly the set the quorum decision was made on.
				FiringSources: firingSources(bySource, now, window),
			}
			if ok, refusal := e.enqueueDispatch(ev); ok {
				toDispatch = append(toDispatch, ev)
			} else {
				refused = append(refused, refusal)
				e.agg[key] = prevAgg
			}
		}
	}
	return toDispatch, skipped, refused
}

// warnExcluded reports at Warn that a source's cycles are being stored but not
// evaluated for alerting. Silent exclusion is the failure alerting exists to
// prevent, so this is not Debug; a stably-skewed source skips one cycle per
// target per interval, so each (source, reason) is emitted at most once per
// alertFreshness window and the suppressed count carries the rest. The window
// doubles as the eviction rule, so a source that stops being excluded — or
// leaves the fleet — drops out of the map.
func (e *Evaluator) warnExcluded(now time.Time, reason string, cy scheduler.Cycle, detail ...any) {
	window := alertFreshness(e.store.Current().Interval)
	key := exclusionKey{source: cy.Source, reason: reason}

	e.excludedMu.Lock()
	rec := e.excluded[key]
	if !rec.at.IsZero() && now.Sub(rec.at) < window {
		rec.suppressed++
		e.excluded[key] = rec
		e.excludedMu.Unlock()
		return
	}
	suppressed := rec.suppressed
	for k, r := range e.excluded {
		if now.Sub(r.at) >= window {
			delete(e.excluded, k)
		}
	}
	e.excluded[key] = exclusionRecord{at: now}
	e.excludedMu.Unlock()

	e.log.Warn("alert.source_excluded",
		append([]any{
			// example_target, not target: the record is keyed by source, so
			// suppressed counts across every target that source reports on.
			"source", cy.Source, "example_target", cy.Target.ID(), "reason", reason,
			"suppressed", suppressed, "window", window,
		}, detail...)...)
}

// scrubHealthAddresses blanks every address-bearing field of an Event for a
// slave-health target, mirroring what slavehealth.Set.Public() strips.
//
// The Probe/Public split keeps addresses out of the API, but alert dispatch is
// a second egress: Event carries the scheduler's TargetRef — built from
// LocalTargets, which holds real addresses — and ActionDispatcher renders
// operator-supplied templates directly over it, so a webhook or exec template
// referencing {{.Target.Target.Host}} or {{range .Cycle.Hops}} would publish
// the slave's address off-box. Scrubbing here covers every dispatcher, present
// and future, because it happens before the Event leaves the evaluator.
//
// Cycle.Hops is dropped wholesale rather than per-field: the terminal hop is
// the slave itself and intermediate hops disclose its transit path, and an
// alert template has no legitimate use for either. Fail closed.
func scrubHealthAddresses(ev Event) Event {
	if !slavehealth.IsHealthGroup(ev.Target.Group) {
		return ev
	}
	ev.Target.Target.Host = ""
	ev.Target.Target.URL = ""
	ev.Target.Target.Family = ""
	ev.Cycle.Target = ev.Target
	ev.Cycle.Hops = nil
	// HTTPSamples carries no address today and health probes are ICMP so it is
	// never populated, but clearing it makes the scrub exhaustive over
	// scheduler.Cycle's address-capable fields — a future field addition then
	// fails visibly here rather than silently reopening the egress.
	ev.Cycle.HTTPSamples = nil
	return ev
}

// tally counts firing and live sources, pruning any that have gone stale.
// now is the master's receive clock: a slave choosing this value could date a
// cycle forward and age every honest source out of the denominator, becoming a
// majority of itself. Must be called with e.mu held.
//
// Pruning is essential rather than cosmetic: a slave that dies while healthy
// would otherwise sit in the denominator forever, so a real outage seen by
// every remaining source could never reach a majority. But pruning drops only
// participation — the evaluation state a recreated entry would start from
// anyway — never the replay identity (seenCycle/lastCycle/pastCycle/ahead):
// deleting the whole entry recreated it with seenCycle false, which admits
// anything, so the redelivery of a pruned source's cycle was applied a second
// time and could resolve a live alert or refire a sustained one. The identity
// is deleted only once no stamp it holds could pass the freshness gate —
// every stamp is at most lastCycle, so past lastCycle+freshness any replay is
// refused upstream and the entry buys nothing. Retention is bounded by the
// same fact: ingest accepts a stamp at most config.MaxFutureSkew ahead of the
// receive time, so an entry outlives its last cycle by at most
// freshness+MaxFutureSkew.
func (e *Evaluator) tally(bySource map[string]*alertState, now time.Time, window, freshness time.Duration) (firing, live int) {
	for src, st := range bySource {
		if now.Sub(st.lastSeen) > window {
			if now.Sub(st.lastCycle) > freshness {
				delete(bySource, src)
			} else {
				st.state = StateOK
				st.consecHits = 0
			}
			continue
		}
		live++
		if st.state == StateFiring {
			firing++
		}
	}
	return firing, live
}

// pruneQuietSources drops per-source state for sources that can no longer
// produce an evaluable cycle. tally is the only other reaper and it runs on
// the quorum path alone, so a non-quorum alert accumulated an alertState — its
// up-to-8 KiB ahead slice included — for every source name that ever pushed to
// it, and firingSources then scanned all of them under e.mu on every
// transition. The rule is tally's: past lastCycle+freshness every stamp the
// entry holds is refused by the freshness gate upstream, so the replay
// identity buys nothing.
//
// A source in StateFiring is exempt whatever its age. Unlike quorum, a
// per-source alert dispatches its resolve from exactly this state when the
// source comes back, and a recreated entry starts at StateOK — so evicting one
// makes the recovery a non-transition and the operator's page never closes.
// The leak that leaves is bounded by sources that died while firing, which is
// the set an operator is already looking at.
func pruneQuietSources(bySource map[string]*alertState, now time.Time, freshness time.Duration) {
	for src, st := range bySource {
		if st.state != StateFiring && now.Sub(st.lastCycle) > freshness {
			delete(bySource, src)
		}
	}
}

// firingSources names the sources currently in StateFiring, sorted so the
// dispatched Event is deterministic rather than map-iteration order. Stale
// sources are skipped but — unlike tally — never deleted, so this is safe to
// call on the non-quorum path where per-source state must survive a gap.
//
// A source with an empty name is a standalone node with no cluster config;
// naming it "" in a webhook payload is noise, so it is omitted. Counting is
// tally's job, which does include it — dropping it from the quorum
// denominator would break single-node deployments.
func firingSources(bySource map[string]*alertState, now time.Time, window time.Duration) []string {
	var out []string
	for src, st := range bySource {
		if src == "" || st.state != StateFiring || now.Sub(st.lastSeen) > window {
			continue
		}
		out = append(out, src)
	}
	slices.Sort(out)
	return out
}

// alertFreshness is how old a cycle may be, measured on the master's receive
// clock, and still be evaluated for alerting. Alerting is a statement about
// now: ingest accepts a cycle up to config.MaxCycleAge (7d) old, and
// evaluating one replays a historical transition as if it were current, which
// a slave can do at will. It is never tighter than the warm-up window — a
// slow-interval deployment must still be able to keep a source live — nor
// than the skew ingest already tolerates, since master and slave clocks are
// compared here and an honest slave at the accepted limit must not be
// silently excluded.
func alertFreshness(interval time.Duration) time.Duration {
	return max(stalenessWindow(interval), config.MaxFutureSkew)
}

// livenessWindow is how long a source's last cycle keeps it counted in tally
// and firingSources, measured from when the master received it. It is the
// freshness window rather than the bare 3×interval because cycles arrive in
// pushed batches on the slave's own cluster.push_every cadence, which config
// does not bound: any cadence that keeps a source's cycles young enough to
// evaluate at all must also keep the source live, or every bursty-but-healthy
// slave is pruned between pushes and a "majority" quorum collapses to
// whichever source delivers continuously. A cadence past this window is
// already losing cycles to the freshness gate, and warnExcluded reports it.
func livenessWindow(interval time.Duration) time.Duration {
	return alertFreshness(interval)
}

// aheadCeiling is the hard ceiling on that count whatever the interval, since
// config bounds no interval from below and the derivation alone reaches ~6e11
// entries at a 1ns schedule. An entry exists to recognise a redelivery, the
// redelivery unit is one batch, and master.cycleDedup already holds
// cluster.MaxCyclesPerBatch identities per source across every target that
// source reports — so past this depth the window upstream of this one has
// rolled too and a longer slice here catches nothing it would not have caught
// first. 1024 int64 is 8 KiB per (target, source), reached only by a source
// whose clock runs ahead.
const aheadCeiling = int64(cluster.MaxCyclesPerBatch)

// aheadCap bounds how many timestamps one source's state remembers as having
// arrived ahead of the master's clock. An entry is consulted only while a
// cycle carrying it could still be evaluated at all, so it lives for the skew
// ingest accepts plus one freshness window, over which an honest producer
// emits one cycle per interval per target — capped at aheadCeiling, which is
// what keeps the derivation from turning an interval nothing bounds into an
// allocation nothing bounds.
func aheadCap(interval time.Duration) int {
	if interval <= 0 {
		interval = time.Minute
	}
	derived := int64((config.MaxFutureSkew+alertFreshness(interval))/interval) + 1
	return int(min(derived, aheadCeiling))
}

// stalenessWindow is the quorum warm-up horizon: three intervals tolerates
// one missed cycle plus scheduling jitter. Liveness pruning uses
// livenessWindow, which is never tighter than this.
func stalenessWindow(interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = time.Minute
	}
	return 3 * interval
}
