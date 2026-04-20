package smokepingconv

import (
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/smokepingconv/parser"
)

func TestMapProbes_FPingAndCurl(t *testing.T) {
	root := &parser.SPRoot{Probes: []parser.SPProbe{
		{Name: "FPing", Type: "FPing", Params: map[string]string{"timeout": "3"}, Keys: []string{"timeout"}},
		{Name: "Curl", Type: "Curl", Params: map[string]string{"urlformat": "http://%host%/", "insecure_ssl": "yes"}, Keys: []string{"urlformat", "insecure_ssl"}},
	}}

	probes, info, notes := mapProbes(root)

	if p, ok := probes["fping"]; !ok || p.Type != "icmp" || p.Timeout != 3*time.Second {
		t.Errorf("fping: %+v", p)
	}
	if p, ok := probes["curl"]; !ok || p.Type != "http" || !p.Insecure {
		t.Errorf("curl: %+v", p)
	}
	if info["FPing"].Key != "fping" || info["FPing"].Type != "icmp" {
		t.Errorf("info FPing: %+v", info["FPing"])
	}
	if info["Curl"].URLFormat != "http://%host%/" {
		t.Errorf("Curl URLFormat: %q", info["Curl"].URLFormat)
	}
	_ = notes
}

func TestMapProbes_UnsupportedSkipped(t *testing.T) {
	root := &parser.SPRoot{Probes: []parser.SPProbe{
		{Name: "SSH", Type: "AnotherSSH", File: "x.conf", LineNo: 12},
	}}
	probes, info, notes := mapProbes(root)
	if _, ok := probes["ssh"]; ok {
		t.Error("SSH should be skipped")
	}
	if _, ok := info["SSH"]; ok {
		t.Error("info should omit skipped probe")
	}
	var seen bool
	for _, n := range notes {
		if indexOf(n.Detail, "AnotherSSH") >= 0 {
			seen = true
		}
	}
	if !seen {
		t.Errorf("expected skip note for AnotherSSH, got %+v", notes)
	}
}

func TestMapProbes_Subprobes(t *testing.T) {
	root := &parser.SPRoot{Probes: []parser.SPProbe{
		{Name: "FPing", Type: "FPing", Subprobes: []parser.SPProbe{
			{Name: "FPingHighPings", Type: "FPing", Params: map[string]string{"pings": "20"}, Keys: []string{"pings"}},
		}},
	}}
	probes, info, _ := mapProbes(root)
	if _, ok := probes["fping"]; !ok {
		t.Error("expected fping")
	}
	if _, ok := probes["fpinghighpings"]; !ok {
		t.Error("expected fpinghighpings")
	}
	if info["FPingHighPings"].Key != "fpinghighpings" {
		t.Errorf("subprobe info: %+v", info)
	}
}
