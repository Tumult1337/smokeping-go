package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Reserved names owned by the cluster health mesh, defined here because
// slavehealth imports this package and so the dependency cannot run the other
// way. slavehealth.Group and slavehealth.ProbeName are these, which is what
// keeps the group Validate refuses and the group IsHealthGroup admits one fact
// rather than two literals a rename can separate.
const (
	ReservedGroup = "_cluster"
	ReservedProbe = "_slave_health"
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

type Probe struct {
	Type    string        `json:"type"`
	Timeout time.Duration `json:"timeout"`
	// Insecure skips TLS verification for HTTP probes. Use for targets with
	// self-signed or expired certs where reachability matters more than cert
	// validity. Ignored by non-HTTP probe types.
	Insecure bool `json:"insecure,omitempty"`
	// NoTrace disables the opportunistic TTL walk the icmp probe runs after
	// its echo batch. Set for slave-health probes when cluster.health_hops is
	// false: on a wide mesh the N^2 hop streams dominate storage, and
	// intermediate hops disclose a slave's transit provider. Ignored by
	// probe types that never trace.
	NoTrace bool `json:"no_trace,omitempty"`
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

	// Advertise is the single IP address peers use to health-probe this
	// slave. Explicit only — never auto-detected. A container on a bridge
	// network sees only its internal address (172.17.0.2), and no range
	// check can distinguish that from a legitimate private peer address,
	// so a detected value would be confidently wrong. Empty means this
	// slave is excluded from the health mesh.
	Advertise string `json:"advertise,omitempty"`

	// SlaveAddrs optionally pins each slave to the one address it may
	// advertise (master-side). A pinned slave claiming anything else is
	// rejected. Unpinned slaves are accepted as-is, so zero-config works.
	SlaveAddrs map[string]string `json:"slave_addrs,omitempty"`

	// HealthAlerts names the alerts attached to every synthesized slave
	// health target (master-side). Health targets live outside the stored
	// config — a user cannot write them, so this is the only way to attach
	// an alert to one. Each name must exist in the top-level alerts block.
	HealthAlerts []string `json:"health_alerts,omitempty"`

	// HealthHops enables traceroute hop collection for health targets.
	// Defaults to true; set false on large meshes where N^2 hop streams
	// (master probes N slaves, each slave probes the other N-1) dominate
	// storage, or where intermediate-hop disclosure of a
	// slave's transit provider is unwanted.
	HealthHops *bool `json:"health_hops,omitempty"`

	Source    string `json:"source,omitempty"`
	PushEvery string `json:"push_every,omitempty"`
	// PullEvery controls how often a slave re-pulls its config from the
	// master. Empty = 60s default; "0" / "0s" = one-shot (pull on startup
	// only, then rely on operator restart for changes). Any positive
	// duration is used as-is.
	PullEvery string `json:"pull_every,omitempty"`
	// InsecureMasterURL permits a plaintext http:// master_url. The shared
	// bearer token and all cycle data otherwise require https — set this only
	// on a trusted network (e.g. TLS terminated at a loopback sidecar).
	InsecureMasterURL bool `json:"insecure_master_url,omitempty"`
}

type Alert struct {
	Condition string   `json:"condition"`
	Sustained int      `json:"sustained"`
	Actions   []string `json:"actions"`
	// Quorum optionally requires several probe sources to agree before this
	// alert dispatches. Absent means each source dispatches independently.
	//
	// omitzero (not omitempty — Quorum is a struct, and encoding/json's
	// omitempty never considers structs empty) keeps configs without quorum
	// byte-identical to pre-quorum output; Quorum{} satisfies reflect's
	// zero-value check so it's dropped before MarshalJSON ever runs.
	Quorum Quorum `json:"quorum,omitzero"`
}

// Quorum gates alert dispatch on how many probe sources agree. Absent (the
// zero value) preserves single-source behaviour: every source dispatches
// independently.
//
// Accepts either "majority" or a positive integer on the wire.
type Quorum struct {
	// Majority requires strictly more than half the live sources.
	Majority bool
	// Min is an absolute minimum count of simultaneously-firing sources.
	Min int
}

func (q Quorum) Enabled() bool { return q.Majority || q.Min > 0 }

// Threshold returns how many sources must be firing simultaneously, given the
// number currently reporting. A majority is strictly more than half, so 2 live
// sources require both — 1-of-2 is a tie, not a majority.
func (q Quorum) Threshold(live int) int {
	if q.Majority {
		return live/2 + 1
	}
	return q.Min
}

func (q Quorum) MarshalJSON() ([]byte, error) {
	switch {
	case q.Majority:
		return []byte(`"majority"`), nil
	case q.Min > 0:
		return []byte(strconv.Itoa(q.Min)), nil
	default:
		return []byte(`null`), nil
	}
}

func (q *Quorum) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "null" {
		*q = Quorum{}
		return nil
	}
	if strings.HasPrefix(raw, `"`) {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("quorum: %w", err)
		}
		if s != "majority" {
			return fmt.Errorf(`quorum: %q is not valid: use "majority" or a positive integer`, s)
		}
		*q = Quorum{Majority: true}
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf(`quorum: %s is not valid: use "majority" or a positive integer: %w`, raw, err)
	}
	if n < 1 {
		return fmt.Errorf("quorum: %d must be at least 1", n)
	}
	*q = Quorum{Min: n}
	return nil
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
	NoTrace  bool   `json:"no_trace"`
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
	// Cluster tokens and storage passwords are credentials, while action URLs
	// commonly embed webhook tokens, so literal placeholders in those fields
	// must fail closed. Other fields retain warning-only behavior for compatibility.
	if fields := unresolvedCredentialFields(raw); len(fields) > 0 {
		return nil, fmt.Errorf("config: unresolved ${...} placeholders in credential fields: %s", strings.Join(fields, ", "))
	}
	if missing := unresolvedVars(data); len(missing) > 0 {
		slog.Warn("config: unresolved ${...} placeholders — env vars not set",
			"vars", missing,
			"hint", "set them in .env (next to your config file) or in the shell before starting")
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
		p := Probe{Type: rp.Type, Insecure: rp.Insecure, NoTrace: rp.NoTrace}
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
		v, ok := os.LookupEnv(name)
		if !ok {
			return match
		}
		// Placeholders live inside JSON string values, so the substituted
		// value must be JSON-escaped — otherwise a value containing a quote,
		// backslash, or newline (a Windows path, a regex, an injected field)
		// either breaks the parse or alters surrounding structure. Marshal as
		// a JSON string and strip the surrounding quotes so it slots cleanly
		// into the existing "${VAR}" string context.
		b, err := json.Marshal(v)
		if err != nil { // unreachable for string input
			return match
		}
		return b[1 : len(b)-1]
	})
}

