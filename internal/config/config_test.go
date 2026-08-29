package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalConfig = `{
  "listen": ":8080",
  "interval": "30s",
  "pings": 10,
  "storage": {
    "clickhouse": {
      "addr": "ch:9000"
    }
  },
  "probes": {
    "icmp": { "type": "icmp", "timeout": "2s" }
  },
  "targets": [
    {
      "group": "core",
      "title": "Core",
      "targets": [
        { "name": "gw", "host": "1.1.1.1", "probe": "icmp" }
      ]
    }
  ]
}`

func writeTmp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
}

func setExampleEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLUSTER_TOKEN", "test-cluster-token")
	t.Setenv("CH_PASSWORD", "test-clickhouse-password")
	t.Setenv("DISCORD_WEBHOOK_URL", "https://discord.example/api/webhooks/test")
}

// loadBytes is a test helper that loads a config from raw JSON bytes.
func loadBytes(t *testing.T, b []byte) (*Config, error) {
	t.Helper()
	return Load(writeTmp(t, string(b)))
}

func TestLoadMinimal(t *testing.T) {
	cfg, err := Load(writeTmp(t, minimalConfig))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s", cfg.Interval)
	}
	if cfg.Pings != 10 {
		t.Errorf("pings = %d, want 10", cfg.Pings)
	}
	if got := cfg.Probes["icmp"].Timeout; got != 2*time.Second {
		t.Errorf("icmp timeout = %v, want 2s", got)
	}
	refs := cfg.AllTargets()
	if len(refs) != 1 || refs[0].ID() != "core/gw" {
		t.Errorf("targets = %+v", refs)
	}
}

