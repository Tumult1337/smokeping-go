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
