# smokeping2gosmokeping Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a standalone Go binary `smokeping2gosmokeping` that reads a SmokePing `Config::Grammar` config and emits an equivalent gosmokeping JSON config plus a sidecar notes file.

**Architecture:** Two-pass — parser (tokenize + build typed IR) then mapper (IR → `*config.Config`). Pure functions, no network or DB. CLI driver in `cmd/smokeping2gosmokeping/`. Parser + mapper in `internal/smokepingconv/`. Deterministic output (stable probe ordering, sorted maps). `-strict` mode gates CI on clean conversions.

**Tech Stack:** Go 1.26, `encoding/json`, `path/filepath`, `bufio`. No new module dependencies.

**Spec:** `docs/superpowers/specs/2026-04-21-smokeping2gosmokeping-design.md`

---

## Preflight

These notes apply to every task below:

- Module path is `github.com/tumult/gosmokeping`. All imports use that prefix.
- The repo has several unrelated uncommitted files (`M CLAUDE.md`, `M internal/probe/*`, etc.). **Never include them in commits.** Always `git add` explicit paths — never `git add .` or `git add -A`.
- Tests are plain `go test`. Goldens are `.json` / `.txt` files under `testdata/`; compare with string-equal after normalizing, or marshal the struct to JSON and compare strings.
- No new Go module dependencies — `go.mod` stays as-is. Use only stdlib.
- Run `go vet ./...` + `go test ./...` before every commit in this plan.

---

## File Structure

```
cmd/smokeping2gosmokeping/
    main.go                          # flag parse, IO, exit codes

internal/smokepingconv/
    notes.go                         # Note, Level, Category, Format (package smokepingconv)
    notes_test.go
    slug.go                          # slug() helper
    slug_test.go
    database.go                      # mapDatabase(root) → (interval, pings, notes)
    database_test.go
    probes.go                        # mapProbes(root) → (probes, info, notes)
    probes_test.go
    targets.go                       # mapTargets(root, info) → ([]Group, notes)
    targets_test.go
    alerts.go                        # mapAlerts(root) → (alerts, notes)
    alerts_test.go
    emit.go                          # Marshal(*config.Config) → []byte with fixed probe order
    emit_test.go
    convert.go                       # Convert(io.Reader, baseDir, path) → (*config.Config, []Note, error)
    convert_test.go                  # end-to-end golden tests

    parser/
        lines.go                     # Line struct, LineKind
        tokenize.go                  # Tokenize(io.Reader, baseDir, path) → []Line, with @include expansion
        tokenize_test.go
        ir.go                        # SPRoot, SPProbe, SPAlert, SPNode, SPUnknown
        build.go                     # Build([]Line) → *SPRoot
        build_test.go

    testdata/
        parser/
            minimal.conf
            minimal.ir.json
            nested.conf
            nested.ir.json
            include.conf
            include_child.conf
            include.ir.json
            multiline.conf
            multiline.ir.json
            cycle_a.conf
            cycle_b.conf
        e2e/
            minimal.conf
            minimal.want.json
            minimal.want.notes.txt
            nested.conf
            nested.want.json
            nested.want.notes.txt
            mixed_probes.conf
            mixed_probes.want.json
            mixed_probes.want.notes.txt
            unsupported.conf
            unsupported.want.json
            unsupported.want.notes.txt

.github/workflows/build.yml          # add converter build + artifact + release asset
Makefile                             # add smokeping2gosmokeping target
README.md                            # add "Migrating from SmokePing" section
```

---

## Task 1: Scaffold the new binary

Create the directory and an empty `main.go` that compiles. This gets the package path wired before we add flags or behavior.

**Files:**
- Create: `cmd/smokeping2gosmokeping/main.go`
- Modify: none yet

- [ ] **Step 1: Create the binary entry point**

Create `cmd/smokeping2gosmokeping/main.go`:

```go
package main

import "fmt"

func main() {
	fmt.Println("smokeping2gosmokeping: not implemented yet")
}
```

- [ ] **Step 2: Verify it builds**

Run: `go build -o /tmp/smokeping2gosmokeping ./cmd/smokeping2gosmokeping && /tmp/smokeping2gosmokeping`
Expected: prints `smokeping2gosmokeping: not implemented yet` and exits 0.

- [ ] **Step 3: Commit**

```bash
git add cmd/smokeping2gosmokeping/main.go
git commit -m "feat(smokeping2gosmokeping): scaffold binary entry point"
```

---

## Task 2: Parser — Line types

Define the line-level types the tokenizer will produce. Small file, no logic, but it's the contract between the two parser stages.

**Files:**
- Create: `internal/smokepingconv/parser/lines.go`

- [ ] **Step 1: Create lines.go**

```go
package parser

// LineKind categorises a single physical line in a SmokePing config.
type LineKind int

const (
	LineBlank LineKind = iota
	LineComment
	LineSection  // *** Name ***
	LineNode     // +name / ++name / +++name ...
	LineAssign   // key = value (with multi-line continuations joined)
)

// Line is a post-tokenize, pre-build record. The builder walks a []Line to
// construct the IR. Section carries the last-seen *** Section *** name so
// consumers don't need to track state.
type Line struct {
	Kind    LineKind
	Depth   int    // Node only: 1, 2, 3 ...
	Name    string // Node name or Assign key
	Value   string // Assign value (joined)
	Section string // last-seen section header, "" before the first one
	File    string // absolute path of the source file
	LineNo  int    // 1-based within File
}
```

- [ ] **Step 2: Verify it builds**