func TestLoadDefaults(t *testing.T) {
	body := `{
      "storage": {"clickhouse": {"addr": "ch:9000"}},
      "probes": { "icmp": { "type": "icmp" } },
      "targets": [{
        "group": "g", "targets": [{ "name": "t", "host": "h", "probe": "icmp" }]
      }]
    }`
	cfg, err := Load(writeTmp(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("listen default = %q", cfg.Listen)
	}
	if cfg.Interval != 5*time.Minute {
		t.Errorf("interval default = %v", cfg.Interval)
	}
	if cfg.Pings != 20 {
		t.Errorf("pings default = %d", cfg.Pings)
	}
	if cfg.Probes["icmp"].Timeout != 5*time.Second {
		t.Errorf("probe timeout default = %v", cfg.Probes["icmp"].Timeout)
	}
}

func TestLoadEnvExpansion(t *testing.T) {
	t.Setenv("CH_PASSWORD", "secret123")
	body := strings.Replace(minimalConfig, `"addr": "ch:9000"`,
		`"addr": "ch:9000", "password": "${CH_PASSWORD}"`, 1)
	cfg, err := Load(writeTmp(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.ClickHouse.Password != "secret123" {
		t.Errorf("password = %q, want secret123", cfg.Storage.ClickHouse.Password)
	}
}

func TestLoadRejectsUnresolvedCredentialPlaceholders(t *testing.T) {
	const placeholder = "${GOSMOKEPING_TEST_UNRESOLVED_SECRET}"
	unsetEnv(t, "GOSMOKEPING_TEST_UNRESOLVED_SECRET")

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "cluster token",
			body: strings.Replace(minimalConfig, `"storage": {`,
				`"cluster": {"token": "`+placeholder+`"}, "storage": {`, 1),
			want: "config: unresolved ${...} placeholders in credential fields: cluster.token",
		},
		{
			name: "storage password",
			body: strings.Replace(minimalConfig, `"addr": "ch:9000"`,
				`"addr": "ch:9000", "password": "`+placeholder+`"`, 1),
			want: "config: unresolved ${...} placeholders in credential fields: storage.clickhouse.password",
		},
		{
			name: "action URL",
			body: strings.Replace(minimalConfig, `"storage": {`,
				`"actions": {"notify": {"type": "discord", "url": "https://discord.example/api/webhooks/`+placeholder+`"}}, "storage": {`, 1),
			want: "config: unresolved ${...} placeholders in credential fields: actions.notify.url",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTmp(t, tc.body))
			if err == nil {
				t.Fatal("Load() error = nil, want unresolved credential placeholder error")
			}
			if got := err.Error(); got != tc.want {
				t.Fatalf("Load() error = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoadWarnsForUnresolvedNonCredentialPlaceholder(t *testing.T) {
	const placeholder = "${GOSMOKEPING_TEST_UNRESOLVED_TITLE}"
	unsetEnv(t, "GOSMOKEPING_TEST_UNRESOLVED_TITLE")

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return attr
		},
	})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	body := strings.Replace(minimalConfig, `"title": "Core"`, `"title": "`+placeholder+`"`, 1)
	cfg, err := Load(writeTmp(t, body))
	if err != nil {
		t.Fatalf("Load() error = %v, want warning-only behavior", err)
	}
	if got := cfg.Targets[0].Title; got != placeholder {
		t.Fatalf("target title = %q, want %q", got, placeholder)
	}
	want := "level=WARN msg=\"config: unresolved ${...} placeholders — env vars not set\" vars=[GOSMOKEPING_TEST_UNRESOLVED_TITLE] hint=\"set them in .env (next to your config file) or in the shell before starting\"\n"
	if got := logs.String(); got != want {
		t.Fatalf("log output = %q, want %q", got, want)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"unknown probe ref", func(c *Config) {
			g := c.Targets[0]
			g.Targets[0].Probe = "nope"
			c.Targets[0] = g
		}, `probe "nope" not defined`},
		{"missing host+url", func(c *Config) {
			g := c.Targets[0]
			g.Targets[0].Host = ""
			g.Targets[0].URL = ""
			c.Targets[0] = g
		}, "host or url is required"},
		{"duplicate target", func(c *Config) {
			g := c.Targets[0]
			g.Targets = append(g.Targets, g.Targets[0])
			c.Targets[0] = g
		}, "duplicate target"},
		{"unknown alert ref", func(c *Config) {
			g := c.Targets[0]
			g.Targets[0].Alerts = []string{"missing"}
			c.Targets[0] = g
		}, `alert "missing" not defined`},
		{"zero pings", func(c *Config) { c.Pings = 0 }, "pings must be positive"},
		{"zero interval", func(c *Config) { c.Interval = 0 }, "interval must be positive"},
		{"missing clickhouse addr", func(c *Config) { c.Storage.ClickHouse.Addr = "" },
			"storage.clickhouse.addr is required"},
		{"bad clickhouse database", func(c *Config) { c.Storage.ClickHouse.Database = "with-hyphen" },
			"storage.clickhouse.database"},
		{"bad clickhouse database injection", func(c *Config) { c.Storage.ClickHouse.Database = "x; DROP TABLE y" },
			"storage.clickhouse.database"},
		{"bad clickhouse cluster", func(c *Config) { c.Storage.ClickHouse.Cluster = "ch-prod-01" },
			"storage.clickhouse.cluster"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeTmp(t, minimalConfig))
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(cfg)
			err = cfg.Validate()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestStorageClickHouseDefaults(t *testing.T) {
	raw := []byte(`{
		"targets": [{"group":"g","targets":[{"name":"t","host":"1.1.1.1","probe":"icmp"}]}],
		"probes": {"icmp": {"type": "icmp"}},
		"storage": {"clickhouse": {"addr": "ch:9000"}}
	}`)
	cfg, err := loadBytes(t, raw)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ch := cfg.Storage.ClickHouse
	if ch.Database != "gosmokeping" {
		t.Errorf("database default: got %q", ch.Database)
	}
	if ch.Username != "default" {
		t.Errorf("username default: got %q", ch.Username)
	}
	if ch.Retention.CycleDays != 365 {
		t.Errorf("cycle_days default: got %d", ch.Retention.CycleDays)
	}
	if ch.Retention.RTTDays != 14 {
		t.Errorf("rtt_days default: got %d", ch.Retention.RTTDays)
	}
	if ch.Retention.HopDays != 90 {
		t.Errorf("hop_days default: got %d", ch.Retention.HopDays)
	}
	if ch.Retention.HTTPDays != 14 {
		t.Errorf("http_days default: got %d", ch.Retention.HTTPDays)
	}
	if ch.Batch.MaxRows != 1000 {
		t.Errorf("batch.max_rows default: got %d", ch.Batch.MaxRows)
	}
	if ch.Batch.MaxInterval != "1s" {
		t.Errorf("batch.max_interval default: got %q", ch.Batch.MaxInterval)
	}
}

func TestStorageClickHouseAddrRequired(t *testing.T) {
	raw := []byte(`{
		"targets": [{"group":"g","targets":[{"name":"t","host":"1.1.1.1","probe":"icmp"}]}],
		"probes": {"icmp": {"type": "icmp"}},
		"storage": {"clickhouse": {}}
	}`)
	if _, err := loadBytes(t, raw); err == nil {
		t.Fatal("expected error for missing addr")
	}
}

func TestStorageClickHouseBadInterval(t *testing.T) {
	raw := []byte(`{
		"targets": [{"group":"g","targets":[{"name":"t","host":"1.1.1.1","probe":"icmp"}]}],
		"probes": {"icmp": {"type": "icmp"}},
		"storage": {"clickhouse": {"addr":"ch:9000","batch":{"max_interval":"nonsense"}}}
	}`)
	if _, err := loadBytes(t, raw); err == nil {
		t.Fatal("expected error for bad max_interval")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	setExampleEnv(t)
	if _, err := Load("../../config.example.json"); err != nil {
		t.Fatalf("example config: %v", err)
	}
}

func TestStoreReload(t *testing.T) {
	p := writeTmp(t, minimalConfig)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(p, cfg)

	ch := make(chan struct{}, 1)
	s.Subscribe(ch)

	modified := strings.Replace(minimalConfig, `"pings": 10`, `"pings": 42`, 1)
	if err := os.WriteFile(p, []byte(modified), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := s.Current().Pings; got != 42 {
		t.Errorf("after reload pings = %d, want 42", got)
	}
	select {
	case <-ch:
		if got := s.Current().Pings; got != 42 {
			t.Errorf("subscriber notified but current pings = %d, want 42", got)
		}
	default:
		t.Error("subscriber not notified")
	}
}

func minimalValidConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{
		Interval: 60 * time.Second,
		Pings:    20,
		Storage:  Storage{ClickHouse: ClickHouse{Addr: "127.0.0.1:9000"}},
		Probes:   map[string]Probe{"icmp": {Type: "icmp", Timeout: 2 * time.Second}},
		Targets: []Group{{
			Group:   "core",
			Targets: []Target{{Name: "gw", Probe: "icmp", Host: "192.0.2.1"}},
		}},
	}
}

// A negative retention reaches Bootstrap's ALTER TABLE ... MODIFY TTL as a
// TTL in the past and expires the table on the next start — fail-open on a
// data-destroying knob, so Validate refuses it instead of defaulting.
func TestValidateBoundsRetentionDays(t *testing.T) {
	fields := []struct {
		name string
		set  func(*Config, int)
	}{
		{"cycle_days", func(c *Config, v int) { c.Storage.ClickHouse.Retention.CycleDays = v }},
		{"rtt_days", func(c *Config, v int) { c.Storage.ClickHouse.Retention.RTTDays = v }},
		{"hop_days", func(c *Config, v int) { c.Storage.ClickHouse.Retention.HopDays = v }},
		{"http_days", func(c *Config, v int) { c.Storage.ClickHouse.Retention.HTTPDays = v }},
	}
	for _, f := range fields {
		t.Run(f.name+" negative", func(t *testing.T) {
			cfg := scheduleConfig(20*time.Second, 10)
			f.set(cfg, -1)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("a negative retention is a TTL in the past and must be refused")
			}
			if !strings.Contains(err.Error(), f.name) {
				t.Errorf("error %q does not name the field %q", err, f.name)
			}
		})
		t.Run(f.name+" past the DateTime span", func(t *testing.T) {
			cfg := scheduleConfig(20*time.Second, 10)
			f.set(cfg, MaxRetentionDays+1)
			if err := cfg.Validate(); err == nil {
				t.Fatal("a retention past the UInt32 DateTime span must be refused")
			}
		})
		// MaxRetentionDays is a sanity bound, not the representable maximum:
		// the TTL is evaluated against each row's own timestamp, so what fits
		// inside DateTime depends on the clock. Validate applies that check
		// too, because a retention it accepts must be one the process can
		// boot with — 7,482 of the values the fixed ceiling admits validated
		// green on load and on SIGHUP and then made the master refuse to
		// start at its next restart, a disagreement invisible until a
		// redeploy since Bootstrap runs only at startup.
		t.Run(f.name+" at the representable maximum", func(t *testing.T) {
			at := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
			orig := validateNow
			validateNow = func() time.Time { return at }
			t.Cleanup(func() { validateNow = orig })

			last := int(MaxDateTime.Sub(at).Hours() / 24)
			if last >= MaxRetentionDays {
				t.Fatalf("fixture is vacuous: the representable maximum %d is not below MaxRetentionDays %d", last, MaxRetentionDays)
			}
			cfg := scheduleConfig(20*time.Second, 10)
			f.set(cfg, last)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("the last representable retention must validate: %v", err)
			}
			cfg = scheduleConfig(20*time.Second, 10)
			f.set(cfg, last+1)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("%d days expires past DateTime but validated — the process will refuse to boot on it", last+1)
			} else if !strings.Contains(err.Error(), f.name) {
				t.Errorf("error %q does not name the field %q", err, f.name)
			}
		})
		t.Run(f.name+" zero still defaults", func(t *testing.T) {
			cfg := scheduleConfig(20*time.Second, 10)
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			r := cfg.Storage.ClickHouse.Retention
			if r.CycleDays != 365 || r.RTTDays != 14 || r.HopDays != 90 || r.HTTPDays != 14 {
				t.Fatalf("defaults changed: %+v", r)
			}
		})
	}
}

