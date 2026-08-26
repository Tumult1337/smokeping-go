# CLAUDE.md prose deltas (group A)

Each entry names the CLAUDE.md text to change and the replacement. Not applied
directly because CLAUDE.md is shared across concurrent agents.

## ICMP cycle budget bullet (A1 + A5)

Replace:

> Both gate on the config defining an icmp probe; neither binds a config with
> none.

with:

> `config.Validate` gates on the config defining an icmp probe *or* on a
> cluster master (a cluster block with a token), because the health mesh
> injects `slavehealth.ProbeDef`'s icmp probe at scheduler-build time — gating
> on the stored map alone let a validated master config become unbuildable
> fleet-wide at the first slave registration. `probe.Build` gates on the map
> it actually receives, which on both master and slave is the post-injection
> one. A standalone config with no icmp probe is still not bound by the
> budget, but `config.ValidatePingCount` bounds `pings` for **every**
> schedule at `config.MaxPingsPerCycle`: the scheduler stamps
> `Sent = cfg.Pings` on any probe error, so an unbounded count produced
> cycles cluster ingest refuses (`sent` past its UInt16 column) and each
> refusal dropped the slave's whole drained batch.

## ICMP sockets bullet (A2)

Replace:

> so `sendOne` matches replies by **sequence number only**, not ID. Don't
> "fix" this — it's correct for both socket types.

with:

