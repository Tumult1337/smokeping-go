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
