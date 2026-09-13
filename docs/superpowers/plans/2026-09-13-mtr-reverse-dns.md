# MTR reverse-DNS labels implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (- [ ]) syntax for tracking.

**Goal:** Resolve, cache, and display PTR names for MTR hops in the path table and history tooltip without weakening health-address redaction or allowing unbounded DNS work.

**Architecture:** Add a bounded reverse-DNS LRU/singleflight component owned by api.Server, enrich the post-redaction hop payloads with one top-level IP-to-hostname map, and consume that map at the two existing React display sites. Resolution is best-effort and request-bounded; storage models and schemas remain unchanged.

**Tech Stack:** Go 1.26 standard library (net.Resolver, net/netip, container/list, context, sync), Chi API, React 19, TypeScript 6, Vite 8.

## Global constraints

- Resolve only post-redaction rows. Empty addresses and the redacted sentinel must never reach the resolver.
- Cache keys are canonical netip.Addr values; response keys preserve the exact IP spelling served in each hop row.
- Positive TTL is 24 hours; negative TTL is 5 minutes; completed LRU capacity is 4,096.
- Run no more than eight DNS lookups server-wide, schedule no more than 128 uncached addresses per response, and stop enrichment after two seconds.
- Coalesce duplicate concurrent misses and propagate cancellation to active lookups.
- PTR output is one trailing-dot-trimmed, UTF-8, control-free name of at most 253 bytes.
- DNS failure omits only the optional label. It must not change hop HTTP status or remove the IP.
- Both API responses always encode a hostnames object; TypeScript treats the field as optional for older-server compatibility.
- Do not add dependencies, schema changes, persisted PTR data, config knobs, background refresh, or forward-confirmed reverse DNS.

---

### Task 1: Bounded reverse-DNS cache

**Files:**
- Create: internal/api/reversedns.go
- Create: internal/api/reversedns_test.go

**Interfaces:**
- Consumes: func(context.Context, string) ([]string, error), matching net.Resolver.LookupAddr.
- Produces: newReverseDNSCache(lookup reverseLookupFunc) *reverseDNSCache and (*reverseDNSCache).hostnames(context.Context, []string) map[string]string for Task 2.

- [ ] **Step 1: Write failing cache and normalization tests**

Before each test, name the production break it catches: repeated lookup despite a live entry, expired result served, wrong canonical key, invalid PTR text published, or oldest entry retained past capacity.

Create internal/api/reversedns_test.go with a fake lookup and this first contract:

