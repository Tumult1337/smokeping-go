# gosmokeping — Grafana dashboard

`dashboard.json` provides a SmokePing-style smoke-band view backed by the
gosmokeping InfluxDB measurements.

## Import

1. In Grafana: **Dashboards → New → Import**, upload `dashboard.json`.
2. Select your InfluxDB v2 Flux datasource when prompted for `DS_INFLUXDB`.
3. (Optional) Edit the `bucket` template variable if your raw bucket is named
   something other than `smokeping_raw`.

## Provisioning

Drop the file into Grafana's provisioned dashboards directory and reference it
from a dashboard provider:

```yaml
# /etc/grafana/provisioning/dashboards/gosmokeping.yaml
apiVersion: 1
providers:
  - name: gosmokeping
    folder: Network
    type: file
    options:
      path: /var/lib/grafana/dashboards/gosmokeping
```

## Panels

- **Latency — $target (smoke band):** median line plus filled bands for
  min/max (lightest) and p5/p95 (medium). Overrides use Grafana's
  `fillBelowTo` to produce the band effect; no custom plugin required.
- **Loss % — $target:** bar chart of per-cycle packet loss.

Both panels repeat over the `target` variable so you get one row per target.
