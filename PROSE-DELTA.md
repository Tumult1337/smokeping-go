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
