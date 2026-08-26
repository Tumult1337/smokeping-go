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
