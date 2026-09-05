package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/publicsuffix"
)

// HTTP issues a GET to Target.URL and measures time-to-first-byte. A response
// outside [200, 400) counts as loss. Body is drained (up to 4KB) to keep the
// connection pool healthy but we don't measure download time. Same-host
// redirects are followed with a per-request cookie jar, so the measurement is
// of the page rather than of a challenge standing in front of it.
type HTTP struct {
	name     string
	timeout  time.Duration
	spacing  time.Duration
	insecure bool
	client   *http.Client

	// famClients caches family-pinned http.Client instances ("v4" / "v6")
	// so each cycle reuses an existing connection pool instead of building
	// a fresh http.Transport. Without this cache, every probe call for a
	// family-pinned target allocates a new transport whose idle TCP
	// connections survive until GC + finalizer reclaim them — a slow leak
	// over weeks of uptime. Bounded to two entries in practice.
	famMu      sync.RWMutex
	famClients map[string]*http.Client
}

// NewHTTP builds an HTTP probe. If insecure is true, TLS verification is
// skipped — intended for targets with self-signed or expired certs where
// reachability is the point, not cert hygiene.
func NewHTTP(name string, timeout time.Duration, insecure bool) *HTTP {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	// Clone DefaultTransport so we keep its sane connection-pool defaults and
	// only override TLS config when asked.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &HTTP{
		name:     name,
		timeout:  timeout,
		spacing:  500 * time.Millisecond,
		insecure: insecure,
		client: &http.Client{
			Timeout:       timeout,
			Transport:     tr,
			CheckRedirect: CheckRedirect,
		},
		famClients: make(map[string]*http.Client),
	}
}

// clientFor returns a client pinned to the given address family, or the shared
// client when family is empty. The family-pinned client clones the default
// transport and overrides its DialContext with a net.Dialer tied to tcp4/tcp6,
// so connection reuse stays per-family rather than global; with maxHTTPRequests
// == 2 that's fine.
//
// Results are cached per family for the life of the probe — building a fresh
// http.Transport on every cycle would leak its connection pool until GC.
func (p *HTTP) clientFor(family string) *http.Client {
	if family == "" {
		return p.client
	}
	p.famMu.RLock()
	c, ok := p.famClients[family]
	p.famMu.RUnlock()
	if ok {
		return c
	}

	p.famMu.Lock()
	defer p.famMu.Unlock()
	// Re-check under the write lock so two readers racing past the fast
	// path don't both construct a client and then disagree on which one
	// the cache holds.
	if c, ok := p.famClients[family]; ok {
		return c
	}
	c = p.buildFamilyClient(family)
	p.famClients[family] = c
	return c
}

func (p *HTTP) buildFamilyClient(family string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if p.insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	network := familyNetwork("tcp", family)
	dialer := &net.Dialer{Timeout: p.timeout, KeepAlive: 30 * time.Second}
	tr.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}
	return &http.Client{
		Timeout:       p.timeout,
		Transport:     tr,
		CheckRedirect: CheckRedirect,
	}
}

func (p *HTTP) Name() string { return p.name }

// MaxHTTPErrLen bounds HTTPSample.Err. It is a truncation length, not a
// legitimacy bound: url.Error embeds the whole request URL and config bounds
// no URL's length, so no ceiling derived from what a probe can emit exists —
// bounding it at the ingest boundary instead turned a long configured URL plus
// a connection failure into a rejected batch. 4096 is what probe_http.error, a
// plain String column, carries per sample; at maxHTTPRequests that is 8 KiB of
// error text per cycle.
const MaxHTTPErrLen = 4096

// errTruncationMark tells an operator reading probe_http.error that the text
// was cut rather than that the transport reported this much.
const errTruncationMark = "…(truncated)…"

// TruncateHTTPErr bounds a probe error before it is stored or serialized,
// keeping a head and a tail because the diagnosis is at the end: url.Error
// prints the whole request URL before the cause, so a head-only cut on a URL
// longer than the bound stored the URL and dropped the "connection refused".
// Cutting both ends on a rune boundary keeps the column free of half a code
// point, and reading from the end rather than unwrapping *url.Error keeps the
// last wrapped cause of any error shape.
func TruncateHTTPErr(s string) string {
	if len(s) <= MaxHTTPErrLen {
		return s
	}
	budget := MaxHTTPErrLen - len(errTruncationMark)
	head := budget / 2
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	tail := len(s) - (budget - head)
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return s[:head] + errTruncationMark + s[tail:]
}

// UserAgent is the User-Agent every outbound HTTP client in this program sets.
// It is one exported constant and not a literal per client, because a client
// that sets none sends Go's default — which names the negotiated protocol
// ("Go-http-client/2.0") and therefore identifies neither the program nor its
// version to the operator of the server it reaches.
const UserAgent = "gosmokeping/1.0 (+https://github.com/tumult/gosmokeping)"

