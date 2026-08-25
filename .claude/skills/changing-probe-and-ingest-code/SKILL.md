---
name: changing-probe-and-ingest-code
description: Use when adding or changing a bound, a validator, a probe's output shape, a cluster-ingest rule, or a test that is meant to prove one of those correct — in gosmokeping. Also use when fixing a defect an adversarial review found here, because in this repo a fix has introduced a worse defect than the hole it closed more often than not. Trigger words — bound, limit, cap, max, validate, reject, refuse, truncate, dedup, mutation test, fail-first, regression test.
---

# Changing probe and ingest code

Five adversarial review rounds over this codebase produced the same handful of
mistakes repeatedly. Each was individually reasonable and each shipped anyway.
These are the specific shapes, with the real instances.

## Bounds

**Derive every bound from the producer's own maximum. Never from an estimate of
typical output.** Four consecutive rounds shipped a bound below what the code
already emits:

| Bound | Shipped | Producer's real maximum |
|---|---|---|
| `MaxHopsPerCycle` | 256 | `maxRounds(10) x maxTTL(30)` = 300 |
| `maxHopRows` | 300,000 | a 7d/15m/30-TTL/6-source timeline = 362,880 |
| `MaxHTTPErrLen` | 4096 | `url.Error` embeds an unbounded configured URL |
| `maxHopTimelineRows` (raw tier) | 172,288 | a 1-hop MTR target at 30ms = 240,000 in 2h |

The consequence is never a smaller DoS window — it is a legitimate producer
refused. Since a permanently-rejected batch now drops, one oversized field
discards up to `MaxCyclesPerBatch` unrelated cycles.

- Write the derivation in the code next to the constant, as an expression of
  real limits where possible (`2 * maxInterfaceNameLen`, `MaxCyclesPerBatch`,
  `probe_hop.ttl` being `UInt8`), not a literal with a comment.
- When the producer's constants are unexported and in another package, mirror
  them and pin the mirror with a test that parses the source and fails on
  drift. `internal/config/tracebounds_test.go` does this.
- If no derivable ceiling exists, that is the finding. Say so, and truncate at
  the producer instead of rejecting at the boundary.
- The same rule governs accepted *shapes*, not just lengths. Rejecting `%`
  inside an IPv6 zone refused a valid Linux interface name — a bound on a
  character set, set below what the producer emits.

## Fixes

**A fix here gets more scrutiny than the defect, not less.** Rounds 1–3 each
closed their findings and opened new ones:

- A hop bound that rejected legitimate output, whose rejection then poisoned the
  slave queue forever.
- A 5-minute clock-skew allowance that became an alert-quorum bypass, because
  the evaluator used the slave's timestamp as its liveness clock.
- A registry gate whose recovery path existed only in the binary that did not
  need it.
- A replay floor keyed on slave-supplied time, which muted a whole peer's
  alerting.

Before committing a fix, state what it now makes possible and test that. When a
review suggests a patch, run the patch before adopting it — one suggested here
(`min(cy.Time, now)`) reopened the bug it was meant to close.

## Tests

A passing test is not evidence. These specific shapes all passed while proving
nothing:

- **A fixture the code under test did not produce cannot fail first.** Two
  "fail-first" tests were already green because their input was a hand-written
  slice. They are pinning controls, not regression tests.
- **A stub that returns instantly collapses every loop iteration into the
  first**, making the loop's structure unobservable. A hoisted per-iteration
  computation passed the whole suite this way.
- **A test that drives a helper never covers the caller's loop.**
- **A test can assert the vulnerability.**
  `TestRedactTerminalHopSilentTerminalKeepsIntermediates` required intermediate
  addresses to survive a silent terminal — the exact fail-open shape — and had
  to be inverted.
- **A mutation must be confirmed applied before its result is believed.** A
  silently-unapplied mutation is indistinguishable from an uncaught one. One
  mutation here compiled to a byte-identical binary (`defaultICMPSpacing` vs the
  same literal), so the build cache legitimately hit — which is itself proof no
  test could ever catch it.

Mutation predictions written in a plan are guesses. Run every row. A row that
comes back other than predicted is a finding about the plan; report it, never
rewrite it to match.

## Seams that hide failures in this repo

- `internal/storage/clickhouse/integration_test.go` is behind the `integration`
  build tag, which the normal gate excludes. A signature change compiles green
  and breaks there. Run `go vet -tags integration ./internal/storage/clickhouse/`.
- The fake `driver.Conn` in `reader_args_test.go` returns no rows, so a dropped
  `Scan` destination is invisible to every unit test. Select-list-only columns
  also add no `?`, so the placeholder-parity guard does not see them. Only the
  live-ClickHouse tests catch either.
- A freshly bootstrapped database creates new columns via `CREATE TABLE`, so it
  cannot detect `Bootstrap` skipping `addColumnStatements`. The production
  instance is always the upgrade case; test it by dropping the columns and
  re-bootstrapping.
- `ui/` has no test runner. Verification there is the compiler, reasoning, or a
  manual reproduction against a real server — say which, and never claim more.
- `docs/superpowers/` is gitignored, so specs and plans never ride the commits
  that implement them.
