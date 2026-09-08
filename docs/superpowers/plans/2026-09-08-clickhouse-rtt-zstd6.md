# ClickHouse RTT ZSTD(6) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make fresh and existing ClickHouse `probe_rtt.rtt_ms` and `probe_http.rtt_ms` columns use `CODEC(ZSTD(6))`, then deploy and rewrite existing parts.

**Architecture:** Fresh-table DDL owns the desired schema, while a small bootstrap reconciliation function emits idempotent `MODIFY COLUMN` statements for existing standalone or clustered tables. Deployment restarts the existing Docker service so bootstrap applies metadata; a one-time, separately invoked `OPTIMIZE TABLE ... FINAL` rewrites historical parts.

**Tech Stack:** Go 1.26.1, ClickHouse SQL, Docker Compose, Go unit/integration tests.

## Global Constraints

- Do not read, print, or copy ClickHouse credential material.
- Do not run a full rewrite from application bootstrap.
- Rewrite `probe_http` before `probe_rtt`, one table at a time, after checking free disk space.
- Preserve the existing clustered `ON CLUSTER <cluster>` behavior.
- Do not force `OPTIMIZE FINAL` if disk headroom or service health is insufficient; normal merges are the rollback path.

---

### Task 1: Pin the desired RTT codecs and reconcile existing tables

**Files:**
- Modify: `internal/storage/clickhouse/schema.go`
- Modify: `internal/storage/clickhouse/schema_test.go`
- Modify: `internal/storage/clickhouse/bootstrap.go`
- Modify: `internal/storage/clickhouse/bootstrap_test.go`

**Interfaces:**
- Consumes: `Bootstrap(ctx context.Context, log *slog.Logger, cfg config.ClickHouse) error`
- Produces: `modifyCodecStatements(cluster string) []string`

- [ ] **Step 1: Write failing schema and bootstrap tests**

Add a schema test that extracts both `rtt_ms` declarations and requires exactly `rtt_ms Float64 CODEC(ZSTD(6))`. Add standalone and clustered bootstrap tests requiring these statements:

```sql
ALTER TABLE probe_rtt MODIFY COLUMN rtt_ms Float64 CODEC(ZSTD(6))
ALTER TABLE probe_http MODIFY COLUMN rtt_ms Float64 CODEC(ZSTD(6))
ALTER TABLE probe_rtt ON CLUSTER ch1 MODIFY COLUMN rtt_ms Float64 CODEC(ZSTD(6))
ALTER TABLE probe_http ON CLUSTER ch1 MODIFY COLUMN rtt_ms Float64 CODEC(ZSTD(6))
```

- [ ] **Step 2: Verify RED**

Run:

```bash
go test ./internal/storage/clickhouse -run 'TestSchemaRTTCodecs|TestModifyCodecStatements'
```

Expected: FAIL because the DDL still contains Gorilla and `modifyCodecStatements` does not exist.

- [ ] **Step 3: Implement the minimal schema and bootstrap changes**

Change both detail RTT DDL declarations to `rtt_ms Float64 CODEC(ZSTD(6))`. Implement `modifyCodecStatements(cluster string) []string` to emit the two exact statements above, and execute them in `Bootstrap` after table creation and additive migrations but before TTL reconciliation. Wrap execution errors as `modify codec: %w (stmt: %s)`.

- [ ] **Step 4: Format and verify GREEN**

Run:

```bash
goimports -w internal/storage/clickhouse/schema.go internal/storage/clickhouse/schema_test.go internal/storage/clickhouse/bootstrap.go internal/storage/clickhouse/bootstrap_test.go
go test ./internal/storage/clickhouse -run 'TestSchemaRTTCodecs|TestModifyCodecStatements'
```

Expected: PASS.

### Task 2: Align operator documentation and verify the repository

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`
- Modify: `docs/migrate-rtt-microseconds.md`

**Interfaces:**
- Consumes: desired schema from Task 1
- Produces: accurate operator documentation distinguishing aggregate float codecs from detail RTT codecs

- [ ] **Step 1: Update documentation**

State that detail `Float64 rtt_ms` columns use `ZSTD(6)` because production RTT mantissas made Gorilla ineffective. Remove the claim that `probe_rtt` and `probe_http` already compress well; keep the microsecond migration document clear that it does not change those tables' types.

- [ ] **Step 2: Run full verification**

Run:

```bash
goimports -w internal/storage/clickhouse/schema.go internal/storage/clickhouse/schema_test.go internal/storage/clickhouse/bootstrap.go internal/storage/clickhouse/bootstrap_test.go
go test ./...
go build ./...
make lint
git diff --check
```

Expected: every command exits 0.

- [ ] **Step 3: Commit and push**

```bash
git add internal/storage/clickhouse/schema.go internal/storage/clickhouse/schema_test.go internal/storage/clickhouse/bootstrap.go internal/storage/clickhouse/bootstrap_test.go README.md CLAUDE.md docs/migrate-rtt-microseconds.md docs/superpowers/plans/2026-09-08-clickhouse-rtt-zstd6.md
git commit -m "perf(clickhouse): use ZSTD6 for detail RTTs"
git push
```

### Task 3: Deploy, verify, and rewrite historical parts

**Files:**
- Inspect: the existing Docker Compose configuration on `smokeping-master`

**Interfaces:**
- Consumes: pushed commit from Task 2 and the host's existing deployment configuration
- Produces: healthy deployed service, live `ZSTD(6)` metadata, and rewritten historical RTT parts

- [ ] **Step 1: Resolve deployment paths and capacity read-only**

Over SSH, inspect running containers, Compose labels/config, current checkout/commit, filesystem free space, and ClickHouse table sizes. Do not inspect environment values or credential files.

- [ ] **Step 2: Pull and restart using the deployment's existing commands**

Run `git pull --ff-only`, then the repository's documented Docker Compose build/restart command. If pull is not a fast-forward or the checkout is dirty, stop without overwriting host changes.

- [ ] **Step 3: Verify deployment before rewrite**

Confirm container/service health, application health endpoint, logs without secret values, and live codec metadata for both columns through the application's existing authenticated execution path.

- [ ] **Step 4: Rewrite one table at a time**

Run:

```sql
OPTIMIZE TABLE gosmokeping.probe_http FINAL;
OPTIMIZE TABLE gosmokeping.probe_rtt FINAL;
```

Only begin `probe_rtt` after `probe_http` completes and free-space/service checks remain healthy.

- [ ] **Step 5: Verify final state**

Confirm both optimizations completed, both codec expressions remain `CODEC(ZSTD(6))`, service health is good, and active-part compressed sizes decreased. Report every verification command and exit status.
