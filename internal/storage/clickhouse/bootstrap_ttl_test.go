package clickhouse

import (
	"strings"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

// The TTL is evaluated against each row's own timestamp, so config's fixed
// ceiling cannot tell whether the sum lands inside DateTime — a retention it
// accepts can still expire past 2106, where Bootstrap would re-emit an
// unrepresentable ALTER on every start.
func TestTTLPastDateTimeCeilingIsRefused(t *testing.T) {
	orig := nowFn
	nowFn = func() time.Time { return time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowFn = orig })

	if err := ttlWithinDateTime(365); err != nil {
		t.Fatalf("the default cycle retention must be accepted: %v", err)
	}
	if err := ttlWithinDateTime(config.MaxRetentionDays); err == nil {
		t.Fatal("config's ceiling expires past DateTime and must be refused here")
	} else if !strings.Contains(err.Error(), "2106") {
		t.Fatalf("error should name the ceiling it exceeded, got %v", err)
	}
	// The boundary itself: the last representable day is accepted.
	days := int(maxDateTime.Sub(nowFn().UTC()).Hours() / 24)
	if err := ttlWithinDateTime(days); err != nil {
		t.Fatalf("the last representable retention must be accepted: %v", err)
	}
}
