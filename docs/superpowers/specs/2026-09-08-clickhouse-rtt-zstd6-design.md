# ClickHouse RTT ZSTD(6) Design

## Goal

Replace `CODEC(Gorilla, ZSTD(1))` with `CODEC(ZSTD(6))` for the Float64
`rtt_ms` columns in `probe_rtt` and `probe_http`. Tests against production data
showed a 38.9% reduction for a representative `probe_rtt.rtt_ms` part, at an
approximately 2.9x encoding-time cost and no material decompression regression.

## Repository changes

- Declare `rtt_ms Float64 CODEC(ZSTD(6))` in the fresh-table DDL for
  `probe_rtt` and `probe_http`.
- Add idempotent bootstrap statements that modify those columns on existing
  tables. Emit `ON CLUSTER <cluster>` when cluster mode is configured.
- Keep codec reconciliation separate from column-addition and retention logic,
  with tests covering standalone and clustered statement generation.
- Update documentation that describes Gorilla as the codec for all float
  columns or claims the detail RTT tables already compress well.

Bootstrap changes column metadata for existing tables. It must not run
`OPTIMIZE TABLE ... FINAL`; forcing a complete rewrite on every process restart
would create unbounded and unnecessary production I/O.

## Deployment and rewrite

After tests pass, push the repository commit, pull it on `smokeping-master`, and
rebuild/restart the repository's existing Docker deployment. The restarted
application applies the idempotent codec metadata changes using its configured
ClickHouse connection.

Verify service health and query ClickHouse metadata for both live column codec
expressions. Then run an explicit full rewrite for `probe_rtt` and `probe_http`
with `OPTIMIZE TABLE ... FINAL`, using the deployment's existing authenticated
execution path. Run one table at a time, beginning with the small `probe_http`
table, and confirm completion and disk headroom before rewriting `probe_rtt`.

## Safety and rollback

- Do not read or expose database credentials; use the application's existing
  configured access path.
- Check free disk space before rewriting. ClickHouse may temporarily require
  space for old and newly merged parts concurrently.
- If deployment health checks fail, restore the previous image/commit and
  restart the service. Existing ZSTD(6) parts remain readable after rollback.
- If rewrite capacity is insufficient, leave the metadata change in place and
  let normal merges migrate historical data gradually.

## Verification

- A focused unit test must fail against the old Gorilla DDL/bootstrap behavior
  and pass after the implementation.
- Run `goimports` on changed Go files, `go test ./...`, `go build ./...`, and the
  repository's configured linter (`make lint`).
- After deployment, verify Docker service/container state, application health,
  live ClickHouse codec metadata, rewrite completion, and resulting part sizes.
