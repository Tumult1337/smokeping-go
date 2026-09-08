# MTR Loss and ICMP Zero-Latency Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make MTR target loss use a direct echo batch, restore the ICMP result invariant, repair MTR history handoffs, and stop legacy sample-less successes from producing 0 ms history.

**Architecture:** MTR runs its existing trace concurrently with a no-trace ICMP echo batch and combines the two independently-owned results. ICMP removes a context-interrupted attempt from `Sent`; cluster ingest normalizes payloads produced by old slaves; ClickHouse reads conservatively remove already-stored phantom successes. Existing DTO and cache clone omissions are fixed at their current boundaries.

**Tech Stack:** Go 1.26.1, `golang.org/x/net/icmp`, ClickHouse SQL, existing React/TypeScript build.

## Global Constraints

- Keep the fix surgical and add no dependency, schema migration, worker pool, retry, or configuration knob.
- Preserve `len(RTTs) == Sent - LossCount` for every newly produced or ingested cycle.
- MTR target `RTTs`, `Sent`, and `LossCount` come only from the direct echo batch; `Hops` comes only from the TTL walk.
- Both MTR operations share the scheduler cycle context and every started goroutine is joined.
- Authenticated slave payloads remain untrusted, bounded input; malformed batches must not create samples or increase successful counts.
- Existing sample-less successes are removed from the read denominator, not converted into completed loss.
- Run `goimports`, gopls diagnostics, `go build ./...`, `go test ./...`, `go test -race ./internal/probe ./internal/cluster/...`, `go vet ./...`, and `npm run build` before completion.

## Security design gate

- **Problem:** Two regressions fabricate healthy MTR loss and zero-latency slave-health successes. Scope is limited to measurement production, cluster normalization, existing cycle reads, and the two proven history handoff omissions.
- **Threat model:** Cluster cycle JSON is controlled by an authenticated but potentially stale or compromised slave. Its counters, RTT slice, and summary can affect unauthenticated charts and alert state; body, batch, counter, and RTT bounds already constrain allocation and work.
- **Attacker:** A payload with excess samples is rejected by `CyclePayload.validate`; one with missing samples can only lose claimed successes during `ToCycle`, never gain them. The summary is recomputed from bounded RTT input, preventing contradictory wire fields from fabricating latency. SQL remains static with bound identity/time arguments.
- **Implementation:** Use the existing ICMP probe, `stats.Compute`, protocol validation, and reader query patterns. Add no abstraction beyond a small payload-normalization helper used at the wire boundary.
- **Compatibility:** Older slaves remain accepted during rollout; their incomplete attempts become no-measurement attempts. Current payload shape and storage schema remain unchanged. Rejecting whole old-slave batches was rejected because one interrupted attempt would discard unrelated valid cycles.

---

### Task 1: Direct target measurements for MTR

**Files:**
- Modify: `internal/probe/mtr.go`
- Modify: `internal/probe/mtr_test.go`

**Interfaces:**
- Consumes: `ICMP.Probe(context.Context, Target, int) (*Result, error)` with `NoTrace=true`; existing `traceFunc`.
- Produces: `MTR.Probe` whose target fields come from the direct result and whose `Hops` come from the trace result.

- [ ] **Step 1: Write failing direct-loss and partial-result tests**

Add an injectable `echo` function expectation in tests. Drive a direct result `{Sent: 10, LossCount: 2, RTTs: eight literal durations}` while the trace eventually reaches the target in every round; assert the combined result remains 10/2/eight RTTs and retains trace hops. Add direct-error and trace-error cases asserting the other component's completed data survives and both calls finish.

- [ ] **Step 2: Verify the tests fail against reachability-derived MTR**

Run: `go test ./internal/probe -run 'TestMTR(UsesDirectTargetLoss|PreservesDirectResultOnTraceError|PreservesHopsOnDirectError)'`

Expected: FAIL because `MTR` has no direct echo seam and current target counters come from `roundStats`.

- [ ] **Step 3: Implement concurrent direct echo and trace composition**

Add a typed echo function field to `MTR`; initialize it from `NewICMP(name, timeout, true).Probe`. Start one bounded operation in a buffered result channel, run the other in the caller, join the channel on every path, and compose:

```go
result := &Result{Hops: trace.hops}
if direct.result != nil {
	result.RTTs = direct.result.RTTs
	result.Sent = direct.result.Sent
	result.LossCount = direct.result.LossCount
}
```

Return contextual joined errors without replacing either partial result. Preserve the raw-socket error diagnostic for trace failure and return a no-measurement target result when direct probing never started.

- [ ] **Step 4: Verify focused probe tests pass**

Run: `gofmt -w internal/probe/mtr.go internal/probe/mtr_test.go`

Run: `go test ./internal/probe -run 'TestMTR'`

Expected: PASS.

- [ ] **Step 5: Commit the task**

