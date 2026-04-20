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
