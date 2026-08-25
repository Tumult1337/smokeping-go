package cluster_test

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/probe"
	"github.com/tumult/gosmokeping/internal/scheduler"
	"github.com/tumult/gosmokeping/internal/stats"
)

// TestCycleRoundTrip guards the wire protocol: FromCycle → JSON → ToCycle must
// preserve every populated field on scheduler.Cycle. Adding a new field to
// scheduler.Cycle (or probe.Hop / probe.HTTPSample / stats.Summary) without
// mirroring it in the DTOs will regress this test as soon as the author
// populates it here.
func TestCycleRoundTrip(t *testing.T) {
	target := config.Target{
		Name:  "web1",
		Host:  "1.2.3.4",
		URL:   "https://example.com",
		Probe: "icmp",
	}

	summary := stats.Summary{
		Min:    1 * time.Microsecond,
		Max:    2 * time.Microsecond,
		Mean:   3 * time.Microsecond,
		Median: 4 * time.Microsecond,
		StdDev: 5 * time.Microsecond,
	}
	for i, spec := range stats.PercentileSet {
		spec.Set(&summary, time.Duration(100+i)*time.Microsecond)
	}

	original := scheduler.Cycle{
		Time:      time.Unix(1700000000, 123456789).UTC(),
		Target:    config.TargetRef{Group: "prod", Target: target},
		ProbeName: "icmp",
		Source:    "slave-a",
		RTTs:      []time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond},
		Sent:      5,
		LossCount: 2,
		Summary:   summary,
		Hops: []probe.Hop{
			{
				Index:   1,
				IP:      "10.0.0.1",
				Unreach: "admin-prohibited",
				RTTs:    []time.Duration{500 * time.Microsecond, 600 * time.Microsecond},
				Sent:    3,
				Lost:    1,
			},
			{
				Index:       2,
				IP:          "10.0.0.2",
				TargetReply: true,
				RTTs:        []time.Duration{900 * time.Microsecond},
				Sent:        3,
				Lost:        2,
			},
		},
		HTTPSamples: []probe.HTTPSample{
			{
				Time:   time.Unix(1700000001, 0).UTC(),
				RTT:    45 * time.Millisecond,
				Status: 200,
				Err:    "",
			},
			{
				Time:   time.Unix(1700000002, 0).UTC(),
				RTT:    0,
				Status: 0,
				Err:    "dial tcp: i/o timeout",
			},
		},
	}

	payload := cluster.FromCycle(original)
	buf, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded cluster.CyclePayload
	if err := json.Unmarshal(buf, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got := decoded.ToCycle(target)

	if !reflect.DeepEqual(got, original) {
		t.Errorf("round-trip mismatch:\n got:  %#v\n want: %#v", got, original)
	}

	// Sanity: every percentile landed where expected so the reflect.DeepEqual
	// above is actually exercising them, not silently accepting zero==zero.
	for i, spec := range stats.PercentileSet {
		want := time.Duration(100+i) * time.Microsecond
		if gotVal := spec.Get(got.Summary); gotVal != want {
			t.Errorf("percentile %s: got %v, want %v", spec.Name, gotVal, want)
		}
	}
}

