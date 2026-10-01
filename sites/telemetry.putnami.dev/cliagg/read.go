package cliagg

import (
	"context"
	stderrors "errors"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/database"
)

var (
	// ErrNoDatasource reports that aggregate reads are unavailable because no
	// database pool was configured.
	ErrNoDatasource = stderrors.New("telemetry aggregate datasource unavailable")
	// ErrProjectionOverflow is reserved for legacy global markers and whole-read
	// ceilings. New write ceilings are dimension-scoped in the report itself.
	ErrProjectionOverflow = stderrors.New("telemetry aggregate projection overflow")
)

const maxCounterRows = 200_000

const (
	overflowSQL = `SELECT DISTINCT dimension
FROM cli_aggregate_overflow
WHERE day >= $1::date AND day < $2::date
ORDER BY dimension`

	deviceCountSQL = `SELECT count(DISTINCT device_id) FROM cli_device_day
WHERE day >= $1::date AND day < $2::date`

	dailyDeviceCountSQL = `SELECT to_char(day, 'YYYY-MM-DD'), count(DISTINCT device_id) FROM cli_device_day
WHERE day >= $1::date AND day < $2::date
GROUP BY day ORDER BY day`

	dailySessionSQL = `SELECT to_char(c.day, 'YYYY-MM-DD'), sum(c.count),
(SELECT count(DISTINCT dc.device_id) FROM cli_daily_contributor dc
 WHERE dc.day = c.day AND dc.dimension = $3)
FROM cli_daily_counter c
WHERE c.day >= $1::date AND c.day < $2::date AND c.dimension = $3
GROUP BY c.day ORDER BY c.day`

	dimensionContributorSQL = `SELECT dimension, count(DISTINCT device_id)
FROM cli_daily_contributor
WHERE day >= $1::date AND day < $2::date AND dimension IN (%s)
GROUP BY dimension`
)

var (
	counterSQL               = buildCounterSQL()
	dimensionContributorsSQL = buildDimensionContributorSQL()
)

func dimensionPlaceholders(offset int) string {
	placeholders := make([]string, 0, len(reportDimensions))
	for i := range reportDimensions {
		placeholders = append(placeholders, "$"+strconv.Itoa(i+offset))
	}
	return strings.Join(placeholders, ", ")
}

func buildCounterSQL() string {
	return `WITH counter_totals AS (
  SELECT dimension, key, sum(count) AS count
  FROM cli_daily_counter
  WHERE day >= $1::date AND day < $2::date
    AND dimension IN (` + dimensionPlaceholders(3) + `)
  GROUP BY dimension, key
), contributor_totals AS (
  SELECT dimension, key, count(DISTINCT device_id) AS contributors
  FROM cli_daily_contributor
  WHERE day >= $1::date AND day < $2::date
    AND dimension IN (` + dimensionPlaceholders(3) + `)
  GROUP BY dimension, key
)
SELECT c.dimension, c.key, c.count, COALESCE(d.contributors, 0)
FROM counter_totals c
LEFT JOIN contributor_totals d USING (dimension, key)
ORDER BY c.dimension, c.key
LIMIT ` + strconv.Itoa(maxCounterRows+1)
}

func buildDimensionContributorSQL() string {
	return strings.Replace(dimensionContributorSQL, "%s", dimensionPlaceholders(3), 1)
}

// ReportSource loads an aggregate report for a normalized reporting window.
type ReportSource interface {
	Report(ctx context.Context, w Window) (Report, error)
}

// PoolSource reads aggregate reports from a lazily resolved database pool.
type PoolSource struct {
	Pool func() *database.Pool
	Now  func() time.Time
}

// Report returns the aggregate report for w or ErrNoDatasource when the pool
// is not configured.
func (s PoolSource) Report(ctx context.Context, w Window) (Report, error) {
	var pool *database.Pool
	if s.Pool != nil {
		pool = s.Pool()
	}
	if pool == nil {
		return Report{}, ErrNoDatasource
	}
	data, err := readRaw(ctx, pool, w)
	if err != nil {
		return Report{}, err
	}
	return buildReport(w, data, s.now()), nil
}

