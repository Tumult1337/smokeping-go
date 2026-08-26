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