Run: `go build ./internal/smokepingconv/parser/...`
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
git add internal/smokepingconv/parser/lines.go
git commit -m "feat(smokepingconv): define parser Line types"
```

---

## Task 3: Parser — Tokenizer (no includes yet)

Tokenize a single file (no `@include` expansion) into `[]Line`. Covers: section headers, node depth markers, `key = value` assignments, multi-line continuations, comments, blanks.

**Files:**
- Create: `internal/smokepingconv/parser/tokenize.go`
- Create: `internal/smokepingconv/parser/tokenize_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/smokepingconv/parser/tokenize_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/smokepingconv/parser/ -run TestTokenize -v`
Expected: FAIL (undefined: `Tokenize`).

- [ ] **Step 3: Implement Tokenize (single file, no @include)**

Create `internal/smokepingconv/parser/tokenize.go`:

```go
package parser

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	reSection = regexp.MustCompile(`^\*\*\*\s+(.+?)\s+\*\*\*\s*$`)
	reNode    = regexp.MustCompile(`^(\++)\s*(\S+.*?)\s*$`)
	reAssign  = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*?)\s*$`)
)

// Tokenize reads a SmokePing config file into []Line. It does NOT expand
// @include — that's the caller's job (see ExpandIncludes). This split keeps
// single-file tokenization pure and testable.
//
// path should be the absolute path of the source file (used for Line.File);
// baseDir is currently unused here but kept in the signature so the API
// matches the include-aware variant that wraps this one.
func Tokenize(r io.Reader, baseDir, path string) ([]Line, error) {
	_ = baseDir
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("tokenize: resolve path %q: %w", path, err)
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		out         []Line
		section     string
		pending     *Line // accumulating a multi-line Assign
		lineNo      int
		pendingLine int
	)

	flush := func() {
		if pending != nil {
			pending.Value = strings.TrimSpace(pending.Value)
			out = append(out, *pending)
			pending = nil
		}
	}

	for scanner.Scan() {
		lineNo++
		raw := scanner.Text()

		// Continuation: previous line ended with `\`.
		if pending != nil {
			pending.Value += strings.TrimSpace(raw)
			if !strings.HasSuffix(pending.Value, "\\") {
				flush()
				continue
			}
			pending.Value = strings.TrimSuffix(pending.Value, "\\")
			continue
		}

		trimmed := strings.TrimSpace(raw)

		if trimmed == "" {
			flush()
			out = append(out, Line{Kind: LineBlank, Section: section, File: abs, LineNo: lineNo})
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			flush()
			out = append(out, Line{Kind: LineComment, Section: section, File: abs, LineNo: lineNo})
			continue
		}
		if m := reSection.FindStringSubmatch(trimmed); m != nil {
			flush()
			section = m[1]
			out = append(out, Line{Kind: LineSection, Section: section, File: abs, LineNo: lineNo})
			continue
		}
		if m := reNode.FindStringSubmatch(trimmed); m != nil {
			flush()
			out = append(out, Line{
				Kind: LineNode, Depth: len(m[1]), Name: m[2],
				Section: section, File: abs, LineNo: lineNo,
			})
			continue
		}
		if m := reAssign.FindStringSubmatch(trimmed); m != nil {
			val := m[2]
			l := Line{Kind: LineAssign, Name: m[1], Value: val, Section: section, File: abs, LineNo: lineNo}
			if strings.HasSuffix(val, "\\") {
				l.Value = strings.TrimSuffix(val, "\\")
				pending = &l
				pendingLine = lineNo
				continue
			}
			out = append(out, l)
			continue
		}
		flush()
		return nil, fmt.Errorf("%s:%d: unrecognised line %q", abs, lineNo, trimmed)
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s:%d: scan: %w", abs, pendingLine, err)
	}
	return out, nil
}
```

- [ ] **Step 4: Run tests and verify they pass**

Run: `go test ./internal/smokepingconv/parser/ -run TestTokenize -v`
Expected: PASS for both subtests.

- [ ] **Step 5: Commit**

```bash
git add internal/smokepingconv/parser/tokenize.go internal/smokepingconv/parser/tokenize_test.go
git commit -m "feat(smokepingconv): tokenize single-file SmokePing config"
```

---

## Task 4: Parser — @include expansion

Wrap `Tokenize` with include-resolution: when an `@include <path>` line appears, tokenize that file inline (relative to the including file's dir). Detect cycles.

**Files:**
- Modify: `internal/smokepingconv/parser/tokenize.go`
- Modify: `internal/smokepingconv/parser/tokenize_test.go`

- [ ] **Step 1: Write failing tests**

Append to `internal/smokepingconv/parser/tokenize_test.go`:

```go
import (
	"os"
	"path/filepath"
)

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
```

- [ ] **Step 2: Run the tests — they should fail**

Run: `go test ./internal/smokepingconv/parser/ -run 'TestTokenize_Include' -v`
Expected: FAIL — include lines currently produce "unrecognised line" errors.

- [ ] **Step 3: Add include handling**

At the top of `tokenize.go`, add:

```go
var reInclude = regexp.MustCompile(`^@include\s+(.+?)\s*$`)
```

Refactor `Tokenize` into a public function that sets up a fresh visited-set and calls the recursive helper. Replace the function body with:

```go
func Tokenize(r io.Reader, baseDir, path string) ([]Line, error) {
	return tokenizeRec(r, baseDir, path, map[string]bool{})
}

func tokenizeRec(r io.Reader, baseDir, path string, visited map[string]bool) ([]Line, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("tokenize: resolve path %q: %w", path, err)
	}
	if visited[abs] {
		return nil, fmt.Errorf("tokenize: include cycle at %s", abs)
	}
	visited[abs] = true
	defer delete(visited, abs)

	// ... existing scanner setup and loop ...
}
```

Then inside the scan loop, before the "unrecognised line" fallback, add:

```go
if m := reInclude.FindStringSubmatch(trimmed); m != nil {
	flush()
	ipath := m[1]
	if !filepath.IsAbs(ipath) {
		ipath = filepath.Join(filepath.Dir(abs), ipath)
	}
	f, ierr := os.Open(ipath)
	if ierr != nil {
		return nil, fmt.Errorf("%s:%d: @include %s: %w", abs, lineNo, ipath, ierr)
	}
	sub, serr := tokenizeRec(f, filepath.Dir(ipath), ipath, visited)
	f.Close()
	if serr != nil {
		return nil, serr
	}
	// Sub-file has its own section state; inherit nothing — the parent
	// section label resumes naturally on subsequent parent lines.
	out = append(out, sub...)
	continue
}
```

Add `"os"` to the import list.

- [ ] **Step 4: Run all parser tests**

Run: `go test ./internal/smokepingconv/parser/ -v`
Expected: PASS for all tests.

- [ ] **Step 5: Commit**

```bash
git add internal/smokepingconv/parser/tokenize.go internal/smokepingconv/parser/tokenize_test.go
git commit -m "feat(smokepingconv): expand @include directives with cycle detection"
```

---

## Task 5: Parser — IR types

Define the typed IR that the builder will emit.

**Files:**
- Create: `internal/smokepingconv/parser/ir.go`

- [ ] **Step 1: Create ir.go**

```go
package parser

// SPRoot is the root of the parsed IR. Ordered slices are used instead of maps
// everywhere ordering matters for deterministic output.
type SPRoot struct {
	Database map[string]string `json:"database,omitempty"`
	Probes   []SPProbe         `json:"probes,omitempty"`
	Alerts   []SPAlert         `json:"alerts,omitempty"`
	Targets  *SPNode           `json:"targets,omitempty"`
	Unknown  []SPUnknown       `json:"unknown,omitempty"`
}

type SPProbe struct {
	Name      string            `json:"name"`
	Type      string            `json:"type"` // inherited from parent for subprobes
	Params    map[string]string `json:"params,omitempty"`
	Keys      []string          `json:"keys,omitempty"` // param insertion order
	Subprobes []SPProbe         `json:"subprobes,omitempty"`
	File      string            `json:"file,omitempty"`
	LineNo    int               `json:"line_no,omitempty"`
}

type SPAlert struct {
	Name   string            `json:"name"`
	Params map[string]string `json:"params,omitempty"`
	Keys   []string          `json:"keys,omitempty"`
	File   string            `json:"file,omitempty"`
	LineNo int               `json:"line_no,omitempty"`
}

type SPNode struct {
	Name     string            `json:"name,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
	Keys     []string          `json:"keys,omitempty"`
	Children []*SPNode         `json:"children,omitempty"`
	File     string            `json:"file,omitempty"`
	LineNo   int               `json:"line_no,omitempty"`
}

