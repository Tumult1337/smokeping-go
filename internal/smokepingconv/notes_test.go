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
