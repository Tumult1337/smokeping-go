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
