package influxv3

import (
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"

	"github.com/tumult/gosmokeping/internal/storage"
)

func TestQuoteIdent(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"group", `"group"`},
		{"rtt_min", `"rtt_min"`},
		{`weird"name`, `"weird""name"`},
	}
	for _, c := range cases {
		if got := quoteIdent(c.in); got != c.want {
			t.Errorf("quoteIdent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDateBinInterval(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{time.Hour, "1 hour"},
		{24 * time.Hour, "1 day"},
		{15 * time.Minute, "15 minutes"},
		{time.Minute, "1 minute"},
		{30 * time.Second, "30 seconds"},
		{time.Second, "1 second"},
		{2 * 24 * time.Hour, "2 days"},
		{3 * time.Hour, "3 hours"},
	}
	for _, c := range cases {
		if got := dateBinInterval(c.d); got != c.want {
			t.Errorf("dateBinInterval(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestBucketForResolution(t *testing.T) {
	if got := bucketForResolution(storage.ResolutionRaw); got != 0 {
		t.Errorf("raw bucket = %v, want 0", got)
	}
	if got := bucketForResolution(storage.Resolution1h); got != time.Hour {
		t.Errorf("1h bucket = %v, want 1h", got)
	}
	if got := bucketForResolution(storage.Resolution1d); got != 24*time.Hour {
		t.Errorf("1d bucket = %v, want 24h", got)
	}
}

func TestTargetWhereClause(t *testing.T) {
	noSrc := targetWhereClause(false)
	if !strings.Contains(noSrc, `"group" = $group`) {
		t.Errorf("missing quoted group keyword: %q", noSrc)
	}
	if strings.Contains(noSrc, "source") {
		t.Errorf("source clause leaked into no-source query: %q", noSrc)
	}
	withSrc := targetWhereClause(true)
	if !strings.Contains(withSrc, "AND source = $source") {
		t.Errorf("source clause missing: %q", withSrc)
	}
}

func TestBuildCycleColumnsRawIncludesAllPercentiles(t *testing.T) {
	cols := buildCycleColumns(false, "")
	for _, acc := range storage.CyclePointPercentileAccessors {
		if !strings.Contains(cols, `"rtt_`+acc.Name+`"`) {
			t.Errorf("raw column list missing percentile %q: %s", acc.Name, cols)
		}
	}
}

func TestBuildCycleColumnsAggregatedWrapsAVG(t *testing.T) {
	cols := buildCycleColumns(true, "1 hours")
	if !strings.Contains(cols, "date_bin(INTERVAL '1 hours', time) AS time") {
		t.Errorf("aggregate columns missing date_bin: %s", cols)
	}
	if !strings.Contains(cols, `MIN("rtt_min") AS rtt_min`) {
		t.Errorf("rtt_min not wrapped in MIN: %s", cols)
	}
	if !strings.Contains(cols, `MAX("rtt_max") AS rtt_max`) {
		t.Errorf("rtt_max not wrapped in MAX: %s", cols)
	}
	if !strings.Contains(cols, `SUM("loss_count") AS loss_count`) {
		t.Errorf("loss_count not wrapped in SUM: %s", cols)
	}
	if !strings.Contains(cols, `SUM("pings_sent") AS pings_sent`) {
		t.Errorf("pings_sent not wrapped in SUM: %s", cols)
	}
}

func TestTimeOf(t *testing.T) {
	wantNS := int64(1700000000_000000000)
	want := time.Unix(0, wantNS).UTC()

	got := timeOf(want)
	if !got.Equal(want) {
		t.Errorf("time.Time round-trip: got %v, want %v", got, want)
	}

	got2 := timeOf(arrow.Timestamp(wantNS))
	if !got2.Equal(want) {
		t.Errorf("arrow.Timestamp conversion: got %v, want %v", got2, want)
	}

	if got3 := timeOf("not a time"); !got3.IsZero() {
		t.Errorf("unsupported type returned %v, want zero", got3)
	}
}

func TestIntOfHopIndexFromString(t *testing.T) {
	// hop_index is stored as a string tag — intOf must parse it on demand
	// or hops queries return 0 for every index.
	if got := intOf("7"); got != 7 {
		t.Errorf("intOf(\"7\") = %d, want 7", got)
	}
	if got := intOf("not-a-number"); got != 0 {
		t.Errorf("intOf garbage returned %d, want 0", got)
	}
}