type SPUnknown struct {
	Section string   `json:"section"`
	Lines   []string `json:"lines,omitempty"`
}
```

- [ ] **Step 2: Verify it builds**

Run: `go build ./internal/smokepingconv/parser/...`
Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add internal/smokepingconv/parser/ir.go
git commit -m "feat(smokepingconv): define parser IR types"
```

---

## Task 6: Parser — IR builder

Walk `[]Line` → `*SPRoot`. Handles section routing, depth tree construction for Targets + Probes.

**Files:**
- Create: `internal/smokepingconv/parser/build.go`
- Create: `internal/smokepingconv/parser/build_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/smokepingconv/parser/build_test.go`:

```go
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
```

- [ ] **Step 2: Run test — should fail**

Run: `go test ./internal/smokepingconv/parser/ -run TestBuild -v`
Expected: FAIL (undefined `Build`).

- [ ] **Step 3: Implement Build**

Create `internal/smokepingconv/parser/build.go`:

```go
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
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/smokepingconv/parser/ -v`
Expected: PASS for all builder + tokenizer tests.

- [ ] **Step 5: Commit**

```bash
git add internal/smokepingconv/parser/build.go internal/smokepingconv/parser/build_test.go
git commit -m "feat(smokepingconv): build IR from tokenized SmokePing config"
```

---

## Task 7: Notes type

Simple value type used by every mapper stage.

**Files:**
- Create: `internal/smokepingconv/notes.go`
- Create: `internal/smokepingconv/notes_test.go`

- [ ] **Step 1: Write failing test**

```go
package smokepingconv

import "testing"

func TestNoteFormat(t *testing.T) {
	n := Note{Level: LevelWarn, Category: CatAlert, Detail: "pattern simplified", Source: "/etc/smokeping.conf:14"}
	got := n.Format()
	want := "warn alert: pattern simplified  (source: /etc/smokeping.conf:14)"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestNoteFormatNoSource(t *testing.T) {
	n := Note{Level: LevelSkip, Category: CatSection, Detail: "*** Presentation *** ignored"}
	got := n.Format()
	want := "skip section: *** Presentation *** ignored"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
```

- [ ] **Step 2: Run — should fail**

Run: `go test ./internal/smokepingconv/ -run TestNote -v`
Expected: FAIL (package doesn't exist yet).

- [ ] **Step 3: Implement**

Create `internal/smokepingconv/notes.go`:

```go
package smokepingconv

import "fmt"

type Level string

const (
	LevelWarn Level = "warn"
	LevelSkip Level = "skip"
)

type Category string

const (
	CatProbe   Category = "probe"
	CatTarget  Category = "target"
	CatAlert   Category = "alert"
	CatGeneral Category = "general"
	CatInclude Category = "include"
	CatSection Category = "section"
)

type Note struct {
	Level    Level
	Category Category
	Detail   string
	Source   string // "<file>:<line>"; empty if unknown
}

func (n Note) Format() string {
	if n.Source == "" {
		return fmt.Sprintf("%s %s: %s", n.Level, n.Category, n.Detail)
	}
	return fmt.Sprintf("%s %s: %s  (source: %s)", n.Level, n.Category, n.Detail, n.Source)
}

// HasPartial reports whether any note indicates a loss-of-fidelity translation
// (warn) or dropped construct (skip). Used by -strict mode.
func HasPartial(notes []Note) bool {
	return len(notes) > 0
}
```

- [ ] **Step 4: Run — should pass**

Run: `go test ./internal/smokepingconv/ -run TestNote -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/smokepingconv/notes.go internal/smokepingconv/notes_test.go
git commit -m "feat(smokepingconv): add Note value type"
```

---

## Task 8: Mapper — slug helper

Small utility used by probes and targets.

**Files:**
- Create: `internal/smokepingconv/slug.go`
- Create: `internal/smokepingconv/slug_test.go`

- [ ] **Step 1: Write failing test**

```go
package smokepingconv

import "testing"

func TestSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"FPing", "fping"},
		{"FPing6", "fping6"},
		{"Europe", "europe"},
		{"Europe & Asia", "europe-asia"},
		{"North_America", "north_america"},
		{"A  B", "a-b"},
		{"", ""},
	}
	for _, c := range cases {
		got := slug(c.in)
		if got != c.want {
			t.Errorf("slug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run — should fail**

Run: `go test ./internal/smokepingconv/ -run TestSlug -v`
Expected: FAIL.

- [ ] **Step 3: Implement slug**

Create `internal/smokepingconv/slug.go`:

```go
package smokepingconv

import "strings"

// slug lowercases s and replaces any run of characters outside [a-z0-9_] with
// a single '-'. Underscores are preserved (SmokePing target names commonly
// use them). Trailing/leading dashes are trimmed.
func slug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := b.String()
	return strings.Trim(out, "-")
}
```

- [ ] **Step 4: Run — should pass**

Run: `go test ./internal/smokepingconv/ -run TestSlug -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/smokepingconv/slug.go internal/smokepingconv/slug_test.go
git commit -m "feat(smokepingconv): add slug helper"
```

---

## Task 9: Mapper — Database section

**Files:**
- Create: `internal/smokepingconv/database.go`
- Create: `internal/smokepingconv/database_test.go`

- [ ] **Step 1: Write failing test**

```go
package smokepingconv

import (
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/smokepingconv/parser"
)

func TestMapDatabase(t *testing.T) {
	root := &parser.SPRoot{
		Database: map[string]string{"step": "60", "pings": "10"},
	}
	interval, pings, notes := mapDatabase(root)
	if interval != 60*time.Second {
		t.Errorf("interval: got %v want 60s", interval)
	}
	if pings != 10 {
		t.Errorf("pings: got %d want 10", pings)
	}
	if len(notes) != 0 {
		t.Errorf("notes: %+v", notes)
	}
}

func TestMapDatabase_Defaults(t *testing.T) {
	root := &parser.SPRoot{Database: map[string]string{}}
	interval, pings, _ := mapDatabase(root)
	if interval != 5*time.Minute || pings != 20 {
		t.Errorf("got %v/%d, want 5m/20", interval, pings)
	}
}

