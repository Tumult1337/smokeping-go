package cluster_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/stats"
)

// maxGatedPings is the largest ping count config.Validate accepts on a schedule
// that reaches ICMPPingBudget — which every cluster config does, because a
// master must carry a cluster token. Searched against the real gates rather
// than restated, so a change to spacing or the budget floor moves it here.
func maxGatedPings(t *testing.T) int {
	t.Helper()
	lo, hi := 1, config.MaxPingsPerCycle
	for lo < hi {
		mid := (lo + hi + 1) / 2
		_, err := config.ICMPPingBudget(config.MaxProbeInterval, mid)
		if err == nil && config.ValidatePingCount(mid) == nil {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// widestCycle is the largest cycle a probe in this tree can emit at the given
// ping count, with every string and latency at the widest value ingest admits.
// walkRounds gives at most MaxTraceRounds*MaxTraceTTL hop rows sharing at most
// that many samples between them, so the rows carry one sample each.
func widestCycle(pings int) cluster.CyclePayload {
	rtt := config.MaxSampleRTT
	label := strings.Repeat("x", config.MaxLabelLen)
	addr := "2001:db8:85a3:1:8a2e:370:7334:9%" + strings.Repeat("y", cluster.MaxHopZoneLen)

	rtts := make([]time.Duration, pings)
	for i := range rtts {
		rtts[i] = rtt
	}
	hops := make([]cluster.HopDTO, config.MaxHopRowsPerCycle)
	for i := range hops {
		hops[i] = cluster.HopDTO{
			Index: config.MaxTraceTTL, IP: addr, Unreach: "admin-prohibited",
			RTTs: []time.Duration{rtt}, Sent: config.MaxTraceRounds, Lost: config.MaxTraceRounds,
		}
	}
	samples := make([]cluster.HTTPSampleDTO, cluster.MaxHTTPSamplesPerCycle)
	for i := range samples {
		samples[i] = cluster.HTTPSampleDTO{
			Time: time.Now(), RTT: rtt, Status: 999, Err: strings.Repeat("e", probe.MaxHTTPErrLen),
		}
	}
	var sum stats.Summary
	for _, spec := range stats.PercentileSet {
		spec.Set(&sum, rtt)
	}
	sum.Min, sum.Max, sum.Mean, sum.Median, sum.StdDev = rtt, rtt, rtt, rtt, rtt
	return cluster.CyclePayload{
		Time: time.Now(), Group: label, Name: label, ProbeName: label, Source: label,
		RTTs: rtts, Sent: 65535, LossCount: 65535, Summary: sum, Hops: hops, HTTPSamples: samples,
	}
}

func batchBytes(t *testing.T, c cluster.CyclePayload, n int) int {
	t.Helper()
	if err := (cluster.CycleBatch{Source: "s", Cycles: []cluster.CyclePayload{c}}).Validate(time.Now()); err != nil {
		t.Fatalf("the fixture is not a batch ingest accepts, so it bounds nothing: %v", err)
	}
	cycles := make([]cluster.CyclePayload, n)
	for i := range cycles {
		cycles[i] = c
	}
	b, err := json.Marshal(cluster.CycleBatch{Source: strings.Repeat("s", config.MaxLabelLen), Cycles: cycles})
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

// MaxCyclesBody has to admit every batch a slave can legitimately send, or the
// master answers 413 and slave.flushOnce drops the batch — silent data loss on
// a config config.Validate accepted. A slave's schedule always comes from a
// master config and a master config always carries a cluster token, so
// ICMPPingBudget always applies and MaxPingsPerCycle is not reachable there.
func TestCyclesBodyCapAdmitsWhatASlaveCanSend(t *testing.T) {
	pings := maxGatedPings(t)
	if pings >= config.MaxPingsPerCycle {
		t.Fatalf("the budget gate admits %d pings, the whole sequence space — this test's premise is that it binds first", pings)
	}
	sent := batchBytes(t, widestCycle(pings), cluster.PushBatchCycles)
	if int64(sent) > int64(cluster.MaxCyclesBody) {
		t.Fatalf("a slave's widest batch is %d B, past the %d B cap: the master 413s it and the slave drops it",
			sent, cluster.MaxCyclesBody)
	}
	// The floor is above 1 so this reddens while there is still room to act,
	// rather than at the moment a legitimate batch starts being dropped.
	if margin := float64(cluster.MaxCyclesBody) / float64(sent); margin < 1.5 {
		t.Errorf("only %.2fx of margin between a slave's widest batch (%d B) and the cap (%d B); the comment records 1.9x",
			margin, sent, cluster.MaxCyclesBody)
	}
}

// The protocol ceiling is deliberately not admitted, and that is stated rather
// than discovered: MaxCyclesPerBatch cycles at this shape are hundreds of MB,
// so a cap covering them would be the memory bound instead of a guard. If this
// ever stops holding, MaxCyclesBody has become the looser of the two and the
// comment on it is wrong.
func TestProtocolCeilingExceedsTheBodyCapOnPurpose(t *testing.T) {
	full := batchBytes(t, widestCycle(maxGatedPings(t)), cluster.MaxCyclesPerBatch)
	if int64(full) <= int64(cluster.MaxCyclesBody) {
		t.Fatalf("MaxCyclesPerBatch cycles encode to %d B, inside the %d B cap — the cap is no longer the binding bound",
			full, cluster.MaxCyclesBody)
	}
}
