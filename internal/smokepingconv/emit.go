package smokepingconv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/tumult/gosmokeping/internal/config"
)

// preferredProbeOrder is the canonical ordering used at the top of the probes
// block. Anything not in this list comes after, sorted alphabetically.
var preferredProbeOrder = []string{"icmp", "tcp", "http", "dns"}

// Marshal returns a deterministic JSON encoding of cfg:
//   - probes keys in preferredProbeOrder first, then remaining keys sorted
//   - alerts / actions sorted alphabetically (Go's default map ordering)
//   - trailing newline so diffs end cleanly
//
// The rest of the config is emitted via standard encoding. The struct shim
// lets us hand-roll the probes block without cloning the entire Config type.
func Marshal(cfg *config.Config) ([]byte, error) {
	probes, err := encodeProbes(cfg.Probes)
	if err != nil {
		return nil, err
	}

	// Build a parallel struct with Probes replaced by raw JSON.
	type shim struct {
		Listen   string                   `json:"listen"`
		Interval string                   `json:"interval"`
		Pings    int                      `json:"pings"`
		Storage  config.Storage           `json:"storage"`
		Probes   json.RawMessage          `json:"probes"`
		Targets  []config.Group           `json:"targets"`
		Alerts   map[string]config.Alert  `json:"alerts,omitempty"`
		Actions  map[string]config.Action `json:"actions,omitempty"`
		Cluster  *config.Cluster          `json:"cluster,omitempty"`
	}
	s := shim{
		Listen:   cfg.Listen,
		Interval: cfg.Interval.String(),
		Pings:    cfg.Pings,
		Storage:  cfg.Storage,
		Probes:   probes,
		Targets:  cfg.Targets,
		Alerts:   cfg.Alerts,
		Actions:  cfg.Actions,
		Cluster:  cfg.Cluster,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&s); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeProbes(probes map[string]config.Probe) (json.RawMessage, error) {
	if len(probes) == 0 {
		return []byte("{}"), nil
	}

	keys := make([]string, 0, len(probes))
	seen := map[string]bool{}
	for _, k := range preferredProbeOrder {
		if _, ok := probes[k]; ok {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range probes {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)

	var buf bytes.Buffer
	buf.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			buf.WriteString(",")
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteString(":")
		vb, err := json.Marshal(probes[k])
		if err != nil {
			return nil, fmt.Errorf("marshal probe %q: %w", k, err)
		}
		buf.Write(vb)
	}
	buf.WriteString("}")
	// Re-pretty-print so the caller gets indented output.
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, buf.Bytes(), "  ", "  "); err != nil {
		return nil, err
	}
	return pretty.Bytes(), nil
}
