import { describe, expect, it } from 'bun:test';
import { runInContext } from '@putnami/runtime';
import { abortableQuery, QueryAbortError } from '../src/abort';
import type { SqlQuery } from '../src/sql-client';

/**
 * The path of a query that is not aborted: abortableQuery starts the query in
 * its call, adds no frame of its own, reads no signal under a request's abort
 * hooks, and leaves no listener once the query settled, so an abort after it
 * never cancels a statement that already answered.
 */

const HOOKS = Symbol.for('putnami.request.abortHooks');

/** A query that records when something subscribes to it and when it is cancelled; it settles on demand. */
function controlledQuery<T extends readonly unknown[]>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const settled = new Promise<T>((resolveQuery, rejectQuery) => {
    resolve = resolveQuery;
    reject = rejectQuery;
  });
  let subscriptions = 0;
  let cancels = 0;
  const query = {
    // biome-ignore lint/suspicious/noThenProperty: a query is awaitable by contract
    then(onfulfilled?: (value: T) => unknown, onrejected?: (reason: unknown) => unknown) {
      subscriptions++;
      return settled.then(onfulfilled, onrejected);
    },
    cancel() {
      cancels++;
    },
  } as unknown as SqlQuery<T>;
  return {
    query,
    resolve,
    reject,
    subscriptions: () => subscriptions,
    cancels: () => cancels,
  };
}

/** A request context with abort hooks, whose `signal` getter counts its reads, as the HTTP context's does. */
function hookedContext() {
  const listeners = new Set<() => void>();
  let ended = false;
  let signalReads = 0;
  const proto = {
    get signal(): AbortSignal {
      signalReads++;
      return new AbortController().signal;
    },
  };
  const context = Object.create(proto) as Record<PropertyKey, unknown>;
  context[HOOKS] = {
    aborted: () => ended,
    onAbort: (listener: () => void) => listeners.add(listener),
    offAbort: (listener: () => void) => listeners.delete(listener),
  };
  return {
    context,
    listeners,
    end: () => {
      ended = true;
      for (const listener of [...listeners]) listener();
    },
    signalReads: () => signalReads,
  };
}

/** An AbortSignal that counts the listeners it holds. */
function countingSignal() {
  const controller = new AbortController();
  const signal = controller.signal;
  let held = 0;
  const add = signal.addEventListener.bind(signal);
  const remove = signal.removeEventListener.bind(signal);
  signal.addEventListener = ((type: string, listener: EventListener, options?: AddEventListenerOptions) => {
    held++;
    add(type, listener, options);
  }) as unknown as typeof signal.addEventListener;
  signal.removeEventListener = ((type: string, listener: EventListener) => {
    held--;
    remove(type, listener);
  }) as unknown as typeof signal.removeEventListener;
  return { controller, signal, held: () => held };
}