```bash
git add internal/probe/mtr.go internal/probe/mtr_test.go
git commit -m "fix(mtr): measure target loss with direct echoes"
```

### Task 2: Preserve MTR history counts through API and cache

**Files:**
- Modify: `internal/api/api.go`
- Modify: `internal/api/api_test.go`
- Modify: `internal/storage/cache.go`
- Modify: `internal/storage/cache_test.go`

**Interfaces:**
- Consumes: `storage.HopPoint.ReplyCount` and `storage.HopsResult.TimelineLoss`.
- Produces: matching `/hops/timeline` JSON and isolated cached copies.

- [ ] **Step 1: Write failing API and clone-isolation tests**

Extend the real timeline handler test fixture with `ReplyCount: 7` and assert decoded JSON contains `ReplyCount == 7`. Add a cache test whose inner result contains one `TimelineLoss` entry, mutate the first returned slice, fetch again, and assert the cached entry still carries the original value.

- [ ] **Step 2: Verify both tests fail for the proven omissions**

Run: `go test ./internal/api ./internal/storage -run 'Test.*(ReplyCount|TimelineLoss)'`

Expected: FAIL with JSON reply count 0 and/or an empty cached timeline-loss slice.

- [ ] **Step 3: Copy both omitted fields**

Set `ReplyCount: h.ReplyCount` in `getHopsTimeline`. Allocate and copy `TimelineLoss` in `cloneHopsResult` alongside `Hops` and `Cycles`.

- [ ] **Step 4: Verify focused packages pass**

Run: `gofmt -w internal/api/api.go internal/api/api_test.go internal/storage/cache.go internal/storage/cache_test.go`

Run: `go test ./internal/api ./internal/storage`

Expected: PASS.

- [ ] **Step 5: Commit the task**

```bash
git add internal/api/api.go internal/api/api_test.go internal/storage/cache.go internal/storage/cache_test.go
git commit -m "fix(mtr): preserve history reply and loss counts"
```

### Task 3: Restore ICMP and cluster cycle consistency

**Files:**
- Modify: `internal/probe/icmp.go`
- Modify: `internal/probe/icmp_test.go`
- Modify: `internal/cluster/protocol.go`
- Modify: `internal/cluster/protocol_test.go`

**Interfaces:**
- Consumes: bounded `CyclePayload.RTTs`, `Sent`, `LossCount`, and untrusted `Summary`.
- Produces: scheduler cycles satisfying `len(RTTs) == Sent - LossCount` with `Summary == stats.Compute(RTTs)`.

- [ ] **Step 1: Change the ICMP regression test to require no phantom success**

Update `TestICMPProbeDoesNotCountCycleCancellationAsLoss` to require `Sent=0`, `LossCount=0`, and no RTT when its only send is interrupted. Add a two-attempt case with one completed failure followed by cancellation and require `Sent=1`, `LossCount=1`, and no RTT.

- [ ] **Step 2: Verify the ICMP test fails on the current `Sent++` path**

Run: `go test ./internal/probe -run 'TestICMPProbeDoesNotCountCycleCancellationAsLoss'`

Expected: FAIL with `Sent=1`, proving the interrupted attempt remains counted.

- [ ] **Step 3: Remove only the interrupted attempt from `Sent`**

In the `i.send` error branch, when `ctx.Err() != nil`, decrement `result.Sent` before returning. Leave completed timeout/error attempts on the existing `LossCount++` path.

- [ ] **Step 4: Verify ICMP tests pass**

Run: `gofmt -w internal/probe/icmp.go internal/probe/icmp_test.go`

Run: `go test ./internal/probe -run 'TestICMP'`

Expected: PASS.

- [ ] **Step 5: Write failing protocol normalization and rejection tests**

Add a round-trip payload with `Sent=10`, `LossCount=9`, no RTTs, and a non-zero forged summary; assert `ToCycle` returns `Sent=9`, `LossCount=9`, and an all-zero recomputed summary. Add validation coverage for `Sent=1`, `LossCount=1`, one RTT; assert validation rejects it because samples exceed the claimed successful count.

- [ ] **Step 6: Verify protocol tests fail before normalization**

Run: `go test ./internal/cluster -run 'TestCyclePayload(NormalizesMissingSamples|RejectsExcessSamples)'`

Expected: FAIL because current validation accepts excess samples and `ToCycle` trusts counters and summary independently.

- [ ] **Step 7: Normalize at the trust boundary**

After existing counter and RTT bounds, reject `len(p.RTTs) > p.Sent-p.LossCount`. In `ToCycle`, set `Sent` to `p.LossCount + len(p.RTTs)` and recompute `Summary` with `stats.Compute(p.RTTs)`. Keep `LossCount` unchanged so missing samples remove unfinished attempts rather than inventing loss.

- [ ] **Step 8: Verify cluster tests pass**

Run: `gofmt -w internal/cluster/protocol.go internal/cluster/protocol_test.go`

Run: `go test ./internal/cluster/...`

