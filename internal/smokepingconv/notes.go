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
