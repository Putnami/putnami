import { afterEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { clientFetchTimeoutMs, DEFAULT_CLIENT_FETCH_TIMEOUT_MS } from '../../src/client/form/fetch-timeout';

type BrowserGlobals = typeof globalThis & {
  window?: { __reactClientFetchTimeoutMs?: number };
};

const globals = globalThis as BrowserGlobals;

afterEach(() => {
  globals.window = undefined;
});

describe('clientFetchTimeoutMs', () => {
  it('defaults to 30s when no window global is present', () => {
    globals.window = undefined;
    expect(DEFAULT_CLIENT_FETCH_TIMEOUT_MS).toBe(30_000);
    expect(clientFetchTimeoutMs()).toBe(30_000);
  });

  specTest(
    'reads the SSR-injected window global when set',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'hydration-containment',
      check: 'the-serialized-fetch-timeout-is-read-from-the-injected-global',
    },
    () => {
      globals.window = { __reactClientFetchTimeoutMs: 5000 };
      expect(clientFetchTimeoutMs()).toBe(5000);
    },
  );

  specTest(
    'ignores non-positive or non-numeric injected values',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'hydration-containment',
      check: 'a-non-numeric-injected-timeout-is-ignored',
    },
    () => {
      globals.window = { __reactClientFetchTimeoutMs: 0 };
      expect(clientFetchTimeoutMs()).toBe(30_000);

      globals.window = { __reactClientFetchTimeoutMs: -10 };
      expect(clientFetchTimeoutMs()).toBe(30_000);

      globals.window = { __reactClientFetchTimeoutMs: undefined };
      expect(clientFetchTimeoutMs()).toBe(30_000);
    },
  );
});
