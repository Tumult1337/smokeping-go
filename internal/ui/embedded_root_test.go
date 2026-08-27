package ui

import (
	"io/fs"
	"strings"
	"testing"
)

// index.html references /favicon.svg and /icon.svg, and the API resolves them
// against this FS — so if `npm run icon` output ever stops reaching dist, the
// SPA fallback answers them with index.html and the tab icon silently
// disappears. Skips when dist is empty, which is the headless build.
func TestEmbeddedDistCarriesTheIconsIndexReferences(t *testing.T) {
	f := FS()
	if f == nil {
		t.Skip("dist not built; run `make ui`")
	}
	index, err := fs.ReadFile(f, "index.html")
	if err != nil {
		t.Fatalf("index.html: %v", err)
	}
	for _, name := range []string{"favicon.svg", "icon.svg"} {
		if !strings.Contains(string(index), "/"+name) {
			continue // not referenced by this build
		}
		if _, err := fs.Stat(f, name); err != nil {
			t.Errorf("index.html references /%s but it is not embedded: %v", name, err)
		}
	}
}
