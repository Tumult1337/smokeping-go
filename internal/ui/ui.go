// Package ui embeds the built React SPA. Run `make ui` to populate ui/dist
// before `go build`, otherwise FS() returns an empty filesystem and the API
// serves only the JSON endpoints.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the embedded dist/ directory as a root-level filesystem, or nil
// if no build output is present (e.g. dev build with only the placeholder).
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil
	}
	// If dist is empty (just the .gitkeep), behave as "no UI" so the API still
	// runs without serving broken index.html.
	entries, err := fs.ReadDir(sub, ".")
	if err != nil || len(entries) == 0 {
		return nil
	}
	hasIndex := false
	for _, e := range entries {
		if e.Name() == "index.html" {
			hasIndex = true
			break
		}
	}
	if !hasIndex {
		return nil
	}
	return sub
}
