# /hops/timeline contract changes

Applies to `GET /api/v1/targets/{group}/{name}/hops/timeline`. The bundled UI
is updated with the binary; a direct consumer of the JSON API is not.

## The hop grid has no raw tier

`step_sec` used to be `0` for windows of 2h or less, meaning "one column per
cycle, size it from the median row gap". It is now always positive: the ladder
returns `max(11s, probe interval)` for ≤2h, `max(5m, interval)` for ≤24h and
`max(15m, interval)` beyond, and the response buckets on that grid.

**Why.** The endpoint refuses a result past `maxHopTimelineRows` (172,288)
rather than serving a prefix, and that ceiling was the grid product — bucket
count × the 256 values a `UInt8` ttl column holds. The raw tier had no bucket
count, so it was justified separately: the trace walks one TTL per 50ms, so 2h
could hold at most 144,000 rows. That spacing does not bound the cycle rate. A
round ends at the target's own reply *before* it pays the spacing, and
`config.Validate` requires only a positive interval, so a one-hop MTR target at
a 30ms interval writes 240,000 `(timestamp, ttl)` rows in 2h — a legitimate
history the endpoint answered `400` for. Bucketing removes the producer's cycle
rate from the row count entirely, which is what makes the cap an assertion
about the schema rather than a bet on how fast an operator probes.

`11s` is `ceil(2h / (MaxHopGridSlots - 1))` rounded up to a whole second: the
7d window at the coarsest 15m step is 673 slots, and no tier may need more than
that, or the row cap stops being a product of the ladder. The step never goes
below the probe interval because a grid finer than the cadence filling it
leaves empty columns, which the heatmap draws as history that was never
collected.

**What an operator sees.** At windows of 2h or less, cycles closer together
than the grid now share a column, folded worst-loss-wins. That fold is new:
the client's own worst-loss-wins pick was across the *responders* of a single
cycle — ECMP siblings and path flaps at one ttl — and never across cycles,
because two cycles carried two timestamps and drew two columns. A column now
holds however many cycles fell in its slot: usually one at an interval at or
above the step, but the step is a whole second and the schedule is not, and
cycles are not aligned to the grid, so a column holding two and a neighbour
holding none is normal rather than a fault.

**No config is refused.** Enforcing a minimum probe interval for hop-producing
probes was the alternative, and it was rejected: the only value that keeps the
current cap is ~11s, which would refuse working sub-11s schedules, and no
producer limit derives it — a one-hop trace has no floor on how fast it can run.

**If you consume the JSON directly**, drop any branch keyed on `step_sec == 0`.
Sizing a column from `step_sec` is correct on every tier now.

## `source` is required

A request without the `source` parameter is refused with `400`. The endpoint
serves one probe origin per request: the window and the ladder bound the grid
and the `UInt8` ttl column bounds its other axis, but nothing bounds how many
sources have rows in a window, so the origin is admitted rather than derived.
An empty value is a source — the untagged pre-cluster origin — so `source=`
alone is a valid request, and `source=master` is what a standalone install
sends.

**This breaks a direct consumer written against an earlier release.** The
bundled UI fetches and draws one canvas per source and has always sent the
parameter, so it never issues the refused request. A script that read the
all-sources response has to loop over `/api/v1/sources` instead.
