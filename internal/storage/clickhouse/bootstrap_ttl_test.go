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
	// The real clock, pinned for this package only: config.Validate reads its
	// own unexported clock, so the two layers can only be compared at the same
	// instant. Values within a day of the ceiling are left to the two
	// single-layer boundary tests, which each pin the clock they check.
	at := time.Now()
	orig := nowFn
	nowFn = func() time.Time { return at }
	t.Cleanup(func() { nowFn = orig })

	last := int(config.MaxDateTime.Sub(at.UTC()).Hours() / 24)
	// Drives Config.Validate, not RetentionWithinDateTime: comparing the
	// backstop against the function it delegates to is a tautology, and
	// deleting the call from Validate leaves it green.
	for _, days := range []int{-1, 0, 1, 365, last - 2, last + 1000, config.MaxRetentionDays} {
		cfg := validatableConfig()
		cfg.Storage.ClickHouse.Retention.CycleDays = days
		validateOK := cfg.Validate() == nil
		bootstrapOK := ttlWithinDateTime(days) == nil
		// days == 0 is the one legal disagreement: Validate reads it as
		// "use the default" and rewrites it before the check.
		if days == 0 {
			if !validateOK {
				t.Errorf("0 days must default rather than fail: %v", cfg.Validate())
			}
			continue
		}
		if bootstrapOK != validateOK {
			t.Errorf("%d days: bootstrap accepts=%v, config accepts=%v — a config that validates green must be one the process can boot with",
				days, bootstrapOK, validateOK)
		}
	}
}

func validatableConfig() *config.Config {
	return &config.Config{
		Listen:   ":8080",
		Interval: time.Minute,
		Pings:    5,
		Storage:  config.Storage{ClickHouse: config.ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]config.Probe{"icmp": {Type: "icmp", Timeout: 2 * time.Second}},
		Targets: []config.Group{{Group: "core", Targets: []config.Target{
			{Name: "gw", Host: "1.1.1.1", Probe: "icmp"},
		}}},
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
