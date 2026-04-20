package smokepingconv

import (
	"fmt"
	"strconv"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/smokepingconv/parser"
)

// ProbeInfo records the gosmokeping-side details for a SmokePing probe name
// so the targets mapper can look up what to emit for a target referencing
// that probe. URLFormat is populated only for http-type probes.
type ProbeInfo struct {
	Key       string // slugged probe name used as the gosmokeping map key
	Type      string // gosmokeping probe type: icmp/tcp/http/dns
	Port      int    // tcp port parsed from probe params; 0 if not set
	URLFormat string // http urlformat template (with %host% / %hostname%)
}

var probeTypeMap = map[string]string{
	"FPing":         "icmp",
	"FPing6":        "icmp",
	"DNS":           "dns",
	"TCPPing":       "tcp",
	"Curl":          "http",
	"EchoPingHttp":  "http",
	"EchoPingHttps": "http",
}

// mapProbes translates the *** Probes *** tree. Returns:
//   - probes: the gosmokeping probes map, keyed by slugged probe name
//   - info:   indexed by the original SmokePing probe name, used by targets
//   - notes:  warn/skip records
func mapProbes(root *parser.SPRoot) (map[string]config.Probe, map[string]ProbeInfo, []Note) {
	probes := map[string]config.Probe{}
	info := map[string]ProbeInfo{}
	var notes []Note

	var visit func(p parser.SPProbe, parentType string)
	visit = func(p parser.SPProbe, parentType string) {
		spType := p.Type
		if spType == "" {
			spType = parentType
		}
		gosmokeType, supported := probeTypeMap[spType]
		if !supported {
			notes = append(notes, Note{
				Level: LevelSkip, Category: CatProbe,
				Detail: fmt.Sprintf("%q (type %s) — no gosmokeping equivalent", p.Name, spType),
				Source: sourceOf(p.File, p.LineNo),
			})
			return
		}

		key := uniqueKey(probes, slug(p.Name))
		pi := ProbeInfo{Key: key, Type: gosmokeType}

		cp := config.Probe{Type: gosmokeType, Timeout: 5 * time.Second}
		if v, ok := p.Params["timeout"]; ok {
			if d, err := parseProbeDuration(v); err == nil {
				cp.Timeout = d
			} else {
				notes = append(notes, Note{
					Level: LevelWarn, Category: CatProbe,
					Detail: fmt.Sprintf("%q timeout %q unparseable, using 5s", p.Name, v),
					Source: sourceOf(p.File, p.LineNo),
				})
			}
		}
		if v, ok := p.Params["insecure_ssl"]; ok && (v == "yes" || v == "true" || v == "1") {
			cp.Insecure = true
		}
		if v, ok := p.Params["port"]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				pi.Port = n
			}
		}
		if v, ok := p.Params["urlformat"]; ok {
			pi.URLFormat = v
		}

		for _, k := range p.Keys {
			switch k {
			case "timeout", "insecure_ssl", "port", "urlformat":
				continue
			}
			notes = append(notes, Note{
				Level: LevelWarn, Category: CatProbe,
				Detail: fmt.Sprintf("%q param %q ignored (no gosmokeping analogue)", p.Name, k),
				Source: sourceOf(p.File, p.LineNo),
			})
		}

		probes[key] = cp
		info[p.Name] = pi

		for _, sub := range p.Subprobes {
			visit(sub, spType)
		}
	}

	for _, p := range root.Probes {
		visit(p, "")
	}

	// Seed defaults for SmokePing built-in probe names that weren't declared
	// in *** Probes ***. Real-world configs often omit the section and just
	// reference `probe = FPing` (etc.) from the targets tree. We only add to
	// `info` so lookups resolve; the probes map entry is synthesized on first
	// reference in Convert.
	for spName, gosmokeType := range probeTypeMap {
		if _, declared := info[spName]; declared {
			continue
		}
		info[spName] = ProbeInfo{Key: slug(spName), Type: gosmokeType}
	}

	return probes, info, notes
}

// parseProbeDuration accepts either a bare integer (SmokePing's seconds) or a
// Go-style duration string ("500ms", "3s").
func parseProbeDuration(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}

func sourceOf(file string, line int) string {
	if file == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", file, line)
}

// uniqueKey returns base when unused; otherwise base-2, base-3, etc.
func uniqueKey(m map[string]config.Probe, base string) string {
	if _, ok := m[base]; !ok {
		return base
	}
	for i := 2; ; i++ {
		k := fmt.Sprintf("%s-%d", base, i)
		if _, ok := m[k]; !ok {
			return k
		}
	}
}
