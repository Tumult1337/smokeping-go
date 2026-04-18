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
  "influxdb": {
    "url": "http://localhost:8086",
    "token": "t",
    "org": "o",
    "bucket_raw": "raw",
    "bucket_1h": "h",
    "bucket_1d": "d"
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
      "influxdb": { "url": "http://x", "bucket_raw": "raw" },
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
	t.Setenv("INFLUX_TOKEN", "secret123")
	body := strings.Replace(minimalConfig, `"token": "t"`, `"token": "${INFLUX_TOKEN}"`, 1)
	cfg, err := Load(writeTmp(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InfluxDB.Token != "secret123" {
		t.Errorf("token = %q, want secret123", cfg.InfluxDB.Token)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"missing influx url", func(c *Config) { c.InfluxDB.URL = "" }, "influxdb.url"},
		{"missing raw bucket", func(c *Config) { c.InfluxDB.BucketRaw = "" }, "bucket_raw"},
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

func TestStoreReload(t *testing.T) {
	p := writeTmp(t, minimalConfig)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(p, cfg)

	ch := make(chan *Config, 1)
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
	case got := <-ch:
		if got.Pings != 42 {
			t.Errorf("subscriber got pings = %d", got.Pings)
		}
	default:
		t.Error("subscriber not notified")
	}
}
