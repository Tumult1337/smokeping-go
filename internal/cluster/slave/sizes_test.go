package slave

import (
	"strings"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
)

// The deployed reference shape CLAUDE.md states, and the cycle count a
// 30-minute outage produces at it. 122 user targets plus the 5 health peers a
// six-node mesh gives each slave.
const (
	deployedTargets  = 122 + 5
	deployedInterval = 20 * time.Second
	outageWindow     = 30 * time.Minute
	outageCycleCount = deployedTargets * int(outageWindow/deployedInterval)
)

// sizeShape builds a cycle of rows hop rows carrying totalRTTs samples between
// them. walkRounds emits one row per (ttl, distinct responder) and a round
// contributes at most one responder per TTL, so the total hop samples one
// cycle can carry never exceeds rounds*ttls however the rows divide them —
// 300 rows of 10 samples each is not a shape any probe here emits.
func sizeShape(rows, totalRTTs, pings, addrLen int) scheduler.Cycle {
	hops := make([]probe.Hop, rows)
	for i := range hops {
		n := totalRTTs / rows
		if i < totalRTTs%rows {
			n++
		}
		hops[i] = probe.Hop{
			Index: i%config.MaxTraceTTL + 1,
			IP:    strings.Repeat("a", addrLen),
			RTTs:  make([]time.Duration, n),
			Sent:  n,
		}
	}
	return scheduler.Cycle{
		Time:      time.Now(),
		Target:    config.TargetRef{Group: "core-backbone", Target: config.Target{Name: "frankfurt", Probe: "icmp"}},
		ProbeName: "icmp",
		Source:    "frankfurt-1",
		RTTs:      make([]time.Duration, pings),
		Hops:      hops,
	}
}

func cycleBytes(c scheduler.Cycle) int64 {
	return payloadStructBytes + payloadHeapBytes(cluster.FromCycle(c))
}

// Every per-cycle figure quoted in CLAUDE.md, in config.DefaultBufferBytes'
// comment and in PushSink's own doc comment is asserted here, so the prose
// cannot drift from the accounting or from what the probes can actually emit.
// A field added to CyclePayload or HopDTO reddens this and the prose is part
// of the fix.
func TestDocumentedCycleSizesAreTheOnesTheAccountingProduces(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rows, rtts  int
		pings, addr int
		want        int64
	}{
		{"no hops, 20 pings", 0, 0, 20, 0, 557},
		{"icmp trace, 14 rows over 3 rounds", 14, 42, 20, 39, 2671},
	} {
		if got := cycleBytes(sizeShape(tc.rows, tc.rtts, tc.pings, tc.addr)); got != tc.want {
			t.Errorf("%s: %d B, prose says %d", tc.name, got, tc.want)
		}
	}

	// The two producer ceilings, at the widest hop address ingest admits.
	const icmpTraceRows = 3 * config.MaxTraceTTL // probe.defaultTraceRounds
	icmpMax := cycleBytes(sizeShape(icmpTraceRows, icmpTraceRows, 20, cluster.MaxHopAddrLen))
	if icmpMax != 16037 {
		t.Errorf("icmp producer max: %d B, prose says 16037", icmpMax)
	}
	mtrMax := cycleBytes(sizeShape(config.MaxHopRowsPerCycle, config.MaxHopRowsPerCycle, 20, cluster.MaxHopAddrLen))
	if mtrMax != 52157 {
		t.Errorf("mtr producer max: %d B, prose says 52157", mtrMax)
	}

	// A 30-minute outage of the worst icmp shape fits the default budget; the
	// same outage of the worst mtr shape does not, which is what the shed
	// ladder exists for.
	icmpOutage := icmpMax * int64(outageCycleCount)
	if icmpOutage >= config.DefaultBufferBytes {
		t.Errorf("30 min of the worst icmp shape is %d B, past the %d default — the prose claims it fits",
			icmpOutage, config.DefaultBufferBytes)
	}
	if got := float64(icmpOutage) / (1 << 20); got < 174 || got > 175 {
		t.Errorf("30 min of the worst icmp shape is %.1f MB, prose says 174.8", got)
	}
	if got := float64(mtrMax*int64(outageCycleCount)) / (1 << 20); got < 568 || got > 569 {
		t.Errorf("30 min of the worst mtr shape is %.1f MB, prose says 568.5", got)
	}
}

// How much path history the default budget keeps through a 30-minute all-mtr
// outage. The number in CLAUDE.md comes from here.
func TestMtrOutageKeepsTheDocumentedPathHistory(t *testing.T) {
	s := quietSink(t, config.DefaultBufferBytes)
	c := sizeShape(config.MaxHopRowsPerCycle, config.MaxHopRowsPerCycle, 20, cluster.MaxHopAddrLen)
	for range outageCycleCount {
		s.OnCycle(t.Context(), c)
	}
	if got := s.Len(); got != outageCycleCount {
		t.Fatalf("buffered %d of %d cycles — every measurement must survive the window", got, outageCycleCount)
	}
	withHops := 0
	for _, p := range s.Drain(0) {
		if len(p.Hops) > 0 {
			withHops++
		}
	}
	if withHops < 4900 || withHops > 5200 {
		t.Errorf("%d cycles kept hop rows, prose says about 5,000", withHops)
	}
	mins := float64(withHops) / (float64(deployedTargets) / deployedInterval.Seconds()) / 60
	if mins < 12.5 || mins > 14 {
		t.Errorf("path history retained is %.1f minutes, prose says about 13", mins)
	}
}
