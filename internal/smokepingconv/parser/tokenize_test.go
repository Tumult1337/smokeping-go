package parser

import (
	"strings"
	"testing"
)

func TestTokenize_Minimal(t *testing.T) {
	src := `# comment line
*** General ***
owner = Alice

*** Targets ***
+ europe
menu = Europe

++ berlin
host = berlin.example.com
menu = Berlin
`
	lines, err := Tokenize(strings.NewReader(src), "/tmp", "/tmp/min.conf")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}

	// Just check structural kinds + section propagation.
	want := []struct {
		kind    LineKind
		section string
		name    string
		depth   int
		value   string
	}{
		{LineComment, "", "", 0, ""},
		{LineSection, "General", "", 0, ""},
		{LineAssign, "General", "owner", 0, "Alice"},
		{LineBlank, "General", "", 0, ""},
		{LineSection, "Targets", "", 0, ""},
		{LineNode, "Targets", "europe", 1, ""},
		{LineAssign, "Targets", "menu", 0, "Europe"},
		{LineBlank, "Targets", "", 0, ""},
		{LineNode, "Targets", "berlin", 2, ""},
		{LineAssign, "Targets", "host", 0, "berlin.example.com"},
		{LineAssign, "Targets", "menu", 0, "Berlin"},
	}
	if len(lines) != len(want) {
		t.Fatalf("line count: got %d want %d:\n%+v", len(lines), len(want), lines)
	}
	for i, w := range want {
		g := lines[i]
		if g.Kind != w.kind || g.Section != w.section || g.Name != w.name || g.Depth != w.depth || g.Value != w.value {
			t.Errorf("line %d: got %+v, want kind=%v section=%q name=%q depth=%d value=%q",
				i, g, w.kind, w.section, w.name, w.depth, w.value)
		}
	}
}

func TestTokenize_MultilineContinuation(t *testing.T) {
	src := `*** Probes ***
+ Curl
urlformat = http://%host%/\
            path?q=1
`
	lines, err := Tokenize(strings.NewReader(src), "/tmp", "/tmp/x.conf")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	var urlformat string
	for _, l := range lines {
		if l.Kind == LineAssign && l.Name == "urlformat" {
			urlformat = l.Value
		}
	}
	want := "http://%host%/path?q=1"
	if urlformat != want {
		t.Errorf("urlformat: got %q want %q", urlformat, want)
	}
}