func TestMapDatabase_UnknownKeyNoted(t *testing.T) {
	root := &parser.SPRoot{
		Database: map[string]string{"step": "60", "pings": "10", "pings_in_graph": "100"},
	}
	_, _, notes := mapDatabase(root)
	var saw bool
	for _, n := range notes {
		if n.Level == LevelWarn && n.Category == CatGeneral && contains(n.Detail, "pings_in_graph") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected warn about pings_in_graph, got %+v", notes)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run — should fail**

Run: `go test ./internal/smokepingconv/ -run TestMapDatabase -v`
Expected: FAIL (undefined `mapDatabase`).

- [ ] **Step 3: Implement**

Create `internal/smokepingconv/database.go`:

```go
package smokepingconv

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/tumult/gosmokeping/internal/smokepingconv/parser"
)

const (
	defaultInterval = 5 * time.Minute
	defaultPings    = 20
)

// mapDatabase translates the *** Database *** section into interval + pings.
// Unknown keys produce warn notes so the operator knows they were seen and
// deliberately dropped.
func mapDatabase(root *parser.SPRoot) (time.Duration, int, []Note) {
	var notes []Note
	interval := defaultInterval
	pings := defaultPings

	if v, ok := root.Database["step"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			notes = append(notes, Note{
				Level: LevelWarn, Category: CatGeneral,
				Detail: fmt.Sprintf("database.step %q invalid — keeping default %s", v, defaultInterval),
			})
		} else {
			interval = time.Duration(n) * time.Second
		}
	}
	if v, ok := root.Database["pings"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			notes = append(notes, Note{
				Level: LevelWarn, Category: CatGeneral,
				Detail: fmt.Sprintf("database.pings %q invalid — keeping default %d", v, defaultPings),
			})
		} else {
			pings = n
		}
	}

	var extras []string
	for k := range root.Database {
		if k == "step" || k == "pings" {
			continue
		}
		extras = append(extras, k)
	}
	sort.Strings(extras)
	for _, k := range extras {
		notes = append(notes, Note{
			Level: LevelWarn, Category: CatGeneral,
			Detail: fmt.Sprintf("database.%s ignored (no gosmokeping analogue)", k),
		})
	}

	return interval, pings, notes
}
```

- [ ] **Step 4: Run — should pass**

Run: `go test ./internal/smokepingconv/ -run TestMapDatabase -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/smokepingconv/database.go internal/smokepingconv/database_test.go
git commit -m "feat(smokepingconv): map Database section"
```

---

## Task 10: Mapper — Probes section

Translate `*** Probes ***` entries to gosmokeping probe defs. Also returns a `nameMap` (SmokePing probe name → gosmokeping key + type) used by the targets mapper to resolve target references.

**Files:**
- Create: `internal/smokepingconv/probes.go`
- Create: `internal/smokepingconv/probes_test.go`

- [ ] **Step 1: Write failing test**

```go
package smokepingconv

import (
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/smokepingconv/parser"
)

func TestMapProbes_FPingAndCurl(t *testing.T) {
	root := &parser.SPRoot{Probes: []parser.SPProbe{
		{Name: "FPing", Type: "FPing", Params: map[string]string{"timeout": "3"}, Keys: []string{"timeout"}},
		{Name: "Curl", Type: "Curl", Params: map[string]string{"urlformat": "http://%host%/", "insecure_ssl": "yes"}, Keys: []string{"urlformat", "insecure_ssl"}},
	}}

	probes, info, notes := mapProbes(root)

	if p, ok := probes["fping"]; !ok || p.Type != "icmp" || p.Timeout != 3*time.Second {
		t.Errorf("fping: %+v", p)
	}
	if p, ok := probes["curl"]; !ok || p.Type != "http" || !p.Insecure {
		t.Errorf("curl: %+v", p)
	}
	if info["FPing"].Key != "fping" || info["FPing"].Type != "icmp" {
		t.Errorf("info FPing: %+v", info["FPing"])
	}
	if info["Curl"].URLFormat != "http://%host%/" {
		t.Errorf("Curl URLFormat: %q", info["Curl"].URLFormat)
	}
	_ = notes
}

func TestMapProbes_UnsupportedSkipped(t *testing.T) {
	root := &parser.SPRoot{Probes: []parser.SPProbe{
		{Name: "SSH", Type: "AnotherSSH", File: "x.conf", LineNo: 12},
	}}
	probes, info, notes := mapProbes(root)
	if _, ok := probes["ssh"]; ok {
		t.Error("SSH should be skipped")
	}
	if _, ok := info["SSH"]; ok {
		t.Error("info should omit skipped probe")
	}
	var seen bool
	for _, n := range notes {
		if indexOf(n.Detail, "AnotherSSH") >= 0 {
			seen = true
		}
	}
	if !seen {
		t.Errorf("expected skip note for AnotherSSH, got %+v", notes)
	}
}

func TestMapProbes_Subprobes(t *testing.T) {
	root := &parser.SPRoot{Probes: []parser.SPProbe{
		{Name: "FPing", Type: "FPing", Subprobes: []parser.SPProbe{
			{Name: "FPingHighPings", Type: "FPing", Params: map[string]string{"pings": "20"}, Keys: []string{"pings"}},
		}},
	}}
	probes, info, _ := mapProbes(root)
	if _, ok := probes["fping"]; !ok {
		t.Error("expected fping")
	}
	if _, ok := probes["fpinghighpings"]; !ok {
		t.Error("expected fpinghighpings")
	}
	if info["FPingHighPings"].Key != "fpinghighpings" {
		t.Errorf("subprobe info: %+v", info)
	}
}
```

- [ ] **Step 2: Run — should fail**

Run: `go test ./internal/smokepingconv/ -run TestMapProbes -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Create `internal/smokepingconv/probes.go`:

```go
package smokepingconv

import (
	"fmt"
	"strconv"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/smokepingconv/parser"
)

// ProbeInfo records the gosmokeping-side details for a SmokePing probe name
// so the targets mapper can look up what to emit for a target referencing
// that probe. URLFormat is populated only for http-type probes.
type ProbeInfo struct {
	Key       string // slugged probe name used as the gosmokeping map key
	Type      string // gosmokeping probe type: icmp/tcp/http/dns
	Port      int    // tcp port parsed from probe params; 0 if not set
	URLFormat string // http urlformat template (with %host% / %hostname%)
}

var probeTypeMap = map[string]string{
	"FPing":         "icmp",
	"FPing6":        "icmp",
	"DNS":           "dns",
	"TCPPing":       "tcp",
	"Curl":          "http",
	"EchoPingHttp":  "http",
	"EchoPingHttps": "http",
}

// mapProbes translates the *** Probes *** tree. Returns:
//   - probes: the gosmokeping probes map, keyed by slugged probe name
//   - info:   indexed by the original SmokePing probe name, used by targets
//   - notes:  warn/skip records
func mapProbes(root *parser.SPRoot) (map[string]config.Probe, map[string]ProbeInfo, []Note) {
	probes := map[string]config.Probe{}
	info := map[string]ProbeInfo{}
	var notes []Note

	var visit func(p parser.SPProbe, parentType string)
	visit = func(p parser.SPProbe, parentType string) {
		spType := p.Type
		if spType == "" {
			spType = parentType
		}
		gosmokeType, supported := probeTypeMap[spType]
		if !supported {
			notes = append(notes, Note{
				Level: LevelSkip, Category: CatProbe,
				Detail: fmt.Sprintf("%q (type %s) — no gosmokeping equivalent", p.Name, spType),
				Source: sourceOf(p.File, p.LineNo),
			})
			return
		}

		key := uniqueKey(probes, slug(p.Name))
		pi := ProbeInfo{Key: key, Type: gosmokeType}

		cp := config.Probe{Type: gosmokeType, Timeout: 5 * time.Second}
		if v, ok := p.Params["timeout"]; ok {
			if d, err := parseProbeDuration(v); err == nil {
				cp.Timeout = d
			} else {
				notes = append(notes, Note{
					Level: LevelWarn, Category: CatProbe,
					Detail: fmt.Sprintf("%q timeout %q unparseable, using 5s", p.Name, v),
					Source: sourceOf(p.File, p.LineNo),
				})
			}
		}
		if v, ok := p.Params["insecure_ssl"]; ok && (v == "yes" || v == "true" || v == "1") {
			cp.Insecure = true
		}
		if v, ok := p.Params["port"]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				pi.Port = n
			}
		}
		if v, ok := p.Params["urlformat"]; ok {
			pi.URLFormat = v
		}

		for _, k := range p.Keys {
			switch k {
			case "timeout", "insecure_ssl", "port", "urlformat":
				continue
			}
			notes = append(notes, Note{
				Level: LevelWarn, Category: CatProbe,
				Detail: fmt.Sprintf("%q param %q ignored (no gosmokeping analogue)", p.Name, k),
				Source: sourceOf(p.File, p.LineNo),
			})
		}

		probes[key] = cp
		info[p.Name] = pi

		for _, sub := range p.Subprobes {
			visit(sub, spType)
		}
	}

	for _, p := range root.Probes {
		visit(p, "")
	}
	return probes, info, notes
}

