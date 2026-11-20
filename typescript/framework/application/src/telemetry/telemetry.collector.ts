/**
 * Telemetry Collector — In-Memory Per-Second Aggregation
 *
 * Metrics are bucketed by Unix-second. Each bucket holds counters,
 * gauges, and histogram aggregates, each histogram counting its observations
 * in the buckets of HISTOGRAM_BOUNDS. Completed buckets (older than the
 * current second) are drained during flush.
 */

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/**
 * Explicit bucket bounds of every histogram, in the metric's unit. They are the
 * bounds the telemetry protocol sets for http.server.duration, which the Go
 * runtime records too, so a backend computes the same percentiles from both.
 */
export const HISTOGRAM_BOUNDS: readonly number[] = Object.freeze([
  0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10_000,
]);

/** Aggregated histogram statistics for a single metric within a time bucket. */
export interface HistogramAggregate {
  count: number;
  sum: number;
  min: number;
  max: number;
  /**
   * Observations per bucket of {@link HISTOGRAM_BOUNDS}: index i counts the
   * values in (bounds[i-1], bounds[i]], and the last index the values above
   * the highest bound.
   */
  bucketCounts: number[];
}

/** Unit and description a metric renders with, the same in every bucket. */
export interface MetricDescription {
  unit?: string;
  description?: string;
}

/** Low-cardinality attributes attached to framework-generated client telemetry. */
export type TelemetryAttributes = Readonly<Record<string, string | number | boolean>>;

export interface AttributedCounterAggregate {
  name: string;
  attributes: TelemetryAttributes;
  value: number;
}

export interface AttributedHistogramAggregate {
  name: string;
  attributes: TelemetryAttributes;
  aggregate: HistogramAggregate;
}

/** Completed span retained until the next OTLP trace flush. */
export interface TelemetrySpanRecord {
  traceId: string;
  spanId: string;
  parentSpanId?: string;
  name: string;
  /** OTLP span kind: internal=1, server=2, client=3, consumer=5. */
  kind: number;
  startTimeUnixNano: string;
  endTimeUnixNano: string;
  attributes: TelemetryAttributes;
  /** OTLP status code: ok=1, error=2. */
  statusCode: number;
  statusMessage?: string;
}

/** A one-second time bucket holding counters, gauges, and histogram aggregates. */
export interface MetricsBucket {
  /** Unix second this bucket represents. */
  ts: number;
  counters: Record<string, number>;
  gauges: Record<string, number>;
  histograms: Record<string, HistogramAggregate>;
  /** Attribute-bearing counters used by generated first-party clients. */
  counterSeries?: AttributedCounterAggregate[];
  /** Attribute-bearing histograms used by generated first-party clients. */
  histogramSeries?: AttributedHistogramAggregate[];
}

// ---------------------------------------------------------------------------
// Collector
// ---------------------------------------------------------------------------

/** Options of a collector. */
export interface TelemetryCollectorOptions {
  /** Spans that make a full batch. */
  spanBatchSize?: number;
  /** Called when the queued spans reach `spanBatchSize`, so the batch can leave at once. */
  onSpanBatchFull?: () => void;
}

/** In-memory metrics collector that aggregates counters, gauges, and histograms into per-second buckets. */
export class TelemetryCollector {
  constructor(private readonly options: TelemetryCollectorOptions = {}) {}

  private buckets = new Map<number, MetricsBucket>();
  /** Attributed series of each bucket by series key, so a record finds its series in O(1). */
  private seriesIndex = new Map<number, Map<string, AttributedCounterAggregate | AttributedHistogramAggregate>>();
  private spans: TelemetrySpanRecord[] = [];
  private descriptions = new Map<string, MetricDescription>();

  /** Return the unix-second for a given timestamp (default: now). */
  private tick(now?: number): number {
    return Math.floor((now ?? Date.now()) / 1000);
  }

  /** The series index of a bucket's second. */
  private indexOf(ts: number): Map<string, AttributedCounterAggregate | AttributedHistogramAggregate> {
    let index = this.seriesIndex.get(ts);
    if (!index) {
      index = new Map();
      this.seriesIndex.set(ts, index);
    }
    return index;
  }

  /** Close a drained bucket: drop its series index and order its series by name, then attributes. */
  private close(bucket: MetricsBucket): MetricsBucket {
    this.seriesIndex.delete(bucket.ts);
    bucket.counterSeries?.sort(compareSeries);
    bucket.histogramSeries?.sort(compareSeries);
    return bucket;
  }

  /** Get or create the bucket for a given unix-second. */
  private bucket(ts: number): MetricsBucket {
    let b = this.buckets.get(ts);
    if (!b) {
      b = { ts, counters: {}, gauges: {}, histograms: {} };
      this.buckets.set(ts, b);
    }
    return b;
  }

  // -----------------------------------------------------------------------
  // Counter — monotonically increasing value
  // -----------------------------------------------------------------------

  /** Increment a counter metric by the given value (default 1). */
  incCounter(name: string, value = 1, now?: number): void {
    const b = this.bucket(this.tick(now));
    b.counters[name] = (b.counters[name] ?? 0) + value;
  }

  /** Increment a metric series selected by a deterministic, low-cardinality attribute set. */
  incCounterWithAttributes(name: string, value: number, attributes: TelemetryAttributes, now?: number): void {
    const bucket = this.bucket(this.tick(now));
    const normalized = normalizeAttributes(attributes);
    const index = this.indexOf(bucket.ts);
    const key = `c\0${seriesKey(name, normalized)}`;
    const current = index.get(key) as AttributedCounterAggregate | undefined;
    if (current) {
      current.value += value;
    } else {
      const entry = { name, attributes: normalized, value };
      index.set(key, entry);
      bucket.counterSeries ??= [];
      bucket.counterSeries.push(entry);
    }
  }