func TestValidateRejectsReservedGroup(t *testing.T) {
	c := minimalValidConfig(t)
	c.Targets = append(c.Targets, Group{
		Group:   "_cluster",
		Targets: []Target{{Name: "impostor", Probe: "icmp", Host: "example.com"}},
	})
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error for the reserved _cluster group")
	}
	if !strings.Contains(err.Error(), "_cluster") {
		t.Fatalf("error %q does not mention the reserved group", err)
	}
}

func TestValidateRejectsReservedProbeName(t *testing.T) {
	c := minimalValidConfig(t)
	c.Probes["_slave_health"] = Probe{Type: "icmp", Timeout: time.Second}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() = nil, want an error for the reserved probe name")
	}
}

func TestParsedSlaveAddrs(t *testing.T) {
	cl := &Cluster{SlaveAddrs: map[string]string{
		"frankfurt-1": "10.44.0.2",
		"tokyo-1":     "2001:db8::7",
	}}
	pins, err := cl.ParsedSlaveAddrs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pins) != 2 {
		t.Fatalf("got %d pins, want 2", len(pins))
	}
	if pins["frankfurt-1"] != netip.MustParseAddr("10.44.0.2") {
		t.Fatalf("got %v for frankfurt-1", pins["frankfurt-1"])
	}
}