// parseProbeDuration accepts either a bare integer (SmokePing's seconds) or a
// Go-style duration string ("500ms", "3s").
func parseProbeDuration(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}

func sourceOf(file string, line int) string {
	if file == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", file, line)
}

// uniqueKey returns base when unused; otherwise base-2, base-3, etc.
func uniqueKey(m map[string]config.Probe, base string) string {
	if _, ok := m[base]; !ok {
		return base
	}
	for i := 2; ; i++ {
		k := fmt.Sprintf("%s-%d", base, i)
		if _, ok := m[k]; !ok {
			return k
		}
	}
}
```

- [ ] **Step 4: Run — should pass**

Run: `go test ./internal/smokepingconv/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/smokepingconv/probes.go internal/smokepingconv/probes_test.go
git commit -m "feat(smokepingconv): map Probes section"
```

---

## Task 11: Mapper — Targets section

Flatten the node tree to gosmokeping groups.

**Files:**
- Create: `internal/smokepingconv/targets.go`
- Create: `internal/smokepingconv/targets_test.go`

- [ ] **Step 1: Write the failing test**

```go
package smokepingconv

import (
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
```

- [ ] **Step 2: Run — should fail**

Run: `go test ./internal/smokepingconv/ -run TestMapTargets -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Create `internal/smokepingconv/targets.go`:

```go
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
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/smokepingconv/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/smokepingconv/targets.go internal/smokepingconv/targets_test.go
git commit -m "feat(smokepingconv): flatten target tree into gosmokeping groups"
```

---

## Task 12: Mapper — Alerts section

**Files:**
- Create: `internal/smokepingconv/alerts.go`
- Create: `internal/smokepingconv/alerts_test.go`

- [ ] **Step 1: Write failing test**

```go
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
```

- [ ] **Step 2: Run — should fail**

Run: `go test ./internal/smokepingconv/ -run TestMapAlerts -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Create `internal/smokepingconv/alerts.go`:

```go
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
```

- [ ] **Step 4: Run — should pass**

Run: `go test ./internal/smokepingconv/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/smokepingconv/alerts.go internal/smokepingconv/alerts_test.go
git commit -m "feat(smokepingconv): map common Alerts patterns to gosmokeping conditions"
```

---

## Task 13: Mapper — deterministic emit shim

Custom JSON marshaller that emits `probes` in a fixed order (`icmp`, `tcp`, `http`, `dns`, then sorted) and wraps everything else through the default encoder.

**Files:**
- Create: `internal/smokepingconv/emit.go`
- Create: `internal/smokepingconv/emit_test.go`

- [ ] **Step 1: Write failing test**

```go
package smokepingconv

import (
	"strings"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

func TestEmit_ProbeOrder(t *testing.T) {
	cfg := &config.Config{
		Listen:   ":8080",
		Interval: 30 * time.Second,
		Pings:    10,
		Probes: map[string]config.Probe{
			"dns":  {Type: "dns", Timeout: 5 * time.Second},
			"icmp": {Type: "icmp", Timeout: 5 * time.Second},
			"http": {Type: "http", Timeout: 5 * time.Second},
			"tcp":  {Type: "tcp", Timeout: 5 * time.Second},
			"curl": {Type: "http", Timeout: 5 * time.Second},
		},
		Actions: map[string]config.Action{"log": {Type: "log"}},
	}
	b, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	iIcmp := strings.Index(s, `"icmp"`)
	iTcp := strings.Index(s, `"tcp"`)
	iHttp := strings.Index(s, `"http"`)
	iDns := strings.Index(s, `"dns"`)
	iCurl := strings.Index(s, `"curl"`)
	if !(iIcmp < iTcp && iTcp < iHttp && iHttp < iDns && iDns < iCurl) {
		t.Errorf("probe order wrong, got:\n%s", s)
	}
	if !strings.HasSuffix(s, "\n") {
		t.Error("missing trailing newline")
	}
}
```

- [ ] **Step 2: Run — should fail**

Run: `go test ./internal/smokepingconv/ -run TestEmit -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Create `internal/smokepingconv/emit.go`:

```go
package smokepingconv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/tumult/gosmokeping/internal/config"
)

// preferredProbeOrder is the canonical ordering used at the top of the probes
// block. Anything not in this list comes after, sorted alphabetically.
var preferredProbeOrder = []string{"icmp", "tcp", "http", "dns"}

// Marshal returns a deterministic JSON encoding of cfg:
//   - probes keys in preferredProbeOrder first, then remaining keys sorted
//   - alerts / actions sorted alphabetically (Go's default map ordering)
//   - trailing newline so diffs end cleanly
//
// The rest of the config is emitted via standard encoding. The struct shim
// lets us hand-roll the probes block without cloning the entire Config type.
func Marshal(cfg *config.Config) ([]byte, error) {
	probes, err := encodeProbes(cfg.Probes)
	if err != nil {
		return nil, err
	}

	// Build a parallel struct with Probes replaced by raw JSON.
	type shim struct {
		Listen   string                      `json:"listen"`
		Interval string                      `json:"interval"`
		Pings    int                         `json:"pings"`
		Storage  config.Storage              `json:"storage"`
		Probes   json.RawMessage             `json:"probes"`
		Targets  []config.Group              `json:"targets"`
		Alerts   map[string]config.Alert     `json:"alerts,omitempty"`
		Actions  map[string]config.Action    `json:"actions,omitempty"`
		Cluster  *config.Cluster             `json:"cluster,omitempty"`
	}
	s := shim{
		Listen:   cfg.Listen,
		Interval: cfg.Interval.String(),
		Pings:    cfg.Pings,
		Storage:  cfg.Storage,
		Probes:   probes,
		Targets:  cfg.Targets,
		Alerts:   cfg.Alerts,
		Actions:  cfg.Actions,
		Cluster:  cfg.Cluster,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&s); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeProbes(probes map[string]config.Probe) (json.RawMessage, error) {
	if len(probes) == 0 {
		return []byte("{}"), nil
	}

	keys := make([]string, 0, len(probes))
	seen := map[string]bool{}
	for _, k := range preferredProbeOrder {
		if _, ok := probes[k]; ok {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range probes {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)

	var buf bytes.Buffer
	buf.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			buf.WriteString(",")
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteString(":")
		vb, err := json.Marshal(probes[k])
		if err != nil {
			return nil, fmt.Errorf("marshal probe %q: %w", k, err)
		}
		buf.Write(vb)
	}
	buf.WriteString("}")
	// Re-pretty-print so the caller gets indented output.
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, buf.Bytes(), "  ", "  "); err != nil {
		return nil, err
	}
	return pretty.Bytes(), nil
}
```

- [ ] **Step 4: Run**

Run: `go test ./internal/smokepingconv/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/smokepingconv/emit.go internal/smokepingconv/emit_test.go
git commit -m "feat(smokepingconv): deterministic JSON marshaller with fixed probe order"
```

---

## Task 14: Top-level Convert

Wire the pieces together: parser stages + mapper stages + `io.Reader`-driven entry point. Builds the storage placeholder, ensures `log` action exists, appends storage-reminder and unknown-section notes.

**Files:**
- Create: `internal/smokepingconv/convert.go`
- Create: `internal/smokepingconv/convert_unit_test.go`

- [ ] **Step 1: Write failing test**

Create `internal/smokepingconv/convert_unit_test.go`:

```go
package smokepingconv

import (
	"strings"
	"testing"
)

func TestConvert_MinimalEndToEnd(t *testing.T) {
	src := `*** Database ***
step = 30
pings = 5

*** Probes ***
+ FPing
timeout = 3

*** Targets ***
probe = FPing

+ europe
++ berlin
host = berlin.example.com
`
	cfg, notes, err := Convert(strings.NewReader(src), "/tmp", "/tmp/x.conf")
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if cfg.Pings != 5 {
		t.Errorf("pings: %d", cfg.Pings)
	}
	if cfg.Interval.Seconds() != 30 {
		t.Errorf("interval: %v", cfg.Interval)
	}
	if cfg.Storage.Backend != "influxv2" {
		t.Errorf("storage: %+v", cfg.Storage)
	}
	if _, ok := cfg.Actions["log"]; !ok {
		t.Error("log action missing")
	}
	if len(cfg.Targets) == 0 || len(cfg.Targets[0].Targets) == 0 {
		t.Fatalf("targets: %+v", cfg.Targets)
	}
	if cfg.Targets[0].Targets[0].Probe != "fping" {
		t.Errorf("target probe: %q", cfg.Targets[0].Targets[0].Probe)
	}
	var sawStorageNote bool
	for _, n := range notes {
		if strings.Contains(n.Detail, "storage.influxv2 is a placeholder") {
			sawStorageNote = true
		}
	}
	if !sawStorageNote {
		t.Errorf("expected storage-placeholder note, got %+v", notes)
	}
}
```

- [ ] **Step 2: Run — should fail**

Run: `go test ./internal/smokepingconv/ -run TestConvert_MinimalEndToEnd -v`
Expected: FAIL (undefined `Convert`).

- [ ] **Step 3: Implement**

Create `internal/smokepingconv/convert.go`:

```go
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
```

- [ ] **Step 4: Run**

Run: `go build ./... && go test ./internal/smokepingconv/... -v`
Expected: build clean, all tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/smokepingconv/convert.go internal/smokepingconv/convert_unit_test.go
git commit -m "feat(smokepingconv): top-level Convert ties all stages together"
```

---

## Task 15: (removed — merged into Task 14)

Intentionally empty; the top-level Convert now lives alongside the mapper stages in one `smokepingconv` package. Proceed to Task 16.

---

## Task 16: CLI driver

Replace the scaffolded `main.go` with the real flag-driven driver.

**Files:**
- Modify: `cmd/smokeping2gosmokeping/main.go`

- [ ] **Step 1: Implement**

Replace `cmd/smokeping2gosmokeping/main.go`:

```go
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/smokepingconv"
)

func main() {
	var (
		inPath    = flag.String("in", "", "path to SmokePing config (required)")
		outPath   = flag.String("out", "", "path to gosmokeping JSON output (required)")
		notesPath = flag.String("notes", "", "notes sidecar path (default: <out>.notes.txt)")
		force     = flag.Bool("force", false, "overwrite -out if it exists")
		strict    = flag.Bool("strict", false, "exit 2 if any construct could not be fully translated")
		logLevel  = flag.String("log-level", "info", "log level: debug|info|warn|error")
	)
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(*logLevel)})))

	if *inPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "error: -in and -out are required")
		flag.Usage()
		os.Exit(1)
	}
	if *notesPath == "" {
		*notesPath = *outPath + ".notes.txt"
	}

	if err := run(*inPath, *outPath, *notesPath, *force, *strict); err != nil {
		var strictErr *strictError
		if ok := asStrict(err, &strictErr); ok {
			slog.Warn(strictErr.Error())
			os.Exit(2)
		}
		slog.Error(err.Error())
		os.Exit(1)
	}
}