~~~go
func TestReverseDNSCachesPositiveAndNegativeResults(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int64
	cache := newReverseDNSCache(func(_ context.Context, addr string) ([]string, error) {
		calls.Add(1)
		if addr == "192.0.2.1" {
			return []string{"edge.example.net."}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
	})
	cache.now = func() time.Time { return now }

	want := map[string]string{"192.0.2.1": "edge.example.net"}
	for range 2 {
		got := cache.hostnames(context.Background(), []string{"192.0.2.1", "192.0.2.2"})
		if !maps.Equal(got, want) {
			t.Fatalf("hostnames = %#v, want %#v", got, want)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("lookup calls = %d, want one positive and one negative", got)
	}
}
~~~

Add focused tests named:

~~~go
TestReverseDNSPositiveEntryExpiresAfter24Hours
TestReverseDNSNegativeEntryExpiresAfter5Minutes
TestReverseDNSCanonicalizesMappedIPv4AndDropsZones
TestReverseDNSEvictsTheLeastRecentlyUsedCompletedEntry
TestReverseDNSRejectsInvalidPTRNames
~~~

Use cache.maxEntries = 2 for eviction. The invalid-name table includes empty text, invalid UTF-8, a newline, DEL, and 254 ASCII bytes, then proves a later usable name in the same resolver answer is selected. Derive expected maps as literals, never through production normalization.

- [ ] **Step 2: Run the new tests and verify RED**

Run:

~~~bash
go test ./internal/api -run '^TestReverseDNS(Caches|Positive|Negative|Canonicalizes|Evicts|Rejects)' -count=1
~~~

Expected: build failure because newReverseDNSCache does not exist. Fix only test compilation mistakes; retain a feature-missing failure.

- [ ] **Step 3: Implement canonicalization, PTR validation, TTLs, and LRU**

Create internal/api/reversedns.go with:

~~~go
const (
	reverseDNSMaxEntries           = 4096
	reverseDNSMaxConcurrent        = 8
	reverseDNSMaxMissesPerResponse = 128
	reverseDNSResponseTimeout      = 2 * time.Second
	reverseDNSPositiveTTL          = 24 * time.Hour
	reverseDNSNegativeTTL          = 5 * time.Minute
)

type reverseLookupFunc func(context.Context, string) ([]string, error)

type reverseDNSEntry struct {
	addr    netip.Addr
	name    string
	expires time.Time
}

type reverseDNSCall struct {
	done chan struct{}
	name string
}

type reverseDNSCache struct {
	lookup reverseLookupFunc
	now    func() time.Time
	limit  chan struct{}

	mu         sync.Mutex
	entries    map[netip.Addr]*list.Element
	order      *list.List
	inflight   map[netip.Addr]*reverseDNSCall
	maxEntries int
}
~~~

Implement canonicalHopAddr with netip.ParseAddr, Unmap, and WithZone(""). Implement usablePTRName by iterating answers in order, removing one trailing root dot, and rejecting empty, invalid UTF-8, over-253-byte, ASCII-control, and DEL-containing names.

Implement locked LRU helpers with order.Front() as most recently used. A live positive or negative entry is a hit; remove an expired entry before reporting a miss. Insertions update an existing element by identity, move it to the front, and evict from the back until len(entries) <= maxEntries.

Implement hostnames in two passes:

1. Parse and deduplicate every exact input string into insertion-ordered canonical-address groups.
2. Read every completed cache hit first.
3. Resolve at most reverseDNSMaxMissesPerResponse missing canonical addresses beneath context.WithTimeout(ctx, reverseDNSResponseTimeout), using no more than reverseDNSMaxConcurrent request workers.
4. Populate every exact spelling in a canonical group when resolution succeeds and always return a non-nil map.

- [ ] **Step 4: Run cache tests and verify GREEN**

Run:

~~~bash
gofmt -w internal/api/reversedns.go internal/api/reversedns_test.go
go test ./internal/api -run '^TestReverseDNS(Caches|Positive|Negative|Canonicalizes|Evicts|Rejects)' -count=1
~~~

Expected: both commands exit 0.

- [ ] **Step 5: Write failing concurrency, admission, and cancellation tests**

Add this coalescing test, with the real cache around only the external lookup fake:

~~~go
func TestReverseDNSCoalescesOneAddress(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	cache := newReverseDNSCache(func(ctx context.Context, _ string) ([]string, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return []string{"edge.example.net."}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})

	const callers = 16
	results := make(chan map[string]string, callers)
	for range callers {
		go func() {
			results <- cache.hostnames(context.Background(), []string{"192.0.2.1"})
		}()
	}
	<-started
	close(release)
	for range callers {
		if got := <-results; got["192.0.2.1"] != "edge.example.net" {
			t.Fatalf("joined result = %#v", got)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("lookup calls = %d, want 1", got)
	}
}
~~~

Add:

~~~go
TestReverseDNSRunsAtMostEightLookupsGlobally
TestReverseDNSSchedulesAtMost128MissesButReturnsAllCacheHits
TestReverseDNSReturnsCompletedNamesAtResponseDeadline
TestReverseDNSCancellationStopsActiveLookups
~~~

For the global test, start more than eight distinct lookups, count active and peak calls atomically, wait on channels until eight have entered, and prove the ninth does not enter before release. For the 128 test, prewarm an address placed after 128 uncached inputs; its cached name must still return while the call count increases by exactly 128. Synchronize with channels and contexts, not sleeps.

- [ ] **Step 6: Run concurrency tests and verify RED**

Run:

~~~bash
go test ./internal/api -run '^TestReverseDNS(Coalesces|Runs|Schedules|Returns|Cancellation)' -count=1
~~~

Expected: at least coalescing or a resource-bound assertion fails until the singleflight/admission path exists.

- [ ] **Step 7: Implement bounded singleflight resolution**

Implement synchronous resolve(ctx, addr) string in this exact order:

1. Under mu, return a live entry or join an existing reverseDNSCall by waiting on done or ctx.Done().
2. If neither exists, release mu, acquire one token from limit or return on cancellation, then recheck cache and in-flight state under mu.
3. If another leader won the race, release the token and join it. Otherwise register the call while holding the token, perform exactly one lookup(ctx, addr.String()), validate one name, and choose the positive or negative TTL.
4. Under mu, insert the completed entry, set the call result, delete only the identical in-flight call, and close done. Release the token on every path.

Token-before-registration plus the second check is required: distinct in-flight leaders stay bounded by eight, and same-key racers still coalesce. Never hold mu while waiting for admission, resolver I/O, another call, or worker output.

- [ ] **Step 8: Run all reverse-DNS tests and the race detector**

Run:

~~~bash
gofmt -w internal/api/reversedns.go internal/api/reversedns_test.go
go test ./internal/api -run '^TestReverseDNS' -count=1
go test -race ./internal/api -run '^TestReverseDNS' -count=1
~~~

Expected: exit 0 with no race report or leaked-test timeout.

- [ ] **Step 9: Commit the cache**

~~~bash
git add internal/api/reversedns.go internal/api/reversedns_test.go
git commit -m "feat(api): add bounded reverse DNS cache"
~~~

---

### Task 2: Enrich both hop API responses after redaction

**Files:**
- Modify: internal/api/api.go
- Modify: internal/api/api_test.go

**Interfaces:**
- Consumes: newReverseDNSCache and reverseDNSCache.hostnames from Task 1.
- Produces: top-level hostnames map[string]string on both hop endpoints and Options.ReverseDNSLookup for deterministic tests.

- [ ] **Step 1: Write failing endpoint and redaction-order tests**

Add withReverseDNSLookup to set Options.ReverseDNSLookup. Add TestGetHopsCarriesReverseDNSNames with two literal addresses and assert:

~~~go
want := map[string]string{
	"192.0.2.1": "edge.example.net",
	"192.0.2.2": "core.example.net",
}
if !maps.Equal(body.Hostnames, want) {
	t.Fatalf("hostnames = %#v, want %#v", body.Hostnames, want)
}
~~~

Add analogous TestGetHopsTimelineCarriesReverseDNSNames with a repeated address, proving one response-map key. Add TestHopResponsesEncodeEmptyHostnameObject and assert raw JSON contains "hostnames":{} rather than null or omission.

Add TestGetHopsReverseDNSRunsAfterHealthRedaction: a health path has one intermediate and one terminal address; the fake sees only the intermediate and no terminal map key. Add TestGetHopsTimelineReverseDNSSkipsAllRedactedHealthAddresses: the fake receives zero calls and the response map is empty.

- [ ] **Step 2: Run endpoint tests and verify RED**

~~~bash
go test ./internal/api -run '^Test(GetHops.*ReverseDNS|HopResponsesEncode)' -count=1
~~~

Expected: build failure because Options.ReverseDNSLookup and the response key do not exist.

- [ ] **Step 3: Wire one cache into the server lifetime**

Append reverseDNS *reverseDNSCache to Server after startAt. Append this exact
field to Options after Version:

~~~go
// ReverseDNSLookup overrides net.DefaultResolver.LookupAddr in tests.
ReverseDNSLookup func(context.Context, string) ([]string, error)
~~~

In New, default nil to net.DefaultResolver.LookupAddr and construct newReverseDNSCache exactly once. Add the net import. Do not build a cache per handler call.

- [ ] **Step 4: Enrich post-redaction rows**

Add:

~~~go
func hopIPs(hops []storage.HopPoint) []string {
	ips := make([]string, 0, len(hops))
	for _, hop := range hops {
		ips = append(ips, hop.IP)
	}
	return ips
}
~~~

Immediately after the existing redactTerminalHops branch in getHops, call:

~~~go
hostnames := s.reverseDNS.hostnames(r.Context(), hopIPs(hops))
~~~

Add hostnames to the response object. Repeat after redactAllHopAddresses in getHopsTimeline and before DTO conversion. Never pass the pre-redaction res.Hops slice.

- [ ] **Step 5: Run endpoint and existing redaction tests**

~~~bash
gofmt -w internal/api/api.go internal/api/api_test.go
go test ./internal/api -run '^(TestGetHops|TestHopResponsesEncode)' -count=1
go test -race ./internal/api -run '^(TestGetHops.*ReverseDNS|TestHopResponsesEncode)' -count=1
~~~

Expected: exit 0, including every existing health-address test.

- [ ] **Step 6: Commit API enrichment**

~~~bash
git add internal/api/api.go internal/api/api_test.go
git commit -m "feat(api): include reverse DNS names with hops"
~~~

---

### Task 3: Display hostname and IP in both MTR views

**Files:**
- Modify: ui/src/api.ts
- Modify: ui/src/HopsTable.tsx
- Modify: ui/src/MtrSection.tsx
- Modify: ui/src/MtrHeatmap.tsx
- Modify: ui/src/styles.css

**Interfaces:**
- Consumes: optional hostnames response map from Task 2.
- Produces: hostname-over-IP table labels and hostname-plus-IP history tooltips, with IP-only fallback.

- [ ] **Step 1: Extend the rolling-compatible API types**

Add this field to HopsResponse and HopsTimelineResponse:

~~~ts
// Optional while a newly built UI may still be served by an older backend.
hostnames?: Record<string, string>;
~~~

Keep HopPoint unchanged so timeline rows never repeat names.

- [ ] **Step 2: Thread names through latest and pinned paths**

In HopsTable, add hostnames state initialized to an empty object. Clear it when keyChanged, set it from r.hostnames ?? {}, and pass it to every HopsPath.

Extend HopsPath with:

~~~ts
hostnames: Readonly<Record<string, string>>;
~~~

In MultiSourceLayout in MtrSection.tsx, mirror that lifecycle for its direct getHops call and pass the map to HopsPath. The single-source branch already uses HopsTable.

- [ ] **Step 3: Render hostname over IP and preserve the IP lookup link**

Replace the non-empty-IP contents in HopsPath with a hop-host wrapper. Render hostnames[h.IP] in hop-hostname when present. Beneath it, keep the existing hop-ip text and bgp.tools link together in hop-address. The href, title, rel, and target remain based on h.IP. Keep existing no-reply and unreachable branches unchanged.

Add:

~~~css
.hops-table .hop-host {
  display: inline-flex;
  flex-direction: column;
  align-items: flex-start;
  line-height: 1.25;
}
.hops-table .hop-hostname { color: var(--text); }
.hops-table .hop-address {
  display: inline-flex;
  align-items: baseline;
}
~~~

Do not hide the IP or make hover the only hostname disclosure.

- [ ] **Step 4: Thread names into the heatmap tooltip**

In MtrHeatmap, add hostnames state. Clear it on a series-key change, set it from r.hostnames ?? {}, and pass it to PathHeatmap as Readonly<Record<string, string>>.

Before the current IP/no-reply tooltip line, render:

~~~tsx
{hostnames[hover.p.IP] && (
  <div className="mtr-heatmap-tip-hostname">{hostnames[hover.p.IP]}</div>
)}
<div>{hover.p.IP || "no reply"}</div>
~~~

Style mtr-heatmap-tip-hostname with color: var(--text). Keep pointer-events and click behavior unchanged.

- [ ] **Step 5: Build the UI**

Run from ui/:

~~~bash
npm run build
~~~

Expected: palette checks, tsc -b, and Vite production build exit 0. Keep the generated internal/ui/dist output; never edit it by hand.

- [ ] **Step 6: Commit the UI**

~~~bash
git add ui/src/api.ts ui/src/HopsTable.tsx ui/src/MtrSection.tsx ui/src/MtrHeatmap.tsx ui/src/styles.css internal/ui/dist
git commit -m "feat(ui): show reverse DNS names for MTR hops"
~~~

---

### Task 4: Documentation and full verification

**Files:**
- Modify: README.md

**Interfaces:**
- Consumes: assembled API and UI.
- Produces: documented additive contract and verified binary.

- [ ] **Step 1: Document the response field**

Near the current hop endpoint notes, add:

~~~markdown
Both hop responses include a hostnames object mapping each resolved hop IP to
its cached PTR name. Missing names are omitted; hop rows retain their IP.
Addresses redacted from _cluster hop rows are not resolved.
~~~

- [ ] **Step 2: Format and check imports**

~~~bash
gofmt -w internal/api/api.go internal/api/api_test.go internal/api/reversedns.go internal/api/reversedns_test.go
goimports -w internal/api/api.go internal/api/api_test.go internal/api/reversedns.go internal/api/reversedns_test.go
git diff --check
~~~

Expected: exit 0. If goimports is unavailable, report that limitation and retain the gofmt result.

- [ ] **Step 3: Run Go verification**

Run separately and record every exit status:

~~~bash
go test ./internal/api -count=1
go test -race ./internal/api -count=1
go build ./...
go test ./...
go vet ./...
~~~

Expected: exit 0 for each. Do not claim the ClickHouse integration suite ran unless CLICKHOUSE_ADDR is configured and the command was executed.

- [ ] **Step 4: Run frontend verification**

From ui/:

~~~bash
npm run build
~~~

Expected: exit 0 from palette checks, TypeScript, and Vite.

- [ ] **Step 5: Check language-server diagnostics**

~~~bash
gopls check internal/api/api.go internal/api/reversedns.go internal/api/api_test.go internal/api/reversedns_test.go
~~~

Expected: exit 0 and no diagnostics. If gopls is unavailable, report it.

- [ ] **Step 6: Perform browser checks when local data and tooling exist**

Verify latest and historical paths in both themes, hover a history cell, click it to pin the path, confirm IP-only fallback, and check a 375px viewport. If no ClickHouse-backed local instance or browser tooling is available, report these checks as unverified.

- [ ] **Step 7: Review security mutations**

Confirm named tests fail conceptually for deletion of either handler's redaction-before-resolution order, the 128-miss cap, eight-call limiter, negative insertion, PTR validation, or LRU eviction. Add a focused test before completion if any deletion is not covered.

- [ ] **Step 8: Commit documentation or verification fixes**

~~~bash
git add README.md
git commit -m "docs: document MTR reverse DNS responses"
~~~

Omit this commit if README.md required no edit; never create an empty commit.

## Rollback

Revert implementation commits in reverse order. The server then stops emitting hostnames, and optional TypeScript fields preserve IP-only rendering during a mixed-version rollout. No ClickHouse, config, or generated-data rollback is required.
