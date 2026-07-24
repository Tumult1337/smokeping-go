# Local slave-health-mesh test harness

A full gosmokeping cluster on one host — ephemeral ClickHouse, a master, and
two slaves — on a private bridge network with static IPs. It exercises the
slave health mesh and alert quorum end to end without a real multi-host fleet.

| Container       | IP            | Role                                    |
|-----------------|---------------|-----------------------------------------|
| `mesh-clickhouse` | 172.28.0.5  | ephemeral storage (throwaway volume)    |
| `mesh-master`     | 172.28.0.10 | master + UI/API on `localhost:8080`     |
| `mesh-slave-a`    | 172.28.0.11 | slave, `advertise: 172.28.0.11`         |
| `mesh-slave-b`    | 172.28.0.12 | slave, `advertise: 172.28.0.12`         |

Each slave has its own address, so — unlike the production `network_mode: host`
compose — it **must** set `cluster.advertise` explicitly. That is the exact
bridge/k8s case the mesh was designed for. The master pins both addresses via
`cluster.slave_addrs`. The master probes both slaves and each slave probes the
other, so every health target has **two** independent observers — enough to
exercise `"quorum": "majority"` on the `slave-unreachable` alert.

## Run

```bash
./deploy/local-mesh/build.sh                                   # host-build the static binary
docker compose -f deploy/local-mesh/docker-compose.yml up -d --build
```

`build.sh` compiles on the host because `npm ci` dies inside the build
container on this machine ("Exit handler never called"). The binary embeds the
UI, so run `make ui` once first if `internal/ui/dist` is empty. Re-run
`build.sh` then `up -d --build` after any code change.

UI: <http://localhost:8080> — the sidebar shows a collapsible **Slaves** group.

## Verify

```bash
# Health targets exist, carry NO host, and list two observers each:
curl -s localhost:8080/api/v1/targets \
  | jq '.[] | select(.group=="_cluster") | {id, host, sources, alerts}'
# → host is absent; sources are ["master","slave-<other>"]; alerts ["slave-unreachable"]

# Slave-pushed health cycles reach storage (not just the master's own view):
curl -s "localhost:8080/api/v1/targets/_cluster/slave-a/cycles?range=5m&source=slave-b" \
  | jq '.cycles | length'          # → > 0

# Terminal hop (the slave's address) is blanked:
curl -s localhost:8080/api/v1/targets/_cluster/slave-a/hops \
  | jq '.hops[].IP'                 # → "" for the terminal hop

# Quorum: take a slave down and watch the master fire once both observers agree.
docker stop mesh-slave-b
docker logs -f mesh-master | grep slave-unreachable
# → "next":"firing","firing":2,"live":2   (majority of 2, no address in the body)
docker start mesh-slave-b
# → "next":"ok","firing":1,"live":2       (resolves once, majority no longer met)
```

## Tear down

```bash
docker compose -f deploy/local-mesh/docker-compose.yml down -v   # -v wipes ClickHouse
```

## Notes

- **Storage is a throwaway container**, not the remote test ClickHouse. `down -v`
  erases it. To point at an external CH instead, edit `storage.clickhouse.addr`
  / `password` in `master.config.json` and drop the `clickhouse` service.
- The token (`local-mesh-dev-token`) and CH password (`clickhouse-dev`) are
  hardcoded dev values — fine for a loopback harness, never reuse them.
- Containers get `NET_RAW` + a permissive `ping_group_range` so ICMP echo and
  the traceroute TTL-walk work as non-root. Because the two slaves are direct
  bridge neighbors, a trace is a single hop, so every hop shown is the terminal
  one and is redacted — there are no intermediate hops to display here.
- Interval is 10s and retention is trimmed to days, so data accrues quickly and
  the volume stays small.
