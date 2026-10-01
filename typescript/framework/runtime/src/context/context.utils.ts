import { AsyncLocalStorage } from 'node:async_hooks';
import type { Context } from './context.type';

// Use a globalThis key so that separate bundles (e.g. @putnami/runtime vs
// @putnami/runtime/inject) share the exact same AsyncLocalStorage instance.
// Without this, each bundled entry point gets its own module-level singleton
// and runInContext() / useContext() operate on different stores.
const GLOBAL_KEY = Symbol.for('__putnami_async_storage__');

const getAsyncStorage = <C>(): AsyncLocalStorage<C> => {
  const g = globalThis as Record<symbol, AsyncLocalStorage<unknown> | undefined>;
  if (!g[GLOBAL_KEY]) {
    g[GLOBAL_KEY] = new AsyncLocalStorage<C>();
  }
  return g[GLOBAL_KEY] as AsyncLocalStorage<C>;
};

export function runInContext<C extends Context, R = unknown>(
  context: C,
  action: (...args: unknown[]) => R | Promise<R>,
): R | Promise<R> {
  return getAsyncStorage().run(context, action);
}

export function tryContext<C = unknown>(): C | undefined {
  return getAsyncStorage()?.getStore() as C | undefined;
}

export function useContext<C = unknown>(): C {
  const store = getAsyncStorage()?.getStore();
  if (!store) {
    throw new Error('useContext() called outside of runInContext(). Wrap your execution in runInContext().');
  }
  return store as C;
}
