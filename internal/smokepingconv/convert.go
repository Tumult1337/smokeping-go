package smokepingconv

import (
	"fmt"
	"io"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/smokepingconv/parser"
)

// Convert reads a SmokePing config from r (treated as if it came from path,
// relative to baseDir for @include resolution) and returns an equivalent
// gosmokeping config plus notes. Tokenize / build errors (malformed input,
// include cycles) become Go errors; soft issues (unmappable alerts, unknown
// probe types) become notes.
func Convert(r io.Reader, baseDir, path string) (*config.Config, []Note, error) {
	lines, err := parser.Tokenize(r, baseDir, path)
	if err != nil {
		return nil, nil, fmt.Errorf("tokenize: %w", err)
	}
	root, err := parser.Build(lines)
	if err != nil {
		return nil, nil, fmt.Errorf("build: %w", err)
	}

	interval, pings, dbNotes := mapDatabase(root)
	probes, info, probeNotes := mapProbes(root)
	groups, targetNotes := mapTargets(root, info, nil)
	alerts, alertNotes := mapAlerts(root)

	for _, g := range groups {
		for _, t := range g.Targets {
			if _, ok := probes[t.Probe]; !ok {
				probes[t.Probe] = config.Probe{Type: defaultTypeFor(t.Probe), Timeout: defaultTimeoutFor(t.Probe)}
			}
		}
	}

	actions := map[string]config.Action{"log": {Type: "log"}}

	cfg := &config.Config{
		Listen:   ":8080",
		Interval: interval,
		Pings:    pings,
		Storage: config.Storage{
			Backend: config.BackendInfluxV2,
			InfluxV2: config.InfluxV2{
				URL:       "${INFLUX_URL}",
				Token:     "${INFLUX_TOKEN}",
				Org:       "${INFLUX_ORG}",
				BucketRaw: "smokeping_raw",
				Bucket1h:  "smokeping_1h",
				Bucket1d:  "smokeping_1d",
			},
		},
		Probes:  probes,
		Targets: groups,
		Alerts:  alerts,
		Actions: actions,
	}

	var notes []Note
	if root.SectionlessTargets {
		notes = append(notes, Note{
			Level: LevelWarn, Category: CatSection,
			Detail: "no *** Section *** header seen — treating all content as Targets (input looks like an @include fragment)",
		})
	}
	notes = append(notes, dbNotes...)
	notes = append(notes, probeNotes...)
	notes = append(notes, targetNotes...)
	notes = append(notes, alertNotes...)
	notes = append(notes, Note{
		Level: LevelWarn, Category: CatGeneral,
		Detail: "storage.influxv2 is a placeholder — edit URL/token/org before running gosmokeping",
	})
	for _, u := range root.Unknown {
		notes = append(notes, Note{
			Level: LevelSkip, Category: CatSection,
			Detail: fmt.Sprintf("*** %s *** ignored (no gosmokeping analogue)", u.Section),
		})
	}

	return cfg, notes, nil
}

// defaultTypeFor guesses a gosmokeping probe type from a slugged name. Used
// only when a target references a probe that was never declared in
// *** Probes *** — a fallback that should almost never fire in real inputs.
func defaultTypeFor(slugName string) string {
	switch slugName {
	case "icmp", "fping", "fping6":
		return "icmp"
	case "tcp", "tcpping":
		return "tcp"
	case "dns":
		return "dns"
	case "http", "https", "curl", "echopinghttp", "echopinghttps":
		return "http"
	default:
		return "icmp"
	}
}

func defaultTimeoutFor(slugName string) time.Duration {
	_ = slugName
	return 5 * time.Second
}
