package smokepingconv

import "strings"

// slug lowercases s and replaces any run of characters outside [a-z0-9_] with
// a single '-'. Underscores are preserved (SmokePing target names commonly
// use them). Trailing/leading dashes are trimmed.
func slug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := b.String()
	return strings.Trim(out, "-")
}