// TestCycleRoundTripEmptySlices checks the nil-hops / nil-http-samples path
// used by non-MTR, non-HTTP cycles. JSON omitempty + absent-key decoding
// round-trips nil → nil, which matters because callers check len() after.
func TestCycleRoundTripEmptySlices(t *testing.T) {
	target := config.Target{Name: "a", Host: "1.1.1.1", Probe: "icmp"}
	original := scheduler.Cycle{
		Time:      time.Unix(1700000000, 0).UTC(),
		Target:    config.TargetRef{Group: "g", Target: target},
		ProbeName: "icmp",
		Source:    "master",
		RTTs:      []time.Duration{time.Millisecond},
		Sent:      1,
	}

	payload := cluster.FromCycle(original)
	buf, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded cluster.CyclePayload
	if err := json.Unmarshal(buf, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := decoded.ToCycle(target)

	if len(got.Hops) != 0 {
		t.Errorf("hops should round-trip empty, got %d", len(got.Hops))
	}
	if len(got.HTTPSamples) != 0 {
		t.Errorf("http samples should round-trip empty, got %d", len(got.HTTPSamples))
	}
}

// A slave is authenticated but not trusted: whatever it puts in unreach lands
// in a LowCardinality dictionary and in browsers, so ToCycle folds anything
// outside the closed set to the fixed fallback. Empty stays empty — inventing
// an annotation would mark every hop unreachable. TargetReply crosses
// verbatim: extra marks only widen master-side redaction, which fails closed.
func TestToCycleNormalizesUnreach(t *testing.T) {
	target := config.Target{Name: "gw"}
	mk := func(unreach string) cluster.CyclePayload {
		return cluster.CyclePayload{Hops: []cluster.HopDTO{{Index: 2, IP: "10.0.0.2", Unreach: unreach}}}
	}
	if got := mk("admin-prohibited").ToCycle(target).Hops[0].Unreach; got != "admin-prohibited" {
		t.Fatalf("valid label rewritten: %q", got)
	}
	if got := mk("").ToCycle(target).Hops[0].Unreach; got != "" {
		t.Fatalf("empty label invented: %q", got)
	}
	if got := mk("<img src=x onerror=alert(1)>").ToCycle(target).Hops[0].Unreach; got != "unreachable-other" {
		t.Fatalf("hostile label survived ingest: %q", got)
	}
}

// FromCycle must carry both annotations at all — without this, the
// normalization test passes against a wire that silently drops the fields.
func TestCycleRoundTripCarriesHopAnnotations(t *testing.T) {
	c := scheduler.Cycle{Hops: []probe.Hop{{Index: 2, IP: "10.0.0.2", Unreach: "no-route", TargetReply: true}}}
	p := cluster.FromCycle(c)
	buf, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back cluster.CyclePayload
	if err := json.Unmarshal(buf, &back); err != nil {
		t.Fatal(err)
	}
	got := back.ToCycle(config.Target{Name: "gw"}).Hops[0]
	if got.Unreach != "no-route" || !got.TargetReply {
		t.Fatalf("annotations lost over the wire: %+v", got)
	}
}

// A cycle timestamp is slave-supplied and nothing bounded it. Far in the
// future it pins that source at the top of QueryLatestHops for as long as the
// lie lasts and evades probe_hop's TTL, which derives from the row timestamp —
// clearing it needs a manual ClickHouse delete. Far in the past it writes rows
// already past retention.
func TestCycleBatchRejectsOutOfRangeTimestamps(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"a decade ahead":      now.AddDate(10, 0, 0),
		"just past the skew":  now.Add(config.MaxFutureSkew + time.Second),
		"just past max age":   now.Add(-config.MaxCycleAge - time.Second),
		"the unix zero value": {},
	}
	for name, ts := range cases {
		batch := cluster.CycleBatch{Source: "edge-1", Cycles: []cluster.CyclePayload{{Time: ts}}}
		if err := batch.Validate(now); err == nil {
			t.Errorf("%s (%s): accepted, want rejected", name, ts)
		}
	}

	ok := map[string]time.Time{
		"now":                 now,
		"within the skew":     now.Add(config.MaxFutureSkew - time.Second),
		"a requeued hour-old": now.Add(-time.Hour),
		"at the age boundary": now.Add(-config.MaxCycleAge + time.Second),
	}
	for name, ts := range ok {
		batch := cluster.CycleBatch{Source: "edge-1", Cycles: []cluster.CyclePayload{{Time: ts}}}
		if err := batch.Validate(now); err != nil {
			t.Errorf("%s (%s): rejected (%v), want accepted", name, ts, err)
		}
	}
}