// A malformed pin must fail loudly at load. Silently dropping it would leave
// the operator believing a slave is pinned when it is not.
func TestParsedSlaveAddrsRejectsGarbage(t *testing.T) {
	cl := &Cluster{SlaveAddrs: map[string]string{"frankfurt-1": "not-an-ip"}}
	if _, err := cl.ParsedSlaveAddrs(); err == nil {
		t.Fatal("ParsedSlaveAddrs() = nil error, want a parse failure")
	}
}

func TestParsedSlaveAddrsRejectsUnreachable(t *testing.T) {
	cl := &Cluster{SlaveAddrs: map[string]string{"frankfurt-1": "127.0.0.1"}}
	if _, err := cl.ParsedSlaveAddrs(); err == nil {
		t.Fatal("ParsedSlaveAddrs() = nil error, want loopback to be rejected")
	}
}

// TestValidateHealthAlertsMustExist covers the only validation that can catch
// a typo in cluster.health_alerts: health targets are synthesized, so there is
// no target-level alerts list to check the name against, and an unknown name
// would otherwise be silently skipped by the evaluator — a slave outage that
// never alerts, with nothing in the logs to say why.
func TestValidateHealthAlertsMustExist(t *testing.T) {
	base := func() *Config {
		return &Config{
			Interval: time.Minute,
			Pings:    10,
			Storage:  Storage{ClickHouse: ClickHouse{Addr: "ch:9000"}},
			Probes:   map[string]Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
			Alerts: map[string]Alert{
				"slave-unreachable": {Condition: "loss_pct > 50", Sustained: 3, Actions: []string{"log"}},
			},
			Actions: map[string]Action{"log": {Type: "log"}},
		}
	}

	cfg := base()
	cfg.Cluster = &Cluster{Token: "t", HealthAlerts: []string{"slave-unreachable"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defined alert rejected: %v", err)
	}

	cfg = base()
	cfg.Cluster = &Cluster{Token: "t", HealthAlerts: []string{"slave-unreachable", "typo-alert"}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("undefined alert accepted")
	}
	if !strings.Contains(err.Error(), "typo-alert") {
		t.Errorf("error must name the offending entry, got: %v", err)
	}
}

// TestExampleConfigWiresHealthAlerts keeps the canonical reference honest:
// slave-unreachable exists as an alert but a user cannot attach it to a
// _cluster target, so cluster.health_alerts is the only thing that makes it
// anything other than dead config.
func TestExampleConfigWiresHealthAlerts(t *testing.T) {
	setExampleEnv(t)
	cfg, err := Load("../../config.example.json")
	if err != nil {
		t.Fatalf("example config: %v", err)
	}
	if cfg.Cluster == nil || len(cfg.Cluster.HealthAlerts) == 0 {
		t.Fatal("example config declares no cluster.health_alerts")
	}
	for _, name := range cfg.Cluster.HealthAlerts {
		if _, ok := cfg.Alerts[name]; !ok {
			t.Errorf("health alert %q is not defined", name)
		}
	}
}

func scheduleConfig(interval time.Duration, pings int) *Config {
	return &Config{
		Interval: interval,
		Pings:    pings,
		Storage:  Storage{ClickHouse: ClickHouse{Addr: "ch:9000"}},
		Probes:   map[string]Probe{"icmp": {Type: "icmp", Timeout: 2 * time.Second}},
	}
}

// A schedule probe.Build refuses must never reach the store: Reload keeps the
// previous targets, so the operator sees green while every node is one restart
// away from a fleet-wide boot failure.
func TestValidateRefusesUnschedulablePingCount(t *testing.T) {
	t.Run("120 pings at 20s is refused", func(t *testing.T) {
		err := scheduleConfig(20*time.Second, 120).Validate()
		if err == nil {
			t.Fatal("120 pings owes 23.8s of spacing against a 20s interval and must be refused")
		}
		for _, want := range []string{"120", "20s"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})

	t.Run("the deployed schedule is accepted", func(t *testing.T) {
		if err := scheduleConfig(20*time.Second, 10).Validate(); err != nil {
			t.Fatalf("10 pings at 20s derives 1.82s and must validate: %v", err)
		}
	})

	t.Run("30 pings at 20s is accepted", func(t *testing.T) {
		if err := scheduleConfig(20*time.Second, 30).Validate(); err != nil {
			t.Fatalf("30 pings at 20s derives 473ms, above the floor: %v", err)
		}
	})

	t.Run("the schedule binds only where an icmp probe exists", func(t *testing.T) {
		cfg := scheduleConfig(20*time.Second, 120)
		cfg.Probes = map[string]Probe{"web": {Type: "http", Timeout: 2 * time.Second}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("an http-only config is not bound by the icmp ping budget: %v", err)
		}
	})

	t.Run("pings past the ingest rtt ceiling is refused with no icmp probe", func(t *testing.T) {
		cfg := scheduleConfig(5*time.Second, 100_000)
		cfg.Probes = map[string]Probe{"web": {Type: "tcp", Timeout: 2 * time.Second}}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("pings=100000 stamps Sent=100000 on a probe error, which cluster ingest refuses; Validate must refuse it first")
		}
		if !strings.Contains(err.Error(), "100000") {
			t.Errorf("error %q does not name the count", err)
		}
	})

	t.Run("pings at the ceiling is accepted with no icmp probe", func(t *testing.T) {
		cfg := scheduleConfig(5*time.Second, MaxPingsPerCycle)
		cfg.Probes = map[string]Probe{"web": {Type: "tcp", Timeout: 2 * time.Second}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("pings at MaxPingsPerCycle is ingestable and must validate: %v", err)
		}
	})

	t.Run("a cluster master is bound with no icmp probe defined", func(t *testing.T) {
		// 20 pings at 4s derives a 10ms budget — fine to store standalone,
		// unbuildable the moment the health mesh injects its icmp probe.
		cfg := scheduleConfig(4*time.Second, 20)
		cfg.Probes = map[string]Probe{"web": {Type: "tcp", Timeout: 2 * time.Second}}
		cfg.Cluster = &Cluster{Token: "secret"}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("a cluster master's schedule gains the slave-health icmp probe, so this must be refused")
		}
		if !strings.Contains(err.Error(), "icmp schedule") {
			t.Errorf("error %q does not name the icmp schedule", err)
		}
	})

	t.Run("the same schedule stays legal standalone", func(t *testing.T) {
		cfg := scheduleConfig(4*time.Second, 20)
		cfg.Probes = map[string]Probe{"web": {Type: "tcp", Timeout: 2 * time.Second}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("no icmp probe can ever be injected without a cluster, so this must validate: %v", err)
		}
	})

	t.Run("Load refuses it too", func(t *testing.T) {
		body := strings.NewReplacer(`"interval": "30s"`, `"interval": "20s"`, `"pings": 10`, `"pings": 120`).Replace(minimalConfig)
		if _, err := Load(writeTmp(t, body)); err == nil {
			t.Fatal("Load must not return a config that cannot build")
		}
	})
}