> so `sendOne` matches replies by **sequence number only**, not ID — don't
> "fix" that, it's correct for both socket types — plus the same
> peer-is-the-resolved-destination check `matchDatagram` applies on the walk
> (`matchEchoReply`, the echo read path's trust boundary): any on-path router
> can see seq and answer from its own address, which made a fully-down target
> read 0% loss with plausible RTTs.

## Path discovery bullet (A3)

Replace:

> Rows are per `(ttl, responder address)` in first-seen order, so ECMP
> siblings each carry their own samples;

with:

> Rows are per `(ttl, responder address, echo-vs-error)` in first-seen order,
> so ECMP siblings each carry their own samples and a responder that both
> echoes and rejects (a rate-limiting firewall answering admin-prohibited from
> the target's own address) yields two rows — mixed onto one, the
> unreachable's error-generation time rode the `TargetReply` marker into MTR's
> RTT mirror and became the target's percentiles, with `len(RTTs)` exceeding
> `Sent−LossCount`. The per-round one-responder-per-TTL bound is unchanged, so
> `MaxHopRowsPerCycle`'s rounds × TTLs derivation still holds;

## "A cycle that sent nothing" bullet (A4)

Replace:

> and MTR's `Sent` is the rounds that actually sent — zero when `traceHops`'
> resolve, which takes no context, spends the cycle deadline before the first
> probe goes out.

with:

> and MTR's `Sent` is the rounds that actually sent — zero when `traceHops`'
> resolve spends the cycle deadline before the first probe goes out
> (`probe.resolveIPAddr` honors the context, so the resolve now fails *at*
> the deadline instead of overrunning it).

## Path discovery bullet, addition (A4)

After the sentence ending "…the walk keeps the whole interval." (ICMP cycle
budget context) or in the path-discovery bullet, add:

> Resolution inside the walk and the icmp echo path goes through
> `probe.resolveIPAddr` — `net.ResolveIPAddr` semantics with the context
> honored. The trace goroutine is joined by an unconditional defer on every
> `ICMP.Probe` return path, so a resolver call that ignored a cancelled cycle
> blocked shutdown (`Scheduler.Run`'s `wg.Wait`, `RunLifecycle`'s
> `<-schedDone`) and every SIGHUP rebuild for the resolver's own timeout —
> tens of seconds per hostname-addressed target against a blackholed
> nameserver.

## Address-family pinning bullet (A4)

Replace:

> ICMP/MTR via `net.ResolveIPAddr("ip"|"ip4"|"ip6")` (shared `traceHops`
> takes family as a parameter);

with:

> ICMP/MTR via `probe.resolveIPAddr("ip"|"ip4"|"ip6")`, `net.ResolveIPAddr`'s
> selection with the context honored (shared `traceHops` takes family as a
> parameter);

## Retention bullet (A7)

Replace:

> per-table TTL set at bootstrap from
> `storage.clickhouse.retention.{cycle,rtt,hop,http}_days` (defaults
> 365/14/90/14).

with:

> per-table TTL set at bootstrap from
> `storage.clickhouse.retention.{cycle,rtt,hop,http}_days` (defaults
> 365/14/90/14; 0 defaults, anything else must be inside
> `[1, config.MaxRetentionDays]` — 49710, the full span of ClickHouse's
> UInt32-second DateTime). A negative value used to pass straight into the
> `MODIFY TTL` Bootstrap re-emits on every start, a TTL in the past that
> expires the whole table.

## Path discovery bullet, addition (A6)

Add (near the matchDatagram sentence about comparing as unmapped netip.Addr):

> Inside the walk the responder's identity stays a `netip.Addr` end to end:
> `ttlReply.addr` and the aggregation rows hold the parsed address from
> `matchDatagram`, and the textual `Hop.IP` is produced once at row emission —
> the wire/storage form is unchanged, and `""` still means "nothing answered
> at this TTL".

# CLAUDE.md deltas from group D (do not merge into CLAUDE.md here — apply on main)

## Window caps bullet (Storage backend section)

The sentence ending "…so widening the raw tier widens the override with it."
should be extended to cover the other overrides, and the paragraph should
mention /status. Replace:

> and `?step=raw` on `/cycles` bounded by `storage.PickCycleStep(span) == 0`
> rather than a second copy of the 2h threshold, so widening the raw tier
> widens the override with it.

with:

> and every `?step=` override on `/cycles` bounded by the ladder's own tier
> for the window (`raw` by `storage.PickCycleStep(span) == 0`, `1h`/`1d` by
> `derived <= override`) rather than second copies of the thresholds, so
> widening a tier widens the override with it — an override finer than the
> ladder's tier is a 400, never served. `/status` scans only
> `api.statusRecentCycles` (50) × the live interval, the count it trims to.

Also delete or amend the claim "Bucketed `/cycles` needs no cap — the ladder
holds it to ~500–1000 points at any width": it is now true *because* the
overrides are clamped to the ladder; before this change `?step=1h&from=-365d`
was 24× the ladder.

## Cycle source stamping bullet

Replace "`Registry.Touch` returns whether it accepted" with:

> `Registry.Touch` returns an error naming why it refused —
> `errRegistryFull` at the `maxRegisteredSlaves` ceiling (503 on
> `/register`, retryable once `Sweep` frees a name) and
> `errSlaveFieldTooLong` for a version/advertise past `maxSlaveFieldLen`
> (400: the request's own bytes can never succeed) — so an operator is no
> longer told "slave registry full" for an oversized advertise.

## Slave push buffer + auth bullet

After the sentence about the 401 → ErrAuth exit, add:

> `ErrRejected` is likewise fatal at boot: `registerForever` and
> `pullConfigInitial` exit non-zero carrying the master's message instead of
> retrying a verdict (invalid `cluster.name`, oversized advertise) the master
> answers identically forever — before this a mis-named slave sat "running"
> while probing nothing. Mid-flight, `refreshLoop` still keeps the last-good
> config on any pull error, matching `Store.Reload`'s reload semantics.

## Slave health mesh bullet

Replace "`cluster.slave_addrs` optionally pins a slave to one address — a
mismatch is refused a health entry, but unpinned slaves are accepted so the
feature works zero-config." with:

> `cluster.slave_addrs` optionally pins a slave to one address — a mismatch
> is refused a health entry, unpinned slaves are accepted so the feature
> works zero-config, and the pins follow the config hot-reload contract:
> the registry reads them through a live closure over `store.Current()`
> (`Registry.SetPinsFn`), re-checked both at `Touch` time and in `Peers()`,
> so a SIGHUP-edited pin drops a mismatched peer on the next scheduler
> signal without waiting for that slave's next heartbeat.

## Rationale trimmed out of code comments (D6 cleanup)

- `api.redactTerminalHops` keying: the maximum stays keyed by
  `(source, timestamp)` rather than source alone even though both current
  callers (QueryLatestHops / QueryHopsAt) pin one timestamp per source —
  over multi-trace rows a source-wide maximum would leave the terminal hop
  of every shorter trace unredacted. The key is `Time.UnixMilli()`, matching
  the reader/cache identity (storage timestamps are DateTime64(3), so ms is
  the storable precision); the earlier `UnixNano` was consistent but odd —
  no disclosure either way (UnixNano is injective over the addressable
  range).
- `master.maxRegisteredSlaves` (512): entries leave only via the hourly 24h
  `Sweep`, so without the ceiling a token holder mints a fresh name per
  request for a day; each name that pushes becomes a permanent ClickHouse
  LowCardinality entry and a `QueryLatestHops` row forever. 512 is ~85× the
  deployed 6-source fleet. Refusing a *new* name never evicts a registered
  one.
- `master.maxSlaveFieldLen` (256): version and advertise arrive as headers
  bounded only by net/http's 1 MiB cap and are both retained per entry
  (advertise inside the log-dedup key even when rejected); the longest
  legal advertise is a 45-byte IPv6 text form and a version is a release
  tag, so 256 is ~5× either.

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

## C13 — rationale moved out of oversized code comments

Trimmed to the one-sentence rule; the surviving facts already live in
CLAUDE.md's "Write buffers" and storage bullets except these two, which
belong wherever those bullets live:

- `flushRetainFactor` (writer.go): on a flush error the batch is retained for
  retry on the next ticker tick rather than dropped; the retained backlog is
  capped at `maxRows x flushRetainFactor (4)` with the oldest overflow dropped
  and counted — the third layer (channel, retained batch, slave ring) that is
  drop-oldest.
- `queryHopsGrid` (reader.go): reading address, unreach and timestamp from
  one argMax tuple costs the timeline any annotation carried by a responder
  that is not the slot's worst; `/hops?at=` still serves every responder's
  own. Ties between responders resolve arbitrarily, as they did client-side.

# CLAUDE.md deltas — group B (alert evaluator, dispatcher, cluster ingest context)

Apply these to the root CLAUDE.md; this file exists because concurrent agents
must not edit CLAUDE.md directly.

## 1. "Alert quorum" bullet — liveness window (B2)

Replace:

> Sources stale beyond 3× the probe interval are pruned from the live count
> so a dead slave can't suppress a real alert.

with:

> Sources stale beyond the liveness window — `livenessWindow`, which is
> `alertFreshness` = max(3× interval, `config.MaxFutureSkew`) — are pruned
> from the live count so a dead slave can't suppress a real alert. It is the
> freshness window rather than the bare 3× interval because cycles arrive in
> pushed batches on the slave's own `cluster.push_every` cadence, which config
> does not bound: any cadence whose cycles the freshness gate still evaluates
> must also keep its source live, or every bursty-but-healthy slave was pruned
> between pushes and a `"majority"` quorum collapsed to whichever source
> delivers continuously (Threshold(1) == 1). A cadence past the window is
> already losing cycles to the freshness gate and is warned about as
> `alert.source_excluded`. The quorum warm-up window stays 3× interval.

## 2. "A cycle is evaluated once while its identity is still held, in order" bullet — prune retention (B1)

Append to that bullet:

> `tally`'s staleness prune drops only quorum participation — `state` and
> `consecHits` reset to what a recreated entry would hold — never the replay
> identity: deleting the whole `alertState` recreated it with `seenCycle`
> false, which admits anything, so the lost-ack redelivery of a pruned
> source's cycle was applied a second time whenever its stamp was still
> alert-fresh (a forward-dated stamp keeps it fresh past the prune), resolving
> a live alert or refiring a sustained one off replayed data. The identity is
> deleted only once `now − lastCycle` exceeds `alertFreshness`: every stamp
> the entry holds is at most `lastCycle`, so past that point any replay is
> refused by the freshness gate upstream and retention buys nothing. The
> bound is the producer's own maximum, not picked — ingest accepts a stamp at
> most `config.MaxFutureSkew` ahead of its receive time, so an entry outlives
> its last cycle by at most `alertFreshness + MaxFutureSkew`.

## 3. Dispatch context (B3) — new paragraph in the alert-quorum section (near the warm-up paragraph)

Add:

> Dispatch is detached from the caller's context
> (`context.WithoutCancel` in `OnCycle`): the transition is committed before
> dispatch and the path is change-gated with no renotify, so an expired
> ingest deadline silently dropped the only FIRING notification an alert
> would ever send while the state read as delivered — the first payload the
> endpoint then saw was the resolve for a page never sent. Each action still
> bounds its own delivery (the dispatcher's 10s HTTP client timeout, exec's
> 10s deadline). Master ingest scopes its detached 30s sink budget per cycle
> inside `deliver`, not per batch, so a few stalled deliveries cannot starve
> the up-to-`MaxCyclesPerBatch` cycles behind them.

## 4. Exec action log (B4) — wherever the credential-scrubbing of dispatcher logs is described

Add (or extend the health-alerts/scrubbing prose):

> Exec failures log only the action name and a fixed category
> (`execFailureCategory`: timeout / exit code / start failed), like the
> webhook/discord `httpFailureCategory` siblings: `a.Command` is env-expanded
> from the raw config bytes and can embed resolved secrets, `exec.Error`
> quotes argv[0], and the command's stdout+stderr is unbounded operator-script
> output.

## 5. Warm-up sweep (B5) — in the "A quorum alert also has a warm-up" paragraph

Append:

> Warm-up state is swept on `Refresh` by the same rule as the aggregate: a
> quorum alert that never dispatched has a warmup entry but no agg entry, so
> sweeping only agg's keys leaked entries for alerts that left the config and
> kept a stale `firstSeen` across a disable/re-enable — the elapsed-window arm
> then paged on the first partial-data evaluation, the exact flap warm-up
> exists to prevent.

## 6. Test-fixture rule (B7) — optional addition to the alert prose or the testing skill

> Every evaluator test that applies more than one cycle per source must stamp
> each with a distinct timestamp (use `cycleAt` + `clk.advance`): the
> duplicate guard skips a reused stamp before any state mutation, which
> silently neutered two tests (`TestQuorumKeepsPerSourceSustainedIndependent`,
> `TestRefreshDropsAggOnQuorumToggle`'s final assertion).
