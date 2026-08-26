package master

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/scheduler"
)

type deadlineSink struct {
	deadlines []time.Time
}

func (s *deadlineSink) OnCycle(ctx context.Context, _ scheduler.Cycle) {
	d, _ := ctx.Deadline()
	s.deadlines = append(s.deadlines, d)
	time.Sleep(5 * time.Millisecond)
}

// One budget over a whole batch let a few stalled deliveries spend it and hand
// every cycle behind them an already-expired context. Each cycle gets its own
// deadline, taken when its delivery starts, nested inside the batch's — which
// is what keeps their sum bounded, since MaxCyclesPerBatch per-cycle budgets
// alone is ~8.5 hours of handler.
func TestIngestGivesEachCycleItsOwnSinkBudget(t *testing.T) {
	store := config.NewStore("", &config.Config{
		Cluster: &config.Cluster{Token: "tok"},
		Targets: []config.Group{{Group: "g", Targets: []config.Target{{Name: "t", Probe: "icmp"}}}},
	})
	sink := &deadlineSink{}
	srv := NewServer(slog.New(slog.DiscardHandler), store, NewRegistry(slog.New(slog.DiscardHandler)), sink, nil)
	srv.registry.Touch("frankfurt-1", "", "", "")

	now := time.Now().UTC()
	batch := cluster.CycleBatch{Source: "frankfurt-1", Cycles: []cluster.CyclePayload{
		{Group: "g", Name: "t", Time: now, Sent: 5},
		{Group: "g", Name: "t", Time: now.Add(time.Second), Sent: 5},
	}}
	if n, _ := srv.ingestBatch(nil, batch); n != 2 {
		t.Fatalf("ingested %d cycles, want 2", n)
	}
	if len(sink.deadlines) != 2 {
		t.Fatalf("sink saw %d cycles, want 2", len(sink.deadlines))
	}
	for i, d := range sink.deadlines {
		if d.IsZero() {
			t.Fatalf("cycle %d delivered with no deadline", i)
		}
	}
	if !sink.deadlines[1].After(sink.deadlines[0]) {
		t.Fatal("both cycles share one batch deadline; a stalled delivery starves every cycle behind it")
	}
}