// A target whose identifiers exceed MaxLabelLen would be served to slaves and
// then refused at ingest, so every cycle for it would be lost with the master
// showing a valid  Refuse it at load instead, where the operator sees it.
func TestValidateBoundsLabelLengths(t *testing.T) {
	long := strings.Repeat("x", MaxLabelLen+1)
	base := func() *Config {
		return &Config{
			Interval: time.Minute,
			Pings:    5,
			Storage:  Storage{ClickHouse: ClickHouse{Addr: "ch:9000"}},
			Probes:   map[string]Probe{"icmp": {Type: "icmp"}},
			Targets: []Group{{Group: "g", Targets: []Target{
				{Name: "t", Probe: "icmp", Host: "1.1.1.1"},
			}}},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("the baseline config is not valid: %v", err)
	}

	cases := map[string]func(*Config){
		"group":  func(c *Config) { c.Targets[0].Group = long },
		"target": func(c *Config) { c.Targets[0].Targets[0].Name = long },
		"probe": func(c *Config) {
			c.Probes[long] = Probe{Type: "icmp"}
			c.Targets[0].Targets[0].Probe = long
		},
	}
	for name, mutate := range cases {
		cfg := base()
		mutate(cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s name of %d bytes accepted, want rejected", name, MaxLabelLen+1)
		}
	}
}

// A cycle's RTT cannot exceed its context deadline, which is the interval — so
// an interval above what probe_rtt can store is a config the master accepts
// and its own ingest bound then refuses, dropping the batch. The ceiling is
// the storage column, and the config ceiling sits under it.
func TestValidateRefusesAnIntervalPastTheStorableRTT(t *testing.T) {
	base := func(d time.Duration) *Config {
		return &Config{
			Interval: d,
			Pings:    1,
			Storage:  Storage{ClickHouse: ClickHouse{Addr: "ch:9000"}},
			Probes:   map[string]Probe{"icmp": {Type: "icmp"}},
			Targets: []Group{{Group: "g", Targets: []Target{
				{Name: "t", Probe: "icmp", Host: "1.1.1.1"},
			}}},
		}
	}
	// Derived, not picked: the ceiling is the storage column's, so the only
	// schedules refused are the ones whose latencies cannot be stored as
	// themselves. A round number below it would refuse working configs.
	if MaxProbeInterval != MaxSampleRTT {
		t.Fatalf("MaxProbeInterval %s is not the storable-rtt ceiling %s", MaxProbeInterval, MaxSampleRTT)
	}
	if err := base(65 * time.Minute).Validate(); err != nil {
		t.Fatalf("a storable interval was refused: %v", err)
	}
	if err := base(MaxProbeInterval).Validate(); err != nil {
		t.Fatalf("the largest schedulable interval was refused: %v", err)
	}
	if err := base(MaxProbeInterval + time.Second).Validate(); err == nil {
		t.Fatal("an interval past the bound was accepted")
	}
}

// MaxSampleRTT is durUS's saturation point, not a round number under it: a
// value above it is stored as something other than itself.
func TestMaxSampleRTTIsTheStorageSaturationPoint(t *testing.T) {
	if got := MaxSampleRTT / time.Microsecond; got != math.MaxUint32 {
		t.Fatalf("MaxSampleRTT is %d microseconds, want MaxUint32 (%d)", got, uint64(math.MaxUint32))
	}
}

// storage.clickhouse.batch feeds the writer directly: max_rows sizes the
// pending slice and max_interval is the flush ticker's period. runTable runs
// on a goroutine with no recover(), so make([]any, 0, -1) and
// time.NewTicker(0) are boot panics with a stack trace rather than config
// errors. Both were defaulted on the zero value only and bounded at neither
// end, while writer.go still called them "validated at config-load".
func TestValidateBoundsBatchKnobs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rows    int
		iv      string
		wantErr bool
	}{
		{"defaults", 0, "", false},
		{"ordinary", 5000, "2s", false},
		{"negative rows", -1, "1s", true},
		{"rows past the ceiling", MaxBatchRows + 1, "1s", true},
		{"zero interval", 1000, "0s", true},
		{"negative interval", 1000, "-1s", true},
		{"interval past the ceiling", 1000, "2h", true},
		{"unparseable interval", 1000, "banana", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := scheduleConfig(20*time.Second, 10)
			cfg.Storage.ClickHouse.Batch.MaxRows = tc.rows
			cfg.Storage.ClickHouse.Batch.MaxInterval = tc.iv
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("max_rows=%d max_interval=%q validated; the writer panics on it at boot", tc.rows, tc.iv)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("max_rows=%d max_interval=%q: %v", tc.rows, tc.iv, err)
			}
		})
	}
}