func TestCycleBatchRejectsOversizedShapes(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	base := cluster.CyclePayload{Time: now}

	cases := map[string]cluster.CycleBatch{
		"too many cycles": {Cycles: func() []cluster.CyclePayload {
			// Timestamps must be in range or the window check masks the count.
			cs := make([]cluster.CyclePayload, cluster.MaxCyclesPerBatch+1)
			for i := range cs {
				cs[i].Time = now
			}
			return cs
		}()},
		"too many hops": {Cycles: []cluster.CyclePayload{
			{Time: now, Hops: make([]cluster.HopDTO, cluster.MaxHopsPerCycle+1)},
		}},
		"too many rtts on a hop": {Cycles: []cluster.CyclePayload{
			{Time: now, Hops: []cluster.HopDTO{{Index: 1, RTTs: make([]time.Duration, cluster.MaxRTTsPerHop+1)}}},
		}},
		"too many http samples": {Cycles: []cluster.CyclePayload{
			{Time: now, HTTPSamples: make([]cluster.HTTPSampleDTO, cluster.MaxHTTPSamplesPerCycle+1)},
		}},
		"negative sent":       {Cycles: []cluster.CyclePayload{{Time: now, Sent: -1}}},
		"negative loss":       {Cycles: []cluster.CyclePayload{{Time: now, LossCount: -1}}},
		"counter over uint16": {Cycles: []cluster.CyclePayload{{Time: now, Sent: 1 << 16}}},
		"hop index over uint8": {Cycles: []cluster.CyclePayload{
			{Time: now, Hops: []cluster.HopDTO{{Index: 256}}},
		}},
		"negative hop index": {Cycles: []cluster.CyclePayload{
			{Time: now, Hops: []cluster.HopDTO{{Index: -1}}},
		}},
		"negative hop lost": {Cycles: []cluster.CyclePayload{
			{Time: now, Hops: []cluster.HopDTO{{Index: 1, Lost: -1}}},
		}},
	}
	for name, batch := range cases {
		batch.Source = "edge-1"
		if err := batch.Validate(now); err == nil {
			t.Errorf("%s: accepted, want rejected", name)
		}
	}

	// The shape a 122-target / 6-source / 20s install actually pushes: the
	// slave drains 100 cycles per tick, an mtr walk is 30 ttls over 10 rounds.
	real := cluster.CycleBatch{Source: "edge-1"}
	for range 100 {
		c := base
		c.Sent, c.LossCount = 20, 1
		c.RTTs = make([]time.Duration, 20)
		for ttl := 1; ttl <= 30; ttl++ {
			c.Hops = append(c.Hops, cluster.HopDTO{Index: ttl, Sent: 10, Lost: 0,
				RTTs: make([]time.Duration, 10)})
		}
		real.Cycles = append(real.Cycles, c)
	}
	if err := real.Validate(now); err != nil {
		t.Fatalf("a real 122-target/6-source/20s batch was rejected: %v", err)
	}
}

// The producer's own ceiling must validate. walkRounds emits one row per
// (ttl, distinct responder) and each round contributes at most one responder
// per ttl, so an MTR cycle whose path diverges every round legitimately emits
// MaxTraceRounds × MaxTraceTTL rows — 300, above the 256 the ingest bound was
// hand-picked at. A deep ECMP fan-out is not a protocol violation.
func TestCycleBatchAcceptsTheProducersWorstCaseHopCount(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	c := cluster.CyclePayload{Time: now, Sent: 10, LossCount: 0}
	for ttl := 1; ttl <= config.MaxTraceTTL; ttl++ {
		for round := range config.MaxTraceRounds {
			c.Hops = append(c.Hops, cluster.HopDTO{
				Index: ttl,
				IP:    netip.AddrFrom4([4]byte{10, byte(ttl), byte(round), 1}).String(),
				Sent:  1,
				RTTs:  []time.Duration{time.Millisecond},
			})
		}
	}
	if len(c.Hops) != config.MaxHopRowsPerCycle {
		t.Fatalf("fixture built %d hops, want the derived producer ceiling %d", len(c.Hops), config.MaxHopRowsPerCycle)
	}
	batch := cluster.CycleBatch{Source: "edge-1", Cycles: []cluster.CyclePayload{c}}
	if err := batch.Validate(now); err != nil {
		t.Fatalf("the producer's own worst case was refused: %v", err)
	}
}

