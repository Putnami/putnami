package cliagg

import (
	"context"
	"errors"

	"go.putnami.dev/database"
)

// ErrSnapshotOverflow reports that a snapshot exceeded the bounded flush
// capacity and was rejected without writing a partial aggregate.
var ErrSnapshotOverflow = errors.New("cli aggregate snapshot exceeds bounded flush capacity")

const (
	retentionSQL = `WITH
contributors AS (DELETE FROM cli_daily_contributor WHERE day < (now() AT TIME ZONE 'UTC')::date - 34 OR day > (now() AT TIME ZONE 'UTC')::date),
counters AS (DELETE FROM cli_daily_counter WHERE day < (now() AT TIME ZONE 'UTC')::date - 34 OR day > (now() AT TIME ZONE 'UTC')::date),
devices AS (DELETE FROM cli_device_day WHERE day < (now() AT TIME ZONE 'UTC')::date - 34 OR day > (now() AT TIME ZONE 'UTC')::date),
overflows AS (DELETE FROM cli_aggregate_overflow WHERE day < (now() AT TIME ZONE 'UTC')::date - 34 OR day > (now() AT TIME ZONE 'UTC')::date),
flushes AS (DELETE FROM cli_applied_flush WHERE created_at < now() - interval '35 days')
DELETE FROM cli_cardinality_admission WHERE day < (now() AT TIME ZONE 'UTC')::date - 34 OR day > (now() AT TIME ZONE 'UTC')::date`

	claimFlushSQL = `INSERT INTO cli_applied_flush(flush_id) VALUES ($1) ON CONFLICT DO NOTHING`

	upsertDeviceDaysSQL = `INSERT INTO cli_device_day(day, device_id)
SELECT day, device_id FROM unnest($1::date[], $2::text[]) AS rows(day, device_id)
ON CONFLICT (day, device_id) DO NOTHING`

	upsertCountersSQL = `INSERT INTO cli_daily_counter(day, dimension, key, count)
SELECT day, dimension, key, count
FROM unnest($1::date[], $2::text[], $3::text[], $4::bigint[]) AS rows(day, dimension, key, count)
ON CONFLICT (day, dimension, key)
DO UPDATE SET count = cli_daily_counter.count + EXCLUDED.count`

	upsertContributorsSQL = `INSERT INTO cli_daily_contributor(day, dimension, key, device_id)
SELECT day, dimension, key, device_id
FROM unnest($1::date[], $2::text[], $3::text[], $4::text[]) AS rows(day, dimension, key, device_id)
ON CONFLICT (day, dimension, key, device_id) DO NOTHING`

	markOverflowSQL = `INSERT INTO cli_aggregate_overflow(day, dimension)
SELECT day, dimension FROM unnest($1::date[], $2::text[]) AS rows(day, dimension)
ON CONFLICT (day, dimension) DO NOTHING`
)

// Flush persists a bounded snapshot with a constant number of set-based
// statements in one transaction. A generated snapshot ID makes replaying the
// exact drained snapshot a no-op; new snapshots remain additive.
func Flush(ctx context.Context, pool *database.Pool, snap Snapshot) error {
	if len(snap.DeviceDays) > maxAccumulatorDeviceDays ||
		len(snap.Counters) > maxAccumulatorCounters ||
		len(snap.Contributors) > maxAccumulatorContributors ||
		len(snap.Overflows) > maxAccumulatorOverflows {
		return ErrSnapshotOverflow
	}
	if pool == nil {
		return nil
	}

	return database.WithTx(ctx, pool, func(txCtx context.Context) error {
		if _, err := pool.Exec(txCtx, retentionSQL); err != nil {
			return err
		}
		if snap.ID != "" {
			tag, err := pool.Exec(txCtx, claimFlushSQL, snap.ID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return nil
			}
		}

		if len(snap.DeviceDays) > 0 {
			days, devices := make([]string, len(snap.DeviceDays)), make([]string, len(snap.DeviceDays))
			for i, row := range snap.DeviceDays {
				days[i], devices[i] = row.Day, row.DeviceID
			}
			if _, err := pool.Exec(txCtx, upsertDeviceDaysSQL, days, devices); err != nil {
				return err
			}
		}
		if len(snap.Counters) > 0 {
			days := make([]string, len(snap.Counters))
			dims := make([]string, len(snap.Counters))
			keys := make([]string, len(snap.Counters))
			counts := make([]int64, len(snap.Counters))
			for i, row := range snap.Counters {
				days[i], dims[i], keys[i], counts[i] = row.Day, row.Dimension, row.Key, row.Count
			}
			if _, err := pool.Exec(txCtx, upsertCountersSQL, days, dims, keys, counts); err != nil {
				return err
			}
		}
		if len(snap.Contributors) > 0 {
			days := make([]string, len(snap.Contributors))
			dims := make([]string, len(snap.Contributors))
			keys := make([]string, len(snap.Contributors))
			devices := make([]string, len(snap.Contributors))
			for i, row := range snap.Contributors {
				days[i], dims[i], keys[i], devices[i] = row.Day, row.Dimension, row.Key, row.DeviceID
			}
			if _, err := pool.Exec(txCtx, upsertContributorsSQL, days, dims, keys, devices); err != nil {
				return err
			}
		}
		if len(snap.Overflows) > 0 {
			days := make([]string, len(snap.Overflows))
			dimensions := make([]string, len(snap.Overflows))
			for i, row := range snap.Overflows {
				days[i], dimensions[i] = row.Day, row.Dimension
			}
			if _, err := pool.Exec(txCtx, markOverflowSQL, days, dimensions); err != nil {
				return err
			}
		}
		return nil
	})
}
