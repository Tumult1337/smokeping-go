# Webhook Packet Loss Resolve Grace Design

## Goal

Prevent a flapping packet loss alert from repeatedly closing and reopening the
same webhook notification during a network attack.

## Behavior

Webhook firing notifications remain immediate. When an alert transitions from
`firing` to `ok`, the `ActionDispatcher` holds that webhook resolve for two
minutes. If the same target and alert fires again during that period, the
pending resolve is canceled and no recovery webhook is sent. If the target
stays healthy for the full period, one resolve webhook is delivered with the
existing `ok` state and message rendering, including the configured message
(`no packetloss` when configured that way).

The grace applies to webhook actions only. Evaluator state transitions and
other action types remain immediate. A latency alert receives the same grace
only if it is configured to use the affected webhook action.

## Design

`ActionDispatcher` owns a mutex-protected map keyed by target ID and alert name.
Each pending resolve stores its event, captured action, and cancellation
function. Dispatching a firing event cancels and removes the matching pending
resolve before delivering the firing webhook. Dispatching a resolve replaces
any existing pending resolve and schedules delivery after the two-minute
constant. The delayed delivery uses a fresh bounded context because the
original dispatch context ends when the evaluator worker returns.

The dispatcher exposes a `Close` method for shutdown cleanup. It cancels all
pending timers and prevents them from delivering after shutdown. Existing
constructors and direct test construction remain valid.

## Testing

Tests use a short injected grace duration to verify immediate firing, delayed
resolve, cancellation on refiring, replacement of duplicate pending resolves,
and shutdown cancellation. Existing webhook payload tests are updated to
explicitly flush or use the injected duration so they remain deterministic.
