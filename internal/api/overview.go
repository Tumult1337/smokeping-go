package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/slavehealth"
	"github.com/tumult/gosmokeping/internal/storage"
)

// overviewWindow maps the UI's range buttons to a duration. Anything not in
// this map silently falls back to 1h — the buttons constrain the input, so a
// bad value here only fires on URL tampering and a 400 wouldn't be useful.
var overviewWindows = map[string]time.Duration{
	"-1h":  time.Hour,
	"-6h":  6 * time.Hour,
	"-24h": 24 * time.Hour,
}

// silentCycleMultiplier sets the staleness threshold for the silent flag:
// a target is silent if the most recent cycle is older than this many probe
// intervals. 5x gives 4 missed cycles before flagging — enough headroom for
// transient drops without hiding genuinely-down targets.
const silentCycleMultiplier = 5

// overviewRowDTO is the JSON shape returned per target. Pointers for the
// metric fields so silent rows serialise as JSON null instead of 0.
type overviewRowDTO struct {
	ID          string     `json:"id"`
	Group       string     `json:"group"`
	GroupTitle  string     `json:"group_title,omitempty"`
	Title       string     `json:"title,omitempty"`
	ProbeType   string     `json:"probe_type,omitempty"`
	LossAvg     *float64   `json:"loss_avg"`
	LossMax     *float64   `json:"loss_max"`
	RTTMedian   *float64   `json:"rtt_median"`
	RTTP95      *float64   `json:"rtt_p95"`
	RTTMax      *float64   `json:"rtt_max"`
	WorstSource string     `json:"worst_source,omitempty"`
	LastSeen    *time.Time `json:"last_seen"`
	Silent      bool       `json:"silent"`
	Sparkline   []*float64 `json:"sparkline"`
}

func (s *Server) getOverview(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		writeErr(w, http.StatusServiceUnavailable, "storage not configured")
		return
	}

	windowStr := r.URL.Query().Get("window")
	span, ok := overviewWindows[windowStr]
	if !ok {
		windowStr = "-1h"
		span = time.Hour
	}

	now := time.Now().UTC()
	to := now
	from := to.Add(-span)

	cfg := s.store.Current()

	source := r.URL.Query().Get("source")
	master := masterSourceName(cfg)
	var registered []string
	if s.slaves != nil {
		registered = s.slaves.Names()
	}
	if source != "" && !knownSource(source, master, registered) {
		writeErr(w, http.StatusBadRequest, "unknown source")
		return
	}

	// Health targets live outside the stored config, so the fleet view has to
	// append them the same way listTargets and resolveTarget do — otherwise
	// the overview (and its "By slave" filter) can never show a slave as down.
	// Overview rows carry no address-bearing field, so the Public view is
	// safe to render here in full.
	allTargets := append(cfg.AllTargets(), s.healthTargets()...)
	targets := allTargets
	if source != "" {
		targets = make([]config.TargetRef, 0, len(allTargets))
		for _, t := range allTargets {
			for _, src := range sourcesFor(t, master, registered) {
				if src == source {
					targets = append(targets, t)
					break
				}
			}
		}
	}

	rows, err := s.reader.QueryOverview(r.Context(), from, to, targets)
	if err != nil {
		s.writeQueryErr(w, "query overview", err)
		return
	}

	// Group reader rows by target ID so the collapse loop can iterate per
	// target — avoids an O(N*M) scan inside the configured-targets loop.
	bySource := make(map[string][]storage.OverviewSourceRow, len(rows))
	for _, row := range rows {
		id := row.Group + "/" + row.Name
		bySource[id] = append(bySource[id], row)
	}

	groupTitles := make(map[string]string, len(cfg.Targets)+1)
	for _, g := range cfg.Targets {
		groupTitles[g.Group] = g.Title
	}
	// The health group has no config entry to read a title from.
	groupTitles[slavehealth.Group] = slavehealth.GroupTitle

	staleThreshold := time.Duration(silentCycleMultiplier) * cfg.Interval

	out := make([]overviewRowDTO, 0, len(targets))
	for _, t := range targets {
		dto := overviewRowDTO{
			ID:         t.ID(),
			Group:      t.Group,
			GroupTitle: groupTitles[t.Group],
			Title:      t.Target.Title,
			ProbeType:  probeType(cfg, t.Target.Probe),
		}
		matches := bySource[t.ID()]
		if source != "" && len(matches) > 0 {
			filtered := make([]storage.OverviewSourceRow, 0, len(matches))
			for _, m := range matches {
				if m.Source == source {
					filtered = append(filtered, m)
				}
			}
			matches = filtered
		}
		if len(matches) == 0 {
			// No rows at all — target is silent. Empty sparkline, nil metrics.
			dto.Silent = true
			out = append(out, dto)
			continue
		}
		collapsed := collapseSources(matches)
		dto.LossAvg = &collapsed.LossAvg
		dto.LossMax = &collapsed.LossMax
		// Left nil when the selected source measured no latency at all: with
		// every bucket fully lost the three aggregates are 0, not absent, and
		// publishing that made a target at 100% loss render as the fleet's
		// fastest — which collapseSources selects deliberately, because it
		// takes the worst-loss row.
		if collapsed.HasRTT {
			dto.RTTMedian = &collapsed.RTTMedian
			dto.RTTP95 = &collapsed.RTTP95
			dto.RTTMax = &collapsed.RTTMax
		}
		dto.WorstSource = collapsed.WorstSource
		ls := collapsed.LastSeen
		dto.LastSeen = &ls
		dto.Sparkline = collapsed.Sparkline
		// Staleness: even with rows, if the most recent cycle is older than
		// 5×interval we flag silent. Catches "alive but stopped".
		if now.Sub(ls) > staleThreshold {
			dto.Silent = true
		}
		out = append(out, dto)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"window": windowStr,
		"source": source,
		"from":   from.Format(time.RFC3339),
		"to":     to.Format(time.RFC3339),
		"rows":   out,
	})
}