type strictError struct{ n int }

func (e *strictError) Error() string {
	return fmt.Sprintf("strict mode: %d note(s) produced", e.n)
}

func asStrict(err error, target **strictError) bool {
	s, ok := err.(*strictError)
	if ok {
		*target = s
	}
	return ok
}

func run(inPath, outPath, notesPath string, force, strict bool) error {
	if !force {
		if _, err := os.Stat(outPath); err == nil {
			return fmt.Errorf("output %s already exists (use -force to overwrite)", outPath)
		}
	}
	f, err := os.Open(inPath)
	if err != nil {
		return fmt.Errorf("open -in: %w", err)
	}
	defer f.Close()

	absIn, _ := filepath.Abs(inPath)
	cfg, notes, err := smokepingconv.Convert(f, filepath.Dir(absIn), absIn)
	if err != nil {
		return err
	}

	// Validate the emitted config — catches mapper bugs before we write.
	if err := cfg.Validate(); err != nil {
		slog.Warn("emitted config fails validation — writing anyway for operator review", "err", err)
	}

	data, err := smokepingconv.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := writeFile(outPath, data); err != nil {
		return fmt.Errorf("write -out: %w", err)
	}

	if len(notes) > 0 {
		if err := writeNotes(notesPath, notes); err != nil {
			return fmt.Errorf("write notes: %w", err)
		}
		for _, n := range notes {
			slog.Info(n.Format())
		}
	}

	if strict && len(notes) > 0 {
		return &strictError{n: len(notes)}
	}
	return nil
}

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeNotes(path string, notes []smokepingconv.Note) error {
	// Sort for determinism: level first (skip > warn), then category, then detail.
	sorted := append([]smokepingconv.Note(nil), notes...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Level != sorted[j].Level {
			return sorted[i].Level < sorted[j].Level
		}
		if sorted[i].Category != sorted[j].Category {
			return sorted[i].Category < sorted[j].Category
		}
		return sorted[i].Detail < sorted[j].Detail
	})
	var b strings.Builder
	for _, n := range sorted {
		b.WriteString(n.Format())
		b.WriteString("\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

var _ = config.BackendInfluxV2 // ensure import is not dropped if Validate() stays commented
```

- [ ] **Step 2: Build + smoke test**

```bash
go build -o /tmp/smokeping2gosmokeping ./cmd/smokeping2gosmokeping
printf '%s\n' '*** Targets ***' 'probe = FPing' '+ berlin' 'host = berlin.example.com' > /tmp/sp.conf
/tmp/smokeping2gosmokeping -in /tmp/sp.conf -out /tmp/sp.json -force
cat /tmp/sp.json
```

Expected: `/tmp/sp.json` contains a valid gosmokeping JSON config (with `"icmp"` in probes, one target `berlin` under group `default`), exit code 0. `/tmp/sp.json.notes.txt` exists and mentions storage placeholder.

- [ ] **Step 3: Commit**

```bash
git add cmd/smokeping2gosmokeping/main.go
git commit -m "feat(smokeping2gosmokeping): CLI driver with -strict and sidecar notes"
```

---

## Task 17: End-to-end golden tests

Exercise `smokepingconv.Convert` end-to-end on real fixtures. Validates output against `config.Load` (round-trip through the real loader).

**Files:**
- Create: `internal/smokepingconv/convert_test.go`
- Create: `internal/smokepingconv/testdata/e2e/minimal.conf`
- Create: `internal/smokepingconv/testdata/e2e/minimal.want.json`
- Create: `internal/smokepingconv/testdata/e2e/minimal.want.notes.txt`
- Create: `internal/smokepingconv/testdata/e2e/mixed_probes.conf`
- Create: `internal/smokepingconv/testdata/e2e/mixed_probes.want.json`
- Create: `internal/smokepingconv/testdata/e2e/mixed_probes.want.notes.txt`
- Create: `internal/smokepingconv/testdata/e2e/unsupported.conf`
- Create: `internal/smokepingconv/testdata/e2e/unsupported.want.json`
- Create: `internal/smokepingconv/testdata/e2e/unsupported.want.notes.txt`

- [ ] **Step 1: Create the test driver**

`internal/smokepingconv/convert_test.go`:

```go
package smokepingconv

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tumult/gosmokeping/internal/config"
)

var update = flag.Bool("update", false, "regenerate golden files")

func TestConvert_Golden(t *testing.T) {
	matches, err := filepath.Glob("testdata/e2e/*.conf")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no fixtures under testdata/e2e")
	}
	for _, in := range matches {
		name := strings.TrimSuffix(filepath.Base(in), ".conf")
		t.Run(name, func(t *testing.T) {
			runGolden(t, in)
		})
	}
}