// cluster.name and cluster.advertise travel as headers on every request the
// slave makes, and the master refuses either past its own limit. Since that
// refusal became fatal the slave exits non-zero on it, so systemd restarts a
// config this package accepted, forever. Every config Validate accepts must be
// one the process can run on.
func TestValidateBoundsTheHeaderBearingClusterFields(t *testing.T) {
	base := func() *Cluster {
		return &Cluster{
			MasterURL: "https://master.example", Token: "tok", Name: "tokyo-1",
		}
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*Cluster)
		wantErr bool
	}{
		{"ordinary", func(*Cluster) {}, false},
		{"name at the limit", func(c *Cluster) { c.Name = strings.Repeat("n", MaxSlaveNameLen) }, false},
		{"name past the limit", func(c *Cluster) { c.Name = strings.Repeat("n", MaxSlaveNameLen+1) }, true},
		{"advertise at the limit", func(c *Cluster) { c.Advertise = strings.Repeat("a", MaxSlaveFieldLen) }, false},
		{"advertise past the limit", func(c *Cluster) { c.Advertise = strings.Repeat("a", MaxSlaveFieldLen+1) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl := base()
			tc.mutate(cl)
			cfg := &Config{Cluster: cl}
			err := cfg.ValidateMinimal()
			if tc.wantErr && err == nil {
				t.Fatal("validated a field the master refuses; the slave exits non-zero on that refusal and systemd restarts it forever")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected: %v", err)
			}
		})
	}
}