describe('abortableQuery on the path of a query that is not aborted', () => {
  it('starts the query in its call under a request’s abort hooks, and reads no signal', async () => {
    const request = hookedContext();
    const pending = controlledQuery<[{ id: number }]>();
    const result = Promise.resolve(runInContext(request.context, () => abortableQuery(pending.query)));
    expect(pending.subscriptions()).toBe(1);
    expect(request.listeners.size).toBe(1);
    const rows: [{ id: number }] = [{ id: 1 }];
    pending.resolve(rows);
    expect((await result) as unknown).toBe(rows);
    expect(request.listeners.size).toBe(0);
    expect(request.signalReads()).toBe(0);
  });

  it('starts the query in its call under a signal, and removes its listener once it settled', async () => {
    const { signal, held } = countingSignal();
    const pending = controlledQuery<[]>();
    const result = Promise.resolve(runInContext({ signal }, () => abortableQuery(pending.query)));
    expect(pending.subscriptions()).toBe(1);
    expect(held()).toBe(1);
    pending.resolve([]);
    await result;
    expect(held()).toBe(0);
  });

  it('starts the query in its call outside any context', async () => {
    const pending = controlledQuery<[]>();
    const result = abortableQuery(pending.query);
    expect(result).toBeInstanceOf(Promise);
    expect(pending.subscriptions()).toBe(1);
    const rows: [] = [];
    pending.resolve(rows);
    expect((await result) as unknown).toBe(rows);
  });

  it('never cancels a query that already settled when the request ends after it', async () => {
    const request = hookedContext();
    const pending = controlledQuery<[]>();
    const result = Promise.resolve(runInContext(request.context, () => abortableQuery(pending.query)));
    pending.resolve([]);
    await result;
    request.end();
    expect(pending.cancels()).toBe(0);

    const { controller, signal } = countingSignal();
    const signalled = controlledQuery<[]>();
    const answer = Promise.resolve(runInContext({ signal }, () => abortableQuery(signalled.query)));
    signalled.resolve([]);
    await answer;
    controller.abort();
    expect(signalled.cancels()).toBe(0);
  });

  it('rejects with the query’s own failure and leaves no listener', async () => {
    const request = hookedContext();
    const pending = controlledQuery<[]>();
    const result = Promise.resolve(runInContext(request.context, () => abortableQuery(pending.query)));
    const failure = Object.assign(new Error('duplicate key'), { code: '23505' });
    pending.reject(failure);
    expect(await result.catch((error: unknown) => error)).toBe(failure);
    expect(request.listeners.size).toBe(0);
    expect(pending.cancels()).toBe(0);
  });

  it('rejects at the abort once, even when the query settles after it', async () => {
    const request = hookedContext();
    const pending = controlledQuery<[]>();
    const result = Promise.resolve(runInContext(request.context, () => abortableQuery(pending.query)));
    request.end();
    pending.resolve([]);
    expect(await result.catch((error: unknown) => error)).toBeInstanceOf(QueryAbortError);
    expect(pending.cancels()).toBe(1);
    expect(request.listeners.size).toBe(0);
  });

  it('never throws: a failing abort hook rejects', async () => {
    const pending = controlledQuery<[]>();
    const broken = new Error('hooks broke');
    const context = {
      [HOOKS]: {
        aborted: () => {
          throw broken;
        },
        onAbort: () => {},
        offAbort: () => {},
      },
    };
    let result: Promise<unknown> | undefined;
    expect(() => {
      result = Promise.resolve(runInContext(context, () => abortableQuery(pending.query)));
    }).not.toThrow();
    expect(await result?.catch((error: unknown) => error)).toBe(broken);
    expect(pending.subscriptions()).toBe(0);
  });

  it('rejects with a throwing abort hook registration, cancels the query and leaves no unread rejection', async () => {
    const broken = new Error('onAbort broke');
    const pending = controlledQuery<[]>();
    const context = {
      [HOOKS]: {
        aborted: () => false,
        onAbort: () => {
          throw broken;
        },
        offAbort: () => {},
      },
    };
    const unhandled = await unhandledRejectionsOf(async () => {
      const result = Promise.resolve(runInContext(context, () => abortableQuery(pending.query)));
      expect(await result.catch((error: unknown) => error)).toBe(broken);
      expect(pending.cancels()).toBe(1);
      pending.reject(cancelled());
    });
    expect(unhandled).toEqual([]);
  });

  it('rejects with a throwing signal listener registration, cancels the query and leaves no unread rejection', async () => {
    const broken = new Error('addEventListener broke');
    const pending = controlledQuery<[]>();
    const signal = new AbortController().signal;
    signal.addEventListener = (() => {
      throw broken;
    }) as unknown as typeof signal.addEventListener;
    const unhandled = await unhandledRejectionsOf(async () => {
      const result = Promise.resolve(runInContext({ signal }, () => abortableQuery(pending.query)));
      expect(await result.catch((error: unknown) => error)).toBe(broken);
      expect(pending.cancels()).toBe(1);
      pending.reject(cancelled());
    });
    expect(unhandled).toEqual([]);
  });
});

/** The failure a cancelled query settles with. */
function cancelled(): Error {
  return Object.assign(new Error('canceling statement due to user request'), { code: '57014' });
}

/** The rejections nobody read while `run` ran, and in the macrotask after it. */
async function unhandledRejectionsOf(run: () => Promise<void>): Promise<unknown[]> {
  const unhandled: unknown[] = [];
  const onUnhandled = (reason: unknown) => unhandled.push(reason);
  process.on('unhandledRejection', onUnhandled);
  try {
    await run();
    await new Promise((resolve) => setTimeout(resolve, 10));
  } finally {
    process.off('unhandledRejection', onUnhandled);
  }
  return unhandled;
}
