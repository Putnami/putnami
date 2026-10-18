import type { HistogramAggregate, MetricsBucket, TelemetryCollector } from '@putnami/application';

/**
 * Merge drained buckets into one view so an assertion does not depend on where
 * a Unix-second boundary falls. Counters sum, gauges keep the last value in
 * time order, histograms combine count, sum, min and max, and attributed series
 * merge by name and attributes. `ts` is the earliest bucket's second.
 *
 * Test-only. `database/test/utils/drain-merged.ts` and
 * `client/test/runtime/drain-merged.ts` carry an identical copy.
 */
export function mergeBuckets(buckets: readonly MetricsBucket[]): MetricsBucket {
  const ordered = [...buckets].sort((a, b) => a.ts - b.ts);
  const merged: MetricsBucket = { ts: ordered[0]?.ts ?? 0, counters: {}, gauges: {}, histograms: {} };

  for (const bucket of ordered) {
    for (const [name, value] of Object.entries(bucket.counters)) {
      merged.counters[name] = (merged.counters[name] ?? 0) + value;
    }
    Object.assign(merged.gauges, bucket.gauges);
    for (const [name, aggregate] of Object.entries(bucket.histograms)) {
      merged.histograms[name] = combine(merged.histograms[name], aggregate);
    }
    for (const entry of bucket.counterSeries ?? []) {
      merged.counterSeries ??= [];
      const current = merged.counterSeries.find((e) => sameSeries(e, entry));
      if (current) current.value += entry.value;
      else merged.counterSeries.push({ ...entry });
    }
    for (const entry of bucket.histogramSeries ?? []) {
      merged.histogramSeries ??= [];
      const current = merged.histogramSeries.find((e) => sameSeries(e, entry));
      if (current) current.aggregate = combine(current.aggregate, entry.aggregate);
      else merged.histogramSeries.push({ ...entry, aggregate: { ...entry.aggregate } });
    }
  }
  return merged;
}

/** Drain every bucket of the collector, including the current second, and merge them. */
export function drainMerged(collector: TelemetryCollector): MetricsBucket {
  return mergeBuckets(collector.drainAll());
}

function combine(left: HistogramAggregate | undefined, right: HistogramAggregate): HistogramAggregate {
  if (!left) return { ...right };
  return {
    count: left.count + right.count,
    sum: left.sum + right.sum,
    min: Math.min(left.min, right.min),
    max: Math.max(left.max, right.max),
  };
}

function sameSeries(
  left: { name: string; attributes: Readonly<Record<string, unknown>> },
  right: { name: string; attributes: Readonly<Record<string, unknown>> },
): boolean {
  return left.name === right.name && JSON.stringify(left.attributes) === JSON.stringify(right.attributes);
}
