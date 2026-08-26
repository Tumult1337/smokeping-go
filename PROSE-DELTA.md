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
