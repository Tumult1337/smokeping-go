package influxv2

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/influxdata/influxdb-client-go/v2/domain"

	"github.com/tumult/gosmokeping/internal/config"
	"github.com/tumult/gosmokeping/internal/stats"
)

// Retention periods for the four tiers (seconds). The 5m tier matches raw
// at 7d because it only serves ≤24h chart queries; longer spans hop to
// the 1h tier whose 180d window is the actual archive horizon.
const (
	retRaw = int64(7 * 24 * 3600)       // 7d
	ret5m  = int64(7 * 24 * 3600)       // 7d
	ret1h  = int64(180 * 24 * 3600)     // 180d
	ret1d  = int64(2 * 365 * 24 * 3600) // 2y
)

// Bootstrap ensures buckets and rollup tasks exist for the v2 backend.
// Safe to call repeatedly. Existing buckets keep their retention with one
// exception: a managed bucket whose current retention is infinite gets
// corrected to the managed default (e.g. 7d for raw). Infinite retention
// on a managed bucket is almost always a holdover from earlier releases
// that left raw uncapped, and leaving it uncapped is what fills the disk
// and wedges the writer. Operators who want a *different* finite retention
// can set it after bootstrap; we won't second-guess any non-zero value.
// Existing rollup tasks keep their Flux body.
func Bootstrap(ctx context.Context, log *slog.Logger, cfg config.InfluxV2) error {
	if cfg.Bucket1h == "" || cfg.Bucket1d == "" {
		log.Info("rollup buckets partially unconfigured — task bootstrap may be partial",
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
	if cfg.Bucket5m != "" {
		buckets = append(buckets, struct {
			name      string
			retention int64
		}{cfg.Bucket5m, ret5m})
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
		existing, err := bAPI.FindBucketByName(ctx, b.name)
		if err == nil {
			if isInfiniteRetention(existing) {
				rrType := domain.RetentionRuleTypeExpire
				existing.RetentionRules = domain.RetentionRules{{
					EverySeconds: b.retention,
					Type:         &rrType,
				}}
				if _, err := bAPI.UpdateBucket(ctx, existing); err != nil {
					return fmt.Errorf("update bucket %q retention: %w", b.name, err)
				}
				log.Warn("corrected infinite retention on managed bucket",
					"name", b.name, "retention_s", b.retention)
				continue
			}
			log.Info("bucket exists", "name", b.name)
			continue
		}
		rrType := domain.RetentionRuleTypeExpire
		if _, err := bAPI.CreateBucketWithNameWithID(ctx, orgID, b.name, domain.RetentionRule{
			EverySeconds: b.retention,
			Type:         &rrType,
		}); err != nil {
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

	if cfg.Bucket5m != "" {
		if err := ensureTask(ctx, log, client, orgID, "gosmokeping-5m-v1", fluxRollup(cfg.BucketRaw, cfg.Bucket5m, 5*time.Minute), "5m"); err != nil {
			return err
		}
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

// isInfiniteRetention reports whether a managed bucket currently has no
// expire policy — either zero rules at all, or any rule with EverySeconds == 0
// (which the v2 API documents as infinite retention).
func isInfiniteRetention(b *domain.Bucket) bool {
	if len(b.RetentionRules) == 0 {
		return true
	}
	for _, r := range b.RetentionRules {
		if r.EverySeconds == 0 {
			return true
		}
	}
	return false
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
// preserving the smoke band: min of mins, max of maxes, mean of medians, mean
// of each percentile, sum of losses and sent. Percentile entries are driven
// by stats.PercentileSet so adding or removing a percentile is a single-file
// edit and can't drift out of sync with the writer/reader.
func fluxRollup(srcBucket, dstBucket string, window time.Duration) string {
	base := []struct{ field, fn string }{
		{"rtt_min", "min"},
		{"rtt_max", "max"},
		{"rtt_mean", "mean"},
		{"rtt_median", "mean"},
		{"rtt_stddev", "mean"},
	}
	tail := []struct{ field, fn string }{
		{"loss_pct", "mean"},
		{"loss_count", "sum"},
		{"pings_sent", "sum"},
	}

	var lines strings.Builder
	for _, e := range base {
		fmt.Fprintf(&lines, "  agg(field: %q, fn: %s),\n", e.field, e.fn)
	}
	for _, spec := range stats.PercentileSet {
		fmt.Fprintf(&lines, "  agg(field: %q, fn: mean),\n", "rtt_"+spec.Name)
	}
	for _, e := range tail {
		fmt.Fprintf(&lines, "  agg(field: %q, fn: %s),\n", e.field, e.fn)
	}

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
%s])
  |> to(bucket: "%s")
`, fluxEscape(srcBucket), formatEvery(window), lines.String(), fluxEscape(dstBucket))
}

func formatEvery(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}
