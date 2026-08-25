# Migration: slave upgrade required, and `probe_hop` gains two columns

This release changes what a master accepts from a slave and adds two columns to
`probe_hop`. Upgrade the master first, then every slave. Do not leave a slave on
the old binary.

## Slaves must be upgraded

A master refuses `POST /cluster/cycles` with **403** when its registry has no
entry for the pushing slave, and the registry is in memory only — a master
restart empties it. An upgraded slave handles that: the 403 becomes
`ErrUnregistered`, it re-registers and keeps the batch, and the next push
succeeds. That path exists only in the new binary.

An **un-upgraded** slave reads the 403 as a generic retryable error and
re-registers only at boot. If it also runs `cluster.pull_every: "0"` it has no
`/config` heartbeat either, so nothing re-creates its registry entry: every push
requeues until the 600-cycle ring overwrites live data. It does not recover on
its own — it must be restarted, and by then the buffered window is gone.

There is no compatibility shim. Upgrading a master while slaves lag loses those
slaves' data for as long as the lag lasts.

## New rejection behaviour

Both of these are new reasons a master returns 4xx on `/cycles`. Upgraded
slaves handle them; the whole batch is refused, never part of it.

| Condition | Status | Slave behaviour |
|-----------|--------|-----------------|
| Batch outside the ingest bounds — cycle count, hop rows, RTT array lengths, counters outside their storage column's range, or a timestamp outside `[now-7d, now+5m]` | 400 | Drops the batch and logs at Error. It previously requeued, which head-of-line blocked the ring until drop-oldest discarded everything behind it. |
| Source not in the master's registry | 403 | Re-registers, keeps the batch, retries on the next push tick. |

407, 408, 421, 425, 429, every 5xx and network errors still requeue unchanged.
That set is each status's own specification, not the master's behaviour, because
any proxy on the path can answer one; 421 also drops the idle connections it was
misrouted over.

A cycle buffered through an outage longer than seven days is now refused and
dropped rather than written already past retention. At the shipped 600-cycle
ring and any realistic interval, no legitimate slave reaches that age.

The hop-row bound is derived from the probe's own ceiling
(`config.MaxHopRowsPerCycle` = 10 rounds × 30 TTLs = 300) rather than picked, so
a deep ECMP path is no longer refused as if it were abuse. An install pushing
fewer than 300 hop rows per cycle — every icmp walk does, at 3 × 30 = 90 — sees
no change.

## Alert liveness now runs off the master's clock

Quorum liveness, per-source staleness and the quorum warm-up window are
measured from when the master received a cycle, not from the timestamp the
cycle carries. A slave chose that timestamp, and ingest accepts one up to five
minutes ahead — enough for a single slave to age every other source out of the
quorum denominator and become a majority of itself.

Two operator-visible consequences:

- A cycle older than `max(3 × interval, 5m)` on arrival is stored but not
  evaluated for alerting. A backlog delivered after an outage longer than that
  window will not replay alert transitions. What drives the state from there
  is the next cycle that is both inside the window *and* newer than the last
  one already evaluated for that source — not merely the next fresh one,
  because a cycle that arrives fresh but does not advance that source's
  timestamp is a redelivery and is skipped.
- A slave whose clock **lags** the master by more than that window stops
  contributing to alerts while its data keeps being stored. Keep NTP working
  on slaves. (A slave more than five minutes *ahead* was already refused at
  ingest.) This is no longer silent: it is logged at Warn as
  `alert.source_excluded` with `reason=clock_skew`, once per source per
  freshness window, carrying the count of cycles the window suppressed.
- A cycle whose timestamp does not advance for its source is skipped before it
  can change alert state, so a requeued batch redelivered after a lost ack no
  longer counts twice toward `sustained`. A producer whose clock steps
  backwards is logged the same way with `reason=duplicate_cycle`.

## Hop addresses are bounded whole, zone included

`netip.ParseAddr` accepts a zone of any length, so a registered slave could put
`fe80::1%<megabytes of text>` in a hop's `ip` and have it stored in
`probe_hop.hop_addr` and served by an unauthenticated `/hops`. Ingest now
bounds the complete encoded address at 76 bytes and requires a zone to be
shaped like an interface name or index. Real link-local hops
(`fe80::1%eth0`, `fe80::1%3`) are unaffected.

Rows written before this release are not rewritten. If you suspect one was
planted, `probe_hop` rows outlive only their TTL, so either wait it out or
delete by `length(hop_addr) > 76` in ClickHouse.

## An interval above 71m34.967295s is now refused at load

`interval` is capped at `config.MaxProbeInterval`, which is exactly
71m34.967295s. A cycle runs under a context whose deadline is the interval, so
the interval is also the largest RTT a probe can measure — and the per-cycle
and per-hop latency summaries (`probe_cycle.*_us`, `probe_hop.*_us`) store
microseconds in a `UInt32`, which `durUS` saturates at, so a larger value stops
being stored as itself. Raw per-ping samples are not the binding column:
`probe_rtt.rtt_ms` is a `Float64`. An interval above the cap therefore produced
latencies the master's own ingest refused, dropping the batch, and stored the
rest saturated.

The cap is the storage limit itself rather than a round number below it, so no
schedule that was working is refused. Nothing changes for the 5s–5m range every
documented deployment uses. An install running an interval above 71 minutes
must lower it before upgrading: `config.Load` fails rather than starting on a
schedule it cannot store.

## A long http error no longer rejects a batch

`HTTPSample.Err` is truncated to 4096 bytes at the probe, and again at ingest
for slaves that predate that. It used to be *refused* at 4096 — but the string
is `Get "<url>": <cause>` and config bounds no URL's length, so a legitimate
5 KiB URL plus a connection failure produced a 400 that permanently dropped the
whole batch, up to 99 unrelated cycles with it. A truncated error string keeps
its head and its tail with `…(truncated)…` between them: the cause is printed
after the URL, so cutting only the tail off a URL longer than the bound stored
the URL and dropped the `connection refused`.

## Schema

`probe_hop` gains `unreach` (`LowCardinality(String)`) and `target_reply`
(`UInt8`). `Bootstrap` issues `ALTER TABLE … ADD COLUMN IF NOT EXISTS` on every
master start, so a normal upgrade needs no manual step; the ALTER is
metadata-only and rewrites no historical part. In cluster mode it is issued
`ON CLUSTER`, and `IF NOT EXISTS` makes concurrent identical bootstraps safe.

**Rolling back across this migration breaks hop writes.** Binaries that predate
these columns insert into `probe_hop` positionally, so they supply two values
too few against the migrated table and every hop insert fails — the same
constraint as
[`migrate-target-group.md`](migrate-target-group.md) and
[`migrate-rtt-microseconds.md`](migrate-rtt-microseconds.md). Flush inserts from
this release forward name their columns explicitly, so rollbacks *after* this
migration are safe. Only the master writes ClickHouse; slaves are unaffected.

## Runbook

```bash
# master first
git pull && make deploy      # or: systemctl restart gosmokeping

# then every slave, before its 600-cycle ring wraps
```

## Verifying

```sql
SELECT name FROM system.columns
WHERE database = currentDatabase() AND table = 'probe_hop'
  AND name IN ('unreach', 'target_reply');
-- expect: two rows
```

Check that no slave is stuck behind the master. On each slave:

```bash
journalctl -u gosmokeping -n 200 | grep -E 'does not know this slave|permanently rejected'
```

`master does not know this slave, re-registering` once after a master restart is
the expected recovery. Repeating every push tick, or
`master permanently rejected the batch`, means that slave needs attention.