func (s PoolSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func readRaw(ctx context.Context, pool *database.Pool, w Window) (raw, error) {
	data := raw{
		dailyDevices: map[string]int64{}, dailySessions: map[string]int64{},
		dailySessionContributors: map[string]int64{},
		counters:                 map[string]map[string]int64{},
		counterContributors:      map[string]map[string]int64{},
		dimensionContributors:    map[string]int64{},
		unavailable:              map[string]bool{},
	}
	err := database.WithTx(ctx, pool, func(txCtx context.Context) error {
		overflows, err := projectionOverflows(txCtx, pool, w)
		if err != nil {
			return err
		}
		if overflows["*"] {
			return ErrProjectionOverflow
		}
		data.unavailable = overflows
		if err := pool.QueryRow(txCtx, deviceCountSQL, w.Start, w.End).Scan(&data.devices); err != nil {
			return err
		}
		rows, err := pool.Query(txCtx, dailyDeviceCountSQL, w.Start, w.End)
		if err != nil {
			return err
		}
		for rows.Next() {
			var day string
			var n int64
			if err := rows.Scan(&day, &n); err != nil {
				rows.Close()
				return err
			}
			data.dailyDevices[normalizeDay(day)] = n
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		rows, err = pool.Query(txCtx, dailySessionSQL, w.Start, w.End, DimOutcome)
		if err != nil {
			return err
		}
		for rows.Next() {
			var day string
			var count, contributors int64
			if err := rows.Scan(&day, &count, &contributors); err != nil {
				rows.Close()
				return err
			}
			day = normalizeDay(day)
			data.dailySessions[day] = count
			data.dailySessionContributors[day] = contributors
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		args := reportArgs(w)
		rows, err = pool.Query(txCtx, counterSQL, args...)
		if err != nil {
			return err
		}
		n := 0
		for rows.Next() {
			n++
			if n > maxCounterRows {
				rows.Close()
				return ErrProjectionOverflow
			}
			var dim, key string
			var count, contributors int64
			if err := rows.Scan(&dim, &key, &count, &contributors); err != nil {
				rows.Close()
				return err
			}
			if data.counters[dim] == nil {
				data.counters[dim] = map[string]int64{}
				data.counterContributors[dim] = map[string]int64{}
			}
			data.counters[dim][key] = count
			data.counterContributors[dim][key] = contributors
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		rows, err = pool.Query(txCtx, dimensionContributorsSQL, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var dim string
			var contributors int64
			if err := rows.Scan(&dim, &contributors); err != nil {
				rows.Close()
				return err
			}
			data.dimensionContributors[dim] = contributors
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		// A second check closes the read-committed race with a flush that records
		// overflow while this report is being assembled.
		overflows, err = projectionOverflows(txCtx, pool, w)
		if err != nil {
			return err
		}
		if overflows["*"] {
			return ErrProjectionOverflow
		}
		for dimension := range overflows {
			data.unavailable[dimension] = true
		}
		return nil
	})
	if err != nil {
		return raw{}, err
	}
	return data, nil
}

func projectionOverflows(ctx context.Context, pool *database.Pool, w Window) (map[string]bool, error) {
	rows, err := pool.Query(ctx, overflowSQL, w.Start, w.End)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	overflows := map[string]bool{}
	for rows.Next() {
		var dimension string
		if err := rows.Scan(&dimension); err != nil {
			return nil, err
		}
		overflows[dimension] = true
	}
	return overflows, rows.Err()
}

func reportArgs(w Window) []any {
	args := make([]any, 0, len(reportDimensions)+2)
	args = append(args, w.Start, w.End)
	for _, dim := range reportDimensions {
		args = append(args, dim)
	}
	return args
}