// MaxHTTPRedirects bounds one request's redirect chain. A cookie challenge
// costs one hop (the 307 that sets the cookie, then the retry it points at),
// so three leaves room for a challenge behind an ordinary scheme or path
// redirect while stopping a chain that never converges.
const MaxHTTPRedirects = 3

// CheckRedirect is the redirect policy every outbound client in this program
// uses. It is one exported function and not a closure per client for the
// reason UserAgent is one constant: the two rules below are a trust decision,
// and a client that reimplements them gets net/http's default instead — ten
// hops to any host the responder names.
//
// A cross-host redirect stops with http.ErrUseLastResponse, which surfaces the
// 3xx itself. The comparison is on host and port, not on the hostname: a hop
// to another port of the same machine reaches a different service, which is
// the internal-service scan the rule exists to refuse. A plain http-to-https
// upgrade writes no explicit port in either URL, so it still matches.
// Following a cross-host hop would let the responder choose the address we
// connect to next, and on the master client net/http would additionally
// rewrite a 302'd POST into a GET and drop the batch body. Stopping rather
// than erroring is deliberate: a host redirecting elsewhere is serving
// normally, and the http probe recorded that 3xx as reachable before this
// policy existed, so erroring would invent loss for a working target.
//
// A downgrade out of https stops too. net/http strips Authorization and Cookie
// only when the host changes (client.go's stripSensitiveHeaders), and this
// policy requires the hosts to be equal, so nothing else would keep the master
// client's bearer token off a plaintext hop.
//
// A redirect net/http answered by changing the method stops for the same
// reason and is the one that costs data: 301, 302 and 303 rewrite a POST into
// a GET and send no body, so following one would ship an empty request the
// master answers 405 to and the slave would record the batch as delivered.
//
// Exceeding the depth is an error, because a chain that does not converge is a
// page the caller never reached.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return http.ErrUseLastResponse
	}
	if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return http.ErrUseLastResponse
	}
	if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return http.ErrUseLastResponse
	}
	if req.Method != via[len(via)-1].Method {
		return http.ErrUseLastResponse
	}
	if len(via) > MaxHTTPRedirects {
		return fmt.Errorf("stopped after %d redirects", MaxHTTPRedirects)
	}
	return nil
}

// MaxJarCookies bounds the distinct (host, domain, path, name) tuples one jar
// retains. net/http/cookiejar bounds nothing, and Set-Cookie is written by the
// responder: for the http probe that responder is an untrusted target, and for
// the master client it is whatever answers the configured URL. A challenge
// sets about three cookies, so 64 admits a site's ordinary session state and
// refuses an origin growing the map without end.
const MaxJarCookies = 64

// MaxJarCookieBytes bounds one cookie's retained size. MaxJarCookies alone
// counts entries, and every field in an entry is responder-written against
// net/http's 10 MB response-header ceiling, so 64 entries could retain tens of
// megabytes for the life of the master client's jar. 4096 is RFC 6265 6.1's
// minimum a server must support, so nothing a real responder sets is refused,
// and the jar's whole retention is MaxJarCookies x this.
const MaxJarCookieBytes = 4096

// NewCookieJar returns a cookie jar bounded at MaxJarCookies distinct keys.
// The public-suffix list is what stops a responder scoping a cookie to a
// registry suffix such as "co.uk" and having it replayed to every host under
// it.
//
// A jar cookiejar.New refuses to build is reported as no jar rather than as an
// error, because http.Client reads a nil Jar as "manage no cookies" — the
// behaviour every caller here had before this existed. The Options is a
// literal, so that branch is unreachable today.
func NewCookieJar() http.CookieJar {
	inner, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil || inner == nil {
		return nil
	}
	return &boundedJar{inner: inner, keys: make(map[string]struct{})}
}

// boundedJar caps the distinct cookie keys its inner jar is asked to store.
// It admits an update to a key already held whatever the count, because a
// challenge cookie rotates on every response: counting each rotation as a new
// entry would exhaust the budget and then refuse the one cookie the caller
// needs.
type boundedJar struct {
	inner http.CookieJar

	mu   sync.Mutex
	keys map[string]struct{}
}

func (j *boundedJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	admitted := cookies[:0:0]
	for _, c := range cookies {
		if len(c.Name)+len(c.Value)+len(c.Path)+len(c.Domain) > MaxJarCookieBytes {
			continue
		}
		key := jarKey(u, c)
		if _, held := j.keys[key]; !held {
			if len(j.keys) >= MaxJarCookies {
				continue
			}
			j.keys[key] = struct{}{}
		}
		admitted = append(admitted, c)
	}
	j.mu.Unlock()
	if len(admitted) > 0 {
		j.inner.SetCookies(u, admitted)
	}
}

