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
