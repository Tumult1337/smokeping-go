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

	// Storage selects the persistence backend. Legacy configs with a
	// top-level "influxdb" key are folded into Storage.InfluxV2 by
	// loadUnvalidated so existing installs keep working with no edit.
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

// BackendName identifies a concrete storage implementation. The list is
// closed on purpose: unknown values are rejected at load time so typos
// don't silently fall through to a default backend.
type BackendName string

const (
	BackendInfluxV2   BackendName = "influxv2"
	BackendInfluxV3   BackendName = "influxv3"
	BackendPrometheus BackendName = "prometheus"
)

// Storage holds the backend selection plus each backend's per-impl
// configuration. Only the block referenced by Backend is consulted; the
// others may be omitted.
type Storage struct {
	Backend    BackendName `json:"backend"`
	InfluxV2   InfluxV2    `json:"influxv2"`
	InfluxV3   InfluxV3    `json:"influxv3"`
	Prometheus Prometheus  `json:"prometheus"`
}

// InfluxV2 configures the InfluxDB v2 backend. BucketRaw is required;
// Bucket1h / Bucket1d are optional and only used if rollup tiers are wanted.
type InfluxV2 struct {
	URL       string `json:"url"`
	Token     string `json:"token"`
	Org       string `json:"org"`
	BucketRaw string `json:"bucket_raw"`
	Bucket1h  string `json:"bucket_1h"`
	Bucket1d  string `json:"bucket_1d"`
}

// InfluxV3 configures the InfluxDB v3 backend. v3 uses databases instead
// of buckets and the gRPC/flight SQL protocol; currently unimplemented.
type InfluxV3 struct {
	URL      string `json:"url"`
	Token    string `json:"token"`
	Database string `json:"database"`
}

// Prometheus configures a remote-write endpoint. Reads would go through
// PromQL on the same host (or a federated Thanos/Mimir). Currently
// unimplemented.
type Prometheus struct {
	RemoteWriteURL string `json:"remote_write_url"`
	QueryURL       string `json:"query_url"`
	BearerToken    string `json:"bearer_token"`
}

// Validate checks the Storage block is internally consistent. Empty
// Backend + no credentials is allowed — the caller treats it as
// "standalone without persistent storage" and logs a warning. A set
// Backend with missing required fields is fatal so operators get a clear
// error at startup instead of silent data loss.
func (s *Storage) Validate() error {
	switch s.Backend {
	case "":
		// Unset backend is only valid when no credentials were supplied
		// for any backend — otherwise the operator clearly intended to
		// use one and we want to refuse to guess which.
		if s.InfluxV2.URL != "" || s.InfluxV3.URL != "" || s.Prometheus.RemoteWriteURL != "" {
			return fmt.Errorf("storage.backend must be set when any backend credentials are configured")
		}
		return nil
	case BackendInfluxV2:
		if s.InfluxV2.URL == "" {
			return fmt.Errorf("storage.influxv2.url is required")
		}
		if s.InfluxV2.BucketRaw == "" {
			return fmt.Errorf("storage.influxv2.bucket_raw is required")
		}
		return nil
	case BackendInfluxV3, BackendPrometheus:
		// Stubs — the factory will surface ErrBackendNotImplemented at
		// open time with a clearer message than Validate can here.
		return nil
	default:
		return fmt.Errorf("storage.backend %q is not recognised", s.Backend)
	}
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
	Listen   string  `json:"listen"`
	Interval string  `json:"interval"`
	Pings    int     `json:"pings"`
	Storage  Storage `json:"storage"`
	// LegacyInfluxDB accepts the pre-backend top-level "influxdb" block so
	// existing configs keep loading without an edit. Folded into
	// Storage.InfluxV2 when Storage is otherwise empty — see loadUnvalidated.
	LegacyInfluxDB *InfluxV2           `json:"influxdb,omitempty"`
	Probes         map[string]rawProbe `json:"probes"`
	Targets        []Group             `json:"targets"`
	Alerts         map[string]Alert    `json:"alerts"`
	Actions        map[string]Action   `json:"actions"`
	Cluster        *Cluster            `json:"cluster,omitempty"`
}

type rawProbe struct {
	Type     string `json:"type"`
	Timeout  string `json:"timeout"`
	Insecure bool   `json:"insecure"`
}

var envVar = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

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
// strict target/influx/alerts checks are skipped — a slave's on-disk config
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
	// Backwards-compat: accept the pre-backend top-level `influxdb` key and
	// fold it into Storage.InfluxV2. Only applied when the new `storage`
	// block is empty so an explicit "storage" always wins.
	if raw.LegacyInfluxDB != nil && cfg.Storage.Backend == "" && cfg.Storage.InfluxV2.URL == "" {
		cfg.Storage.Backend = BackendInfluxV2
		cfg.Storage.InfluxV2 = *raw.LegacyInfluxDB
	}
	// Default to v2 when the backend is left empty but a v2 config block is
	// present — matches the behaviour operators got before Storage existed.
	if cfg.Storage.Backend == "" && cfg.Storage.InfluxV2.URL != "" {
		cfg.Storage.Backend = BackendInfluxV2
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
// influx, targets, and alerts are served by the master over the wire.
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
	if err := c.Storage.Validate(); err != nil {
		return err
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
