package smokepingconv

import (
	"strings"
	"testing"
	"time"

	"github.com/tumult/gosmokeping/internal/config"
)

func TestEmit_ProbeOrder(t *testing.T) {
	cfg := &config.Config{
		Listen:   ":8080",
		Interval: 30 * time.Second,
		Pings:    10,
		Probes: map[string]config.Probe{
			"dns":  {Type: "dns", Timeout: 5 * time.Second},
			"icmp": {Type: "icmp", Timeout: 5 * time.Second},
			"http": {Type: "http", Timeout: 5 * time.Second},
			"tcp":  {Type: "tcp", Timeout: 5 * time.Second},
			"curl": {Type: "http", Timeout: 5 * time.Second},
		},
		Actions: map[string]config.Action{"log": {Type: "log"}},
	}
	b, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	iIcmp := strings.Index(s, `"icmp"`)
	iTcp := strings.Index(s, `"tcp"`)
	iHttp := strings.Index(s, `"http"`)
	iDns := strings.Index(s, `"dns"`)
	iCurl := strings.Index(s, `"curl"`)
	if !(iIcmp < iTcp && iTcp < iHttp && iHttp < iDns && iDns < iCurl) {
		t.Errorf("probe order wrong, got:\n%s", s)
	}
	if !strings.HasSuffix(s, "\n") {
		t.Error("missing trailing newline")
	}
}