// Validate must apply the master's whole admission rule, not a subset. Since
// a marked 400 became fatal to the slave process, a name the master refuses is
// a name systemd restarts forever — on a config this package accepted.
// "master" is the natural copy-paste from a master config, whose cluster.source
// defaults to exactly that.
func TestValidateRefusesEveryNameTheMasterWould(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"tokyo-1", true},
		{strings.Repeat("n", MaxSlaveNameLen), true},
		{strings.Repeat("n", MaxSlaveNameLen+1), false},
		{"master", false},
		{"fra\t1", false},
		{"fra\x7f1", false},
	} {
		t.Run(fmt.Sprintf("%q", tc.name), func(t *testing.T) {
			if got := ValidSlaveName(tc.name); got != tc.ok {
				t.Fatalf("ValidSlaveName = %v, want %v", got, tc.ok)
			}
			cfg := &Config{Cluster: &Cluster{
				MasterURL: "https://master.example", Token: "tok", Name: tc.name,
			}}
			err := cfg.ValidateMinimal()
			if tc.ok && err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("validated a name the master refuses; the slave exits non-zero on that refusal and systemd restarts it forever")
			}
		})
	}
}

// The registry inverts slave_addrs to decide which peer may hold an address.
// Two names on one address made that choice depend on Go's map iteration
// order — a different health mesh, and a different scheduler fingerprint, per
// signal. ParseReachableAddr unmaps, so the duplicate can be invisible.
func TestParsedSlaveAddrsRefusesADuplicatePin(t *testing.T) {
	for _, pins := range []map[string]string{
		{"tokyo-1": "10.0.0.5", "tokyo-1a": "10.0.0.5"},
		{"a": "10.0.0.5", "b": "::ffff:10.0.0.5"},
	} {
		c := &Cluster{SlaveAddrs: pins}
		if _, err := c.ParsedSlaveAddrs(); err == nil {
			t.Fatalf("%v accepted: the registry's inversion is then non-deterministic and peer selection flips per signal", pins)
		}
	}
	c := &Cluster{SlaveAddrs: map[string]string{"a": "10.0.0.5", "b": "10.0.0.6"}}
	if _, err := c.ParsedSlaveAddrs(); err != nil {
		t.Fatalf("distinct pins must load: %v", err)
	}
}

// The rule is about a SLAVE's name. Applying it from Validate too refused a
// master config over cluster.name — a field nothing on the master path reads —
// with a message that is nonsense on the node that IS the master. cluster.name
// "master" is also the natural value for a master's own config to carry.
func TestMasterConfigIsNotRefusedOverClusterName(t *testing.T) {
	cfg := scheduleConfig(20*time.Second, 10)
	cfg.Cluster = &Cluster{Token: "tok", Source: "master", Name: "master"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a master config was refused over a slave-only field: %v", err)
	}
	cfg.Cluster.Name = "node\x7f1"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a master config was refused over a control character in a field it never reads: %v", err)
	}
	// The slave path still applies it.
	slave := &Config{Cluster: &Cluster{
		MasterURL: "https://master.example", Token: "tok", Name: "master",
	}}
	if err := slave.ValidateMinimal(); err == nil {
		t.Fatal("a slave named \"master\" still validated; the master refuses it and the slave now exits on that refusal")
	}
}

// Every bound here is otherwise pinned only by an input sized from the
// constant it guards — `MaxBatchRows + 1` exceeds the cap for every value of
// the cap — so none of them redden when the constant drifts. These literals
// are the second half: the test reddens both when a guard goes and when its
// constant moves. Each figure is the consequence, not a preference.
func TestBoundsHoldTheirDocumentedValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  int
		want int
		why  string
	}{
		{"MaxBatchRows", MaxBatchRows, 1_000_000,
			"four flushers each preallocate make([]any, 0, maxRows); at 1e9 that is ~64 GB before a row arrives"},
		{"MaxSlaveNameLen", MaxSlaveNameLen, 128,
			"the cardinality bound on a LowCardinality source label, and an inbound header net/http bounds only at 1 MiB"},
		{"MaxSlaveFieldLen", MaxSlaveFieldLen, 256,
			"retained per registry entry, and inside the log-dedup key even when ParseAdvertise rejects it"},
		{"MaxRetentionDays", MaxRetentionDays, 36_500,
			"a sanity ceiling; the representable maximum is the clock check in RetentionWithinDateTime"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d — %s. If the change is deliberate, update this literal and say why here.",
				tc.name, tc.got, tc.want, tc.why)
		}
	}
	if MaxBatchInterval != time.Hour {
		t.Errorf("MaxBatchInterval = %s, want 1h — it is the writer's flush ticker period", MaxBatchInterval)
	}
}

