package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"time"
)

type Config struct {
	Listen   string        `json:"listen"`
	Interval time.Duration `json:"interval"`
	Pings    int           `json:"pings"`

	// Storage configures the ClickHouse persistence backend.
	Storage Storage `json:"storage"`

	Probes  map[string]Probe  `json:"probes"`
	Targets []Group           `json:"targets"`
	Alerts  map[string]Alert  `json:"alerts"`
	Actions map[string]Action `json:"actions"`
	// Cluster is optional. Absent = standalone (pre-cluster behavior).
	// Present on a master to enable /api/v1/cluster/* endpoints and stamp
	// locally-probed cycles with Source (default "master"). Present on a
	// slave with MasterURL+Token+Name set.
	Cluster *Cluster `json:"cluster,omitempty"`
}

// Storage holds the ClickHouse backend configuration.
type Storage struct {
	ClickHouse ClickHouse `json:"clickhouse"`
}

// ClickHouse configures the ClickHouse storage backend.
type ClickHouse struct {
	Addr      string              `json:"addr"`
	Database  string              `json:"database"`
	Username  string              `json:"username"`
	Password  string              `json:"password"`
	TLS       bool                `json:"tls"`
	Cluster   string              `json:"cluster"`
	Retention ClickHouseRetention `json:"retention"`
	Batch     ClickHouseBatch     `json:"batch"`
}

// ClickHouseRetention configures per-table TTL in days.
type ClickHouseRetention struct {
	CycleDays int `json:"cycle_days"`
	RTTDays   int `json:"rtt_days"`
	HopDays   int `json:"hop_days"`
	HTTPDays  int `json:"http_days"`
}

// ClickHouseBatch configures the write batcher: flush when MaxRows is
// reached or MaxInterval elapses, whichever comes first.
type ClickHouseBatch struct {
	MaxRows     int    `json:"max_rows"`
	MaxInterval string `json:"max_interval"`
}

// Validate checks the Storage block is internally consistent.
func (s *Storage) Validate() error {
	return nil // full validation happens in applyDefaults via Load
}

type Probe struct {
	Type    string        `json:"type"`
	Timeout time.Duration `json:"timeout"`
	// Insecure skips TLS verification for HTTP probes. Use for targets with
	// self-signed or expired certs where reachability matters more than cert
	// validity. Ignored by non-HTTP probe types.
	Insecure bool `json:"insecure,omitempty"`
}

type Group struct {
	Group   string   `json:"group"`
	Title   string   `json:"title"`
	Targets []Target `json:"targets"`
}

type Target struct {
	Name string `json:"name"`
	// Title is an optional display label; falls back to Name in the UI.
	Title  string   `json:"title,omitempty"`
	Host   string   `json:"host,omitempty"`
	URL    string   `json:"url,omitempty"`
	Probe  string   `json:"probe"`
	Alerts []string `json:"alerts,omitempty"`
	// Family pins the address family for probes that resolve a hostname.
	// "" means system default (whatever getaddrinfo picks first), "v4"
	// forces A / IPv4, "v6" forces AAAA / IPv6. Applies to every probe
	// type — ICMP/MTR via ResolveIPAddr("ip4"|"ip6"), TCP via the dialer
	// network ("tcp4"|"tcp6"), HTTP via a family-pinned DialContext on a
	// cloned transport, and DNS via a pinned Dial on the net.Resolver.
	Family string `json:"family,omitempty"`
	// Slaves restricts probing to the listed slave names. When empty, the
	// master and every registered slave probe this target (pre-assignment
	// default). When non-empty, only listed slaves probe it — the master
	// skips it locally, and slaves not in the list receive a filtered
	// /cluster/config that omits it entirely.
	Slaves []string `json:"slaves,omitempty"`
}

// Cluster configures master/slave coordination. Fields used by each role:
//
//	master: Token (required), Source (default "master")
//	slave:  MasterURL, Token, Name (all required), PushEvery (default 5s),
//	        PullEvery (default 60s; "0s" disables periodic config refresh)
//
// Slave identity is set via the --slave CLI flag, not a field here, so the
// same config shape serves both roles without a redundant role= key.
type Cluster struct {
	MasterURL string `json:"master_url,omitempty"`
	Token     string `json:"token,omitempty"`
	Name      string `json:"name,omitempty"`
	Source    string `json:"source,omitempty"`
	PushEvery string `json:"push_every,omitempty"`
	// PullEvery controls how often a slave re-pulls its config from the
	// master. Empty = 60s default; "0" / "0s" = one-shot (pull on startup
	// only, then rely on operator restart for changes). Any positive
	// duration is used as-is.
	PullEvery string `json:"pull_every,omitempty"`
}

