import { describe, expect, it } from 'bun:test';
import { runInContext } from '@putnami/runtime';
import { specTest } from '@putnami/spectest';
import { abortableQuery, QueryAbortError, throwIfAborted, useAbortSignal } from '../src/abort';
import type { SqlQuery } from '../src/sql-client';

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

/** A query whose connection never answers: it settles only if cancel() settles it. */
function hungQuery(settleOnCancel: boolean): { query: SqlQuery<never[]>; cancels: () => number } {
  let cancels = 0;
  let reject: (error: Error) => void = () => {};
  const pending = new Promise<never[]>((_, rejectQuery) => {
    reject = rejectQuery;
  });
  const query = Object.assign(pending, {
    cancel: () => {
      cancels += 1;
      if (settleOnCancel) reject(new Error('canceling statement due to user request'));
    },
  }) as unknown as SqlQuery<never[]>;
  return { query, cancels: () => cancels };
}

describe('abortableQuery', () => {
  specTest(
    'unwinds at the abort even when the connection never answers',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'cancellation',
      check: 'a-query-in-flight-unwinds-at-the-abort',
    },
    async () => {
      for (const settleOnCancel of [false, true]) {
        const controller = new AbortController();
        const hung = hungQuery(settleOnCancel);
        const outcome = Promise.resolve(runInContext({ signal: controller.signal }, () => abortableQuery(hung.query)));
        controller.abort(new DOMException('Request Timeout', 'TimeoutError'));
        expect(await outcome.catch((error: unknown) => error)).toBeInstanceOf(QueryAbortError);
        expect(hung.cancels()).toBe(1);
      }
    },
  );

  it('returns the rows of a query that answers before any abort', async () => {
    const controller = new AbortController();
    const rows = [{ id: 1 }];
    const query = Object.assign(Promise.resolve(rows), { cancel: () => {} }) as unknown as SqlQuery<never[]>;
    const result = await runInContext({ signal: controller.signal }, () => abortableQuery(query));
    expect(result as unknown).toEqual(rows);
    controller.abort();
  });
});
