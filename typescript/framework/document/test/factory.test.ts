import { afterEach, beforeEach, describe, expect } from 'bun:test';
import { resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { MemoryAdapter } from '../src/adapter/memory.adapter';
import { document } from '../src/document.plugin';
import { closeAllBackends, closeBackend, useBackend } from '../src/factory';

function fullConfig(overrides: Record<string, unknown> = {}) {
  return {
    backend: 'memory',
    projectId: undefined,
    databaseId: '(default)',
    emulatorHost: undefined,
    credentials: undefined,
    strictIndexes: false,
    slowOperationThresholdMs: 0,
    ...overrides,
  };
}

describe('document backend factory', () => {
  beforeEach(() => {
    resetConfigLoader();
    process.env.CONFIG_DATA = JSON.stringify({
      document: {
        backend: 'memory',
        analytics: {
          backend: 'memory',
        },
      },
    });
  });

  afterEach(async () => {
    await closeAllBackends();
    resetConfigLoader();
    delete process.env.CONFIG_DATA;
  });

  specTest(
    'reuses the same backend instance for the same store',
    {
      feature: 'typescript/document-repository',
      requirement: 'store-lifecycle',
      check: 'one-managed-backend-is-reused-per-store',
    },
    async () => {
      const first = await useBackend();
      const second = await useBackend();

      expect(first).toBe(second);
      expect(first).toBeInstanceOf(MemoryAdapter);
    },
  );

  specTest(
    'keeps named stores isolated',
    {
      feature: 'typescript/document-repository',
      requirement: 'store-lifecycle',
      check: 'different-store-names-stay-isolated',
    },
    async () => {
      const defaultStore = await useBackend();
      const analyticsStore = await useBackend('analytics');

      expect(defaultStore).not.toBe(analyticsStore);
    },
  );

  specTest(
    'returns unmanaged one-off backends for explicit config overrides',
    {
      feature: 'typescript/document-repository',
      requirement: 'store-lifecycle',
      check: 'an-explicit-config-override-returns-an-unmanaged-backend',
    },
    async () => {
      const managed = await useBackend();
      const unmanaged = await useBackend(undefined, fullConfig());

      expect(unmanaged).not.toBe(managed);
      expect(unmanaged).toBeInstanceOf(MemoryAdapter);
    },
  );

  specTest(
    'recreates a backend after closeBackend',
    {
      feature: 'typescript/document-repository',
      requirement: 'store-lifecycle',
      check: 'close-backend-permits-clean-recreation',
    },
    async () => {
      const first = await useBackend();
      await closeBackend();
      const second = await useBackend();

      expect(second).not.toBe(first);
    },
  );

  specTest(
    'closes every managed backend when the document plugin stops',
    {
      feature: 'typescript/document-repository',
      requirement: 'store-lifecycle',
      check: 'the-plugin-closes-every-managed-backend-on-stop',
    },
    async () => {
      const first = await useBackend();

      await document().stop({} as never);

      expect(await useBackend()).not.toBe(first);
    },
  );
});
