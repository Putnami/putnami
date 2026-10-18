import { describe, expect, it } from 'bun:test';
import { TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import { drainMetrics } from '../../src/testing/drain-metrics';

describe('drainMetrics', () => {
  // These two instants are one millisecond apart and fall in different Unix
  // seconds: the split a loaded runner produces by chance.
  const lastMsOfSecond = 1_700_000_000_999;
  const firstMsOfNextSecond = 1_700_000_001_000;

  it('reproduces the defect: drainAll()[0] misses metrics recorded after a second boundary', () => {
    const collector = new TelemetryCollector();
    collector.incCounter('document.transaction.started', 1, lastMsOfSecond);
    collector.incCounter('document.transaction.committed', 1, firstMsOfNextSecond);

    const buckets = collector.drainAll();
    expect(buckets).toHaveLength(2);
    expect(buckets[0].counters['document.transaction.committed']).toBeUndefined();
  });

  it('sees both counters across the boundary', () => {
    const collector = new TelemetryCollector();
    collector.incCounter('document.transaction.started', 1, lastMsOfSecond);
    collector.incCounter('document.transaction.committed', 1, firstMsOfNextSecond);

    const merged = drainMetrics(collector);
    expect(merged.counters['document.transaction.started']).toBe(1);
    expect(merged.counters['document.transaction.committed']).toBe(1);
  });

  it('sums counters, keeps the last gauge in time order, and combines histograms', () => {
    const collector = new TelemetryCollector();
    // Recorded out of time order on purpose: the later second must win the gauge.
    collector.setGauge('active', 0, firstMsOfNextSecond);
    collector.setGauge('active', 1, lastMsOfSecond);
    collector.incCounter('calls', 2, lastMsOfSecond);
    collector.incCounter('calls', 3, firstMsOfNextSecond);
    collector.observeHistogram('duration', 4, lastMsOfSecond);
    collector.observeHistogram('duration', 10, firstMsOfNextSecond);
    collector.observeHistogram('duration', 1, firstMsOfNextSecond);

    const merged = drainMetrics(collector);
    expect(merged.counters['calls']).toBe(5);
    expect(merged.gauges['active']).toBe(0);
    expect(merged.histograms['duration']).toEqual({ count: 3, sum: 15, min: 1, max: 10 });
  });

  it('merges attributed series by name and attributes', () => {
    const collector = new TelemetryCollector();
    const attributes = { 'rpc.service': 'catalog' };
    collector.incCounterWithAttributes('calls', 1, attributes, lastMsOfSecond);
    collector.incCounterWithAttributes('calls', 2, attributes, firstMsOfNextSecond);
    collector.observeHistogramWithAttributes('duration', 5, attributes, lastMsOfSecond);
    collector.observeHistogramWithAttributes('duration', 7, attributes, firstMsOfNextSecond);

    const merged = drainMetrics(collector);
    expect(merged.counterSeries).toEqual([{ name: 'calls', attributes, value: 3 }]);
    expect(merged.histogramSeries).toEqual([
      { name: 'duration', attributes, aggregate: { count: 2, sum: 12, min: 5, max: 7 } },
    ]);
  });

  it('keeps attributed series in the order the collector keeps them in one bucket', () => {
    const collector = new TelemetryCollector();
    collector.incCounterWithAttributes('a', 1, { k: 'v' }, lastMsOfSecond);
    collector.incCounterWithAttributes('c', 1, { k: 'v' }, lastMsOfSecond);
    collector.incCounterWithAttributes('b', 1, { k: 'v' }, firstMsOfNextSecond);

    const merged = drainMetrics(collector);
    expect(merged.counterSeries?.map((entry) => entry.name)).toEqual(['a', 'b', 'c']);
  });

  it('returns an empty bucket when nothing was recorded', () => {
    const merged = drainMetrics(new TelemetryCollector());
    expect(merged.counters).toEqual({});
    expect(merged.gauges).toEqual({});
    expect(merged.histograms).toEqual({});
  });
});
