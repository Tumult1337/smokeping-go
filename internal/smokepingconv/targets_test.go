package smokepingconv

import (
	"strings"
	"testing"

	"github.com/tumult/gosmokeping/internal/smokepingconv/parser"
)

func TestMapTargets_FlattenNested(t *testing.T) {
	// Targets/europe/germany/berlin  (host set)
	// Targets/europe/germany/munich  (host set)
	// Targets/europe/france          (host set — target directly under depth-1 group)
	// Targets/solo                    (host set — depth-1 leaf → group="default")
	root := &parser.SPRoot{Targets: &parser.SPNode{
		Name: "", Params: map[string]string{"probe": "FPing"},
		Children: []*parser.SPNode{
			{Name: "europe", Params: map[string]string{"title": "Europe"}, Children: []*parser.SPNode{
				{Name: "germany", Params: map[string]string{"title": "Germany"}, Children: []*parser.SPNode{
					{Name: "berlin", Params: map[string]string{"host": "berlin.example.com", "title": "Berlin"}},
					{Name: "munich", Params: map[string]string{"host": "munich.example.com"}},
				}},
				{Name: "france", Params: map[string]string{"host": "france.example.com"}},
			}},
			{Name: "solo", Params: map[string]string{"host": "solo.example.com"}},
		},
	}}

	info := map[string]ProbeInfo{"FPing": {Key: "fping", Type: "icmp"}}
	groups, notes := mapTargets(root, info, nil)
	_ = notes

	// Expect three groups: europe/germany, europe, default
	byGroup := map[string]int{}
	for _, g := range groups {
		byGroup[g.Group] = len(g.Targets)
	}
	if byGroup["europe/germany"] != 2 {
		t.Errorf("europe/germany: got %d targets, want 2 (all: %+v)", byGroup["europe/germany"], byGroup)
	}
	if byGroup["europe"] != 1 {
		t.Errorf("europe: got %d, want 1", byGroup["europe"])
	}
	if byGroup["default"] != 1 {
		t.Errorf("default: got %d, want 1", byGroup["default"])
	}

	// Inherited probe resolved correctly.
	for _, g := range groups {
		for _, tgt := range g.Targets {
			if tgt.Probe != "fping" {
				t.Errorf("target %s/%s: probe=%q want fping", g.Group, tgt.Name, tgt.Probe)
			}
		}
	}
}

func TestMapTargets_HttpURLFormat(t *testing.T) {
	root := &parser.SPRoot{Targets: &parser.SPNode{
		Children: []*parser.SPNode{
			{Name: "web", Params: map[string]string{"probe": "Curl"}, Children: []*parser.SPNode{
				{Name: "example", Params: map[string]string{"host": "example.com"}},
			}},
		},
	}}
	info := map[string]ProbeInfo{"Curl": {Key: "curl", Type: "http", URLFormat: "https://%host%/"}}
	groups, _ := mapTargets(root, info, nil)
	if len(groups) != 1 || len(groups[0].Targets) != 1 {
		t.Fatalf("groups: %+v", groups)
	}
	tgt := groups[0].Targets[0]
	if tgt.URL != "https://example.com/" || tgt.Host != "" {
		t.Errorf("target: %+v", tgt)
	}
}

func TestMapTargets_SlavesAndNomasterpoll(t *testing.T) {
	root := &parser.SPRoot{Targets: &parser.SPNode{
		Params: map[string]string{"probe": "FPing"},
		Children: []*parser.SPNode{
			{Name: "edge", Params: map[string]string{"slaves": "a, b"}, Children: []*parser.SPNode{
				{Name: "host1", Params: map[string]string{"host": "h1.example.com", "nomasterpoll": "yes"}},
				{Name: "host2", Params: map[string]string{"host": "h2.example.com", "slaves": "c"}},
				{Name: "host3", Params: map[string]string{"host": "h3.example.com", "nomasterpoll": "yes"}},
			}},
		},
	}}
	info := map[string]ProbeInfo{"FPing": {Key: "fping", Type: "icmp"}}
	groups, notes := mapTargets(root, info, nil)
	if len(groups) != 1 || len(groups[0].Targets) != 3 {
		t.Fatalf("groups: %+v", groups)
	}
	h1 := groups[0].Targets[0]
	h2 := groups[0].Targets[1]
	if len(h1.Slaves) != 2 || h1.Slaves[0] != "a" || h1.Slaves[1] != "b" {
		t.Errorf("h1 slaves (inherited): %+v", h1.Slaves)
	}
	if len(h2.Slaves) != 1 || h2.Slaves[0] != "c" {
		t.Errorf("h2 slaves (child override): %+v", h2.Slaves)
	}
	var count int
	for _, n := range notes {
		if strings.Contains(n.Detail, "nomasterpoll") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 nomasterpoll note, got %d: %+v", count, notes)
	}
}

func TestMapTargets_TcpWithPort(t *testing.T) {
	root := &parser.SPRoot{Targets: &parser.SPNode{
		Children: []*parser.SPNode{
			{Name: "svc", Params: map[string]string{"probe": "TCPPing"}, Children: []*parser.SPNode{
				{Name: "api", Params: map[string]string{"host": "api.example.com"}},
			}},
		},
	}}
	info := map[string]ProbeInfo{"TCPPing": {Key: "tcpping", Type: "tcp", Port: 443}}
	groups, _ := mapTargets(root, info, nil)
	if groups[0].Targets[0].Host != "api.example.com:443" {
		t.Errorf("tcp host: %q", groups[0].Targets[0].Host)
	}
}
