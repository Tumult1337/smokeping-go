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
