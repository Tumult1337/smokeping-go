package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A challenge answers the first request with a 307 and a cookie, and serves
// the page only once that cookie comes back. Before the jar existed the probe
// recorded the 307 itself as a reachable target: 307 is inside [200, 400), so
// an operator saw a healthy series for a page the probe never reached.
func TestProbeSolvesASameHostCookieChallenge(t *testing.T) {
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("challenge"); err != nil {
			http.SetCookie(w, &http.Cookie{Name: "challenge", Value: "solved", Path: "/"})
			http.Redirect(w, r, r.URL.Path, http.StatusTemporaryRedirect)
			return
		}
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := NewHTTP("http", 5*time.Second, false)
	res, err := p.Probe(context.Background(), Target{Name: "t", URL: srv.URL}, 1)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if res.LossCount != 0 {
		t.Fatalf("loss %d, want 0", res.LossCount)
	}
	if got := res.HTTPSamples[0].Status; got != http.StatusOK {
		t.Fatalf("status %d, want 200 — the probe stopped at the challenge", got)
	}
	if got := served.Load(); got != 1 {
		t.Fatalf("the page was served %d times, want 1", got)
	}
}

// The jar is per request, so the second request of a cycle re-solves the
// challenge instead of skipping it. Two samples that measure different work
// are two samples that cannot share a percentile.
func TestTheCookieJarDoesNotSurviveTheRequest(t *testing.T) {
	var challenges, solved atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("challenge"); err == nil {
			solved.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		challenges.Add(1)
		http.SetCookie(w, &http.Cookie{Name: "challenge", Value: "solved", Path: "/"})
		http.Redirect(w, r, r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	p := NewHTTP("http", 5*time.Second, false)
	p.spacing = time.Millisecond
	res, err := p.Probe(context.Background(), Target{Name: "t", URL: srv.URL}, 2)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if res.Sent != 2 || res.LossCount != 0 {
		t.Fatalf("sent %d loss %d, want 2 and 0", res.Sent, res.LossCount)
	}
	// The count of requests *carrying* the cookie is 2 under either jar
	// lifetime and separates nothing. The count of challenges *issued* is the
	// observable: a jar surviving the cycle solves the second request without
	// one.
	if got := challenges.Load(); got != 2 {
		t.Fatalf("the target issued %d challenges, want 2 — a jar outlived its request", got)
	}
	if got := solved.Load(); got != 2 {
		t.Fatalf("%d requests reached the page, want 2", got)
	}
}

// A responder must not choose the next host the probe connects to. The
// redirect is surfaced as the 3xx it is rather than as loss, which is what
// this probe recorded for a redirecting target before it followed anything.
func TestProbeStopsAtACrossHostRedirect(t *testing.T) {
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()

	// Both listeners are on 127.0.0.1 and differ only by port, which is why
	// the policy compares URL.Host rather than the hostname.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer target.Close()

	p := NewHTTP("http", 5*time.Second, false)
	res, err := p.Probe(context.Background(), Target{Name: "t", URL: target.URL}, 1)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if got := res.HTTPSamples[0].Status; got != http.StatusFound {
		t.Fatalf("status %d, want 302 — the redirect was followed", got)
	}
	if got := elsewhere.Load(); got != 0 {
		t.Fatalf("the redirect target was reached %d times, want 0", got)
	}
	if res.LossCount != 0 {
		t.Fatalf("loss %d, want 0 — a redirecting host is serving normally", res.LossCount)
	}
}

// A chain that never converges is a page the probe cannot reach, so it counts
// as loss. Four requests is the original plus MaxHTTPRedirects hops.
func TestProbeStopsAfterMaxHTTPRedirects(t *testing.T) {
	var hits atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, srv.URL+"/next"+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()

	p := NewHTTP("http", 5*time.Second, false)
	res, _ := p.Probe(context.Background(), Target{Name: "t", URL: srv.URL}, 1)
	if res.LossCount != 1 {
		t.Fatalf("loss %d, want 1 — an unbounded chain read as reachable", res.LossCount)
	}
	// A literal, not MaxHTTPRedirects+1: an expectation derived from the
	// constant it guards holds for every value of that constant.
	if got := hits.Load(); got != 4 {
		t.Fatalf("the server saw %d requests, want 4", got)
	}
	if MaxHTTPRedirects != 3 {
		t.Fatalf("MaxHTTPRedirects = %d; the literal above pins 3", MaxHTTPRedirects)
	}
}

// 301, 302 and 303 make net/http rewrite a POST into a bodyless GET. Following
// one would ship an empty request that the master answers 405 to, which the
// slave records as a delivered batch.
func TestCheckRedirectStopsWhenNetHTTPRewritesTheMethod(t *testing.T) {
	post, err := http.NewRequest(http.MethodPost, "https://master.test/api/v1/cluster/cycles", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := http.NewRequest(http.MethodGet, "https://master.test/api/v1/cluster/cycles", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := CheckRedirect(rewritten, []*http.Request{post}); got != http.ErrUseLastResponse {
		t.Fatalf("CheckRedirect = %v, want ErrUseLastResponse — the body would be dropped", got)
	}
	// The positive counterpart: the same host and the same method is followed,
	// so the refusal above is the method check and not a blanket refusal.
	kept, err := http.NewRequest(http.MethodPost, "https://master.test/elsewhere", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if got := CheckRedirect(kept, []*http.Request{post}); got != nil {
		t.Fatalf("CheckRedirect = %v, want nil for a same-host same-method hop", got)
	}
}

// net/http/cookiejar bounds nothing, and Set-Cookie is written by the
// responder. The rotation case is the one a naive counter fails: a challenge
// cookie is reissued on every response, so counting each reissue as a new
// entry exhausts the budget and then refuses the one cookie the caller needs.
func TestBoundedJarCapsDistinctKeysAndStillAdmitsRotation(t *testing.T) {
	jar := NewCookieJar()
	u, err := url.Parse("https://target.test/")
	if err != nil {
		t.Fatal(err)
	}

	const flooded = 200
	if MaxJarCookies >= flooded {
		t.Fatalf("MaxJarCookies = %d; the fixture floods only %d and cannot reach the cap", MaxJarCookies, flooded)
	}
	flood := make([]*http.Cookie, 0, flooded)
	for i := range flooded {
		flood = append(flood, &http.Cookie{Name: "c" + itoa(i), Value: "v", Path: "/"})
	}
	jar.SetCookies(u, flood)
	// A literal, for the reason the redirect depth is one.
	if got := len(jar.Cookies(u)); got != 64 {
		t.Fatalf("jar holds %d cookies, want 64 (MaxJarCookies = %d)", got, MaxJarCookies)
	}

	// c0 was admitted before the cap was reached; reissuing it must update the
	// value rather than be refused as a new key.
	jar.SetCookies(u, []*http.Cookie{{Name: "c0", Value: "rotated", Path: "/"}})
	if got := len(jar.Cookies(u)); got != 64 {
		t.Fatalf("after rotation the jar holds %d cookies, want 64", got)
	}
	for _, c := range jar.Cookies(u) {
		if c.Name == "c0" {
			if c.Value != "rotated" {
				t.Fatalf("c0 = %q, want the rotated value", c.Value)
			}
			return
		}
	}
	t.Fatal("the rotated cookie was dropped")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// The master client shares one jar between its push loop and its config
// refresh loop, so the key map is written from two goroutines. Sequential
// tests cannot see that: this one drives the contended path.
func TestBoundedJarIsSafeUnderConcurrentUse(t *testing.T) {
	jar := NewCookieJar()
	u, err := url.Parse("https://target.test/")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 64 {
				jar.SetCookies(u, []*http.Cookie{{Name: "c" + itoa(g*64+i), Value: "v", Path: "/"}})
				_ = jar.Cookies(u)
			}
		}()
	}
	wg.Wait()
	if got := len(jar.Cookies(u)); got != 64 {
		t.Fatalf("jar holds %d cookies, want 64", got)
	}
}

// The budget is charged on cookiejar's own storage id, not on the wire bytes.
// A Set-Cookie carrying no Path attribute takes its path from the request, so
// keyed on the raw fields one budget slot bought an inner entry per request
// path — a responder picking its own Location values grew the jar without end.
func TestTheJarCapCannotBeBypassedByOmittingThePath(t *testing.T) {
	jar := NewCookieJar()
	for i := range 300 {
		u, err := url.Parse("https://target.test/p" + itoa(i) + "/x")
		if err != nil {
			t.Fatal(err)
		}
		jar.SetCookies(u, []*http.Cookie{{Name: "same", Value: "v"}})
	}
	// Every path is its own cookiejar entry, so the cap has to see 300
	// distinct keys and admit 64 of them.
	held := 0
	for i := range 300 {
		u, err := url.Parse("https://target.test/p" + itoa(i) + "/x")
		if err != nil {
			t.Fatal(err)
		}
		held += len(jar.Cookies(u))
	}
	if held != 64 {
		t.Fatalf("the jar holds %d entries under a cap of 64", held)
	}
}

// Every field of a cookie is responder-written, so an entry cap alone bounds
// no memory: 64 entries of a megabyte each is 64 MB retained for the life of
// the master client's jar.
func TestTheJarRefusesACookiePastMaxJarCookieBytes(t *testing.T) {
	jar := NewCookieJar()
	u, err := url.Parse("https://target.test/")
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "huge", Value: strings.Repeat("A", 100000), Path: "/"}})
	if got := len(jar.Cookies(u)); got != 0 {
		t.Fatalf("the jar took %d oversized cookies, want 0", got)
	}
	// The positive counterpart: an ordinary cookie is still admitted, so the
	// refusal above is the ceiling and not a jar that stores nothing.
	jar.SetCookies(u, []*http.Cookie{{Name: "ok", Value: "v", Path: "/"}})
	if got := len(jar.Cookies(u)); got != 1 {
		t.Fatalf("the jar holds %d ordinary cookies, want 1", got)
	}
}

// net/http strips Authorization and Cookie only when the host changes, and
// this policy requires the hosts to be equal, so nothing else keeps the
// cluster bearer token off a plaintext hop.
func TestCheckRedirectRefusesAnHTTPSDowngrade(t *testing.T) {
	secure, err := http.NewRequest(http.MethodGet, "https://master.test/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := http.NewRequest(http.MethodGet, "http://master.test/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := CheckRedirect(plain, []*http.Request{secure}); got != http.ErrUseLastResponse {
		t.Fatalf("CheckRedirect = %v, want ErrUseLastResponse for an https-to-http hop", got)
	}
	// The upgrade is the case the port-less host match exists for, and it must
	// still be followed.
	if got := CheckRedirect(secure, []*http.Request{plain}); got != nil {
		t.Fatalf("CheckRedirect = %v, want nil for an http-to-https upgrade", got)
	}
}
