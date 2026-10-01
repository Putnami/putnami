/**
 * Telemetry Collector — In-Memory Per-Second Aggregation
 *
 * Metrics are bucketed by Unix-second. Each bucket holds counters,
 * gauges, and histogram aggregates. Completed buckets (older than the
 * current second) are drained during flush.
 */

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/** Aggregated histogram statistics for a single metric within a time bucket. */
export interface HistogramAggregate {
  count: number;
  sum: number;
  min: number;
  max: number;
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
  /** OTLP span kind: server=2, client=3. */
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

/** In-memory metrics collector that aggregates counters, gauges, and histograms into per-second buckets. */
export class TelemetryCollector {
  private buckets = new Map<number, MetricsBucket>();
  private spans: TelemetrySpanRecord[] = [];

  /** Return the unix-second for a given timestamp (default: now). */
  private tick(now?: number): number {
    return Math.floor((now ?? Date.now()) / 1000);
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
    bucket.counterSeries ??= [];
    const key = seriesKey(name, normalized);
    const current = bucket.counterSeries.find((entry) => seriesKey(entry.name, entry.attributes) === key);
    if (current) current.value += value;
    else bucket.counterSeries.push({ name, attributes: normalized, value });
    bucket.counterSeries.sort(compareSeries);
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
  // Histogram — distribution tracking (count, sum, min, max)
  // -----------------------------------------------------------------------

  /** Record a histogram observation, updating count, sum, min, and max. */
  observeHistogram(name: string, value: number, now?: number): void {
    const b = this.bucket(this.tick(now));
    const h = b.histograms[name];
    if (h) {
      h.count += 1;
      h.sum += value;
      if (value < h.min) h.min = value;
      if (value > h.max) h.max = value;
    } else {
      b.histograms[name] = { count: 1, sum: value, min: value, max: value };
    }
  }

  /** Record a histogram observation selected by deterministic attributes. */
  observeHistogramWithAttributes(name: string, value: number, attributes: TelemetryAttributes, now?: number): void {
    const bucket = this.bucket(this.tick(now));
    const normalized = normalizeAttributes(attributes);
    bucket.histogramSeries ??= [];
    const key = seriesKey(name, normalized);
    const current = bucket.histogramSeries.find((entry) => seriesKey(entry.name, entry.attributes) === key);
    if (current) {
      const aggregate = current.aggregate;
      aggregate.count += 1;
      aggregate.sum += value;
      aggregate.min = Math.min(aggregate.min, value);
      aggregate.max = Math.max(aggregate.max, value);
    } else {
      bucket.histogramSeries.push({
        name,
        attributes: normalized,
        aggregate: { count: 1, sum: value, min: value, max: value },
      });
    }
    bucket.histogramSeries.sort(compareSeries);
  }

  /** Queue an immutable completed span for the next trace flush. */
  addSpan(span: TelemetrySpanRecord): void {
    this.spans.push(Object.freeze({ ...span, attributes: normalizeAttributes(span.attributes) }));
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
        flushed.push(bucket);
        this.buckets.delete(ts);
      }
    }

    return flushed.sort((a, b) => a.ts - b.ts);
  }

  /**
   * Drain ALL buckets including the current one. Used during shutdown.
   */
  drainAll(): MetricsBucket[] {
    const all = [...this.buckets.values()].sort((a, b) => a.ts - b.ts);
    this.buckets.clear();
    return all;
  }

  /** True when no metrics have been recorded. */
  isEmpty(): boolean {
    return this.buckets.size === 0 && this.spans.length === 0;
  }
}

function normalizeAttributes(attributes: TelemetryAttributes): TelemetryAttributes {
  return Object.freeze(
    Object.fromEntries(Object.entries(attributes).sort(([left], [right]) => left.localeCompare(right))),
  );
}

function seriesKey(name: string, attributes: TelemetryAttributes): string {
  return `${name}\0${JSON.stringify(attributes)}`;
}

function compareSeries(
  left: AttributedCounterAggregate | AttributedHistogramAggregate,
  right: AttributedCounterAggregate | AttributedHistogramAggregate,
): number {
  return seriesKey(left.name, left.attributes).localeCompare(seriesKey(right.name, right.attributes));
}
