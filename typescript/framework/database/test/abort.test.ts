import { describe, expect, it } from 'bun:test';
import { runInContext } from '@putnami/runtime';
import { specTest } from '@putnami/spectest';
import { QueryAbortError, throwIfAborted, useAbortSignal } from '../src/abort';

describe('QueryAbortError', () => {
  it('should have the expected name and code', () => {
    const error = new QueryAbortError();
    expect(error.name).toBe('QueryAbortError');
    expect(error.code).toBe('57014');
    expect(error.message).toBe('Query aborted');
  });

  it('should accept a custom message', () => {
    const error = new QueryAbortError('custom abort');
    expect(error.message).toBe('custom abort');
    expect(error.code).toBe('57014');
  });
});

describe('useAbortSignal', () => {
  it('should return undefined when no signal in context', async () => {
    await runInContext({}, () => {
      expect(useAbortSignal()).toBeUndefined();
    });
  });

  it('should return undefined outside of context', () => {
    expect(useAbortSignal()).toBeUndefined();
  });

  it('should return the signal from context', async () => {
    const controller = new AbortController();
    await runInContext({ signal: controller.signal }, () => {
      const signal = useAbortSignal();
      expect(signal).toBe(controller.signal);
      expect(signal?.aborted).toBe(false);
    });
  });

  it('should return an already-aborted signal', async () => {
    const controller = new AbortController();
    controller.abort();

    await runInContext({ signal: controller.signal }, () => {
      const signal = useAbortSignal();
      expect(signal?.aborted).toBe(true);
    });
  });
});

describe('throwIfAborted', () => {
  it('should not throw when no signal in context', async () => {
    await runInContext({}, () => {
      expect(() => throwIfAborted()).not.toThrow();
    });
  });

  it('should not throw when signal is not aborted', async () => {
    const controller = new AbortController();
    await runInContext({ signal: controller.signal }, () => {
      expect(() => throwIfAborted()).not.toThrow();
    });
  });

  specTest(
    'should throw QueryAbortError when signal is aborted',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'cancellation',
      check: 'a-query-after-abort-raises-query-abort-error',
    },
    async () => {
      const controller = new AbortController();
      controller.abort();

      await runInContext({ signal: controller.signal }, () => {
        expect(() => throwIfAborted()).toThrow(QueryAbortError);
      });
    },
  );
});
