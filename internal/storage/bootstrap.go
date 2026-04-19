package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/influxdata/influxdb-client-go/v2/domain"

	"github.com/tumult/gosmokeping/internal/config"
)

// Retention periods for the three tiers (seconds).
const (
	retRaw = int64(7 * 24 * 3600)       // 7d
	ret1h  = int64(180 * 24 * 3600)     // 180d
	ret1d  = int64(2 * 365 * 24 * 3600) // 2y
)

// Bootstrap ensures buckets and rollup tasks exist. Safe to call repeatedly.
// If a bucket already exists its retention is not modified (operators may have
// intentionally tuned it). If a task with the same name exists, its Flux is
// left as-is for the same reason.
func Bootstrap(ctx context.Context, log *slog.Logger, cfg config.InfluxDB) error {
	if cfg.Bucket1h == "" || cfg.Bucket1d == "" {
		log.Info("rollup buckets not configured, skipping task bootstrap",
			"bucket_1h", cfg.Bucket1h, "bucket_1d", cfg.Bucket1d)
	}

	client := influxdb2.NewClient(cfg.URL, cfg.Token)
	defer client.Close()

	orgID, err := lookupOrgID(ctx, client, cfg.Org)
	if err != nil {
		return fmt.Errorf("lookup org %q: %w", cfg.Org, err)
	}

	buckets := []struct {
		name      string
		retention int64
	}{
		{cfg.BucketRaw, retRaw},
	}
	if cfg.Bucket1h != "" {
		buckets = append(buckets, struct {
			name      string
			retention int64
		}{cfg.Bucket1h, ret1h})
	}
	if cfg.Bucket1d != "" {
		buckets = append(buckets, struct {
			name      string
			retention int64
		}{cfg.Bucket1d, ret1d})
	}

	bAPI := client.BucketsAPI()
	for _, b := range buckets {
		if _, err := bAPI.FindBucketByName(ctx, b.name); err == nil {
			log.Info("bucket exists", "name", b.name)
			continue
		}
		rrType := domain.RetentionRuleTypeExpire
		_, err := bAPI.CreateBucketWithNameWithID(ctx, orgID, b.name, domain.RetentionRule{
			EverySeconds: b.retention,
			Type:         &rrType,
		})
		if err != nil {
			return fmt.Errorf("create bucket %q: %w", b.name, err)
		}
		log.Info("bucket created", "name", b.name, "retention_s", b.retention)
	}

	// Task names are versioned so a schema change (e.g. new percentile fields)
	// triggers creation of a fresh task. Prior versions are deleted here so we
	// don't duplicate rollup writes.
	if err := deleteObsoleteTasks(ctx, log, client, orgID, "gosmokeping-1h", "gosmokeping-1h-v2", "gosmokeping-1h-v3"); err != nil {
		return err
	}
	if err := deleteObsoleteTasks(ctx, log, client, orgID, "gosmokeping-1d", "gosmokeping-1d-v2", "gosmokeping-1d-v3"); err != nil {
		return err
	}

	if cfg.Bucket1h != "" {
		if err := ensureTask(ctx, log, client, orgID, "gosmokeping-1h-v4", fluxRollup(cfg.BucketRaw, cfg.Bucket1h, time.Hour), "1h"); err != nil {
			return err
		}
	}
	if cfg.Bucket1d != "" {
		if err := ensureTask(ctx, log, client, orgID, "gosmokeping-1d-v4", fluxRollup(cfg.Bucket1h, cfg.Bucket1d, 24*time.Hour), "1d"); err != nil {
			return err
		}
	}
	return nil
}