// knownSource reports whether name is the master's source label or a
// currently-registered slave. Fail-closed gate for the ?source= filter.
func knownSource(name, master string, registered []string) bool {
	if name == master {
		return true
	}
	for _, n := range registered {
		if n == name {
			return true
		}
	}
	return false
}

// probeType resolves a probe key to its Type. Empty if the key is unknown —
// matches listTargets' behaviour.
func probeType(cfg *config.Config, key string) string {
	// The synthetic health probe is injected at scheduler-build time and is
	// never in cfg.Probes, so resolve it from its definition instead.
	if key == slavehealth.ProbeName {
		return slavehealth.ProbeDef(0, false).Type
	}
	if p, ok := cfg.Probes[key]; ok {
		return p.Type
	}
	return ""
}

// collapsedRow holds the worst-source-per-target aggregates the JSON DTO
// renders. Kept separate from overviewRowDTO so the collapse logic can be
// tested without going through JSON.
type collapsedRow struct {
	LossAvg   float64
	LossMax   float64
	RTTMedian float64
	RTTP95    float64
	RTTMax    float64
	// HasRTT is the selected source's own flag: with every bucket fully lost
	// the three aggregates read 0, which is a latency nobody measured.
	HasRTT      bool
	WorstSource string
	LastSeen    time.Time
	Sparkline   []*float64
}

// collapseSources picks worst-source-per-target. "Worst" = highest LossAvg,
// tiebreak by highest RTTMax. The returned RTTMax is the max across all
// sources (not just the worst one) — a transient spike on a less-lossy
// source still matters. Sparklines are merged element-wise via max across
// sources for the same reason.
func collapseSources(rows []storage.OverviewSourceRow) collapsedRow {
	// Deterministic tiebreak in the comparator below relies on source order
	// being stable, so sort first. Sort key matches "worst comes first" so
	// the head element wins.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].LossAvg != rows[j].LossAvg {
			return rows[i].LossAvg > rows[j].LossAvg
		}
		if rows[i].RTTMax != rows[j].RTTMax {
			return rows[i].RTTMax > rows[j].RTTMax
		}
		return rows[i].Source < rows[j].Source
	})
	worst := rows[0]

	out := collapsedRow{
		LossAvg:     worst.LossAvg,
		LossMax:     worst.LossMax,
		RTTMedian:   worst.RTTMedian,
		RTTP95:      worst.RTTP95,
		HasRTT:      worst.HasRTT,
		WorstSource: worst.Source,
		LastSeen:    worst.LastSeen,
	}
	// RTTMax = max across all sources.
	out.RTTMax = worst.RTTMax
	for _, r := range rows[1:] {
		if r.RTTMax > out.RTTMax {
			out.RTTMax = r.RTTMax
		}
		if r.LastSeen.After(out.LastSeen) {
			out.LastSeen = r.LastSeen
		}
	}
	// Sparkline = element-wise max across sources. Picks the longest input
	// length as the output length; shorter sparklines pad with nil.
	maxLen := 0
	for _, r := range rows {
		if len(r.Sparkline) > maxLen {
			maxLen = len(r.Sparkline)
		}
	}
	if maxLen > 0 {
		out.Sparkline = make([]*float64, maxLen)
		for i := range out.Sparkline {
			var best *float64
			for _, r := range rows {
				if i >= len(r.Sparkline) || r.Sparkline[i] == nil {
					continue
				}
				if best == nil || *r.Sparkline[i] > *best {
					v := *r.Sparkline[i]
					best = &v
				}
			}
			out.Sparkline[i] = best
		}
	}
	return out
}
