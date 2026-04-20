package parser

import (
	"os"
	"path/filepath"
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

func TestTokenize_Include(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.conf")
	child := filepath.Join(dir, "child.conf")
	if err := os.WriteFile(parent, []byte("*** Targets ***\n@include child.conf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(child, []byte("+ berlin\nhost = x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines, err := Tokenize(f, dir, parent)
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	var sawNode, sawHost bool
	for _, l := range lines {
		if l.Kind == LineNode && l.Name == "berlin" {
			sawNode = true
			if !filepath.IsAbs(l.File) || !strings.HasSuffix(l.File, "child.conf") {
				t.Errorf("node File should point at child.conf abs path, got %q", l.File)
			}
		}
		if l.Kind == LineAssign && l.Name == "host" && l.Value == "x" {
			sawHost = true
		}
	}
	if !sawNode || !sawHost {
		t.Fatalf("did not see included content; lines=%+v", lines)
	}
}

func TestTokenize_IncludeCycle(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.conf")
	b := filepath.Join(dir, "b.conf")
	if err := os.WriteFile(a, []byte("@include b.conf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("@include a.conf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := Tokenize(f, dir, a); err == nil {
		t.Fatal("expected cycle error, got nil")
	}
}
