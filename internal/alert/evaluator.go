package alert

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
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
	}
	if err := e.refreshConditions(); err != nil {
		return nil, err
	}
	return e, nil
}

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
	return nil
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
func (e *Evaluator) pruneStaleAggregates(oldEnabled map[string]bool) {
	for key := range e.agg {
		was, existed := oldEnabled[key.alert]
		now, stillExists := e.quorumEnabled[key.alert]
		if !existed || !stillExists || was != now {
			delete(e.agg, key)
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
	window := stalenessWindow(cfg.Interval)
	aheadLimit := aheadCap(cfg.Interval)
	// Skipped whole like a cycle that sent nothing, so the source ages out
	// rather than voting on data it replayed out of its own history.
	if age := now.Sub(cy.Time); age > alertFreshness(cfg.Interval) {
		e.warnExcluded(now, reasonClockSkew, cy, "age", age, "limit", alertFreshness(cfg.Interval))
		return
	}

	var toDispatch []Event
	var skipped []time.Duration

	e.mu.Lock()
	for _, name := range alerts {
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
			if prev != st.state {
				toDispatch = append(toDispatch, Event{
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
				})
			}
			continue
		}

		firing, live := e.tally(bySource, now, window)
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
		if next == StateFiring && prevAgg != StateFiring && !w.ready(now, window) {
			next = prevAgg
		}

		if prevAgg != next {
			e.agg[key] = next
			toDispatch = append(toDispatch, Event{
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
			})
		}
	}
	e.mu.Unlock()

	if len(skipped) > 0 {
		e.warnExcluded(now, reasonDuplicate, cy, "alerts", len(skipped), "behind", slices.Max(skipped))
	}

	// Dispatch outside the lock so a slow webhook doesn't stall evaluation
	// for other targets running concurrently.
	for _, ev := range toDispatch {
		e.log.Info("alert state change",
			"target", ev.Target.ID(), "alert", ev.AlertName, "source", cy.Source,
			"prev", ev.Prev, "next", ev.Next, "hits", ev.Cycle.Sent,
			"firing", ev.Firing, "live", ev.Live)
		e.dispatcher.Dispatch(ctx, scrubHealthAddresses(ev))
	}
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

// tally counts firing and live sources, evicting any that have gone stale.
// now is the master's receive clock: a slave choosing this value could date a
// cycle forward and age every honest source out of the denominator, becoming a
// majority of itself. Must be called with e.mu held.
//
// Pruning is essential rather than cosmetic: a slave that dies while healthy
// would otherwise sit in the denominator forever, so a real outage seen by
// every remaining source could never reach a majority.
func (e *Evaluator) tally(bySource map[string]*alertState, now time.Time, window time.Duration) (firing, live int) {
	for src, st := range bySource {
		if now.Sub(st.lastSeen) > window {
			delete(bySource, src)
			continue
		}
		live++
		if st.state == StateFiring {
			firing++
		}
	}
	return firing, live
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
// a slave can do at will. It is never tighter than the liveness window it
// feeds — a slow-interval deployment must still be able to keep a source live
// — nor than the skew ingest already tolerates, since master and slave clocks
// are compared here and an honest slave at the accepted limit must not be
// silently excluded.
func alertFreshness(interval time.Duration) time.Duration {
	return max(stalenessWindow(interval), config.MaxFutureSkew)
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

// stalenessWindow is how long a source's last cycle stays counted, measured
// from when the master received it. Three intervals tolerates one missed cycle
// plus scheduling jitter and one push cadence.
func stalenessWindow(interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = time.Minute
	}
	return 3 * interval
}
