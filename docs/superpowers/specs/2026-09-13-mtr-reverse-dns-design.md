# MTR reverse-DNS labels

**Status:** approved 2026-09-13

## Problem

The MTR path table and history tooltip identify a responding hop only by its IP
address. Operators must leave the page or recognize the address to understand
which router answered. The server should resolve PTR names once, cache them,
and send the names with both hop API responses so the UI can identify a hop
without issuing DNS requests from the browser.

This change adds best-effort reverse-DNS labels to the existing MTR views. It
does not change probing, stored hop rows, ClickHouse schemas, forward-confirm
PTR names, add configuration knobs, or expose addresses that the health-target
redaction already hides.

## Chosen approach

`GET /api/v1/targets/{group}/{name}/hops` and
`GET /api/v1/targets/{group}/{name}/hops/timeline` will gain a top-level
`hostnames` object whose keys are the exact IP strings present in that
response and whose values are normalized PTR names:

```json
{
  "hops": [{"IP": "192.0.2.1"}],
  "hostnames": {"192.0.2.1": "router.example.net"}
}
```

The map is always present and is empty when no name was found. A response-level
map avoids repeating one hostname in every bucketed timeline row. It also keeps
`storage.HopPoint` and the timeline's deliberately slim DTO unchanged. Adding a
`Hostname` field to every hop row was rejected because a stable route would
repeat the same string hundreds or thousands of times in a history response.

The path table will show the PTR name as the primary host label with the IP
directly beneath it. If no name exists, it will retain the current IP-only
presentation. The MTR-history hover tooltip will likewise show the name and IP
when available, or only the IP otherwise. Clicking a history cell continues to
pin the path table to that cycle, where the same always-visible presentation is
used. PTR names are supplementary labels: links to bgp.tools remain keyed to
the IP, and no interaction depends on a name being present.

## Server component and data flow

The API package will own one reverse-DNS cache for the lifetime of `Server`.
It accepts `netip.Addr` keys and uses an injected lookup function compatible
with `net.Resolver.LookupAddr`; production defaults to
`net.DefaultResolver.LookupAddr`. The standard library is sufficient, so no
dependency is added.

Each hop handler keeps its current order:

1. resolve and authorize the target;
2. query storage;
3. apply health-target address redaction;
4. collect unique, parseable, non-redacted addresses from the rows that will
   actually be served;
5. resolve those addresses through the shared cache;
6. write the existing payload plus `hostnames`.

Resolution after redaction is a required boundary. Empty addresses and the
timeline's `redacted` sentinel do not parse as `netip.Addr` and therefore never
reach DNS. Cache identity uses `Addr.Unmap()` with any zone removed, while the
response map retains each row's exact IP spelling so the TypeScript client can
look it up directly without reparsing addresses.

For a successful lookup, the first usable PTR name is returned. One trailing
root dot is removed for display. Empty names, invalid UTF-8, ASCII control
characters, and names longer than the DNS textual maximum of 253 bytes are
discarded. React's normal text rendering provides HTML escaping; names are
never used as markup, URLs, or identifiers.

## Cache and resource bounds

The cache is a mutex-protected LRU with at most 4,096 completed entries. A
positive PTR result lives for 24 hours. Lookup errors and valid no-name answers
are negative-cached for 5 minutes, preventing repeated history requests from
turning absent PTR records into repeated DNS traffic. Expired entries are
never served.

Concurrent misses for one canonical address coalesce onto one lookup. At most
eight resolver calls run across the whole server at once. A handler returns
cached names for every unique canonical address in the response, schedules at
most 128 uncached addresses, and spends at most two seconds total enriching
the response. It returns every name completed within that budget and omits the
rest; DNS failure never changes an otherwise valid hop response into an HTTP
error. Cached hits do not consume the new-lookup limit and are read before
scheduling misses.

The limits are fixed implementation constants rather than configuration. A
normal current path has at most 30 TTLs, while 128 allows substantial path
churn in a history window without making DNS work proportional to the
timeline's maximum 172,288 rows. The shared concurrency ceiling bounds actual
network activity across simultaneous unauthenticated requests.

## Five-lens design gate

### Problem

The smallest correct scope is API-time enrichment plus two existing UI read
sites. Persisting PTR data or resolving during probes would couple transient
DNS labels to measurement storage and is outside the requirement.

### Threat model

The hop endpoints are unauthenticated. Request parameters select a configured
target, source, and bounded time window; stored addresses can originate from
local probes or authenticated cluster slaves and remain untrusted at the API
boundary. DNS answers are an untrusted upstream response. The protected assets
are server availability, DNS egress capacity, and the `_cluster` health
addresses already covered by API redaction.

### Attacker

- Repeated cache misses could amplify a small GET into DNS work. The response
  address cap, global concurrency limit, two-second budget, bounded LRU, and
  positive/negative TTLs bound that work.
- Concurrent requests for one address could stampede the resolver. Per-address
  in-flight coalescing permits one actual lookup.
- Churned addresses could grow memory without limit. The completed-entry LRU
  is capped at 4,096 entries; in-flight work is bounded by the global limiter.
- A PTR answer could carry hostile text or excessive data. The server accepts
  one bounded, UTF-8, control-free label and the UI renders it only as escaped
  text.
- Resolving before redaction could disclose or query a protected health
  address. Both handlers resolve only their post-redaction rows, and endpoint
  tests prove the resolver never receives a redacted address.
- DNS ambiguity or failure could block hop visibility. Enrichment fails open
  only for the optional label, never for address authorization or storage: the
  response keeps the IP and omits the hostname.

### Implementation

A focused API-owned cache, an injected standard-library-shaped lookup
function, and a response helper are sufficient. No storage changes, background
goroutines, external packages, or configuration surface are introduced. All
lookup goroutines are request-bounded and joined before the response is
written.

### Compatibility and usability

Top-level additive JSON fields are compatible with existing clients. The
bundled TypeScript client treats `hostnames` as optional while rolling upgrades
may pair a new UI with an older server. The response-level map preserves the
existing hop DTOs and history payload optimization. The rejected per-row field
is marginally simpler at a single call site but worse for the larger endpoint
and storage/API separation.

## Error behavior

Storage, validation, overload, and redaction behavior remain unchanged. PTR
timeouts, resolver failures, invalid answers, and the per-response cap produce
an absent map entry, not an endpoint error. Negative results are cached without
their error text, and individual lookup failures are not logged, avoiding log
flooding and unnecessary address disclosure. Context cancellation stops new
work and propagates to active resolver calls.

## Testing and verification

Tests will be written and observed failing before production edits. Go tests
will cover positive and negative cache hits, both TTL expiries with an injected
clock, LRU eviction, canonical address identity, duplicate-miss coalescing,
global concurrency, response limits, cancellation, invalid PTR answers, and
partial results at the deadline. Handler tests will assert both hop endpoints
carry the exact hostname map and that health-target redaction prevents lookup
of protected addresses.

The TypeScript API types will model `hostnames?: Record<string, string>` for
rolling compatibility. The path table and heatmap will share one small label
renderer/helper so tests or builds cannot drift on fallback behavior. The UI
build will verify both call sites and the existing API contract.

Final verification will run formatting, focused API tests, `go build ./...`,
`go test ./...`, `go test -race ./internal/api`, `go vet ./...`, and the UI
production build. Browser verification will cover the latest and pinned path
table, history hover, missing PTR fallback, both themes, and a 375px viewport
when browser tooling is available.

## Rollback

Reverting the implementation removes the cache, `hostnames` response keys, and
UI labels. There is no schema, stored-data, configuration, or generated-code
migration to reverse. Older clients already ignore the additive response key.
