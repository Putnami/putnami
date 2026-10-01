import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { TelemetryCollector, setCollector } from '@putnami/application';
import {
  type QueryMetrics,
  recordPoolClosed,
  recordPoolCount,
  recordPoolCreated,
  recordQuery,
  recordQueryError,
  recordSlowQuery,
  recordTransaction,
} from '../src/observability';

describe('SQL Metrics', () => {
  let collector: TelemetryCollector;

  beforeEach(() => {
    collector = new TelemetryCollector();
    setCollector(collector);
  });

  afterEach(() => {
    setCollector(undefined);
  });

  describe('recordQuery', () => {
    it('should emit counter and histogram for a query', () => {
      const metrics: QueryMetrics = {
        operation: 'find',
        table: 'users',
        duration: 42,
        rowCount: 10,
      };

      recordQuery(metrics);

      const buckets = collector.drainAll();
      expect(buckets.length).toBe(1);

      const bucket = buckets[0];
      expect(bucket.counters['sql.find.users']).toBe(1);
      expect(bucket.histograms['sql.find.users.duration']).toBeDefined();
      expect(bucket.histograms['sql.find.users.duration'].count).toBe(1);
      expect(bucket.histograms['sql.find.users.duration'].sum).toBe(42);
      expect(bucket.histograms['sql.query.duration']).toBeDefined();
      expect(bucket.histograms['sql.query.duration'].sum).toBe(42);
    });

    it('should accumulate counters across multiple calls', () => {
      for (let i = 0; i < 5; i++) {
        recordQuery({ operation: 'save', table: 'orders', duration: 10 + i });
      }

      const buckets = collector.drainAll();
      const bucket = buckets[0];
      expect(bucket.counters['sql.save.orders']).toBe(5);
      expect(bucket.histograms['sql.save.orders.duration'].count).toBe(5);
      expect(bucket.histograms['sql.save.orders.duration'].min).toBe(10);
      expect(bucket.histograms['sql.save.orders.duration'].max).toBe(14);
    });

    it('should handle different operations independently', () => {
      recordQuery({ operation: 'find', table: 'users', duration: 5, rowCount: 3 });
      recordQuery({ operation: 'save', table: 'users', duration: 15, rowCount: 1 });
      recordQuery({ operation: 'delete', table: 'users', duration: 8, rowCount: 1 });

      const buckets = collector.drainAll();
      const bucket = buckets[0];
      expect(bucket.counters['sql.find.users']).toBe(1);
      expect(bucket.counters['sql.save.users']).toBe(1);
      expect(bucket.counters['sql.delete.users']).toBe(1);
      expect(bucket.histograms['sql.query.duration'].count).toBe(3);
    });
  });

  describe('recordQueryError', () => {
    it('should emit error counter and duration histogram', () => {
      recordQueryError('save', 'users', 100, new Error('connection refused'));

      const buckets = collector.drainAll();
      const bucket = buckets[0];
      expect(bucket.counters['sql.save.users.error']).toBe(1);
      expect(bucket.counters['sql.query.error']).toBe(1);
      expect(bucket.histograms['sql.query.duration'].sum).toBe(100);
    });

    it('should handle non-Error objects', () => {
      recordQueryError('find', 'orders', 50, 'string error');

      const buckets = collector.drainAll();
      const bucket = buckets[0];
      expect(bucket.counters['sql.find.orders.error']).toBe(1);
      expect(bucket.counters['sql.query.error']).toBe(1);
    });
  });

  describe('recordSlowQuery', () => {
    it('should emit slow query counter when duration exceeds threshold', () => {
      const metrics: QueryMetrics = {
        operation: 'find',
        table: 'users',
        duration: 300,
      };

      recordSlowQuery(metrics, 200);

      const buckets = collector.drainAll();
      const bucket = buckets[0];
      expect(bucket.counters['sql.query.slow']).toBe(1);
    });

    it('should not emit when duration is below threshold', () => {
      const metrics: QueryMetrics = {
        operation: 'find',
        table: 'users',
        duration: 50,
      };

      recordSlowQuery(metrics, 200);

      expect(collector.isEmpty()).toBe(true);
    });

    it('should emit when duration equals threshold', () => {
      const metrics: QueryMetrics = {
        operation: 'save',
        table: 'orders',
        duration: 200,
      };

      recordSlowQuery(metrics, 200);

      const buckets = collector.drainAll();
      const bucket = buckets[0];
      expect(bucket.counters['sql.query.slow']).toBe(1);
    });
  });

  describe('Connection pool metrics', () => {
    it('should emit pool created counter', () => {
      recordPoolCreated('auth');

      const buckets = collector.drainAll();
      const bucket = buckets[0];
      expect(bucket.counters['sql.pool.created']).toBe(1);
    });

    it('should emit pool closed counter', () => {
      recordPoolClosed('auth');

      const buckets = collector.drainAll();
      const bucket = buckets[0];
      expect(bucket.counters['sql.pool.closed']).toBe(1);
    });

    it('should set pool count gauge', () => {
      recordPoolCount(3);

      const buckets = collector.drainAll();
      const bucket = buckets[0];
      expect(bucket.gauges['sql.pool.count']).toBe(3);
    });

    it('should update pool count gauge to 0', () => {
      recordPoolCount(3);
      recordPoolCount(0);

      const buckets = collector.drainAll();
      const bucket = buckets[0];
      expect(bucket.gauges['sql.pool.count']).toBe(0);
    });
  });

  describe('recordTransaction', () => {
    it('emits duration, retries and outcome metrics on commit', () => {
      recordTransaction({ datasource: 'orders', outcome: 'committed', duration: 12 });

      const bucket = collector.drainAll()[0];
      expect(bucket.histograms['sql.tx.duration'].sum).toBe(12);
      expect(bucket.histograms['sql.tx.retries'].sum).toBe(0);
      expect(bucket.counters['sql.tx.outcome.committed']).toBe(1);
      // A commit emits no rollback metric.
      expect(Object.keys(bucket.counters).some((k) => k.startsWith('sql.tx.rollback.'))).toBe(false);
    });

    it('emits the rollback cause counter on rollback', () => {
      recordTransaction({ datasource: 'orders', outcome: 'rolled-back', duration: 5, cause: '40001' });

      const bucket = collector.drainAll()[0];
      expect(bucket.counters['sql.tx.outcome.rolled-back']).toBe(1);
      expect(bucket.counters['sql.tx.rollback.40001']).toBe(1);
      expect(bucket.histograms['sql.tx.duration'].sum).toBe(5);
    });

    it('defaults a missing rollback cause to "unknown"', () => {
      recordTransaction({ outcome: 'rolled-back', duration: 1 });

      const bucket = collector.drainAll()[0];
      expect(bucket.counters['sql.tx.rollback.unknown']).toBe(1);
    });
  });

  describe('No-op when collector is not set', () => {
    it('should not throw when telemetry is disabled', () => {
      setCollector(undefined);

      expect(() => {
        recordQuery({ operation: 'find', table: 'users', duration: 10 });
        recordQueryError('save', 'users', 10, new Error('fail'));
        recordSlowQuery({ operation: 'find', table: 'users', duration: 500 }, 200);
        recordPoolCreated('test');
        recordPoolClosed('test');
        recordPoolCount(0);
      }).not.toThrow();
    });
  });
});
