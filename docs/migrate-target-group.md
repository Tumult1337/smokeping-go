# Migration: per-target identity gains `target_group`

`probe_rtt`, `probe_hop` and `probe_http` gain a `target_group` column, and
every read now filters on `(target_id, target_group)` instead of `target_id`
alone. `probe_cycle` already had the column and is unchanged on disk.

## Why

`target_id` holds the target *name*. Config dedupes on `group/name`, so two
groups may legally contain the same name — and until now every such pair
shared one set of rows. On a 122-target install, 22 targets (`core-backbone/*`
and `retn/*` sharing 11 city names) returned byte-identical data drawn from two
unrelated networks, and the MTR path table showed whichever network last wrote
a cycle.

It is also a security boundary. Slave-health targets live in the reserved
`_cluster` group and are named after the slave, and the API redacts hop
addresses based on the group of the target being queried. A user target that
happened to share a slave's name would read the health target's rows with no
redaction, exposing the slave's advertised address.

## What happens to existing data

| Table | Effect |
|-------|--------|
| `probe_cycle` | Nothing. It has always written `target_group`, so all retained history stays visible and becomes correctly separated the moment the group predicate applies. |
| `probe_rtt`, `probe_hop`, `probe_http` | Rows written before this change read as `target_group = ''` and stop appearing on target endpoints. Nothing is deleted; they age out on the existing TTL (14d / 90d / 14d). New writes carry the real group and are correct immediately. |

There is deliberately no `OR target_group = ''` fallback. Those rows never
recorded a group, so a fallback could not attribute them to the right target —
it would show the same merged rows under both targets and reinstate the
health-hop disclosure this change exists to close.

## Runbook

`Bootstrap` issues `ALTER TABLE … ADD COLUMN IF NOT EXISTS target_group … AFTER
target_id` on every start, so a normal upgrade needs no manual step. The ALTER
is metadata-only: no historical part is rewritten.

```bash
git pull && make deploy      # or: systemctl restart gosmokeping
```

**Do not run an old binary against the migrated tables.** The writer uses
positional batch inserts, so a build that predates the new column supplies one
value too few and its inserts fail — the same constraint documented in
[`migrate-rtt-microseconds.md`](migrate-rtt-microseconds.md). Stop old writers
before or during the upgrade rather than running mixed versions. In cluster
mode this only concerns the master; slaves never write to ClickHouse.

Bootstrap runs before the writer and reader open, so a failed ALTER aborts
startup rather than leaving a writer pointed at a table it cannot insert into.

### Cluster mode

The ALTER is issued `ON CLUSTER` when `storage.clickhouse.cluster` is set, and
`IF NOT EXISTS` makes concurrent bootstraps from several nodes safe — the first
distributed DDL task adds the column and the rest no-op. If the DDL queue
exceeds `distributed_ddl_task_timeout` the query reports a timeout while remote
execution continues; treat that as a bootstrap failure, confirm convergence in
`system.columns`, and restart. `IF NOT EXISTS` makes concurrent *identical*
bootstraps safe; it does not make mixed binary versions safe.

## Verifying

```sql
SELECT table, name FROM system.columns
WHERE database = currentDatabase() AND name = 'target_group'
ORDER BY table;
-- expect: probe_cycle, probe_hop, probe_http, probe_rtt

-- new rows carry a group; pre-migration rows read as ''
SELECT target_group, count() FROM probe_hop GROUP BY target_group ORDER BY 2 DESC;
```

Two targets sharing a name should now diverge. Against the API:

```bash
curl -s 'localhost:8080/api/v1/targets/core-backbone/frankfurt/hops' | sha256sum
curl -s 'localhost:8080/api/v1/targets/retn/frankfurt/hops'          | sha256sum
```

Before the migration these hashes matched. After it they must differ, and each
path's terminal hop should be that target's own address.
