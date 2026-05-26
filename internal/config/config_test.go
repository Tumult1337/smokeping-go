package config

import (
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