func runGolden(t *testing.T, in string) {
	t.Helper()
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	abs, _ := filepath.Abs(in)
	cfg, notes, err := Convert(f, filepath.Dir(abs), abs)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	// Normalise file paths in notes to basenames for stability.
	for i := range notes {
		if notes[i].Source != "" {
			notes[i].Source = basenameSource(notes[i].Source)
		}
	}

	data, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	notesStr := formatNotes(notes)

	wantJSON := strings.TrimSuffix(in, ".conf") + ".want.json"
	wantNotes := strings.TrimSuffix(in, ".conf") + ".want.notes.txt"

	if *update {
		_ = os.WriteFile(wantJSON, data, 0o644)
		_ = os.WriteFile(wantNotes, []byte(notesStr), 0o644)
		return
	}

	got := string(data)
	if gotWant, err := os.ReadFile(wantJSON); err != nil {
		t.Fatalf("read golden %s: %v", wantJSON, err)
	} else if got != string(gotWant) {
		t.Errorf("JSON mismatch for %s\n--- want ---\n%s\n--- got ---\n%s", in, gotWant, got)
	}
	if gotWant, err := os.ReadFile(wantNotes); err != nil {
		t.Fatalf("read golden %s: %v", wantNotes, err)
	} else if notesStr != string(gotWant) {
		t.Errorf("notes mismatch for %s\n--- want ---\n%s\n--- got ---\n%s", in, gotWant, notesStr)
	}

	// Validate that the emitted JSON parses + validates via the real loader.
	tmp := filepath.Join(t.TempDir(), "out.json")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(tmp); err != nil {
		t.Errorf("emitted config fails config.Load: %v", err)
	}
}

func formatNotes(notes []Note) string {
	sorted := append([]Note(nil), notes...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Level != sorted[j].Level {
			return sorted[i].Level < sorted[j].Level
		}
		if sorted[i].Category != sorted[j].Category {
			return sorted[i].Category < sorted[j].Category
		}
		return sorted[i].Detail < sorted[j].Detail
	})
	var b strings.Builder
	for _, n := range sorted {
		b.WriteString(n.Format())
		b.WriteString("\n")
	}
	return b.String()
}

// basenameSource turns "/abs/path/to/file.conf:12" into "file.conf:12" so
// golden files don't depend on the checkout directory.
func basenameSource(s string) string {
	idx := strings.LastIndex(s, ":")
	if idx < 0 {
		return filepath.Base(s)
	}
	return filepath.Base(s[:idx]) + s[idx:]
}
```

- [ ] **Step 2: Write the minimal fixture**

`internal/smokepingconv/testdata/e2e/minimal.conf`:

```
*** Database ***
step = 30
pings = 10

*** Probes ***
+ FPing
timeout = 3

*** Targets ***
probe = FPing

+ europe
menu = Europe

++ berlin
host = berlin.example.com
title = Berlin
```

- [ ] **Step 3: Generate initial goldens and sanity-check them**

Run: `go test ./internal/smokepingconv/ -run TestConvert_Golden -update`

Open `internal/smokepingconv/testdata/e2e/minimal.want.json`. Verify:
- `"listen": ":8080"`
- `"interval": "30s"`
- `"pings": 10`
- `"probes"` contains `"icmp"` first with `"type": "icmp"`, `"timeout": "3s"`, and also `"fping"` if FPing was recorded — actually since probe key is `fping` (slugged SmokePing name), we expect `"fping"` only. If you see both `icmp` AND `fping`, the mapper is double-emitting; trace and fix.
- `"targets"` has one group `"europe"` with target `"berlin"`, `host: "berlin.example.com"`, `probe: "fping"`.

Open `minimal.want.notes.txt`. Expect at least the storage-placeholder warning.

- [ ] **Step 4: Add the mixed-probes fixture**

`internal/smokepingconv/testdata/e2e/mixed_probes.conf`:

```
*** Database ***
step = 60
pings = 20

*** Probes ***
+ FPing
timeout = 5

+ Curl
urlformat = https://%host%/
insecure_ssl = yes

+ TCPPing
port = 443
timeout = 2

+ DNS
timeout = 3

*** Alerts ***
+ bigloss
type = loss
pattern = >50%,>50%,>50%

+ slow
type = rtt
pattern = >100,>100

