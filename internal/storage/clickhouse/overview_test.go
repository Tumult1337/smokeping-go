package clickhouse

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

// A target fully down for one overview slot has weight 0 in every row of that
// bucket, and quantilesExactWeighted then yields 0 — which a plain groupArray
// carried into the sparkline as a non-nil 0.0 the row-min normalization drew
// at the bottom, i.e. as the row's best latency. Both spark arrays must
// exclude such buckets, under the same guard, so the slot stays nil and
// renders as a gap.
func TestOverviewSparklineExcludesFullyLostSlots(t *testing.T) {
	conn := &recordConn{}
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if _, err := (&Reader{conn: conn}).QueryOverview(context.Background(), from, from.Add(time.Hour),
		[]config.TargetRef{{Group: "core", Target: config.Target{Name: "gw"}}}); err != nil {
		t.Fatal(err)
	}
	sparkIf := regexp.MustCompile(`groupArrayIf\([^)]*\)?[^,]*, b_recv_total > 0\)\s+AS spark_(idx|val)`)
	if got := len(sparkIf.FindAllString(conn.query, -1)); got != 2 {
		t.Fatalf("found %d guarded spark aggregates, want both spark_idx and spark_val excluding zero-received buckets:\n%s", got, conn.query)
	}
	if regexp.MustCompile(`groupArray\(`).MatchString(conn.query) {
		t.Fatalf("an unguarded groupArray still feeds the sparkline:\n%s", conn.query)
	}
}
