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
	Time       time.Time
	Target     config.TargetRef
	AlertName  string
	Alert      config.Alert
	Prev       State
	Next       State
	Cycle      scheduler.Cycle
}

// Dispatcher delivers alert events. Must be safe for concurrent use.
type Dispatcher interface {
	Dispatch(ctx context.Context, e Event)
}

type alertKey struct {
	target string
	alert  string
}

type alertState struct {
	state      State
	consecHits int // consecutive cycles the condition has been true
}

// Evaluator is a scheduler.Sink that evaluates each cycle against the alerts
// configured on the target and emits state transitions to a Dispatcher.
// State is kept in-memory per (target, alert); on restart all alerts start in
// StateOK, which avoids spurious "RESOLVED" events after a crash.
type Evaluator struct {
	log        *slog.Logger
	store      *config.Store
	dispatcher Dispatcher
	conds      map[string]Condition // alert name → parsed condition

	mu     sync.Mutex
	states map[alertKey]*alertState
}

func NewEvaluator(log *slog.Logger, store *config.Store, dispatcher Dispatcher) (*Evaluator, error) {
	e := &Evaluator{
		log:        log,
		store:      store,
		dispatcher: dispatcher,
		conds:      make(map[string]Condition),
		states:     make(map[alertKey]*alertState),
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

	e.mu.Lock()
	defer e.mu.Unlock()

	for _, name := range alerts {
		alertCfg, ok := cfg.Alerts[name]
		if !ok {
			continue
		}
		cond, ok := e.conds[name]
		if !ok {
			continue
		}

		key := alertKey{target: cy.Target.ID(), alert: name}
		st, ok := e.states[key]
		if !ok {
			st = &alertState{state: StateOK}
			e.states[key] = st
		}

		triggered := cond.Eval(cy)
		prev := st.state

		if triggered {
			st.consecHits++
			switch st.state {
			case StateOK:
				st.state = StatePending
			case StatePending:
				if st.consecHits >= alertCfg.Sustained {
					st.state = StateFiring
				}
			}
		} else {
			st.consecHits = 0
			st.state = StateOK
		}

		if prev != st.state {
			e.log.Info("alert state change",
				"target", cy.Target.ID(), "alert", name,
				"prev", prev, "next", st.state, "hits", st.consecHits)
			e.dispatcher.Dispatch(ctx, Event{
				Time:      cy.Time,
				Target:    cy.Target,
				AlertName: name,
				Alert:     alertCfg,
				Prev:      prev,
				Next:      st.state,
				Cycle:     cy,
			})
		}
	}
}
