package slave

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
)

// The assembled runner through a master outage, driven end to end rather than
// per part: cycles into the real sink, a real httptest master refusing and
// then accepting, and the real flushOnce/requeue path between them.
//
// The 30-minute window is scaled rather than slept: the outage is expressed as
// a cycle count at the deployed rate, which is what the budget actually has to
// hold. Sleeping the real duration is not a test anyone runs.
func TestSlaveSurvivesAMasterOutageWithItsMeasurements(t *testing.T) {
	// The deployed shape's 30-minute cycle count, scaled down by 20 alongside
	// the budget so the ratio of cycles to bytes is unchanged and the test runs
	// in milliseconds.
	const outageCycles = outageCycleCount / 20
	budget := config.DefaultBufferBytes / 20

	var mu sync.Mutex
	var accepting bool
	var got []cluster.CyclePayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !accepting {
			http.Error(w, "master down", http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var batch cluster.CycleBatch
		if err := json.Unmarshal(body, &batch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := batch.Validate(time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got = append(got, batch.Cycles...)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := NewRunner(slog.New(slog.DiscardHandler), &config.Config{
		Cluster: &config.Cluster{
			MasterURL: srv.URL, Token: "tok", Name: "frankfurt-1", BufferBytes: budget,
		},
	}, "v9")

	ctx := context.Background()
	sent := make([]string, 0, outageCycles)
	for i := range outageCycles {
		// The producer's own worst case: 300 rows over 10 rounds.
		c := mtrCycle(string(rune('a'+i%26)), config.MaxHopRowsPerCycle, config.MaxTraceRounds)
		c.Time = time.Now().Add(-time.Duration(outageCycles-i) * time.Second)
		c.Sent = 20
		c.LossCount = i % 21
		sent = append(sent, c.Target.Target.Name)
		r.sink.OnCycle(ctx, c)

		// The push loop keeps trying throughout the outage and keeps failing.
		if i%50 == 0 {
			if _, err := r.flushOnce(ctx); err != nil {
				t.Fatalf("flushOnce during the outage: %v", err)
			}
		}
		if b := r.sink.Bytes(); b > budget && r.sink.Len() > 1 {
			t.Fatalf("cycle %d: buffer holds %d bytes, past the %d budget", i, b, budget)
		}
	}

	mu.Lock()
	accepting = true
	mu.Unlock()

	// One tick's drain must clear the whole backlog.
	for {
		pushed, err := r.flushOnce(ctx)
		if err != nil {
			t.Fatalf("flushOnce after recovery: %v", err)
		}
		if pushed < r.batchLimit {
			break
		}
	}
	if left := r.sink.Len(); left != 0 {
		t.Fatalf("%d cycles still buffered after the recovery drain", left)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != outageCycles {
		t.Fatalf("master received %d cycles, want every one of the %d probed", len(got), outageCycles)
	}
	// Every measurement survives, in order.
	withHops := 0
	for i, p := range got {
		if p.Name != sent[i] {
			t.Fatalf("cycle %d is %q, want %q — the backlog arrived out of order", i, p.Name, sent[i])
		}
		if p.Sent != 20 || p.LossCount != i%21 {
			t.Fatalf("cycle %d lost its measurement: sent=%d loss=%d", i, p.Sent, p.LossCount)
		}
		if len(p.Hops) > 0 {
			withHops++
		}
	}
	// Path history is best-effort past the budget, and the newest cycles are
	// the ones that keep it.
	if withHops == 0 {
		t.Fatal("no cycle kept its hop rows, so the ladder shed more than it had to")
	}
	if withHops == len(got) {
		t.Fatalf("all %d cycles kept their hops, so the budget was never reached and this proves nothing", len(got))
	}
	for i := len(got) - withHops; i < len(got); i++ {
		if len(got[i].Hops) == 0 {
			t.Fatalf("cycle %d has no hops but %d newer-or-equal cycles do — the surviving path history is not the newest",
				i, withHops)
		}
	}
	t.Logf("outage of %d cycles: every measurement delivered, %d/%d kept hop rows (%d B budget)",
		outageCycles, withHops, len(got), budget)
}
