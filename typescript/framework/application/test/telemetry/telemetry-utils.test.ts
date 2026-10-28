import { afterEach, beforeEach, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import {
  clearCollector,
  getCollector,
  incCounter,
  observeHistogram,
  setCollector,
  setGauge,
} from '../../src/telemetry/telemetry.utils';
import { drainMetrics } from '../../src/testing/drain-metrics';

describe('telemetry utils', () => {
  let collector: TelemetryCollector;

  beforeEach(() => {
    collector = new TelemetryCollector();
    setCollector(collector);
  });

  afterEach(() => {
    setCollector(undefined);
  });

  specTest(
    'incCounter should delegate to the collector',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'collector-ownership',
      check: 'custom-metrics-record-into-the-active-collector',
    },
    () => {
      incCounter('my.counter');
      incCounter('my.counter', 5);

      const bucket = drainMetrics(collector);
      expect(bucket.counters['my.counter']).toBe(6);
    },
  );

  specTest(
    'setGauge should delegate to the collector',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'collector-ownership',
      check: 'custom-metrics-record-into-the-active-collector',
    },
    () => {
      setGauge('my.gauge', 42);

      const bucket = drainMetrics(collector);
      expect(bucket.gauges['my.gauge']).toBe(42);
    },
  );

  specTest(
    'observeHistogram should delegate to the collector',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'collector-ownership',
      check: 'custom-metrics-record-into-the-active-collector',
    },
    () => {
      observeHistogram('my.hist', 10);
      observeHistogram('my.hist', 20);

      const bucket = drainMetrics(collector);
      const h = bucket.histograms['my.hist'];
      expect(h.count).toBe(2);
      expect(h.sum).toBe(30);
      expect(h.min).toBe(10);
      expect(h.max).toBe(20);
    },
  );

  specTest(
    'should be a no-op when no collector is set',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'collector-ownership',
      check: 'custom-metrics-are-no-ops-without-a-collector',
    },
    () => {
      setCollector(undefined);

      // These should not throw
      incCounter('x');
      setGauge('x', 1);
      observeHistogram('x', 1);

      expect(getCollector()).toBeUndefined();
    },
  );
});

describe('clearCollector (multi-app safety)', () => {
  afterEach(() => {
    setCollector(undefined);
  });

  specTest(
    'clears the global only when it still points at the given collector',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'collector-ownership',
      check: 'stop-clears-only-its-own-collector',
    },
    () => {
      const a = new TelemetryCollector();
      setCollector(a);
      clearCollector(a);
      expect(getCollector()).toBeUndefined();
    },
  );

  specTest(
    'does not clear a collector installed by another app',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'collector-ownership',
      check: 'stop-clears-only-its-own-collector',
    },
    () => {
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
    },
  );
});
