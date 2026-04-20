package smokepingconv

import (
	"testing"

	"github.com/tumult/gosmokeping/internal/smokepingconv/parser"
)

func TestMapAlerts_LossUniform(t *testing.T) {
	root := &parser.SPRoot{Alerts: []parser.SPAlert{{
		Name: "bigloss",
		Params: map[string]string{
			"type":    "loss",
			"pattern": ">50%,>50%,>50%",
		},
		Keys: []string{"type", "pattern"},
	}}}
	alerts, notes := mapAlerts(root)
	a, ok := alerts["bigloss"]
	if !ok {
		t.Fatalf("no bigloss: %+v", alerts)
	}
	if a.Condition != "loss_pct > 50" || a.Sustained != 3 {
		t.Errorf("alert: %+v", a)
	}
	_ = notes
}

func TestMapAlerts_RTT(t *testing.T) {
	root := &parser.SPRoot{Alerts: []parser.SPAlert{{
		Name: "slow",
		Params: map[string]string{"type": "rtt", "pattern": ">100,>100"},
		Keys:   []string{"type", "pattern"},
	}}}
	alerts, _ := mapAlerts(root)
	a := alerts["slow"]
	if a.Condition != "rtt_median > 100ms" || a.Sustained != 2 {
		t.Errorf("alert: %+v", a)
	}
}

func TestMapAlerts_Unreachable(t *testing.T) {
	root := &parser.SPRoot{Alerts: []parser.SPAlert{{
		Name: "down",
		Params: map[string]string{"type": "loss", "pattern": "==U"},
		Keys:   []string{"type", "pattern"},
	}}}
	alerts, _ := mapAlerts(root)
	a := alerts["down"]
	if a.Condition != "loss_pct >= 100" || a.Sustained != 1 {
		t.Errorf("alert: %+v", a)
	}
}

func TestMapAlerts_ComplexPatternSkipped(t *testing.T) {
	root := &parser.SPRoot{Alerts: []parser.SPAlert{{
		Name: "weird",
		Params: map[string]string{"type": "rtt", "pattern": "<10,<10,<10,>100,>100"},
		Keys:   []string{"type", "pattern"},
	}}}
	alerts, notes := mapAlerts(root)
	if _, ok := alerts["weird"]; ok {
		t.Errorf("complex pattern should be skipped, got %+v", alerts)
	}
	var saw bool
	for _, n := range notes {
		if indexOf(n.Detail, "weird") >= 0 {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected note, got %+v", notes)
	}
}