func (j *boundedJar) Cookies(u *url.URL) []*http.Cookie { return j.inner.Cookies(u) }

// jarKey mirrors net/http/cookiejar's own storage id, domain;path;name with
// the domain canonicalized and an absent path defaulted from the request URL.
// A key that is not the inner jar's key bounds nothing: keyed on the wire
// bytes, a Set-Cookie carrying no Path attribute is one key here and one entry
// per request path there, so a responder choosing its own Location values grew
// the jar without end under a single budget slot. cookiejar exports neither
// the id nor defaultPath, so this is mirrored from its jar.go rather than
// restated from RFC 6265.
func jarKey(u *url.URL, c *http.Cookie) string {
	domain := strings.ToLower(strings.TrimPrefix(c.Domain, "."))
	if domain == "" {
		domain = strings.ToLower(u.Hostname())
	}
	path := c.Path
	if path == "" || path[0] != '/' {
		path = defaultCookiePath(u.Path)
	}
	return domain + ";" + path + ";" + c.Name
}

// defaultCookiePath mirrors cookiejar.defaultPath: RFC 6265 5.1.4's
// default-path, which is the request path up to its last "/".
func defaultCookiePath(p string) string {
	if p == "" || p[0] != '/' {
		return "/"
	}
	i := strings.LastIndex(p, "/")
	if i == 0 {
		return "/"
	}
	return p[:i]
}

// maxHTTPRequests caps samples per cycle. HTTP is far more expensive than a
// ping (TLS handshake, server log entries, possible rate limits / WAF flags),
// so we deliberately do at most a couple per interval regardless of cfg.Pings.
// A sample is not one request: a followed chain costs up to MaxHTTPRedirects
// more, so the ceiling an operator sizes a WAF rate limit against is
// maxHTTPRequests x (MaxHTTPRedirects + 1), and the responder decides where in
// that range a cycle lands.
const maxHTTPRequests = 2

func (p *HTTP) Probe(ctx context.Context, t Target, count int) (*Result, error) {
	if t.URL == "" {
		return nil, errors.New("http: url required")
	}
	if count > maxHTTPRequests {
		count = maxHTTPRequests
	}
	if count < 1 {
		count = 1
	}
	result := &Result{
		RTTs:        make([]time.Duration, 0, count),
		HTTPSamples: make([]HTTPSample, 0, count),
	}
	client := p.clientFor(t.Family)
	var lastErr error

	for n := range count {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Sent++
		sampleTime := time.Now()
		rtt, status, err := p.one(ctx, client, t.URL)
		sample := HTTPSample{Time: sampleTime, RTT: rtt, Status: status}
		if err != nil {
			result.LossCount++
			lastErr = err
			sample.Err = TruncateHTTPErr(err.Error())
			slog.Debug("http probe failed", "probe", p.name, "target", t.Name, "url", t.URL, "status", status, "err", err)
		} else {
			result.RTTs = append(result.RTTs, rtt)
		}
		result.HTTPSamples = append(result.HTTPSamples, sample)
		if n < count-1 {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(p.spacing):
			}
		}
	}
	if result.LossCount == result.Sent && lastErr != nil {
		return result, fmt.Errorf("http: all %d requests failed: %w", result.Sent, lastErr)
	}
	return result, nil
}

// one issues a single request. Returns RTT, status code (0 if no response was
// received), and any error. A non-2xx/3xx response returns a non-nil error but
// the status code is still reported so the UI can distinguish 404 from TCP
// refused.
func (p *HTTP) one(ctx context.Context, client *http.Client, url string) (time.Duration, int, error) {
	var firstByte time.Time
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}
	reqCtx, cancel := context.WithTimeout(httptrace.WithClientTrace(ctx, trace), p.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("User-Agent", UserAgent)

	// The jar lives for this one request. clientFor's clients are shared
	// across targets and goroutines, so a jar on one would be state a probed
	// host writes and every other target reads; and a jar surviving the cycle
	// would make the first sample pay the challenge and the rest skip it,
	// which measures our own cache rather than the target. The copy shares the
	// Transport, so the connection pool is untouched.
	perRequest := *client
	perRequest.Jar = NewCookieJar()

	start := time.Now()
	resp, err := perRequest.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain a bounded amount so the transport can pool the connection.
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)

	rtt := time.Since(start)
	if !firstByte.IsZero() {
		rtt = firstByte.Sub(start)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return rtt, resp.StatusCode, fmt.Errorf("status %d", resp.StatusCode)
	}
	return rtt, resp.StatusCode, nil
}
