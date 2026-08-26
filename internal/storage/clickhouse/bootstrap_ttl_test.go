package clickhouse

import (
	"os"
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
		t.Fatal("config's fixed ceiling expires past DateTime and must be refused here")
	} else if !strings.Contains(err.Error(), "2106") {
		t.Fatalf("error should name the ceiling it exceeded, got %v", err)
	}
	// The boundary itself: the last representable day is accepted.
	days := int(maxDateTime.Sub(nowFn().UTC()).Hours() / 24)
	if err := ttlWithinDateTime(days); err != nil {
		t.Fatalf("the last representable retention must be accepted: %v", err)
	}
}

// The two layers must refuse the same set. Bootstrap runs only at startup, so
// a retention Validate accepts and this refuses is a master that reloads green
// and then will not boot — visible to nobody until a redeploy.
func TestBootstrapAndValidateAgreeOnEveryRetention(t *testing.T) {
	at := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	orig := nowFn
	nowFn = func() time.Time { return at }
	t.Cleanup(func() { nowFn = orig })

	last := int(config.MaxDateTime.Sub(at).Hours() / 24)
	for _, days := range []int{1, 365, last - 1, last, last + 1, last + 1000, config.MaxRetentionDays} {
		bootstrapOK := ttlWithinDateTime(days) == nil
		validateOK := config.RetentionWithinDateTime(days, at) == nil
		if bootstrapOK != validateOK {
			t.Errorf("%d days: bootstrap accepts=%v, config accepts=%v — the layers disagree",
				days, bootstrapOK, validateOK)
		}
	}
}

// A fresh install must not create its tables before the retention is checked:
// PerTableDDL embeds the same value in CREATE TABLE, so an unrepresentable one
// was written into the table definition and only then refused, leaving nothing
// that could repair it on a later start.
func TestBootstrapChecksRetentionBeforeAnyDDL(t *testing.T) {
	raw, err := os.ReadFile("bootstrap.go")
	if err != nil {
		t.Fatalf("read bootstrap.go: %v", err)
	}
	src := string(raw)
	guard := strings.Index(src, "if err := ttlWithinDateTime(t.days); err != nil {")
	ddl := strings.Index(src, "for _, ddl := range PerTableDDL(")
	if guard < 0 || ddl < 0 {
		t.Fatalf("bootstrap.go no longer has both the guard (%d) and the CREATE loop (%d)", guard, ddl)
	}
	if guard > ddl {
		t.Fatal("the retention guard runs after PerTableDDL — a fresh install creates its tables with an unrepresentable TTL, then aborts")
	}
}