// deleteObsoleteTasks removes task revisions older than `keep`. Safe to call
// repeatedly; no-ops when nothing matches. We look up each legacy name
// individually because older gosmokeping releases wrote under different names.
func deleteObsoleteTasks(ctx context.Context, log *slog.Logger, client influxdb2.Client, orgID string, legacyNames ...string) error {
	tAPI := client.TasksAPI()
	for _, name := range legacyNames {
		tasks, err := tAPI.FindTasks(ctx, &api.TaskFilter{Name: name, OrgID: orgID})
		if err != nil {
			return fmt.Errorf("list tasks %q: %w", name, err)
		}
		for _, t := range tasks {
			if err := tAPI.DeleteTask(ctx, &t); err != nil {
				return fmt.Errorf("delete task %q: %w", name, err)
			}
			log.Info("deleted obsolete rollup task", "name", name, "id", t.Id)
		}
	}
	return nil
}

func lookupOrgID(ctx context.Context, client influxdb2.Client, name string) (string, error) {
	org, err := client.OrganizationsAPI().FindOrganizationByName(ctx, name)
	if err != nil {
		return "", err
	}
	if org == nil || org.Id == nil {
		return "", errors.New("organization has no id")
	}
	return *org.Id, nil
}

func ensureTask(ctx context.Context, log *slog.Logger, client influxdb2.Client, orgID, name, flux, every string) error {
	tAPI := client.TasksAPI()
	tasks, err := tAPI.FindTasks(ctx, &api.TaskFilter{Name: name, OrgID: orgID})
	if err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}
	if len(tasks) > 0 {
		log.Info("rollup task exists", "name", name)
		return nil
	}
	if _, err := tAPI.CreateTaskWithEvery(ctx, name, flux, every, orgID); err != nil {
		return fmt.Errorf("create task %q: %w", name, err)
	}
	log.Info("rollup task created", "name", name, "every", every)
	return nil
}

// fluxRollup returns a Flux task that rolls probe_cycle from src to dst,
// preserving the smoke band: min of mins, max of maxes, mean of medians, p5 of
// p5s, p95 of p95s, sum of losses and sent.
func fluxRollup(srcBucket, dstBucket string, window time.Duration) string {
	return fmt.Sprintf(`
src = from(bucket: "%s")
  |> range(start: -task.every)
  |> filter(fn: (r) => r._measurement == "probe_cycle")

agg = (field, fn) =>
  src
    |> filter(fn: (r) => r._field == field)
    |> aggregateWindow(every: %s, fn: fn, createEmpty: false)
    |> set(key: "_measurement", value: "probe_cycle")

union(tables: [
  agg(field: "rtt_min",    fn: min),
  agg(field: "rtt_max",    fn: max),
  agg(field: "rtt_mean",   fn: mean),
  agg(field: "rtt_median", fn: mean),
  agg(field: "rtt_stddev", fn: mean),
  agg(field: "rtt_p5",     fn: mean),
  agg(field: "rtt_p10",    fn: mean),
  agg(field: "rtt_p15",    fn: mean),
  agg(field: "rtt_p20",    fn: mean),
  agg(field: "rtt_p25",    fn: mean),
  agg(field: "rtt_p30",    fn: mean),
  agg(field: "rtt_p35",    fn: mean),
  agg(field: "rtt_p40",    fn: mean),
  agg(field: "rtt_p45",    fn: mean),
  agg(field: "rtt_p55",    fn: mean),
  agg(field: "rtt_p60",    fn: mean),
  agg(field: "rtt_p65",    fn: mean),
  agg(field: "rtt_p70",    fn: mean),
  agg(field: "rtt_p75",    fn: mean),
  agg(field: "rtt_p80",    fn: mean),
  agg(field: "rtt_p85",    fn: mean),
  agg(field: "rtt_p90",    fn: mean),
  agg(field: "rtt_p95",    fn: mean),
  agg(field: "loss_pct",   fn: mean),
  agg(field: "loss_count", fn: sum),
  agg(field: "pings_sent", fn: sum),
])
  |> to(bucket: "%s")
`, fluxEscape(srcBucket), formatEvery(window), fluxEscape(dstBucket))
}

func formatEvery(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}
