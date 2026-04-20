package parser

import "fmt"

// Build folds a []Line (as produced by Tokenize) into a typed IR. It does no
// value interpretation — parameters stay as raw strings for the mapper to
// handle. Returns an error only for structural problems (a node whose depth
// jumps by more than 1, an orphan Assign before any Node, etc.).
func Build(lines []Line) (*SPRoot, error) {
	root := &SPRoot{Database: map[string]string{}}
	targetRoot := &SPNode{Params: map[string]string{}}
	root.Targets = targetRoot

	// Per-section state.
	var (
		// Probe stack: probeStack[0] is the current top-level probe; deeper
		// entries are subprobes. Depth matches the LineNode Depth.
		probeStack []*SPProbe
		// Current alert pointer.
		currentAlert *SPAlert
		// Target node stack: targetStack[d] is the node at depth d. Index 0
		// is the virtual root (no name).
		targetStack = []*SPNode{targetRoot}
		// Most-recent node we were assigning into (for Assign lines before
		// any node at depth 0 in a Targets section — they land on the root).
		currentTarget = targetRoot
		// Unknown-section accumulator.
		unknown *SPUnknown
	)

	addProbeParam := func(k, v string) error {
		if len(probeStack) == 0 {
			return fmt.Errorf("probes: assign %q before any + probe", k)
		}
		p := probeStack[len(probeStack)-1]
		if p.Params == nil {
			p.Params = map[string]string{}
		}
		if _, ok := p.Params[k]; !ok {
			p.Keys = append(p.Keys, k)
		}
		p.Params[k] = v
		return nil
	}

	addNodeParam := func(n *SPNode, k, v string) {
		if n.Params == nil {
			n.Params = map[string]string{}
		}
		if _, ok := n.Params[k]; !ok {
			n.Keys = append(n.Keys, k)
		}
		n.Params[k] = v
	}

	for _, l := range lines {
		switch l.Kind {
		case LineBlank, LineComment:
			continue
		case LineSection:
			// Close any unknown-section accumulator.
			if unknown != nil {
				root.Unknown = append(root.Unknown, *unknown)
				unknown = nil
			}
			// Reset per-section state transitions we care about.
			probeStack = nil
			currentAlert = nil
			// Start capturing raw lines for unrecognised sections. On a
			// LineSection, the section name lives in l.Section (the tokenizer
			// sets Section = current section for every line, including the
			// section header itself).
			switch l.Section {
			case "General", "Database", "Probes", "Alerts", "Targets":
				// known
			default:
				unknown = &SPUnknown{Section: l.Section}
			}
			continue
		}

		// Inside an unknown section, capture raw assignments verbatim for notes.
		if unknown != nil {
			if l.Kind == LineAssign {
				unknown.Lines = append(unknown.Lines, fmt.Sprintf("%s = %s", l.Name, l.Value))
			}
			continue
		}

		switch l.Section {
		case "General":
			// Not retained — gosmokeping has no analogue. Tracked as skip-section
			// by default (mapper emits a single note).
			continue

		case "Database":
			if l.Kind == LineAssign {
				root.Database[l.Name] = l.Value
			}

		case "Probes":
			switch l.Kind {
			case LineNode:
				// Pop stack down to Depth-1, then push a new probe at this depth.
				for len(probeStack) >= l.Depth {
					probeStack = probeStack[:len(probeStack)-1]
				}
				p := SPProbe{Name: l.Name, File: l.File, LineNo: l.LineNo}
				if l.Depth == 1 {
					p.Type = l.Name
					root.Probes = append(root.Probes, p)
					probeStack = append(probeStack, &root.Probes[len(root.Probes)-1])
				} else {
					if len(probeStack) == 0 {
						return nil, fmt.Errorf("probes: subprobe %q at depth %d with no parent", l.Name, l.Depth)
					}
					parent := probeStack[len(probeStack)-1]
					p.Type = parent.Type
					parent.Subprobes = append(parent.Subprobes, p)
					probeStack = append(probeStack, &parent.Subprobes[len(parent.Subprobes)-1])
				}
			case LineAssign:
				if err := addProbeParam(l.Name, l.Value); err != nil {
					return nil, err
				}
			}

		case "Alerts":
			switch l.Kind {
			case LineNode:
				root.Alerts = append(root.Alerts, SPAlert{Name: l.Name, File: l.File, LineNo: l.LineNo, Params: map[string]string{}})
				currentAlert = &root.Alerts[len(root.Alerts)-1]
			case LineAssign:
				if currentAlert == nil {
					// Alerts-level common params (to=, from=) are ignored with a noop.
					continue
				}
				if _, ok := currentAlert.Params[l.Name]; !ok {
					currentAlert.Keys = append(currentAlert.Keys, l.Name)
				}
				currentAlert.Params[l.Name] = l.Value
			}

		case "Targets":
			switch l.Kind {
			case LineNode:
				if l.Depth > len(targetStack) {
					return nil, fmt.Errorf("%s:%d: target %q jumps depth (%d > %d)", l.File, l.LineNo, l.Name, l.Depth, len(targetStack))
				}
				// Pop back to the parent at l.Depth-1.
				targetStack = targetStack[:l.Depth]
				parent := targetStack[len(targetStack)-1]
				child := &SPNode{Name: l.Name, Params: map[string]string{}, File: l.File, LineNo: l.LineNo}
				parent.Children = append(parent.Children, child)
				targetStack = append(targetStack, child)
				currentTarget = child
			case LineAssign:
				addNodeParam(currentTarget, l.Name, l.Value)
			}
		}
	}

	if unknown != nil {
		root.Unknown = append(root.Unknown, *unknown)
	}
	return root, nil
}