  // -----------------------------------------------------------------------
  // Gauge — point-in-time value (last write wins within the second)
  // -----------------------------------------------------------------------

  /** Set a gauge metric to a point-in-time value (last write wins within the second). */
  setGauge(name: string, value: number, now?: number): void {
    const b = this.bucket(this.tick(now));
    b.gauges[name] = value;
  }

  // -----------------------------------------------------------------------
  // Histogram — distribution tracking (count, sum, min, max, bucket counts)
  // -----------------------------------------------------------------------

  /** Record a histogram observation, updating count, sum, min, max and its bucket count. */
  observeHistogram(name: string, value: number, now?: number): void {
    const b = this.bucket(this.tick(now));
    const h = b.histograms[name];
    if (h) observe(h, value);
    else b.histograms[name] = firstObservation(value);
  }

  /** Record a histogram observation selected by deterministic attributes. */
  observeHistogramWithAttributes(name: string, value: number, attributes: TelemetryAttributes, now?: number): void {
    const bucket = this.bucket(this.tick(now));
    const normalized = normalizeAttributes(attributes);
    const index = this.indexOf(bucket.ts);
    const key = `h\0${seriesKey(name, normalized)}`;
    const current = index.get(key) as AttributedHistogramAggregate | undefined;
    if (current) {
      observe(current.aggregate, value);
    } else {
      const entry = { name, attributes: normalized, aggregate: firstObservation(value) };
      index.set(key, entry);
      bucket.histogramSeries ??= [];
      bucket.histogramSeries.push(entry);
    }
  }

  /** Give a metric the unit and description it renders with; the last call wins. */
  describeMetric(name: string, description: MetricDescription): void {
    this.descriptions.set(name, Object.freeze({ ...description }));
  }

  /** The unit and description of every described metric, by name. */
  metricDescriptions(): Readonly<Record<string, MetricDescription>> {
    return Object.fromEntries(this.descriptions);
  }

  /** Queue an immutable completed span for the next trace flush. */
  addSpan(span: TelemetrySpanRecord): void {
    this.spans.push(Object.freeze({ ...span, attributes: normalizeAttributes(span.attributes) }));
    const { spanBatchSize, onSpanBatchFull } = this.options;
    if (onSpanBatchFull && spanBatchSize !== undefined && this.spans.length >= spanBatchSize) onSpanBatchFull();
  }

  /** Drain every completed span. Spans are complete records and need no time-bucket delay. */
  drainSpans(): TelemetrySpanRecord[] {
    const spans = this.spans;
    this.spans = [];
    return spans;
  }

  // -----------------------------------------------------------------------
  // Drain — extract completed buckets (everything older than `now`)
  // -----------------------------------------------------------------------

  /**
   * Remove and return all buckets whose timestamp is strictly before the
   * current second. The in-progress (current) bucket is kept.
   */
  drain(now?: number): MetricsBucket[] {
    const currentTick = this.tick(now);
    const flushed: MetricsBucket[] = [];

    for (const [ts, bucket] of this.buckets) {
      if (ts < currentTick) {
        flushed.push(this.close(bucket));
        this.buckets.delete(ts);
      }
    }

    return flushed.sort((a, b) => a.ts - b.ts);
  }

  /**
   * Drain ALL buckets including the current one. Used during shutdown.
   */
  drainAll(): MetricsBucket[] {
    const all = [...this.buckets.values()].sort((a, b) => a.ts - b.ts).map((bucket) => this.close(bucket));
    this.buckets.clear();
    this.seriesIndex.clear();
    return all;
  }

  /** True when no metrics have been recorded. */
  isEmpty(): boolean {
    return this.buckets.size === 0 && this.spans.length === 0;
  }
}

/** The index of the {@link HISTOGRAM_BOUNDS} bucket that counts a value. */
export function histogramBucketIndex(value: number): number {
  const index = HISTOGRAM_BOUNDS.findIndex((bound) => value <= bound);
  return index === -1 ? HISTOGRAM_BOUNDS.length : index;
}

function firstObservation(value: number): HistogramAggregate {
  const bucketCounts = new Array<number>(HISTOGRAM_BOUNDS.length + 1).fill(0);
  bucketCounts[histogramBucketIndex(value)] = 1;
  return { count: 1, sum: value, min: value, max: value, bucketCounts };
}

function observe(aggregate: HistogramAggregate, value: number): void {
  aggregate.count += 1;
  aggregate.sum += value;
  if (value < aggregate.min) aggregate.min = value;
  if (value > aggregate.max) aggregate.max = value;
  aggregate.bucketCounts[histogramBucketIndex(value)] += 1;
}

function normalizeAttributes(attributes: TelemetryAttributes): TelemetryAttributes {
  return Object.freeze(
    Object.fromEntries(Object.entries(attributes).sort(([left], [right]) => left.localeCompare(right))),
  );
}

function seriesKey(name: string, attributes: TelemetryAttributes): string {
  return `${name}\0${JSON.stringify(attributes)}`;
}

/** Orders attributed series by name, then attributes: the order each bucket keeps. */
export function compareSeries(
  left: AttributedCounterAggregate | AttributedHistogramAggregate,
  right: AttributedCounterAggregate | AttributedHistogramAggregate,
): number {
  return seriesKey(left.name, left.attributes).localeCompare(seriesKey(right.name, right.attributes));
}
