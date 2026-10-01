import { describe, expect, it } from 'bun:test';
import { TelemetryCollector } from '../../src/telemetry/telemetry.collector';

describe('TelemetryCollector', () => {
  describe('counter', () => {
    it('should increment a counter by 1 by default', () => {
      const collector = new TelemetryCollector();
      const ts = 1_700_000_000_000; // fixed timestamp

      collector.incCounter('requests', 1, ts);
      collector.incCounter('requests', 1, ts);

      const buckets = collector.drainAll();
      expect(buckets).toHaveLength(1);
      expect(buckets[0].counters.requests).toBe(2);
    });

    it('should increment by a custom value', () => {
      const collector = new TelemetryCollector();
      const ts = 1_700_000_000_000;

      collector.incCounter('bytes', 256, ts);
      collector.incCounter('bytes', 512, ts);

      const buckets = collector.drainAll();
      expect(buckets[0].counters.bytes).toBe(768);
    });

    it('should bucket counters by second', () => {
      const collector = new TelemetryCollector();
      const ts1 = 1_700_000_000_000; // second 1700000000
      const ts2 = 1_700_000_001_500; // second 1700000001

      collector.incCounter('req', 1, ts1);
      collector.incCounter('req', 1, ts2);

      const buckets = collector.drainAll();
      expect(buckets).toHaveLength(2);
      expect(buckets[0].ts).toBe(1_700_000_000);
      expect(buckets[0].counters.req).toBe(1);
      expect(buckets[1].ts).toBe(1_700_000_001);
      expect(buckets[1].counters.req).toBe(1);
    });
  });

  describe('gauge', () => {
    it('should set gauge to value (last write wins)', () => {
      const collector = new TelemetryCollector();
      const ts = 1_700_000_000_000;

      collector.setGauge('connections', 5, ts);
      collector.setGauge('connections', 3, ts);

      const buckets = collector.drainAll();
      expect(buckets[0].gauges.connections).toBe(3);
    });
  });

  describe('histogram', () => {
    it('should track count, sum, min, max', () => {
      const collector = new TelemetryCollector();
      const ts = 1_700_000_000_000;

      collector.observeHistogram('duration', 10, ts);
      collector.observeHistogram('duration', 50, ts);
      collector.observeHistogram('duration', 5, ts);

      const buckets = collector.drainAll();
      const h = buckets[0].histograms.duration;
      expect(h.count).toBe(3);
      expect(h.sum).toBe(65);
      expect(h.min).toBe(5);
      expect(h.max).toBe(50);
    });

    it('should handle single observation', () => {
      const collector = new TelemetryCollector();
      const ts = 1_700_000_000_000;

      collector.observeHistogram('latency', 42, ts);

      const buckets = collector.drainAll();
      const h = buckets[0].histograms.latency;
      expect(h.count).toBe(1);
      expect(h.sum).toBe(42);
      expect(h.min).toBe(42);
      expect(h.max).toBe(42);
    });
  });

  describe('drain', () => {
    it('should only drain completed buckets (not the current second)', () => {
      const collector = new TelemetryCollector();
      const now = 1_700_000_002_000;

      // Two buckets in the past
      collector.incCounter('a', 1, 1_700_000_000_000);
      collector.incCounter('b', 1, 1_700_000_001_000);
      // One bucket in the current second
      collector.incCounter('c', 1, now);

      const flushed = collector.drain(now);
      expect(flushed).toHaveLength(2);
      expect(flushed[0].ts).toBe(1_700_000_000);
      expect(flushed[1].ts).toBe(1_700_000_001);

      // Current bucket still present
      const remaining = collector.drainAll();
      expect(remaining).toHaveLength(1);
      expect(remaining[0].counters.c).toBe(1);
    });

    it('should return empty array when no completed buckets', () => {
      const collector = new TelemetryCollector();
      expect(collector.drain()).toEqual([]);
    });

    it('should return buckets sorted by timestamp', () => {
      const collector = new TelemetryCollector();
      // Insert out of order
      collector.incCounter('b', 1, 1_700_000_002_000);
      collector.incCounter('a', 1, 1_700_000_000_000);
      collector.incCounter('c', 1, 1_700_000_001_000);

      const flushed = collector.drainAll();
      expect(flushed[0].ts).toBe(1_700_000_000);
      expect(flushed[1].ts).toBe(1_700_000_001);
      expect(flushed[2].ts).toBe(1_700_000_002);
    });
  });

  describe('drainAll', () => {
    it('should drain everything including the current second', () => {
      const collector = new TelemetryCollector();
      collector.incCounter('a', 1);

      const all = collector.drainAll();
      expect(all).toHaveLength(1);
      expect(collector.isEmpty()).toBe(true);
    });
  });

  describe('isEmpty', () => {
    it('should return true when empty', () => {
      const collector = new TelemetryCollector();
      expect(collector.isEmpty()).toBe(true);
    });

    it('should return false after recording', () => {
      const collector = new TelemetryCollector();
      collector.incCounter('x');
      expect(collector.isEmpty()).toBe(false);
    });
  });

  describe('mixed metrics in same bucket', () => {
    it('should support all three metric types in one bucket', () => {
      const collector = new TelemetryCollector();
      const ts = 1_700_000_000_000;

      collector.incCounter('http.GET./api.200', 1, ts);
      collector.setGauge('http.active_connections', 10, ts);
      collector.observeHistogram('http.GET./api.duration', 42, ts);

      const buckets = collector.drainAll();
      expect(buckets).toHaveLength(1);

      const b = buckets[0];
      expect(b.counters['http.GET./api.200']).toBe(1);
      expect(b.gauges['http.active_connections']).toBe(10);
      expect(b.histograms['http.GET./api.duration'].count).toBe(1);
    });
  });

  it('aggregates attributed client metrics by a deterministic bounded attribute set', () => {
    const collector = new TelemetryCollector();
    const now = 1_700_000_000_000;
    const attributes = {
      'rpc.system': 'putnami',
      'rpc.service': 'catalog.items',
      'rpc.method': 'listWidgets',
      'network.protocol.name': 'rest-json',
    };

    collector.incCounterWithAttributes('rpc.client.calls', 1, attributes, now);
    collector.incCounterWithAttributes('rpc.client.calls', 2, { ...attributes }, now);
    collector.observeHistogramWithAttributes('rpc.client.duration', 12.5, attributes, now);

    const bucket = collector.drainAll()[0];
    expect(bucket.counterSeries).toEqual([{ name: 'rpc.client.calls', attributes, value: 3 }]);
    expect(bucket.histogramSeries).toEqual([
      {
        name: 'rpc.client.duration',
        attributes,
        aggregate: { count: 1, sum: 12.5, min: 12.5, max: 12.5 },
      },
    ]);
  });
});
