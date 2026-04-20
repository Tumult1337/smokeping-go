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
