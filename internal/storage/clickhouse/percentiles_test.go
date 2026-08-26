package clickhouse

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/stats"
	"github.com/tumult/gosmokeping/internal/storage"
)

// Nothing iterates stats.PercentileSet to build SQL: the writer INSERT, the
// raw SELECT, the bucketed rollup and the DTO are four hand-named lists, so a
// percentile added to the set used to reach none of them silently. This walks
// the set against all four; TestFlushColumnParity separately pins that the
// INSERT's value count matches its column list.
func TestCyclePercentileColumnsFollowPercentileSet(t *testing.T) {
	ref := config.TargetRef{Group: "core", Target: config.Target{Name: "gw"}}
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)

	rawConn := &recordConn{}
	if _, err := (&Reader{conn: rawConn}).QueryCycles(context.Background(), ref, from, to, storage.QueryFilter{}); err != nil {
		t.Fatal(err)
	}
	bucketConn := &recordConn{}
	if _, err := (&Reader{conn: bucketConn}).QueryCycles(context.Background(), ref, from, to, storage.QueryFilter{Step: time.Hour}); err != nil {
		t.Fatal(err)
	}

	dto := reflect.TypeOf(storage.CyclePoint{})
	for _, spec := range stats.PercentileSet {
		col := spec.Name + "_us"
		if !strings.Contains(insertProbeCycle, col) {
			t.Errorf("writer INSERT does not name %s", col)
		}
		if !strings.Contains(ddlProbeCycle, col) {
			t.Errorf("probe_cycle DDL does not declare %s", col)
		}
		if !strings.Contains(rawConn.query, col+" / 1000.0") {
			t.Errorf("raw cycle SELECT does not read %s", col)
		}
		want := fmt.Sprintf("quantilesExactWeighted(%.2f)(%s,", spec.Ratio, col)
		if !strings.Contains(bucketConn.query, want) {
			t.Errorf("bucketed cycle SELECT does not roll up %s at its own ratio (%s)", col, want)
		}
		field := strings.ToUpper(spec.Name)
		if f, ok := dto.FieldByName(field); !ok || f.Type.Kind() != reflect.Float64 {
			t.Errorf("storage.CyclePoint has no float64 field %s for %s", field, col)
		}
	}

	// The reverse direction: a percentile removed from the set but left in a
	// list is the same silent disagreement.
	pcol := regexp.MustCompile(`p\d+_us`)
	for name, text := range map[string]string{
		"writer INSERT":   insertProbeCycle,
		"probe_cycle DDL": ddlProbeCycle,
		"raw SELECT":      rawConn.query,
		"bucketed SELECT": bucketConn.query,
	} {
		distinct := map[string]struct{}{}
		for _, m := range pcol.FindAllString(text, -1) {
			distinct[m] = struct{}{}
		}
		if got := len(distinct); got != len(stats.PercentileSet) {
			t.Errorf("%s names %d percentile columns, PercentileSet has %d", name, got, len(stats.PercentileSet))
		}
	}
}
