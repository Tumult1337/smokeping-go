package alert

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
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
	// lastSeen is the cycle timestamp, not wall-clock. Cycle time is already
	// a deterministic injected input, which makes staleness pruning testable
	// without a fake clock.
	lastSeen time.Time
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

	mu     sync.Mutex
	states map[aggKey]map[string]*alertState // target+alert → source → per-source state
	agg    map[aggKey]State                  // target+alert → last dispatched aggregate state (quorum alerts only)
}

func NewEvaluator(log *slog.Logger, store *config.Store, dispatcher Dispatcher) (*Evaluator, error) {
	e := &Evaluator{
		log:        log,
		store:      store,
		dispatcher: dispatcher,
		conds:      make(map[string]Condition),
		states:     make(map[aggKey]map[string]*alertState),
		agg:        make(map[aggKey]State),
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
	return e.refreshConditions()
}

func (e *Evaluator) refreshConditions() error {
	cfg := e.store.Current()
	conds := make(map[string]Condition, len(cfg.Alerts))
	for name, a := range cfg.Alerts {
		c, err := ParseCondition(a.Condition)
		if err != nil {
			return fmt.Errorf("alert %q: %w", name, err)
		}
		conds[name] = c
	}
	e.conds = conds
	return nil
}

func (e *Evaluator) OnCycle(ctx context.Context, cy scheduler.Cycle) {
	cfg := e.store.Current()
	alerts := cy.Target.Target.Alerts
	if len(alerts) == 0 {
		return
	}

	var toDispatch []Event

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
		st.lastSeen = cy.Time

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
				})
			}
			continue
		}

		firing, live := e.tally(bySource, cy.Time, quorumWindow(cfg.Interval))
		next := StateOK
		if live > 0 && firing >= alertCfg.Quorum.Threshold(live) {
			next = StateFiring
		}
		prevAgg, seen := e.agg[key]
		if !seen {
			prevAgg = StateOK
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
			})
		}
	}
	e.mu.Unlock()

	// Dispatch outside the lock so a slow webhook doesn't stall evaluation
	// for other targets running concurrently.
	for _, ev := range toDispatch {
		e.log.Info("alert state change",
			"target", ev.Target.ID(), "alert", ev.AlertName, "source", cy.Source,
			"prev", ev.Prev, "next", ev.Next, "hits", ev.Cycle.Sent,
			"firing", ev.Firing, "live", ev.Live)
		e.dispatcher.Dispatch(ctx, ev)
	}
}

// tally counts firing and live sources, evicting any that have gone stale.
// Must be called with e.mu held.
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

// quorumWindow is how long a source's last cycle stays counted. Three
// intervals tolerates one missed cycle plus scheduling jitter, and is wide
// enough that ordinary clock skew between master and slaves — the cycle
// timestamps come from different hosts — cannot evict a live source.
func quorumWindow(interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = time.Minute
	}
	return 3 * interval
}
