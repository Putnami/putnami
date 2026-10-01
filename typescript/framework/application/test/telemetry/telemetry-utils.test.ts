import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import {
  clearCollector,
  getCollector,
  incCounter,
  observeHistogram,
  setCollector,
  setGauge,
} from '../../src/telemetry/telemetry.utils';

describe('telemetry utils', () => {
  let collector: TelemetryCollector;

  beforeEach(() => {
    collector = new TelemetryCollector();
    setCollector(collector);
  });

  afterEach(() => {
    setCollector(undefined);
  });

  it('incCounter should delegate to the collector', () => {
    incCounter('my.counter');
    incCounter('my.counter', 5);

    const buckets = collector.drainAll();
    expect(buckets).toHaveLength(1);
    expect(buckets[0].counters['my.counter']).toBe(6);
  });

  it('setGauge should delegate to the collector', () => {
    setGauge('my.gauge', 42);

    const buckets = collector.drainAll();
    expect(buckets[0].gauges['my.gauge']).toBe(42);
  });

  it('observeHistogram should delegate to the collector', () => {
    observeHistogram('my.hist', 10);
    observeHistogram('my.hist', 20);

    const buckets = collector.drainAll();
    const h = buckets[0].histograms['my.hist'];
    expect(h.count).toBe(2);
    expect(h.sum).toBe(30);
    expect(h.min).toBe(10);
    expect(h.max).toBe(20);
  });

  it('should be a no-op when no collector is set', () => {
    setCollector(undefined);

    // These should not throw
    incCounter('x');
    setGauge('x', 1);
    observeHistogram('x', 1);

    expect(getCollector()).toBeUndefined();
  });
});

describe('clearCollector (multi-app safety)', () => {
  afterEach(() => {
    setCollector(undefined);
  });

  it('clears the global only when it still points at the given collector', () => {
    const a = new TelemetryCollector();
    setCollector(a);
    clearCollector(a);
    expect(getCollector()).toBeUndefined();
  });

  it('does not clear a collector installed by another app', () => {
    const a = new TelemetryCollector();
    const b = new TelemetryCollector();

    // App A starts, then App B starts and overwrites the global.
    setCollector(a);
    setCollector(b);
    expect(getCollector()).toBe(b);

    // App A shutting down must not null out App B's collector.
    clearCollector(a);
    expect(getCollector()).toBe(b);

    // App B shutting down clears it.
    clearCollector(b);
    expect(getCollector()).toBeUndefined();
  });
});