// The bound stays a bound: it is derived from the producer ceiling with
// headroom, not equal to it, and one row past it is still refused.
func TestHopBoundIsDerivedWithHeadroom(t *testing.T) {
	if cluster.MaxHopsPerCycle <= config.MaxHopRowsPerCycle {
		t.Fatalf("MaxHopsPerCycle %d leaves no headroom over the producer ceiling %d",
			cluster.MaxHopsPerCycle, config.MaxHopRowsPerCycle)
	}
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	batch := cluster.CycleBatch{Source: "edge-1", Cycles: []cluster.CyclePayload{
		{Time: now, Hops: make([]cluster.HopDTO, cluster.MaxHopsPerCycle+1)},
	}}
	if err := batch.Validate(now); err == nil {
		t.Fatal("one row past the bound was accepted")
	}
}

// Validate bounded collection *lengths* but not the values inside them. A
// registered slave could put an arbitrary string in every hop's ip — text that
// is not an address at all, and that lands in probe_hop.hop_addr, a
// LowCardinality dictionary an unauthenticated /hops then serves back. Same
// for the http error string, the identifiers, and every timestamp below the
// cycle's own.
func TestCycleBatchRejectsHostileLeafValues(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	long := strings.Repeat("x", config.MaxLabelLen+1)

	cases := map[string]cluster.CyclePayload{
		"group over the label bound":  {Time: now, Group: long},
		"name over the label bound":   {Time: now, Name: long},
		"probe over the label bound":  {Time: now, ProbeName: long},
		"source over the label bound": {Time: now, Source: long},

		"loss exceeds sent":     {Time: now, Sent: 5, LossCount: 6},
		"hop lost exceeds sent": {Time: now, Hops: []cluster.HopDTO{{Index: 1, Sent: 2, Lost: 3}}},

		"hop ip is not an address":  {Time: now, Hops: []cluster.HopDTO{{Index: 1, IP: "not-an-ip"}}},
		"hop ip is a hostname":      {Time: now, Hops: []cluster.HopDTO{{Index: 1, IP: "gw.example.com"}}},
		"hop ip carries a port":     {Time: now, Hops: []cluster.HopDTO{{Index: 1, IP: "10.0.0.1:53"}}},
		"hop ip is a padded string": {Time: now, Hops: []cluster.HopDTO{{Index: 1, IP: strings.Repeat("A", 4096)}}},

		"negative cycle rtt": {Time: now, RTTs: []time.Duration{-time.Millisecond}},
		"absurd cycle rtt":   {Time: now, RTTs: []time.Duration{config.MaxSampleRTT + 1}},
		"negative hop rtt": {Time: now, Hops: []cluster.HopDTO{
			{Index: 1, IP: "10.0.0.1", Sent: 1, RTTs: []time.Duration{-1}},
		}},
		"absurd summary max": {Time: now, Summary: stats.Summary{Max: config.MaxSampleRTT + 1}},
		"negative summary median": {Time: now, Summary: stats.Summary{
			Median: -time.Second,
		}},
		"absurd summary percentile": {Time: now, Summary: func() stats.Summary {
			var s stats.Summary
			stats.PercentileSet[len(stats.PercentileSet)-1].Set(&s, config.MaxSampleRTT+1)
			return s
		}()},

		"http sample dated years ahead": {Time: now, HTTPSamples: []cluster.HTTPSampleDTO{
			{Time: now.AddDate(3, 0, 0), Status: 200},
		}},
		"http sample dated past max age": {Time: now, HTTPSamples: []cluster.HTTPSampleDTO{
			{Time: now.Add(-config.MaxCycleAge - time.Second), Status: 200},
		}},
		"http status wraps uint16": {Time: now, HTTPSamples: []cluster.HTTPSampleDTO{
			{Time: now, Status: 1 << 16},
		}},
		"http status negative": {Time: now, HTTPSamples: []cluster.HTTPSampleDTO{
			{Time: now, Status: -1},
		}},
		"http rtt negative": {Time: now, HTTPSamples: []cluster.HTTPSampleDTO{
			{Time: now, Status: 200, RTT: -time.Second},
		}},
	}
	for name, c := range cases {
		batch := cluster.CycleBatch{Source: "edge-1", Cycles: []cluster.CyclePayload{c}}
		if err := batch.Validate(now); err == nil {
			t.Errorf("%s: accepted, want rejected", name)
		}
	}
}

