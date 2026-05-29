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
	out := strings.Trim(b.String(), "-")
	if out == "" {
		// All-special or empty input (e.g. "---", "") would otherwise yield an
		// empty key/id that propagates silently and fails config.Validate.
		return "unnamed"
	}
	return out
}

// slugAll slugs each element of in, returning a new slice (nil for nil/empty).
func slugAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = slug(s)
	}
	return out
}