// unresolvedVars returns the sorted unique names of `${VAR}` placeholders
// that survived expandEnv — i.e. env vars referenced by the config but
// not actually set when the binary started.
func unresolvedVars(data []byte) []string {
	matches := envVar.FindAllSubmatch(data, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		seen[string(m[1])] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func unresolvedCredentialFields(raw rawConfig) []string {
	var fields []string
	if raw.Cluster != nil && envVar.MatchString(raw.Cluster.Token) {
		fields = append(fields, "cluster.token")
	}
	if envVar.MatchString(raw.Storage.ClickHouse.Password) {
		fields = append(fields, "storage.clickhouse.password")
	}
	for name, action := range raw.Actions {
		if envVar.MatchString(action.URL) {
			fields = append(fields, "actions."+name+".url")
		}
	}
	sort.Strings(fields)
	return fields
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
	u, err := url.Parse(c.Cluster.MasterURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("cluster.master_url %q is not a valid URL", c.Cluster.MasterURL)
	}
	if u.Scheme != "https" && !c.Cluster.InsecureMasterURL {
		return fmt.Errorf("cluster.master_url must use https (set cluster.insecure_master_url to allow http on a trusted network)")
	}
	if c.Cluster.Token == "" {
		return fmt.Errorf("cluster.token is required for slave mode")
	}
	if c.Cluster.Name == "" {
		return fmt.Errorf("cluster.name is required for slave mode")
	}
	return validateClusterHeaders(c.Cluster)
}

// validateClusterHeaders bounds the two cluster fields that travel as request
// headers. Called from ValidateMinimal only — the slave path — because that is
// where the invariant lives: a *slave's* name must be one the master accepts.
// Applying it from Validate as well refused a MASTER config over cluster.name,
// a field nothing on the master path reads, with the message "cluster.name
// \"master\" is one the master refuses" on the node that is the master. The
// fix belonged at the invariant, not at the shared helper that was in scope. The master refuses either past its own limit, and since that
// refusal became fatal the slave exits non-zero on it — so a config this
// package accepts would have systemd restarting it forever. The bound lives
// here as well as at the master for the reason ICMPPingBudget does.
func validateClusterHeaders(c *Cluster) error {
	if c == nil {
		return nil
	}
	if c.Name != "" && !ValidSlaveName(c.Name) {
		return fmt.Errorf("cluster.name %q is one the master refuses: must be ≤%d bytes, not \"master\", and free of control characters", c.Name, MaxSlaveNameLen)
	}
	if len(c.Advertise) > MaxSlaveFieldLen {
		return fmt.Errorf("cluster.advertise is %d bytes, past the %d the master accepts", len(c.Advertise), MaxSlaveFieldLen)
	}
	return nil
}

// MaxSlaveNameLen and MaxSlaveFieldLen bound the two cluster fields that
// travel as request headers. The master enforces the same limits; they live
// here so a config it would refuse is never stored, and master's own constants
// are defined from these so the two cannot drift.
const (
	MaxSlaveNameLen  = 128
	MaxSlaveFieldLen = 256
)

// ValidSourceLabel bounds a cycle's source stamp. Empty is legal — Load
// defaults it to "master" — and unlike a slave name it may be "master", which
// is what the master itself uses.
func ValidSourceLabel(source string) bool {
	if len(source) > MaxLabelLen {
		return false
	}
	for _, c := range source {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// ValidSlaveName is the master's own admission rule for a cluster name, kept
// here so Validate applies exactly it rather than a subset. Mirroring only the
// length left `cluster.name: "master"` — the natural copy-paste from a master
// config, whose cluster.source defaults to that — and a name carrying a stray
// control character from a templated value validating locally and then being
// refused at every /register and /config, which since that refusal became
// fatal is a crash loop under systemd on a config this package accepted.
func ValidSlaveName(name string) bool {
	if name == "" || len(name) > MaxSlaveNameLen {
		return false
	}
	if name == "master" {
		return false
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// ICMP schedule policy. These live here rather than in probe because a
// schedule the icmp probe cannot serve must never be stored or served:
// probe.Build reads the same values, so the two layers cannot drift.
const (
	// ICMPPingSpacing is the gap probe.NewICMP schedules between echoes.
	ICMPPingSpacing = 200 * time.Millisecond
	// MinPingBudget is the smallest per-ping deadline a schedule may derive at
	// full loss. It is a plausibility threshold, not a proof of uselessness: a
	// 49ms budget still succeeds against a 10ms LAN target, but against the
	// 10-150ms band of a normal WAN path a config this tight is not intentional.
	MinPingBudget = 50 * time.Millisecond
	// icmpTraceSeqReserve mirrors the TTL walk's traceRounds*(traceMaxTTL+1)
	// sequence window in probe, which pins the two together at compile time.
	icmpTraceSeqReserve = 3 * (30 + 1)
	// MaxPingsPerCycle is the echo sequence space left above that window: past
	// it no batch can be placed clear of the walk without wrapping into it.
	MaxPingsPerCycle = 1<<16 - icmpTraceSeqReserve
)

// MaxRetentionDays bounds each storage.clickhouse.retention knob (days of
// per-table TTL). Below 1 day the ALTER ... MODIFY TTL Bootstrap re-emits on
// every start is a TTL already in the past, which expires the whole table.
// The ceiling is a sanity bound and not a derived maximum: the TTL is
// evaluated against each row's own timestamp, so what is representable
// depends on when the row was written, which no compile-time constant knows.
// RetentionWithinDateTime is the real check, and Validate applies it here so
// a config this package accepts is one the process can boot with.
const MaxRetentionDays = 36_500

// MaxBatchRows and MaxBatchInterval bound storage.clickhouse.batch. Both feed
// the writer directly — max_rows sizes the pending slice, max_interval is the
// flush ticker's period — and runTable runs on a goroutine with no recover(),
// so an unbounded value is a boot panic rather than a config error. The
// ceilings are generous sanity bounds: a batch past a million rows or a flush
// interval past an hour is a misconfiguration whatever the deployment.
const (
	MaxBatchRows     = 1_000_000
	MaxBatchInterval = time.Hour
)

// MaxDateTime is toDateTime's own ceiling: ClickHouse DateTime is UInt32
// seconds from the epoch, so 2106-02-07 06:28:15 UTC is the last instant a
// TTL sum can name.
var MaxDateTime = time.Unix(math.MaxUint32, 0).UTC()

// RetentionWithinDateTime refuses a retention whose expiry, measured from now,
// falls outside DateTime. It lives here rather than in the storage package for
// the reason ICMPPingBudget does: Validate calls it, so a retention the
// process cannot boot with is never stored. Without that, ~7,482 of the values
// MaxRetentionDays admits validated green on load and on SIGHUP and then made
// the master refuse to start at its next restart — a disagreement invisible
// until a redeploy, since Bootstrap runs only at startup.
func RetentionWithinDateTime(days int, now time.Time) error {
	// Both ends. Bounding only the ceiling let the case this function's own
	// callers cite — a TTL already in the past, which expires the whole table
	// on the next merge — through the storage-side backstop, where the config
	// that reached it was by definition not validated.
	if days < 1 {
		return fmt.Errorf("%d days is a TTL in the past, which expires the whole table", days)
	}
	// Above the sanity ceiling before the arithmetic: AddDate wraps on a large
	// enough day count, so the comparison below returns nil for values far
	// past 2106 and the backstop admits what it exists to refuse.
	if days > MaxRetentionDays {
		return fmt.Errorf("%d days is past the %d-day ceiling", days, MaxRetentionDays)
	}
	if expiry := now.UTC().AddDate(0, 0, days); expiry.After(MaxDateTime) {
		return fmt.Errorf("%d days expires at %s, past DateTime's %s ceiling",
			days, expiry.Format("2006-01-02"), MaxDateTime.Format("2006-01-02"))
	}
	return nil
}

// validateNow is the clock Validate measures retention against, injectable so
// a test can pin the boundary without the answer drifting a day at midnight.
var validateNow = time.Now

// MaxLabelLen bounds every identifier that becomes a ClickHouse
// LowCardinality value — a group, a target name, a probe name, a cycle's
// source. Config.Validate refuses a longer one so a config the master accepts
// is always ingestable from a slave, and cluster's ingest refuses a longer one
// so a slave cannot mint a label the config never held. 256 is double the
// master's 128-byte slave-name ceiling and far above any real target name.
const MaxLabelLen = 256

// TTL-walk bounds. They live here for the same reason the ICMP schedule
// policy above does: the producer is probe and the consumer is cluster's
// ingest bound, and config is the only package both import.
const (
	// MaxTraceRounds mirrors probe's maxRounds — the most rounds one MTR
	// cycle walks. probe's icmp walk runs 3.
	MaxTraceRounds = 10
	// MaxTraceTTL mirrors probe's MTR.maxTTL — the deepest TTL a round walks.
	MaxTraceTTL = 30
	// MaxHopRowsPerCycle is walkRounds' exact ceiling: it emits one row per
	// (ttl, distinct responder), and a round contributes at most one responder
	// per TTL, so a path that diverges every round yields rounds × TTLs rows.
	// A TTL nothing answered emits one row, which is inside the same product.
	MaxHopRowsPerCycle = MaxTraceRounds * MaxTraceTTL
)

// Latency bounds. They live here, like the timestamp window below, because the
// producer of a latency is the scheduler's cycle deadline — an interval this
// package accepts — and the consumer is cluster's ingest bound.
const (
	// MaxSampleRTT bounds one latency value: a cycle RTT, a hop RTT, an http
	// sample's RTT, and every stats.Summary field. Negative is not a latency,
	// and the ceiling is exactly where clickhouse's durUS stops storing a
	// value as itself — MaxUint32 microseconds, 71m34.967295s.
	MaxSampleRTT = math.MaxUint32 * time.Microsecond
	// MaxProbeInterval is the longest schedule Validate accepts. A cycle runs
	// under a context whose deadline is the interval, so the interval is also
	// the largest RTT a probe can measure; capping it here is what keeps every
	// config the master accepts ingestable from a slave. It is MaxSampleRTT
	// exactly rather than a round number under it, so the only schedules
	// refused are the ones whose own latencies cannot be stored as themselves.
	MaxProbeInterval = MaxSampleRTT
)

// Cycle timestamp bounds. They live here rather than in cluster because both
// the ingest that refuses an out-of-range timestamp and the reader that must
// keep already-stored ones off the API have to agree on the same window.
const (
	// MaxFutureSkew is how far ahead of the master's clock a cycle may be
	// stamped. NTP-synced hosts agree to milliseconds; a drifting VM is
	// seconds to minutes out. Beyond it a row pins itself as its source's
	// "latest" for as long as the lie lasts and outlives probe_hop's TTL,
	// which is derived from the row timestamp — only a manual ClickHouse
	// delete clears it.
	MaxFutureSkew = 5 * time.Minute
	// MaxCycleAge is how stale a pushed cycle may be. slave.PushSink is a
	// 600-cycle drop-oldest ring, so even a multi-day master outage delivers
	// cycles far younger than this; a row older than the shortest default
	// retention (14d, probe_rtt and probe_http) is written already expired.
	MaxCycleAge = 7 * 24 * time.Hour
)

// HasICMPProbe reports whether any probe is subject to the ping schedule
// below, which is an icmp property — MTR's worst case is unrelated, and every
// other probe type ignores spacing entirely.
func HasICMPProbe(probes map[string]Probe) bool {
	for _, p := range probes {
		if p.Type == "icmp" {
			return true
		}
	}
	return false
}

// Separator bytes the fingerprint encodings delimit their levels with, and the
// escape that keeps an operator-supplied name from spelling one. They live here
// because scheduler.Fingerprint and slavehealth.Set.Fingerprint are two builders
// of one format whose outputs are concatenated into a single rebuild decision.
const (
	SepField = '\x1f'
	SepList  = '\x1c'
	SepEntry = '\x1e'
	SepBlock = '\x1d'
)

var sepEscaper = strings.NewReplacer(
	"\\", `\\`,
	string(SepField), `\u`,
	string(SepList), `\f`,
	string(SepEntry), `\r`,
	string(SepBlock), `\g`,
)

// EscapeSeparators makes a name unable to forge a fingerprint's structure. A
// group, target or alert name is bounded only by MaxLabelLen — no character
// class — so a raw separator inside one otherwise reads back as a level break
// and two materially different configs hash alike.
func EscapeSeparators(s string) string {
	if !strings.ContainsAny(s, "\\\x1c\x1d\x1e\x1f") {
		return s
	}
	return sepEscaper.Replace(s)
}

// ValidateInterval bounds the cycle period for every schedule, icmp or not:
// scheduler.loopTarget passes it to rand.Int64N and time.NewTicker, both of
// which panic on a non-positive value from a goroutine no recover() covers.
// probe.Build applies it as defence in depth for a master-supplied config a
// slave never validated.
func ValidateInterval(interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("interval must be positive, got %s", interval)
	}
	if interval > MaxProbeInterval {
		return fmt.Errorf("interval %s exceeds %s, past which a measured rtt is no longer storable", interval, MaxProbeInterval)
	}
	return nil
}

// ValidatePingCount bounds pings for every schedule, icmp or not: the
// scheduler stamps Sent = pings on any probe error and a cycle carries up to
// one RTT per ping, so a count past MaxPingsPerCycle — cluster ingest's
// rtts-per-cycle ceiling, itself below the UInt16 sent/lost columns — is a
// cycle no master can ingest. probe.Build applies the same bound as defence
// in depth for a slave building a master-supplied config.
func ValidatePingCount(pings int) error {
	if pings <= 0 {
		return fmt.Errorf("pings must be positive, got %d", pings)
	}
	if pings > MaxPingsPerCycle {
		return fmt.Errorf("pings=%d exceeds %d, the most rtt samples cluster ingest admits per cycle", pings, MaxPingsPerCycle)
	}
	return nil
}

// ICMPPingBudget returns the per-ping deadline a full-loss cycle derives and
// refuses the schedule when it falls below MinPingBudget. The icmp probe
// shortens each echo to fit the cycle, but no shortening rescues a schedule
// whose spacing alone overruns it.
func ICMPPingBudget(interval time.Duration, pings int) (time.Duration, error) {
	if pings <= 0 {
		return 0, fmt.Errorf("pings must be positive, got %d", pings)
	}
	// Both bounds precede the multiplication below, which overflows int64 for
	// absurd values and wraps negative, deriving a passing budget from an
	// impossible schedule.
	if maxPings := int(interval/ICMPPingSpacing) + 1; pings > maxPings {
		return 0, fmt.Errorf("pings=%d exceeds the %d that fit interval %s at %s spacing, so no per-ping budget exists",
			pings, maxPings, interval, ICMPPingSpacing)
	}
	if pings > MaxPingsPerCycle {
		return 0, fmt.Errorf("pings=%d exceeds the %d echo sequence numbers that stay clear of the TTL walk, so no batch fits interval %s",
			pings, MaxPingsPerCycle, interval)
	}
	budget := (interval - time.Duration(pings-1)*ICMPPingSpacing) / time.Duration(pings)
	if budget < MinPingBudget {
		return budget, fmt.Errorf("pings=%d at interval %s derives a %s per-ping budget, below the %s minimum: reduce pings or raise interval",
			pings, interval, budget.Round(time.Millisecond), MinPingBudget)
	}
	return budget, nil
}

func (c *Config) Validate() error {
	if err := ValidateInterval(c.Interval); err != nil {
		return err
	}
	if err := ValidatePingCount(c.Pings); err != nil {
		return err
	}
	// A cluster master's probe map gains slavehealth's icmp probe at
	// scheduler-build time, so its schedule is bound whether or not the stored
	// map defines one — gating on the stored map alone let a validated config
	// become unbuildable fleet-wide at the first slave registration.
	if HasICMPProbe(c.Probes) || (c.Cluster != nil && c.Cluster.Token != "") {
		if _, err := ICMPPingBudget(c.Interval, c.Pings); err != nil {
			return fmt.Errorf("icmp schedule: %w", err)
		}
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
	for _, r := range []struct {
		name string
		days *int
		def  int
	}{
		{"cycle_days", &ch.Retention.CycleDays, 365},
		{"rtt_days", &ch.Retention.RTTDays, 14},
		{"hop_days", &ch.Retention.HopDays, 90},
		{"http_days", &ch.Retention.HTTPDays, 14},
	} {
		if *r.days == 0 {
			*r.days = r.def
			continue
		}
		if *r.days < 0 || *r.days > MaxRetentionDays {
			return fmt.Errorf("storage.clickhouse.retention.%s %d is outside [1, %d]: Bootstrap re-emits it as MODIFY TTL on every start, and a TTL in the past expires the table", r.name, *r.days, MaxRetentionDays)
		}
		if err := RetentionWithinDateTime(*r.days, validateNow()); err != nil {
			return fmt.Errorf("storage.clickhouse.retention.%s: %w", r.name, err)
		}
	}
	if ch.Batch.MaxRows == 0 {
		ch.Batch.MaxRows = 1000
	}
	// Bounded at both ends, like the retention knobs above and for the same
	// reason: the writer sizes a slice and a ticker from these, and neither
	// runTable nor the goroutine it runs on has a recover(). A zero or
	// negative value validated green and panicked the process at boot with a
	// stack trace instead of a config error.
	if ch.Batch.MaxRows < 0 || ch.Batch.MaxRows > MaxBatchRows {
		return fmt.Errorf("storage.clickhouse.batch.max_rows %d is outside [1, %d]: it sizes the writer's pending slice", ch.Batch.MaxRows, MaxBatchRows)
	}
	if ch.Batch.MaxInterval == "" {
		ch.Batch.MaxInterval = "1s"
	}
	if d, err := time.ParseDuration(ch.Batch.MaxInterval); err != nil {
		return fmt.Errorf("storage.clickhouse.batch.max_interval: %w", err)
	} else if d <= 0 || d > MaxBatchInterval {
		return fmt.Errorf("storage.clickhouse.batch.max_interval %s is outside (0, %s]: it is the writer's flush ticker, which panics on a non-positive period", d, MaxBatchInterval)
	}

	seenTargets := make(map[string]string)
	if _, taken := c.Probes[ReservedProbe]; taken {
		return fmt.Errorf("probe %q is reserved for cluster slave health", ReservedProbe)
	}

	for _, g := range c.Targets {
		if g.Group == "" {
			return fmt.Errorf("group name is required")
		}
		// The health mesh owns this group. A user-defined target here would
		// shadow a synthetic one and inherit its address-stripping treatment
		// in the API, so reject it at load rather than resolve it silently.
		if g.Group == ReservedGroup {
			return fmt.Errorf("group %q is reserved for cluster slave health", ReservedGroup)
		}
		if len(g.Group) > MaxLabelLen {
			return fmt.Errorf("group name is %d bytes, limit %d", len(g.Group), MaxLabelLen)
		}
		for _, t := range g.Targets {
			if t.Name == "" {
				return fmt.Errorf("group %q: target name is required", g.Group)
			}
			if len(t.Name) > MaxLabelLen {
				return fmt.Errorf("group %q: target name is %d bytes, limit %d", g.Group, len(t.Name), MaxLabelLen)
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
			if len(t.Probe) > MaxLabelLen {
				return fmt.Errorf("target %q: probe name is %d bytes, limit %d", id, len(t.Probe), MaxLabelLen)
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

	if c.Cluster != nil {
		// cluster.source is the label the master stamps on its own cycles, so it
		// is a LowCardinality value on all four tables and a key in the alert
		// evaluator's per-source state — the same class MaxLabelLen bounds for a
		// group, a target and a probe, and the only one that was never checked.
		if !ValidSourceLabel(c.Cluster.Source) {
			return fmt.Errorf("cluster.source must be at most %d bytes with no control characters", MaxLabelLen)
		}
		if _, err := c.Cluster.ParsedSlaveAddrs(); err != nil {
			return err
		}
		for _, a := range c.Cluster.HealthAlerts {
			if _, ok := c.Alerts[a]; !ok {
				return fmt.Errorf("cluster.health_alerts: alert %q not defined", a)
			}
		}
	}

	return nil
}

// ParseReachableAddr validates a single IP literal meant to name a peer the
// health mesh can probe — never a hostname, prefix, or host:port. It is the
// single source of truth for "can this address be a probe destination
// between mesh peers", shared by master.ParseAdvertise (a slave-reported
// value) and Cluster.ParsedSlaveAddrs (an operator-written pin); callers
// should wrap the returned error with call-site context.
//
// Private and unique-local ranges are deliberately accepted: a deployment
// may address its peers entirely within RFC1918 / fc00::/7, so rejecting
// them would break that case.
// Addresses that cannot name a reachable peer — unspecified, loopback,
// multicast, link-local — are rejected.
func ParseReachableAddr(raw string) (netip.Addr, error) {
	if raw == "" {
		return netip.Addr{}, fmt.Errorf("address is empty")
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%q: not an IP literal: %w", raw, err)
	}
	// Normalise ::ffff:a.b.c.d to a.b.c.d so two spellings of one address
	// collide in duplicate checks rather than registering as two peers.
	addr = addr.Unmap()
	switch {
	case addr.IsUnspecified():
		return netip.Addr{}, fmt.Errorf("%q: unspecified address", raw)
	case addr.IsLoopback():
		return netip.Addr{}, fmt.Errorf("%q: loopback is not reachable from peers", raw)
	case addr.IsMulticast(), addr.IsInterfaceLocalMulticast(), addr.IsLinkLocalMulticast():
		return netip.Addr{}, fmt.Errorf("%q: multicast is not a probe destination", raw)
	case addr.IsLinkLocalUnicast():
		return netip.Addr{}, fmt.Errorf("%q: link-local is not routable between peers", raw)
	}
	return addr, nil
}

// ParsedSlaveAddrs converts the optional name→address pin map into netip form.
// Parsing happens once at load so a typo surfaces at startup rather than
// silently leaving a slave unpinned at registration time — a pin that fails
// open is worse than no pin, because the operator believes it is enforced.
func (c *Cluster) ParsedSlaveAddrs() (map[string]netip.Addr, error) {
	if len(c.SlaveAddrs) == 0 {
		return nil, nil
	}
	out := make(map[string]netip.Addr, len(c.SlaveAddrs))
	// Sorted, so a duplicate value is reported against the same pair on every
	// load rather than against whichever name map iteration reached first.
	names := slices.Sorted(maps.Keys(c.SlaveAddrs))
	owner := make(map[netip.Addr]string, len(names))
	for _, name := range names {
		addr, err := ParseReachableAddr(c.SlaveAddrs[name])
		if err != nil {
			return nil, fmt.Errorf("cluster.slave_addrs[%q]: %w", name, err)
		}
		// Values must be unique: the registry inverts this map to decide which
		// peer may hold an address, and two names on one address made that
		// choice depend on Go's map iteration order — a different health mesh,
		// and a different scheduler fingerprint, per signal. ParseReachableAddr
		// unmaps, so 10.0.0.5 and ::ffff:10.0.0.5 collide here too.
		if first, dup := owner[addr]; dup {
			return nil, fmt.Errorf("cluster.slave_addrs: %q and %q both pin %s; one address can name only one slave", first, name, addr)
		}
		owner[addr] = name
		out[name] = addr
	}
	return out, nil
}

// HopsEnabled reports whether health targets collect traceroute hops.
// Defaults to true: the hops view is the reason the mesh uses ICMP rather
// than a bare echo, so an operator must opt out explicitly.
func (c *Cluster) HopsEnabled() bool {
	return c == nil || c.HealthHops == nil || *c.HealthHops
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