// The bounds must not refuse what a probe legitimately produces: an empty hop
// ip means "nothing answered at this TTL", an ipv6 hop address with a zone is
// a link-local responder, and an http sample stamped a moment before its cycle
// is ordinary.
func TestCycleBatchAcceptsLegitimateLeafValues(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	c := cluster.CyclePayload{
		Time:      now,
		Group:     "core-backbone",
		Name:      "frankfurt",
		ProbeName: "icmp",
		Source:    "edge-1",
		Sent:      20,
		LossCount: 20,
		RTTs:      []time.Duration{12 * time.Millisecond, 0},
		Summary:   stats.Summary{Min: time.Millisecond, Max: 40 * time.Millisecond},
		Hops: []cluster.HopDTO{
			{Index: 1, IP: "", Sent: 3, Lost: 3},
			{Index: 2, IP: "10.0.0.1", Sent: 3, Lost: 0, RTTs: []time.Duration{time.Millisecond}},
			{Index: 3, IP: "2001:db8::1", Sent: 3, Lost: 1},
			{Index: 4, IP: "fe80::1%eth0", Sent: 3, Lost: 0},
			{Index: 5, IP: "::ffff:10.0.0.9", Sent: 3, Lost: 0, TargetReply: true},
		},
		HTTPSamples: []cluster.HTTPSampleDTO{
			{Time: now.Add(-time.Second), RTT: 250 * time.Millisecond, Status: 200},
			{Time: now, Status: 0, Err: `Get "https://example.test": dial tcp 10.0.0.1:443: connect: connection refused`},
		},
	}
	batch := cluster.CycleBatch{Source: "edge-1", Cycles: []cluster.CyclePayload{c}}
	if err := batch.Validate(now); err != nil {
		t.Fatalf("a legitimate cycle was refused: %v", err)
	}
}

// hop_addr is a LowCardinality dictionary, so three spellings of one address
// are three permanent entries. ToCycle stores the parsed address's canonical
// form, which is also what the API's redaction compares on.
func TestToCycleCanonicalizesHopAddresses(t *testing.T) {
	target := config.Target{Name: "gw"}
	cases := map[string]string{
		"2001:0DB8::0001": "2001:db8::1",
		"10.0.0.1":        "10.0.0.1",
		"fe80::1%eth0":    "fe80::1%eth0",
		"":                "",
	}
	for in, want := range cases {
		p := cluster.CyclePayload{Hops: []cluster.HopDTO{{Index: 1, IP: in}}}
		if got := p.ToCycle(target).Hops[0].IP; got != want {
			t.Errorf("ToCycle(%q) stored %q, want %q", in, got, want)
		}
	}
	// Fail closed if validation was somehow skipped: unparseable text must
	// never reach the dictionary as itself.
	p := cluster.CyclePayload{Hops: []cluster.HopDTO{{Index: 1, IP: "not-an-ip"}}}
	if got := p.ToCycle(target).Hops[0].IP; got != "" {
		t.Errorf("unvalidated text stored as %q, want the empty no-reply value", got)
	}
}

