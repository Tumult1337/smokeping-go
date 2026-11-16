package ui

import (
	"io/fs"
	"os"
	"strings"
	"testing"
)

const requireUIEnv = "GOSMOKEPING_REQUIRE_UI"

// index.html references these root files and the API serves each one only if
// it is embedded, so a build that stops copying one answers it with a 404.
func TestEmbeddedDistCarriesTheIconsIndexReferences(t *testing.T) {
	requireUI := os.Getenv(requireUIEnv) == "1"
	f := FS()
	if f == nil {
		if requireUI {
			t.Fatalf("dist not built and %s=1: run `make ui`", requireUIEnv)
		}
		t.Skip("dist not built; run `make ui`")
	}
	index, err := fs.ReadFile(f, "index.html")
	if err != nil {
		t.Fatalf("index.html: %v", err)
	}
	for _, name := range []string{"favicon.svg", "icon.svg", "theme.js"} {
		if !strings.Contains(string(index), "/"+name) {
			if requireUI {
				t.Errorf("index.html does not reference /%s and %s=1 requires it", name, requireUIEnv)
			}
			continue
		}
		if _, err := fs.Stat(f, name); err != nil {
			t.Errorf("index.html references /%s but it is not embedded: %v", name, err)
		}
	}
}