// no_trace carried a JSON tag on the public Probe type and was read on four
// paths, but rawProbe never parsed it — so an operator setting it on an icmp
// probe got no error and no effect, and the TTL walk the field exists to stop
// kept running.
func TestLoadParsesNoTrace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
	  "interval": "20s", "pings": 5,
	  "storage": {"clickhouse": {"addr": "127.0.0.1:9000"}},
	  "probes": {"quiet": {"type": "icmp", "timeout": "1s", "no_trace": true},
	             "loud":  {"type": "icmp", "timeout": "1s"}},
	  "targets": [{"group": "g", "targets": [{"name": "t", "host": "127.0.0.1", "probe": "quiet"}]}]
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Probes["quiet"].NoTrace {
		t.Error(`"no_trace": true was dropped on load`)
	}
	if cfg.Probes["loud"].NoTrace {
		t.Error("an absent no_trace defaulted to true")
	}
}

// cluster.source is the label the master stamps on its own cycles: a
// LowCardinality value on all four tables and a key in the alert evaluator's
// per-source state, and the one identifier of that class that was unchecked.
func TestValidateBoundsClusterSource(t *testing.T) {
	base := func(source string) *Config {
		return &Config{
			Interval: 20 * time.Second,
			Pings:    5,
			Storage:  Storage{ClickHouse: ClickHouse{Addr: "127.0.0.1:9000"}},
			Probes:   map[string]Probe{"icmp": {Type: "icmp", Timeout: time.Second}},
			Targets: []Group{{Group: "g", Targets: []Target{
				{Name: "t", Host: "127.0.0.1", Probe: "icmp"},
			}}},
			Cluster: &Cluster{Token: "tok", Source: source},
		}
	}
	if err := base(strings.Repeat("a", MaxLabelLen+1)).Validate(); err == nil {
		t.Error("Validate accepted a cluster.source past MaxLabelLen")
	}
	if err := base("fra\x001").Validate(); err == nil {
		t.Error("Validate accepted a cluster.source carrying a control character")
	}
	if err := base("fra1").Validate(); err != nil {
		t.Errorf("Validate refused an ordinary cluster.source: %v", err)
	}
}

// buffer_bytes is refused rather than clamped: a slave that cannot hold what
// its operator asked for must say so at boot instead of silently holding less.
// Zero means the default, so it stays legal.
func TestBufferBytesBoundsAreSlaveOnly(t *testing.T) {
	slaveCfg := func(b int64) *Config {
		return &Config{Cluster: &Cluster{
			MasterURL: "https://master.example", Token: "tok", Name: "tokyo-1", BufferBytes: b,
		}}
	}
	for _, tc := range []struct {
		name  string
		bytes int64
		ok    bool
	}{
		{"zero means the default", 0, true},
		{"exactly the minimum", MinBufferBytes, true},
		{"exactly the maximum", MaxBufferBytes, true},
		{"one byte under the minimum", MinBufferBytes - 1, false},
		{"one byte over the maximum", MaxBufferBytes + 1, false},
		{"negative", -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := slaveCfg(tc.bytes).ValidateMinimal()
			if tc.ok && err != nil {
				t.Fatalf("ValidateMinimal(%d) = %v, want nil", tc.bytes, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ValidateMinimal(%d) = nil, want a refusal", tc.bytes)
			}
		})
	}

	// Nothing on the master path reads the field, so Validate must not refuse
	// a master config over it — the mistake validateClusterHeaders records.
	master := &Config{
		Interval: 20 * time.Second,
		Pings:    3,
		Storage:  Storage{ClickHouse: ClickHouse{Addr: "127.0.0.1:9000"}},
		Targets:  []Group{{Group: "core", Targets: []Target{{Name: "gw", Host: "192.0.2.1", Probe: "icmp"}}}},
		Probes:   map[string]Probe{"icmp": {Type: "icmp"}},
		Cluster:  &Cluster{Token: "tok", BufferBytes: MaxBufferBytes + 1},
	}
	if err := master.Validate(); err != nil {
		t.Fatalf("Validate refused a master config over buffer_bytes, a field the master never reads: %v", err)
	}
}