// netip.ParseAddr treats any non-empty zone as valid, so `fe80::1%<megabytes>`
// parsed, canonicalized and landed in probe_hop.hop_addr — a LowCardinality
// column an unauthenticated /hops serves back. Bounding len(Hops) and requiring
// the text to parse both held; the leaf was still unbounded.
func TestHopAddrZoneIsBounded(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	maxZone := strings.Repeat("z", cluster.MaxHopZoneLen)

	rejected := map[string]string{
		"zone of megabytes":      "fe80::1%" + strings.Repeat("a", 1<<20),
		"zone one past the cap":  "fe80::1%" + maxZone + "z",
		"zone carries a newline": "fe80::1%eth0\nX-Injected: 1",
		"zone carries a NUL":     "fe80::1%eth\x000",
		"zone carries a DEL":     "fe80::1%eth\x7f0",
		"zone carries a slash":   "fe80::1%../../etc",
		"zone carries a percent": "fe80::1%eth0%eth1",
	}
	for name, ip := range rejected {
		batch := cluster.CycleBatch{Source: "edge-1", Cycles: []cluster.CyclePayload{
			{Time: now, Hops: []cluster.HopDTO{{Index: 1, IP: ip}}},
		}}
		if err := batch.Validate(now); err == nil {
			t.Errorf("%s: accepted, want rejected", name)
		}
	}

	// Every shape internal/probe can put in a zone: Go fills one from
	// net.Interface.Name, or the decimal interface index when the name is
	// unknown. 2147483647 is the widest an int32 index gets.
	accepted := []string{
		"fe80::1%eth0", "fe80::1%3", "fe80::1%2147483647",
		"fe80::1%enp0s31f6", "fe80::1%eth0.100", "fe80::1%br-1a2b3c",
		"fe80::1%wg0", "fe80::1%veth1a2b3c4", "fe80::1%tailscale0",
		"fe80::1%" + maxZone,
		"::ffff:10.0.0.9%eth0",
	}
	for _, ip := range accepted {
		batch := cluster.CycleBatch{Source: "edge-1", Cycles: []cluster.CyclePayload{
			{Time: now, Hops: []cluster.HopDTO{{Index: 1, IP: ip}}},
		}}
		if err := batch.Validate(now); err != nil {
			t.Errorf("%q: refused a zone the producer emits: %v", ip, err)
		}
	}
}

// The whole-value bound is checked before netip.ParseAddr, so a 90 MiB string
// is refused without being scanned. It is redundant with the zone bound for
// every value that parses, which is why the rejection reason is asserted: a
// length message proves the order, a zone message proves the guard is gone.
func TestHopAddrLengthIsBoundedBeforeParsing(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	huge := "fe80::1%" + strings.Repeat("a", 1<<20)
	batch := cluster.CycleBatch{Source: "edge-1", Cycles: []cluster.CyclePayload{
		{Time: now, Hops: []cluster.HopDTO{{Index: 1, IP: huge}}},
	}}
	err := batch.Validate(now)
	if err == nil {
		t.Fatal("accepted, want rejected")
	}
	want := fmt.Sprintf("is %d bytes, limit %d", len(huge), cluster.MaxHopAddrLen)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("rejected as %q, want the length bound %q — the value was parsed first", err, want)
	}
}

// The encoded address is bounded whole, not only its zone: the bound must sit
// at the longest textual form netip accepts plus a maximal zone.
func TestHopAddrLengthBoundCoversTheLongestParseableForm(t *testing.T) {
	longest := "2001:0db8:0000:0000:0000:0000:255.255.255.255%" + strings.Repeat("z", cluster.MaxHopZoneLen)
	if len(longest) != cluster.MaxHopAddrLen {
		t.Fatalf("longest parseable form is %d bytes, MaxHopAddrLen is %d", len(longest), cluster.MaxHopAddrLen)
	}
	if _, err := netip.ParseAddr(longest); err != nil {
		t.Fatalf("the form the bound is sized from does not parse: %v", err)
	}
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	batch := cluster.CycleBatch{Source: "edge-1", Cycles: []cluster.CyclePayload{
		{Time: now, Hops: []cluster.HopDTO{{Index: 1, IP: longest}}},
	}}
	if err := batch.Validate(now); err != nil {
		t.Fatalf("the producer's longest encodable address was refused: %v", err)
	}
}

// ToCycle is the fail-closed second reading: text validate would have refused
// must never reach hop_addr, and a zone the producer does emit must survive.
func TestToCycleDropsAnOversizedZone(t *testing.T) {
	target := config.Target{Name: "frankfurt", Probe: "icmp"}
	cy := cluster.CyclePayload{Hops: []cluster.HopDTO{
		{Index: 1, IP: "fe80::1%" + strings.Repeat("a", 1<<20)},
		{Index: 2, IP: "fe80::1%eth0"},
	}}.ToCycle(target)
	if got := cy.Hops[0].IP; got != "" {
		t.Errorf("oversized zone stored as %d bytes, want dropped", len(got))
	}
	if got := cy.Hops[1].IP; got != "fe80::1%eth0" {
		t.Errorf("legitimate zone stored as %q, want fe80::1%%eth0", got)
	}
}

