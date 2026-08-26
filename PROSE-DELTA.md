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