type Alert struct {
	Condition string   `json:"condition"`
	Sustained int      `json:"sustained"`
	Actions   []string `json:"actions"`
}

type Action struct {
	Type     string `json:"type"`
	URL      string `json:"url,omitempty"`
	Command  string `json:"command,omitempty"`
	Template string `json:"template,omitempty"`
}

type rawConfig struct {
	Listen   string              `json:"listen"`
	Interval string              `json:"interval"`
	Pings    int                 `json:"pings"`
	Storage  Storage             `json:"storage"`
	Probes   map[string]rawProbe `json:"probes"`
	Targets  []Group             `json:"targets"`
	Alerts   map[string]Alert    `json:"alerts"`
	Actions  map[string]Action   `json:"actions"`
	Cluster  *Cluster            `json:"cluster,omitempty"`
}

type rawProbe struct {
	Type     string `json:"type"`
	Timeout  string `json:"timeout"`
	Insecure bool   `json:"insecure"`
}

var envVar = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

// chIdent matches a ClickHouse identifier safe to interpolate raw into
// DDL (no backtick-quoting needed). Database name and cluster name are
// both interpolated this way in internal/storage/clickhouse/{schema,bootstrap}.go.
// Restricting to this character set means a typo with a hyphen, dot, or
// SQL-syntax payload fails fast at config-load instead of producing a
// confusing CH syntax error mid-bootstrap.
var chIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func Load(path string) (*Config, error) {
	cfg, err := loadUnvalidated(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadMinimal is the slave-mode loader. Same parsing rules as Load but the
// strict target/storage/alerts checks are skipped — a slave's on-disk config
// only carries its own listen port and cluster{} block; the real target list
// arrives from the master over the wire.
func LoadMinimal(path string) (*Config, error) {
	cfg, err := loadUnvalidated(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.ValidateMinimal(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func loadUnvalidated(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	data = expandEnv(data)

	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg := &Config{
		Listen:  raw.Listen,
		Pings:   raw.Pings,
		Storage: raw.Storage,
		Targets: raw.Targets,
		Alerts:  raw.Alerts,
		Actions: raw.Actions,
		Cluster: raw.Cluster,
		Probes:  make(map[string]Probe, len(raw.Probes)),
	}
	if cfg.Cluster != nil && cfg.Cluster.Source == "" {
		cfg.Cluster.Source = "master"
	}

	if raw.Interval == "" {
		cfg.Interval = 5 * time.Minute
	} else {
		d, err := time.ParseDuration(raw.Interval)
		if err != nil {
			return nil, fmt.Errorf("invalid interval %q: %w", raw.Interval, err)
		}
		cfg.Interval = d
	}

	if cfg.Pings == 0 {
		cfg.Pings = 20
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}

	for name, rp := range raw.Probes {
		p := Probe{Type: rp.Type, Insecure: rp.Insecure}
		if rp.Timeout != "" {
			d, err := time.ParseDuration(rp.Timeout)
			if err != nil {
				return nil, fmt.Errorf("probe %q: invalid timeout %q: %w", name, rp.Timeout, err)
			}
			p.Timeout = d
		} else {
			p.Timeout = 5 * time.Second
		}
		cfg.Probes[name] = p
	}

	return cfg, nil
}

func expandEnv(data []byte) []byte {
	return envVar.ReplaceAllFunc(data, func(match []byte) []byte {
		name := string(match[2 : len(match)-1])
		if v, ok := os.LookupEnv(name); ok {
			return []byte(v)
		}
		return match
	})
}

// ValidateMinimal is a relaxed Validate used for a slave's local config. A
// slave only needs listen/log-level plumbing and a populated cluster{} block;
// storage, targets, and alerts are served by the master over the wire.
func (c *Config) ValidateMinimal() error {
	if c.Cluster == nil {
		return fmt.Errorf("cluster block is required for slave mode")
	}
	if c.Cluster.MasterURL == "" {
		return fmt.Errorf("cluster.master_url is required for slave mode")
	}
	if c.Cluster.Token == "" {
		return fmt.Errorf("cluster.token is required for slave mode")
	}
	if c.Cluster.Name == "" {
		return fmt.Errorf("cluster.name is required for slave mode")
	}
	return nil
}

func (c *Config) Validate() error {
	if c.Interval <= 0 {
		return fmt.Errorf("interval must be positive")
	}
	if c.Pings <= 0 {
		return fmt.Errorf("pings must be positive")
	}
	ch := &c.Storage.ClickHouse
	if ch.Addr == "" {
		return fmt.Errorf("storage.clickhouse.addr is required")
	}
	if ch.Database == "" {
		ch.Database = "gosmokeping"
	}
	if !chIdent.MatchString(ch.Database) {
		return fmt.Errorf("storage.clickhouse.database %q: must match %s", ch.Database, chIdent.String())
	}
	if ch.Cluster != "" && !chIdent.MatchString(ch.Cluster) {
		return fmt.Errorf("storage.clickhouse.cluster %q: must match %s", ch.Cluster, chIdent.String())
	}
	if ch.Username == "" {
		ch.Username = "default"
	}
	if ch.Retention.CycleDays == 0 {
		ch.Retention.CycleDays = 365
	}
	if ch.Retention.RTTDays == 0 {
		ch.Retention.RTTDays = 14
	}
	if ch.Retention.HopDays == 0 {
		ch.Retention.HopDays = 90
	}
	if ch.Retention.HTTPDays == 0 {
		ch.Retention.HTTPDays = 14
	}
	if ch.Batch.MaxRows == 0 {
		ch.Batch.MaxRows = 1000
	}
	if ch.Batch.MaxInterval == "" {
		ch.Batch.MaxInterval = "1s"
	}
	if _, err := time.ParseDuration(ch.Batch.MaxInterval); err != nil {
		return fmt.Errorf("storage.clickhouse.batch.max_interval: %w", err)
	}

	seenTargets := make(map[string]string)
	for _, g := range c.Targets {
		if g.Group == "" {
			return fmt.Errorf("group name is required")
		}
		for _, t := range g.Targets {
			if t.Name == "" {
				return fmt.Errorf("group %q: target name is required", g.Group)
			}
			id := g.Group + "/" + t.Name
			if prev, dup := seenTargets[id]; dup {
				return fmt.Errorf("duplicate target %q (also in group %q)", id, prev)
			}
			seenTargets[id] = g.Group
			if t.Probe == "" {
				return fmt.Errorf("target %q: probe is required", id)
			}
			if _, ok := c.Probes[t.Probe]; !ok {
				return fmt.Errorf("target %q: probe %q not defined", id, t.Probe)
			}
			if t.Host == "" && t.URL == "" {
				return fmt.Errorf("target %q: host or url is required", id)
			}
			switch t.Family {
			case "", "v4", "v6":
			default:
				return fmt.Errorf("target %q: family must be \"v4\", \"v6\", or empty (got %q)", id, t.Family)
			}
			for _, a := range t.Alerts {
				if _, ok := c.Alerts[a]; !ok {
					return fmt.Errorf("target %q: alert %q not defined", id, a)
				}
			}
		}
	}

	for name, a := range c.Alerts {
		if a.Condition == "" {
			return fmt.Errorf("alert %q: condition is required", name)
		}
		if a.Sustained <= 0 {
			return fmt.Errorf("alert %q: sustained must be positive", name)
		}
		for _, act := range a.Actions {
			if _, ok := c.Actions[act]; !ok {
				return fmt.Errorf("alert %q: action %q not defined", name, act)
			}
		}
	}

	return nil
}

func (c *Config) AllTargets() []TargetRef {
	var out []TargetRef
	for _, g := range c.Targets {
		for _, t := range g.Targets {
			out = append(out, TargetRef{Group: g.Group, Target: t})
		}
	}
	return out
}

type TargetRef struct {
	Group  string
	Target Target
}

func (r TargetRef) ID() string {
	return r.Group + "/" + r.Target.Name
}
