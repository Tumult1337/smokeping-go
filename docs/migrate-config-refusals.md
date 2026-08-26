# Migration: configs that used to load and now do not

This release adds three load-time refusals. Each closes a case where the
process accepted a config and then failed — at boot, at the next restart, or
silently at runtime. Check yours against the list **before** rolling the
binary out, because a refused config exits non-zero rather than degrading.

There is no validate-only flag: start the new binary against a scratch
ClickHouse and read the error, or check your config against the three sections
below by hand.

## 1. `storage.clickhouse.batch` is bounded at both ends

`max_rows` must be in `[1, 1000000]` and `max_interval` in `(0, 1h]`.

Previously both were defaulted only when absent and bounded at neither end, so
`max_rows: -1` or `max_interval: "0s"` validated green and then panicked the
writer goroutine at boot — `makeslice: cap out of range` and `non-positive
interval for NewTicker` respectively, with a stack trace instead of a config
error. Nothing sane sets either, but a templating bug that renders an empty
value as `0` did.

**If you hit this:** remove the key to take the default (1000 rows / 1s), or
set a positive value.

## 2. Retention is checked against the clock, not just a constant

`storage.clickhouse.retention.{cycle,rtt,hop,http}_days` must still be inside
`[1, 36500]`, and must now also expire inside ClickHouse's `DateTime` range,
which ends 2106-02-07.

The TTL is evaluated against each row's own timestamp, so what is representable
depends on when the row was written — no compile-time constant knows it. Only
`Bootstrap` checked that before, and `Bootstrap` runs at startup: about 7,482
of the values the fixed ceiling admitted reloaded green over `SIGHUP` and then
refused to start at the next restart, which is a redeploy away from being
discovered.

**If you hit this:** the practical ceiling as of 2026 is roughly 29,000 days
(~79 years). Any real retention is far below it; a value in that range is
almost always a units mistake (hours or minutes written as days).

## 3. A cluster node's icmp ping budget is checked even without an icmp probe

`config.ICMPPingBudget` refuses a schedule whose full-loss derived budget
`(interval − (pings−1) × 200ms) / pings` falls below 50ms. It is now applied
when the config defines an icmp probe **or** when the config is a cluster
master or slave (a `cluster` block with a token) — not only when the stored
`probes` map names one.

This is deliberate and is not being narrowed. The slave-health mesh injects an
icmp probe at scheduler-build time, so gating on the stored map alone let a
master validate a config that became unbuildable fleet-wide at the first slave
registration — green on the master, every node failing at its next restart.
Gating on "does any peer advertise an address" is not knowable from one node's
config, which is what makes the wider gate the correct one.

**If you hit this:** an http-only or dns-only cluster node with a tight
schedule (for example `interval: 4s, pings: 20`, a 10ms budget) is refused even
though it defines no icmp probe. Raise `interval` or lower `pings` until the
derived budget clears 50ms. The error names the derived figure.

## Not a refusal, but a behaviour change: `?step=`

`?step=raw|1h|1d` on `/api/v1/targets/{group}/{name}/cycles` is now bounded by
the ladder's own tier for the requested window. An override *finer* than the
tier — `raw` past 2h, `1h` past the 1h tier (30d) — answers **400** instead of
running a scan the ladder exists to prevent. `1d` is unaffected.

The parameter is back-compat surface from the InfluxDB era; a dashboard pinning
`step=1h` over a 90d window is the case that breaks. Drop the parameter and let
the ladder pick, which is what it does by default.
