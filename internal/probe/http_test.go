package probe

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// url.Error embeds the whole request URL, and config bounds no URL's length, so
// a legitimate 5 KiB URL plus a connection failure produced an error string
// that the cluster ingest bound refused — dropping the whole batch, up to 99
// unrelated cycles with it. The producer truncates instead.
func TestHTTPProbeTruncatesTransportError(t *testing.T) {
	p := NewHTTP("http", time.Second, false)
	// Port 1 with no listener: a deterministic connection failure, not a DNS
	// lookup that could resolve differently per host.
	longURL := "http://127.0.0.1:1/" + strings.Repeat("a", 8192)

	res, err := p.Probe(context.Background(), Target{Name: "t", URL: longURL}, 1)
	if err == nil {
		t.Fatal("want a transport failure")
	}
	if len(res.HTTPSamples) != 1 {
		t.Fatalf("got %d samples, want 1", len(res.HTTPSamples))
	}
	got := res.HTTPSamples[0].Err
	if got == "" {
		t.Fatal("sample carries no error text")
	}
	if len(got) > MaxHTTPErrLen {
		t.Fatalf("stored error is %d bytes, limit %d", len(got), MaxHTTPErrLen)
	}
	if !strings.Contains(got, "127.0.0.1:1") {
		t.Fatalf("truncated error lost the request identity: %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("truncated error lost the diagnosis: %q", got)
	}
}

// url.Error prints the URL before the cause, so cutting only the tail off a
// URL longer than the bound stores the URL and drops the one part an operator
// acts on. The URL here is past MaxHTTPErrLen by itself.
func TestTruncateHTTPErrKeepsTheCausePastTheBound(t *testing.T) {
	long := "https://example.test/" + strings.Repeat("a", 2*MaxHTTPErrLen)
	e := &url.Error{
		Op:  "Get",
		URL: long,
		Err: errors.New("dial tcp 10.0.0.1:443: connect: connection refused"),
	}
	got := TruncateHTTPErr(e.Error())
	if len(got) > MaxHTTPErrLen {
		t.Fatalf("stored error is %d bytes, limit %d", len(got), MaxHTTPErrLen)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncation produced invalid utf-8: %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("truncation dropped the cause: %q", got)
	}
	if !strings.HasPrefix(got, `Get "https://example.test/`) {
		t.Fatalf("truncation dropped the request identity: %q", got[:64])
	}
}

// Truncation must not split a rune: probe_http.error is a String column and an
// operator reads it, so half a code point is a corrupt row, and a URL may
// legitimately carry multi-byte text.
func TestTruncateHTTPErrIsRuneSafe(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"ascii", strings.Repeat("x", MaxHTTPErrLen*2)},
		{"multibyte", strings.Repeat("é", MaxHTTPErrLen)},
		{"four byte", strings.Repeat("😀", MaxHTTPErrLen)},
	} {
		got := TruncateHTTPErr(tc.in)
		if len(got) > MaxHTTPErrLen {
			t.Errorf("%s: %d bytes, limit %d", tc.name, len(got), MaxHTTPErrLen)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: truncation produced invalid utf-8", tc.name)
		}
	}
	short := `Get "https://example.test": connection refused`
	if got := TruncateHTTPErr(short); got != short {
		t.Errorf("an error inside the bound was rewritten: %q", got)
	}
}