// A free-text field must not be able to reject a batch. Err carries a
// url.Error whose URL config never bounds, and ErrRejected drops the whole
// batch — up to 99 unrelated valid cycles with it. It is truncated at the
// producer and again at ingest, never refused.
func TestOversizedHTTPErrIsTruncatedNotRejected(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	huge := `Get "https://` + strings.Repeat("a", 64<<10) + `": dial tcp: connection refused`
	c := cluster.CyclePayload{
		Time: now, Group: "core", Name: "gw", Sent: 1,
		HTTPSamples: []cluster.HTTPSampleDTO{{Time: now, Status: 0, Err: huge}},
	}
	batch := cluster.CycleBatch{Source: "edge-1", Cycles: []cluster.CyclePayload{c}}
	if err := batch.Validate(now); err != nil {
		t.Fatalf("a long error string rejected the batch: %v", err)
	}
	got := c.ToCycle(config.Target{Name: "gw", Probe: "http"}).HTTPSamples[0].Err
	if len(got) > probe.MaxHTTPErrLen {
		t.Fatalf("stored error is %d bytes, limit %d", len(got), probe.MaxHTTPErrLen)
	}
	if !strings.HasPrefix(got, `Get "https://aaa`) {
		t.Fatalf("truncation dropped the head of the error: %q", got[:min(64, len(got))])
	}
}

// The RTT bound is the storage column's, and the config ceiling sits under it,
// so no schedule Config.Validate accepts can produce a latency ingest refuses.
func TestMaxSampleRTTCoversEveryConfigurableInterval(t *testing.T) {
	if config.MaxProbeInterval > config.MaxSampleRTT {
		t.Fatalf("an interval of %s is configurable but an rtt of %s is not ingestable",
			config.MaxProbeInterval, config.MaxSampleRTT)
	}
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	c := cluster.CyclePayload{
		Time: now, Group: "core", Name: "gw", Sent: 1,
		RTTs:    []time.Duration{config.MaxProbeInterval},
		Summary: stats.Summary{Max: config.MaxProbeInterval},
		Hops: []cluster.HopDTO{
			{Index: 1, IP: "10.0.0.1", Sent: 1, RTTs: []time.Duration{config.MaxProbeInterval}},
		},
		HTTPSamples: []cluster.HTTPSampleDTO{{Time: now, RTT: config.MaxProbeInterval, Status: 200}},
	}
	batch := cluster.CycleBatch{Source: "edge-1", Cycles: []cluster.CyclePayload{c}}
	if err := batch.Validate(now); err != nil {
		t.Fatalf("a cycle at the largest configurable interval was refused: %v", err)
	}
}

// The zone bound multiplies the byte ceiling on an unauthenticated /hops:
// clickhouse.maxHopRows is derived in rows, so hop_addr's width is what turns
// it into a response size. It must sit above what any platform this binary
// ships for can emit, and no further.
func TestHopZoneBoundIsSizedForTheShippedPlatforms(t *testing.T) {
	// IFNAMSIZ is 16 on Linux, macOS and the BSDs, so an interface name is at
	// most 15 bytes; Go's fallback zone is a decimal int32 interface index.
	const ifNameMax = 15
	if longest := len("2147483647"); ifNameMax < longest {
		t.Fatalf("a decimal int32 index is %d bytes, wider than the name ceiling %d", longest, ifNameMax)
	}
	if cluster.MaxHopZoneLen < ifNameMax {
		t.Errorf("MaxHopZoneLen %d is below the producer ceiling %d", cluster.MaxHopZoneLen, ifNameMax)
	}
	// Twice the producer's ceiling is the headroom convention this package
	// already uses for MaxHopsPerCycle. Past that the bound stops being
	// derived and starts inflating the /hops response cap.
	if cluster.MaxHopZoneLen > 2*ifNameMax {
		t.Errorf("MaxHopZoneLen %d exceeds twice the producer ceiling %d", cluster.MaxHopZoneLen, 2*ifNameMax)
	}
}
