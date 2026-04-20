package parser

import (
	"strings"
	"testing"
)

func TestBuild_TargetsAndProbes(t *testing.T) {
	src := `*** Database ***
step = 60
pings = 10

*** Probes ***
+ FPing
timeout = 3

+ Curl
urlformat = http://%host%/

*** Alerts ***
+ bigloss
type = loss
pattern = >50%,>50%,>50%

*** Targets ***
probe = FPing
menu = Top

+ europe
menu = Europe

++ berlin
host = berlin.example.com
menu = Berlin

++ paris
host = paris.example.com
`
	lines, err := Tokenize(strings.NewReader(src), "/tmp", "/tmp/x.conf")
	if err != nil {
		t.Fatal(err)
	}
	root, err := Build(lines)
	if err != nil {
		t.Fatal(err)
	}

	if root.Database["step"] != "60" || root.Database["pings"] != "10" {
		t.Errorf("database: %+v", root.Database)
	}
	if len(root.Probes) != 2 {
		t.Fatalf("probes: got %d want 2 (%+v)", len(root.Probes), root.Probes)
	}
	if root.Probes[0].Name != "FPing" || root.Probes[0].Type != "FPing" {
		t.Errorf("probe[0]: %+v", root.Probes[0])
	}
	if root.Probes[1].Params["urlformat"] != "http://%host%/" {
		t.Errorf("probe[1] params: %+v", root.Probes[1].Params)
	}
	if len(root.Alerts) != 1 || root.Alerts[0].Name != "bigloss" {
		t.Fatalf("alerts: %+v", root.Alerts)
	}
	if root.Targets == nil || len(root.Targets.Children) != 1 {
		t.Fatalf("targets root: %+v", root.Targets)
	}
	if root.Targets.Params["probe"] != "FPing" {
		t.Errorf("targets root probe: %v", root.Targets.Params)
	}
	europe := root.Targets.Children[0]
	if europe.Name != "europe" || len(europe.Children) != 2 {
		t.Fatalf("europe: %+v", europe)
	}
	if europe.Children[0].Name != "berlin" || europe.Children[0].Params["host"] != "berlin.example.com" {
		t.Errorf("berlin: %+v", europe.Children[0])
	}
}

func TestBuild_UnknownSection(t *testing.T) {
	src := `*** Presentation ***
template = /etc/smokeping/basepage.html
charset = utf-8
`
	lines, err := Tokenize(strings.NewReader(src), "/tmp", "/tmp/x.conf")
	if err != nil {
		t.Fatal(err)
	}
	root, err := Build(lines)
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Unknown) != 1 || root.Unknown[0].Section != "Presentation" {
		t.Fatalf("unknown: %+v", root.Unknown)
	}
	if len(root.Unknown[0].Lines) < 2 {
		t.Errorf("expected captured lines, got %+v", root.Unknown[0].Lines)
	}
}
