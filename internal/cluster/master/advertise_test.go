package master

import "testing"

func TestParseAdvertise(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"public v4", "203.0.113.9", true},
		{"private v4 (wireguard mesh)", "10.44.0.2", true},
		{"public v6", "2001:db8::1", true},
		{"ULA v6", "fd00::1", true},
		{"empty", "", false},
		{"loopback v4", "127.0.0.1", false},
		{"loopback v6", "::1", false},
		{"unspecified v4", "0.0.0.0", false},
		{"unspecified v6", "::", false},
		{"multicast v4", "224.0.0.1", false},
		{"multicast v6", "ff02::1", false},
		{"link-local v4", "169.254.1.1", false},
		{"link-local v6", "fe80::1", false},
		{"hostname not accepted", "slave.example.com", false},
		{"cidr not accepted", "10.44.0.0/24", false},
		{"host:port not accepted", "10.44.0.2:8080", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAdvertise(tc.raw)
			if tc.ok && err != nil {
				t.Fatalf("ParseAdvertise(%q) = error %v, want success", tc.raw, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ParseAdvertise(%q) = %v, want error", tc.raw, got)
			}
			if tc.ok && !got.IsValid() {
				t.Fatalf("ParseAdvertise(%q) returned an invalid Addr", tc.raw)
			}
		})
	}
}

// An IPv4-mapped IPv6 form must normalise to plain v4 so two spellings of the
// same address collide in the duplicate check added in Task 2.
func TestParseAdvertiseUnmapsV4(t *testing.T) {
	got, err := ParseAdvertise("::ffff:203.0.113.9")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Is4() {
		t.Fatalf("got %v (Is4=%v), want unmapped IPv4", got, got.Is4())
	}
}