Expected: PASS.

- [ ] **Step 9: Commit the task**

```bash
git add internal/probe/icmp.go internal/probe/icmp_test.go internal/cluster/protocol.go internal/cluster/protocol_test.go
git commit -m "fix(icmp): exclude interrupted echo attempts"
```

### Task 4: Normalize already-stored phantom successes on reads

**Files:**
- Modify: `internal/storage/clickhouse/reader.go`
- Modify: `internal/storage/clickhouse/reader_args_test.go`
- Modify: `internal/storage/clickhouse/integration_test.go`

**Interfaces:**
- Consumes: legacy `probe_cycle` rows where `sent > lost` but every latency summary column is zero.
- Produces: raw/bucketed `CyclePoint` values with phantom successes removed and no zero-latency weighted percentile.

- [ ] **Step 1: Write failing query-contract tests**

Extend reader query tests to assert both cycle query forms derive an effective sent count that equals `lost` when `rtt_max_us = 0`, and that bucket percentile weights are zero for those rows. Keep the existing placeholder/argument assertion active.

- [ ] **Step 2: Verify query-contract tests fail on current SQL**

Run: `go test ./internal/storage/clickhouse -run 'TestCycleQueriesExcludeSamplelessSuccesses'`

Expected: FAIL because raw returns stored counters and bucketed SQL weights by `sent-lost` even when all summary columns are zero.

- [ ] **Step 3: Implement conservative SQL normalization**

For raw rows, select effective `sent` as `if(rtt_max_us = 0, lost, sent)` and derive loss percentage from that effective denominator. For bucketed rows, define the same effective-success expression once in a `WITH`, use it for latency weights, sum effective sent for the denominator, and retain `sum(lost)` unchanged. Ensure all-zero/full-loss buckets continue returning finite zero summaries.

- [ ] **Step 4: Add live ClickHouse integration coverage**

Insert one valid partial-loss cycle and one legacy-shaped sample-less-success cycle into a bucket. Assert raw output converts the legacy row to full completed loss and bucketed output gets latency only from the valid cycle, with counters excluding the unfinished attempt.

- [ ] **Step 5: Verify reader tests**

Run: `gofmt -w internal/storage/clickhouse/reader.go internal/storage/clickhouse/reader_args_test.go internal/storage/clickhouse/integration_test.go`

Run: `go test ./internal/storage/clickhouse`

Expected: PASS; integration-tagged tests may report skipped without `CLICKHOUSE_ADDR`.

- [ ] **Step 6: Commit the task**

```bash
git add internal/storage/clickhouse/reader.go internal/storage/clickhouse/reader_args_test.go internal/storage/clickhouse/integration_test.go
git commit -m "fix(clickhouse): ignore sampleless cycle successes"
```

### Task 5: Full verification and documentation

**Files:**
- Modify: `docs/superpowers/specs/2026-09-08-mtr-direct-target-loss-design.md`
- Create: `docs/superpowers/plans/2026-09-08-mtr-loss-and-icmp-zero-latency.md`

**Interfaces:**
- Consumes: all four implementation tasks.
- Produces: a reproducible verification record and committed approved design/plan.

- [ ] **Step 1: Format and inspect diagnostics**

Run `goimports -w` on every changed Go file. Run gopls diagnostics for each changed package and resolve every new diagnostic.

- [ ] **Step 2: Run full Go verification**

Run: `go build ./...`

Run: `go test ./...`

Run: `go test -race ./internal/probe ./internal/cluster/...`

Run: `go vet ./...`

Expected: every command exits 0.

- [ ] **Step 3: Verify the embedded UI artifact**

Run: `npm run build` from `ui/`.

Expected: palette checks, TypeScript build, and Vite build exit 0.

- [ ] **Step 4: Run integration tests when ClickHouse is configured**

Run: `go test -tags=integration ./internal/storage/clickhouse`

Expected: PASS when `CLICKHOUSE_ADDR` is available; otherwise report the exact environmental skip/failure without claiming live verification.

- [ ] **Step 5: Review the final diff and commit docs**

Run: `git diff --check`

Run: `git status --short`

Then commit only the approved design and implementation plan:

```bash
git add -f docs/superpowers/specs/2026-09-08-mtr-direct-target-loss-design.md docs/superpowers/plans/2026-09-08-mtr-loss-and-icmp-zero-latency.md
git commit -m "docs: cover icmp zero-latency regression"
```

## Self-review

- Spec coverage: direct MTR loss, partial/error ownership, DTO handoff, cache cloning, ICMP cancellation, rolling-upgrade normalization, historical reads, security bounds, verification, and rollback all map to tasks above.
- Placeholder scan: no deferred implementation or test placeholders remain.
- Type consistency: every task uses existing `probe.Result`, `stats.Summary`, `cluster.CyclePayload`, `scheduler.Cycle`, `storage.HopsResult`, and `storage.CyclePoint` field names.
