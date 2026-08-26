# Prose and cross-ownership deltas from the storage (group C) agent

Each entry names the file another agent owns (or CLAUDE.md) and the exact
change requested. Storage-side halves of these fixes are already committed on
this branch.

## C3 — `/hops?at=` relative form floods the hops cache (request to the API agent)

`internal/api/api.go`, `getHops`: the relative-duration form of `at`
(`?at=-1h`) resolves against a fresh `time.Now()`, so an identically polled
URL mints a new millisecond-precision cache key per request against the
16-entry hops LRU shared with the 7d timeline entries (pre-regression, the
60s-quantized key coalesced these for a whole minute). Storage now exports
`storage.CoalesceHopsAt(at)` (floors to the cache's 60s `to`-quantum). Please
apply it in `getHops` **only when the `at` parameter took the
relative-duration branch** — RFC3339 and unix-seconds forms are absolute pins
and must keep their precision (RFC3339 is the heatmap click-through carrying
`WorstTime` at full ms per the UI time-axis contract). `parseTimeParam` does
not currently report which branch parsed; the smallest change is to check
`strings.HasPrefix(atStr, "-") || strings.HasPrefix(atStr, "+")` *after* a
failed `ParseInt` in the handler, or to have `resolveTimeParam` return the
form. A relative pin's reference point is the server's own clock, so the
≤60s shift is inside the slack the request already carries.

Until this is wired, `storage.CoalesceHopsAt`'s only read site is its
regression test (`TestCachingReader_HopsAt_CoalescedRelativePinsShareOneEntry`).

## C9 — `maxHopRows` is now derived; CLAUDE.md's numbers change

CLAUDE.md, storage bullet, the paragraph starting "`clickhouse.maxHopRows`
(485,280) covers the two pinned reads": `maxHopRows` is now
`maxHopSources (512, mirroring master's maxRegisteredSlaves, pinned by
TestMaxHopSourcesMirrorsTheMasterRegistry) x cluster.MaxHopsPerCycle (600)`
= **307,200**. The old 485,280 was the orphaned pre-split timeline formula
(674 x 30 x 6 x 4) with a post-hoc 808-source rationalisation. Replace the
paragraph's numbers: "so the cap clears 808 sources — above the 512 live
names" becomes "one cycle per source across the 512 names the registry admits
at once, exactly". In the "Parsing is not bounding" paragraph, the worst-case
byte ceiling "485,280 x 76 B ~= 36.9 MB, where a zone of 256 would have made
it ~=146 MB" becomes "307,200 x 76 B ~= 23.3 MB (a zone of 256 would have
made it ~=92 MB)".

Note the cap bounds *live-registry* sources by derivation; sources swept from
the registry keep rows in probe_hop for its retention, so a pinned read can
in principle name more than 512 historical sources — reaching the cap is
still reported as ErrHopsTruncated -> 400, never truncated, exactly as
before (the old literal had the same property at a higher number with no
derivation at all).

## C11 — `stats.PercentileSet`'s doc claims iteration that does not happen

`internal/stats/percentiles.go` (owned elsewhere; storage only adds the
guard): the sentence "The writer and reader iterate this list to build the
ClickHouse column set and the per-bucket quantilesExactWeighted rollup." is
false — the writer INSERT, raw SELECT, bucketed rollup and DTO are four
hand-named lists. Replace it with:

> Cluster ingest walks it to bound every percentile, and
> `clickhouse.TestCyclePercentileColumnsFollowPercentileSet` fails whichever
> of the four hand-named column lists (writer INSERT, raw and bucketed
> SELECTs, `storage.CyclePoint`) misses or outgrows an entry.

CLAUDE.md's ingest-bounds sentence ("walked through stats.PercentileSet so a
new percentile is covered the day it is added") is about `boundSummary` and
stays true as written.

Known limit of the guard: the raw SELECT's *scan destination list* cannot be
checked without a live server (the fake conn returns no rows), so a column
added to the SELECT but not to Scan still only fails against ClickHouse —
same seam the skill already documents.

## C12 — cache Stats() wiring (request to the API agent), and the DTO clone

- `CachingReader.Stats()` / `storage.CacheStats` (internal/storage/cache.go)
  has no production read site — repo rule says wire it or delete it. Request:
  serve it on `/api/v1/health` next to `writer_drops` (e.g. a `cache` object
  with `cycles_hits/cycles_misses/hops_hits/hops_misses`). If the API side
  would rather not carry the field, delete `Stats()`, `CacheStats`, and the
  three `TestCachingReader_Stats_*` tests together — half-deleting leaves the
  same dead surface.
- `internal/api`'s `cycleLossDTO` clones `storage.CycleCounters`
  field-for-field; consider serializing `storage.CycleCounters` directly (it
  is already the wire shape) or deriving the DTO in one place. Storage cannot
  fix this side.
- For the record: the group-C finding also named `queryCycleCounters` as
  missing from the placeholder-parity guard; it already has
  `TestCycleCounterQueryPlaceholdersMatchArgs`, so only `QueryOverview` was
  actually uncovered and is now covered.

## C8 — CI gate lacks -race

`make test` and `.github/workflows/build.yml` run `go test` without `-race`,
so every race-detector-reliant test proves nothing in CI. Request (CI/Make
are outside storage ownership): run
`go test -race ./internal/storage/... ./internal/probe/... ./internal/cluster/... ./internal/alert/... ./internal/scheduler/...`
(the concurrency-sensitive packages) in the gate, matching the global rule
that concurrency-sensitive code is tested with -race.
