package smokepingconv

import (
	"fmt"
	"strings"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/smokepingconv/parser"
)

// mapAlerts translates *** Alerts *** entries. Only a handful of SmokePing
// pattern shapes map cleanly; anything else is skipped with a note that
// preserves the original pattern verbatim so a human can translate by hand.
func mapAlerts(root *parser.SPRoot) (map[string]config.Alert, []Note) {
	out := map[string]config.Alert{}
	var notes []Note

	for _, a := range root.Alerts {
		typ := a.Params["type"]
		pattern := a.Params["pattern"]
		src := sourceOf(a.File, a.LineNo)

		cond, sustained, ok := translateAlertPattern(typ, pattern)
		if !ok {
			notes = append(notes, Note{
				Level: LevelSkip, Category: CatAlert,
				Detail: fmt.Sprintf("%q pattern %q (type=%s) not expressible as gosmokeping condition — skipped", a.Name, pattern, typ),
				Source: src,
			})
			continue
		}
		out[slug(a.Name)] = config.Alert{
			Condition: cond,
			Sustained: sustained,
			Actions:   []string{"log"},
		}
		if _, ok := a.Params["to"]; ok {
			notes = append(notes, Note{
				Level: LevelWarn, Category: CatAlert,
				Detail: fmt.Sprintf("%q had email recipients — not wired (gosmokeping has no built-in email action)", a.Name),
				Source: src,
			})
		}
	}
	return out, notes
}

// translateAlertPattern returns (condition, sustained, ok). Recognises:
//   - loss, N repetitions of ">X%" → loss_pct > X, sustained N
//   - rtt,  N repetitions of ">X"  → rtt_median > Xms, sustained N
//   - loss, any mix of "==U" / "==100%" matchers (treated as the same) → loss_pct >= 100
func translateAlertPattern(typ, pattern string) (string, int, bool) {
	matchers := splitCSV(pattern)
	if len(matchers) == 0 {
		return "", 0, false
	}

	// All-unreachable form.
	if typ == "loss" {
		allU := true
		for _, m := range matchers {
			if m != "==U" && m != "==100%" {
				allU = false
				break
			}
		}
		if allU {
			return "loss_pct >= 100", len(matchers), true
		}
	}

	// Uniform ">X[%]" across all matchers.
	first := matchers[0]
	for _, m := range matchers {
		if m != first {
			return "", 0, false
		}
	}
	if !strings.HasPrefix(first, ">") {
		return "", 0, false
	}
	val := strings.TrimPrefix(first, ">")
	switch typ {
	case "loss":
		if !strings.HasSuffix(val, "%") {
			return "", 0, false
		}
		val = strings.TrimSuffix(val, "%")
		return fmt.Sprintf("loss_pct > %s", val), len(matchers), true
	case "rtt":
		return fmt.Sprintf("rtt_median > %sms", val), len(matchers), true
	default:
		return "", 0, false
	}
}
