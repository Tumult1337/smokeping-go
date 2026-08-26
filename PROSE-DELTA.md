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
