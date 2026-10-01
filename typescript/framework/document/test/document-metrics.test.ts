import { afterEach, beforeEach, describe, expect } from 'bun:test';
import { setCollector, TelemetryCollector } from '@putnami/application';
import { resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { Collection, DocumentId, Field } from '../src/collection';
import { closeAllBackends, useBackend } from '../src/factory';
import {
  recordBackendClosed,
  recordBackendCount,
  recordBackendCreated,
  recordDocumentError,
  recordDocumentOp,
  recordSlowDocumentOp,
  recordTransactionCommitted,
  recordTransactionRolledBack,
  recordTransactionStarted,
} from '../src/observability';
import { Repository } from '../src/repository/repository';

function configureMemory(overrides: Record<string, unknown> = {}) {
  resetConfigLoader();
  process.env.CONFIG_DATA = JSON.stringify({
    document: {
      backend: 'memory',
      slowOperationThresholdMs: 0,
      analytics: {
        backend: 'memory',
      },
      ...overrides,
    },
  });
}

describe('document observability', () => {
  let collector: TelemetryCollector;

  beforeEach(() => {
    collector = new TelemetryCollector();
    setCollector(collector);
    configureMemory();
  });

  afterEach(async () => {
    await closeAllBackends();
    setCollector(undefined);
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
  });

  specTest(
    'records success metrics for document operations',
    {
      feature: 'typescript/document-repository',
      requirement: 'operation-observability',
      check: 'a-successful-operation-records-its-metrics',
    },
    () => {
      recordDocumentOp({
        operation: 'find',
        collection: 'users',
        duration: 42,
        rowCount: 3,
      });

      const bucket = collector.drainAll()[0];
      expect(bucket.counters['document.find.users']).toBe(1);
      expect(bucket.histograms['document.find.users.duration']).toBeDefined();
      expect(bucket.histograms['document.find.users.duration'].sum).toBe(42);
      expect(bucket.histograms['document.operation.duration'].sum).toBe(42);
    },
  );

  specTest(
    'records collection-scoped duration for failed operations',
    {
      feature: 'typescript/document-repository',
      requirement: 'operation-observability',
      check: 'a-failed-operation-records-collection-scoped-duration',
    },
    () => {
      recordDocumentError('save', 'users', 17, new Error('boom'));

      const bucket = collector.drainAll()[0];
      expect(bucket.counters['document.save.users.error']).toBe(1);
      expect(bucket.counters['document.operation.error']).toBe(1);
      expect(bucket.histograms['document.save.users.duration']).toBeDefined();
      expect(bucket.histograms['document.save.users.duration'].sum).toBe(17);
      expect(bucket.histograms['document.operation.duration'].sum).toBe(17);
    },
  );

  specTest(
    'records slow operations when the threshold is exceeded',
    {
      feature: 'typescript/document-repository',
      requirement: 'operation-observability',
      check: 'a-slow-operation-is-recorded-past-the-threshold',
    },
    () => {
      recordSlowDocumentOp(
        {
          operation: 'find',
          collection: 'users',
          duration: 250,
        },
        200,
      );

      const bucket = collector.drainAll()[0];
      expect(bucket.counters['document.operation.slow']).toBe(1);
    },
  );

  specTest(
    'records backend lifecycle metrics when closing all backends',
    {
      feature: 'typescript/document-repository',
      requirement: 'operation-observability',
      check: 'backend-lifecycle-metrics-are-recorded-on-close',
    },
    async () => {
      await useBackend();
      await useBackend('analytics');

      await closeAllBackends();

      const bucket = collector.drainAll()[0];
      expect(bucket.counters['document.backend.created']).toBe(2);
      expect(bucket.counters['document.backend.closed']).toBe(2);
      expect(bucket.gauges['document.backend.count']).toBe(0);
    },
  );

  specTest(
    'records committed transaction metrics and active gauge transitions',
    {
      feature: 'typescript/document-repository',
      requirement: 'operation-observability',
      check: 'a-committed-transaction-records-its-metrics-and-gauge',
    },
    () => {
      recordTransactionStarted('default');

      let bucket = collector.drainAll()[0];
      expect(bucket.counters['document.transaction.started']).toBe(1);
      expect(bucket.gauges['document.transaction.active']).toBe(1);

      recordTransactionCommitted('default', 12);

      bucket = collector.drainAll()[0];
      expect(bucket.counters['document.transaction.committed']).toBe(1);
      expect(bucket.histograms['document.transaction.duration']).toBeDefined();
      expect(bucket.histograms['document.transaction.duration'].sum).toBe(12);
      expect(bucket.gauges['document.transaction.active']).toBe(0);
    },
  );

  specTest(
    'records rolled back transaction metrics',
    {
      feature: 'typescript/document-repository',
      requirement: 'operation-observability',
      check: 'a-rolled-back-transaction-records-its-metrics',
    },
    () => {
      recordTransactionStarted('default');
      collector.drainAll();

      recordTransactionRolledBack('default', 7, new Error('boom'));

      const bucket = collector.drainAll()[0];
      expect(bucket.counters['document.transaction.rolled_back']).toBe(1);
      expect(bucket.histograms['document.transaction.duration']).toBeDefined();
      expect(bucket.histograms['document.transaction.duration'].sum).toBe(7);
      expect(bucket.gauges['document.transaction.active']).toBe(0);
    },
  );

  specTest(
    'records slow failures from repository operations',
    {
      feature: 'typescript/document-repository',
      requirement: 'operation-observability',
      check: 'a-slow-failure-is-recorded-from-a-repository-operation',
    },
    async () => {
      configureMemory({ slowOperationThresholdMs: 10 });

      const repo = new Repository(
        Collection('users_observability', {
          id: DocumentId(String),
          email: Field(String),
        }),
      );

      const originalDateNow = Date.now;
      let calls = 0;
      Date.now = () => {
        calls++;
        return calls === 1 ? 0 : 25;
      };

      try {
        await expect(repo.save({ id: 'user-1' })).rejects.toThrow('Validation failed');
      } finally {
        Date.now = originalDateNow;
      }

      const bucket = collector.drainAll()[0];
      expect(bucket.counters['document.operation.slow']).toBe(1);
      expect(bucket.counters['document.operation.error']).toBe(1);
      expect(bucket.counters['document.save.users_observability.error']).toBe(1);
      expect(bucket.histograms['document.save.users_observability.duration'].sum).toBe(25);
    },
  );

  specTest(
    'is safe when telemetry is disabled',
    {
      feature: 'typescript/document-repository',
      requirement: 'operation-observability',
      check: 'recording-is-safe-when-telemetry-is-disabled',
    },
    () => {
      setCollector(undefined);

      expect(() => {
        recordDocumentOp({ operation: 'get', collection: 'users', duration: 5 });
        recordDocumentError('save', 'users', 5, new Error('fail'));
        recordSlowDocumentOp({ operation: 'find', collection: 'users', duration: 500 }, 200);
        recordBackendCreated('default');
        recordBackendClosed('default');
        recordBackendCount(0);
        recordTransactionStarted('default');
        recordTransactionCommitted('default', 5);
      }).not.toThrow();
    },
  );
});