*** Targets ***
probe = FPing

+ network
menu = Network
++ cloudflare
host = 1.1.1.1

+ web
menu = Web
probe = Curl
++ example
host = example.com
alerts = slow

+ services
menu = Services
++ api
probe = TCPPing
host = api.example.com
```

- [ ] **Step 5: Add the unsupported fixture**

`internal/smokepingconv/testdata/e2e/unsupported.conf`:

```
*** General ***
owner = Alice

*** Presentation ***
template = /etc/smokeping/basepage.html
charset = utf-8

*** Database ***
step = 60
pings = 20

*** Probes ***
+ FPing
timeout = 5

+ AnotherSSH
binary = /usr/bin/ssh

*** Alerts ***
+ weird
type = rtt
pattern = <10,<10,<10,>100,>100

*** Targets ***
probe = FPing

+ host1
host = host1.example.com

+ ssh-only
probe = AnotherSSH
host = ssh.example.com
```

- [ ] **Step 6: Regenerate and eyeball goldens**

Run: `go test ./internal/smokepingconv/ -run TestConvert_Golden -update`

Inspect each `.want.notes.txt`. For `mixed_probes`:
- `bigloss` should translate to `loss_pct > 50`, `sustained: 3`.
- `slow` should translate to `rtt_median > 100ms`, `sustained: 2`.
- The example target should inherit `probe = Curl` and emit a URL `https://example.com/`.
- The api target should have host `api.example.com:443`.

For `unsupported`:
- `AnotherSSH` probe skipped → note.
- `weird` alert skipped → note.
- `ssh-only` target dropped → note.
- Presentation section → skip note.

- [ ] **Step 7: Lock in goldens**

Run: `go test ./internal/smokepingconv/ -v`
Expected: PASS, no updates needed.

- [ ] **Step 8: Commit**

```bash
git add internal/smokepingconv/convert_test.go internal/smokepingconv/testdata/e2e
git commit -m "test(smokepingconv): end-to-end golden tests for conversion"
```

---

## Task 18: Makefile target

**Files:**
- Modify: `Makefile`

- [ ] **Step 1: Read current content**

Confirm contents match what's shown in the spec. The `.PHONY` list needs the new target.

- [ ] **Step 2: Edit Makefile**

Change the `.PHONY` line from:

```makefile
.PHONY: build test ui ui-dev dev clean tidy lint
```

to:

```makefile
.PHONY: build test ui ui-dev dev clean tidy lint smokeping2gosmokeping build-all
```

Append at the end of the file (after `setcap`):

```makefile
smokeping2gosmokeping:
	$(GO) build -ldflags="$(LDFLAGS)" -o smokeping2gosmokeping ./cmd/smokeping2gosmokeping

build-all: build smokeping2gosmokeping
```

- [ ] **Step 3: Verify**

Run: `make smokeping2gosmokeping && ls -l smokeping2gosmokeping && rm smokeping2gosmokeping`
Expected: binary produced, then cleaned up (the repo's `.gitignore` doesn't cover it).

- [ ] **Step 4: Commit**

```bash
git add Makefile
git commit -m "build: add smokeping2gosmokeping Makefile target"
```

---

## Task 19: GitHub workflow — build + release the converter

**Files:**
- Modify: `.github/workflows/build.yml`

- [ ] **Step 1: Edit workflow**

In `.github/workflows/build.yml`, after the existing `Build binary` step and the `upload-artifact` for `gosmokeping-linux-amd64`, insert:

```yaml
      - name: Build smokeping2gosmokeping
        run: |
          go build -trimpath -ldflags="-s -w" -o build/smokeping2gosmokeping-linux-amd64 ./cmd/smokeping2gosmokeping

      - uses: actions/upload-artifact@v6
        with:
          name: smokeping2gosmokeping-linux-amd64
          path: build/smokeping2gosmokeping-linux-amd64
```

Then in the `Create GitHub release` step, change:

```bash
gh release create "${GITHUB_REF_NAME}" \
  --title "${GITHUB_REF_NAME}" \
  --generate-notes \
  ./build/gosmokeping-linux-amd64
```

to:

```bash
gh release create "${GITHUB_REF_NAME}" \
  --title "${GITHUB_REF_NAME}" \
  --generate-notes \
  ./build/gosmokeping-linux-amd64 \
  ./build/smokeping2gosmokeping-linux-amd64
```

- [ ] **Step 2: Validate locally**

Run: `go vet ./... && go build -trimpath -ldflags="-s -w" -o /tmp/s2g-linux ./cmd/smokeping2gosmokeping && rm /tmp/s2g-linux`
Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/build.yml
git commit -m "ci: build + release smokeping2gosmokeping alongside gosmokeping"
```

---

## Task 20: README — migration section

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Add a Migrating section**

Append to `README.md` (or near whichever install/usage section exists; if there's a table of contents, add an entry):

```markdown
## Migrating from SmokePing

`smokeping2gosmokeping` reads a SmokePing `Config::Grammar` config and emits
an equivalent gosmokeping JSON config. It follows `@include` directives and
translates the common probe/alert shapes; constructs it can't map cleanly
(unusual probe types, complex alert patterns, `*** Presentation ***` settings)
are recorded in a sidecar notes file for human review.

```bash
smokeping2gosmokeping -in /etc/smokeping/config -out config.json
# writes config.json and config.json.notes.txt
```

Storage credentials are emitted as `${INFLUX_URL}` / `${INFLUX_TOKEN}` /
`${INFLUX_ORG}` placeholders — set them in the environment (or in a `.env`
file next to `config.json`) before starting gosmokeping. Add `-strict` to
make the tool exit 2 when any construct couldn't be fully translated, useful
for CI-driven config generation.
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: README section on migrating from SmokePing"
```

---

## Task 21: Final smoke + vet

- [ ] **Step 1: Run everything**

```bash
go vet ./...
go test ./...
go build -o /tmp/gosmokeping ./cmd/gosmokeping
go build -o /tmp/smokeping2gosmokeping ./cmd/smokeping2gosmokeping
rm /tmp/gosmokeping /tmp/smokeping2gosmokeping
```

Expected: all green. (The primary binary still needs the UI dist dir; if `go build ./cmd/gosmokeping` complains about `//go:embed`, skip it — the converter is what matters for this PR, and the main binary is already covered by existing CI.)

- [ ] **Step 2: Run the smoke test from Task 16 again**

```bash
go build -o /tmp/smokeping2gosmokeping ./cmd/smokeping2gosmokeping
printf '%s\n' '*** Targets ***' 'probe = FPing' '+ berlin' 'host = berlin.example.com' > /tmp/sp.conf
/tmp/smokeping2gosmokeping -in /tmp/sp.conf -out /tmp/sp.json -force
cat /tmp/sp.json
cat /tmp/sp.json.notes.txt
rm /tmp/smokeping2gosmokeping /tmp/sp.conf /tmp/sp.json /tmp/sp.json.notes.txt
```

Expected: valid gosmokeping JSON + a notes file whose only line is the storage-placeholder warning.

- [ ] **Step 3: No commit** — this is verification only.
