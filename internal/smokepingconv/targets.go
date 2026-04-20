package smokepingconv

import (
	"fmt"
	"strings"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/smokepingconv/parser"
)

// mapTargets walks the SmokePing target tree and emits a flat []config.Group.
// inheritedProbe / inheritedAlerts propagate from ancestors (closest wins,
// matching SmokePing semantics). probeInfo maps SmokePing probe names to
// their gosmokeping-side info (key, type, port, urlformat).
func mapTargets(root *parser.SPRoot, probeInfo map[string]ProbeInfo, extNotes []Note) ([]config.Group, []Note) {
	notes := extNotes
	groupIdx := map[string]int{} // group path -> index into groupsOrdered
	var groupsOrdered []config.Group
	seenIDs := map[string]bool{}

	rootNode := root.Targets
	if rootNode == nil {
		return nil, notes
	}

	// A stack mirrors the ancestor chain: entries[0] is the virtual root.
	type frame struct {
		node   *parser.SPNode
		path   []string // slugged path to this node, excluding the node itself
		probe  string   // inherited probe reference
		alerts []string // inherited alert list
	}

	var walk func(f frame)
	walk = func(f frame) {
		n := f.node
		pathSelf := f.path
		if n.Name != "" {
			pathSelf = append(append([]string{}, f.path...), slug(n.Name))
		}

		probe := f.probe
		alerts := f.alerts
		if v, ok := n.Params["probe"]; ok {
			probe = v
		}
		if v, ok := n.Params["alerts"]; ok {
			alerts = splitCSV(v)
		}

		host := n.Params["host"]
		if host != "" {
			// This node is a leaf target.
			pi, ok := probeInfo[probe]
			if !ok {
				notes = append(notes, Note{
					Level: LevelSkip, Category: CatTarget,
					Detail: fmt.Sprintf("%s probe=%q unknown/unsupported — dropped", strings.Join(pathSelf, "/"), probe),
					Source: sourceOf(n.File, n.LineNo),
				})
				for _, c := range n.Children {
					walk(frame{node: c, path: pathSelf, probe: probe, alerts: alerts})
				}
				return
			}

			// Group path is pathSelf minus the last element (the leaf name
			// itself becomes the target Name). If that leaves no ancestors,
			// use "default".
			var group string
			var name string
			if len(pathSelf) == 0 {
				group = "default"
				name = slug(n.Name)
			} else {
				name = pathSelf[len(pathSelf)-1]
				if len(pathSelf) == 1 {
					group = "default"
				} else {
					group = strings.Join(pathSelf[:len(pathSelf)-1], "/")
				}
			}

			tgt := config.Target{Name: name, Probe: pi.Key}
			if v := n.Params["title"]; v != "" {
				tgt.Title = v
			} else if v := n.Params["menu"]; v != "" {
				tgt.Title = v
			}
			if len(alerts) > 0 {
				tgt.Alerts = alerts
			}
			switch pi.Type {
			case "http":
				urlfmt := pi.URLFormat
				if urlfmt == "" {
					urlfmt = "http://%host%/"
				}
				tgt.URL = expandURL(urlfmt, host)
			case "tcp":
				if pi.Port > 0 {
					tgt.Host = fmt.Sprintf("%s:%d", host, pi.Port)
				} else {
					tgt.Host = host
				}
			default:
				tgt.Host = host
			}

			id := group + "/" + name
			suffix := 2
			base := name
			for seenIDs[id] {
				name = fmt.Sprintf("%s-%d", base, suffix)
				id = group + "/" + name
				suffix++
			}
			if name != tgt.Name {
				notes = append(notes, Note{
					Level: LevelWarn, Category: CatTarget,
					Detail: fmt.Sprintf("name collision at %s/%s — renamed to %s", group, tgt.Name, name),
					Source: sourceOf(n.File, n.LineNo),
				})
				tgt.Name = name
			}
			seenIDs[id] = true

			idx, exists := groupIdx[group]
			if !exists {
				groupsOrdered = append(groupsOrdered, config.Group{Group: group, Title: groupTitleFrom(pathSelf, n)})
				idx = len(groupsOrdered) - 1
				groupIdx[group] = idx
			}
			groupsOrdered[idx].Targets = append(groupsOrdered[idx].Targets, tgt)
		}

		for _, c := range n.Children {
			walk(frame{node: c, path: pathSelf, probe: probe, alerts: alerts})
		}
	}

	walk(frame{node: rootNode, path: nil, probe: rootNode.Params["probe"], alerts: splitCSV(rootNode.Params["alerts"])})

	return groupsOrdered, notes
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// expandURL substitutes SmokePing's %host% / %hostname% placeholders.
func expandURL(format, host string) string {
	out := strings.ReplaceAll(format, "%host%", host)
	out = strings.ReplaceAll(out, "%hostname%", host)
	return out
}

// groupTitleFrom returns a human-facing title for a group. It's a best-effort
// guess: the leaf's parent ancestor name, capitalised if nothing else fits.
// We don't have access to intermediate node params here, so leave it empty
// unless the leaf itself carries a group-level title.
func groupTitleFrom(pathSelf []string, leaf *parser.SPNode) string {
	_ = leaf
	if len(pathSelf) < 2 {
		return ""
	}
	// Use the parent segment, e.g. "europe/germany" → "Germany".
	parent := pathSelf[len(pathSelf)-2]
	if parent == "" {
		return ""
	}
	return strings.ToUpper(parent[:1]) + parent[1:]
}
