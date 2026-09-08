# MTR direct target-loss accounting

**Status:** approved 2026-09-08

## Problem

MTR currently derives target-level `Sent` and `LossCount` from TTL-walk
reachability. A round keeps increasing TTL until an echo or unreachable reply
ends it. If a packet that already had enough TTL to reach the destination is
lost, a later probe at a higher TTL can answer and mark the whole round as
reached. One eventual echo therefore hides earlier unanswered target probes
and biases MTR target loss toward zero.

The production transition from ICMP to MTR demonstrates the mismatch. For
`as/tube-hosting`, all six sources regularly recorded direct ICMP loss until
the switch around 2026-09-08 03:36 CEST, then every source began reporting 0%
target loss while current `probe_hop` rows still contained unanswered probes.
The two measurements do not currently have comparable semantics.

Two independent handoff omissions also make the MTR history misleading:

- `queryHopsGrid` computes `HopPoint.ReplyCount`, but `getHopsTimeline` does
  not copy it into `hopTimelineDTO`, so JSON always publishes `ReplyCount: 0`.
- `QueryHopsTimeline` returns `HopsResult.TimelineLoss`, but
  `cloneHopsResult` copies only `Hops` and `Cycles`, so the caching reader
  always drops timeline target loss before it reaches the API.

Yesterday's partial-round fix also introduced a separate regression in the
shared ICMP probe used by slave health. The probe increments `Sent` before
waiting for a reply, then returns on cycle cancellation without recording
either a loss or an RTT. That produces an impossible cycle such as
`Sent=10, LossCount=9, RTTs=[]`: downstream code infers one success, while its
latency summary is all zero. These rows render as 0 ms lines on a log chart.

## Chosen approach

MTR will run two measurements concurrently under the existing cycle context:

1. A direct ICMP echo batch measures the destination's RTT and packet loss.
2. The existing TTL walk measures per-hop path, RTT, and loss.

The MTR result takes `Sent`, `LossCount`, and target RTT samples only from the
direct batch. It takes `Hops` only from the TTL walk. TTL-walk reachability no
longer changes target-level loss, so a later path-discovery reply cannot erase
an unanswered direct probe.

The batch and walk both use the MTR count after its existing ten-round cap.
This keeps the displayed denominator and traffic ceiling unchanged. Running
them concurrently keeps cycle duration at approximately the slower operation,
not their sum.

### Alternatives rejected

- Derive target loss from rows marked `TargetReply`: cheap, but wrong when a
  route changes length because losses at an old target TTL may belong to a new
  intermediate hop. Multiple target rows can also count one round repeatedly.
- Keep reachability loss and only relabel it: internally consistent, but it
  does not provide the direct packet-loss measurement required for alerts and
  comparison with historical ICMP cycles.

## Data flow and ownership

MTR owns one direct-batch result and one trace result per call. The TTL walk
runs through a bounded result channel and is joined on every return path so no
goroutine or raw socket outlives the cycle. The direct batch reuses the ICMP
probe's existing timeout budgeting, reply validation, and actual-attempt
accounting rather than introducing a second echo implementation.

The combined result is:

- `RTTs`, `Sent`, `LossCount`: direct echo batch;
- `Hops`: TTL walk;
- error: contextual error from either component, while preserving every real
  partial measurement the other component completed.

If direct probing sends nothing, target loss remains unknown (`Sent == 0`);
the scheduler/writer already omit such a `probe_cycle` row. Real hop rows may
still be retained. If the trace fails, direct target measurements remain real
and can be stored, while the returned error keeps the missing path visible in
logs. Context cancellation is honored by both operations.

No schema or wire-format migration is required. Existing ICMP and MTR history
is not rewritten. New MTR `probe_cycle` rows use direct packet-loss semantics.

## ICMP cancellation invariant

An echo attempt interrupted by the cycle context has not completed its
observation window. The ICMP probe will remove that attempt from `Sent` before
returning its partial result. Every emitted result therefore preserves
`len(RTTs) == Sent - LossCount`: completed failures count as loss, completed
replies carry an RTT, and an unfinished attempt counts as neither.

For rolling upgrades, master ingest will normalize a payload whose successful
count exceeds its RTT sample count. It will remove only those sample-less
successes from `Sent` and recompute the latency summary from the received RTTs.
This accepts old-slave data without manufacturing zero-latency replies or
rejecting the rest of its batch. Payloads where counters claim fewer successes
than samples remain invalid rather than silently discarding real samples.

Existing malformed rows cannot be repaired in storage without guessing at
events that were never measured. Cycle reads will apply the same conservative
normalization: sample-less successes are removed from the aggregate sent
denominator and zero summaries carry no latency weight. This repairs current
history views while preserving every completed loss and every real RTT.

## History handoff fixes

`getHopsTimeline` will copy `h.ReplyCount` into `hopTimelineDTO`. The existing
optional TypeScript fallback remains for rolling upgrades against older
servers.

`cloneHopsResult` will deep-copy `TimelineLoss` alongside `Hops` and `Cycles`.
This applies to cold leaders, cache hits, waiters, and stale-cache fallback,
all of which pass through the same clone helper.

These counts contain no new address or identity information and do not alter
the endpoint's existing row bounds. The security surface is unchanged.

## Error and resource behavior

The direct batch and trace each open their existing bounded ICMP socket and
share the scheduler's deadline. Peak MTR cost increases by one socket and one
bounded operation per in-flight target cycle. No retries, worker pools, or
unbounded queues are added.

Reply validation remains fail-closed: echo type, sequence, identifier where
applicable, and resolved destination must match. A hostile network reply cannot
become a target success merely because the trace accepted another packet.

## Testing and verification

Regression tests will prove:

- a trace that eventually reaches the target cannot overwrite two losses from
  a ten-probe direct batch;
- direct partial/error results and trace partial/error results retain only the
  measurements actually taken, without leaking a goroutine;
- timeline JSON publishes a non-zero `ReplyCount` supplied by storage;
- the caching reader preserves and isolates `TimelineLoss` on misses and hits.
- cancellation after an ICMP send does not leave a sample-less success;
- master ingest normalizes an old-slave sample-less success but rejects a
  payload carrying more RTTs than its counters permit;
- raw and bucketed cycle reads exclude existing sample-less successes and do
  not emit a zero-latency percentile for them.

Tests will be written and observed failing before production edits. After the
focused tests pass, changed Go files will be formatted and the repository will
run `go build ./...`, `go test ./...`, the configured Go linter, and
`go test -race` for the probe package because MTR gains concurrent ownership.
The UI build will verify the unchanged TypeScript compatibility fallback and
the embedded frontend artifact.

## Rollback

Reverting the implementation commit restores reachability-based MTR target
loss, the two history omissions, and the ICMP cancellation regression without
requiring a schema rollback. Stored rows remain readable in either version.
