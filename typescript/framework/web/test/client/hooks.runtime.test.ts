import { afterAll, afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { FakeDocument } from '../utils/fake-document';

// Capture the real modules so the mocks can be restored after this file. bun's
// `mock.restore()` does not undo `mock.module()`, so without this the react and
// react-router stubs would leak into other suites that import the real modules.
const realReact = { ...(await import('react')) };
const realReactRouter = { ...(await import('react-router')) };
const originalFetch = globalThis.fetch;

let stateCursor = 0;
let refCursor = 0;
let effectCursor = 0;
let memoCursor = 0;
const stateStore: unknown[] = [];
const stateSetters: ReturnType<typeof mock>[] = [];
const refStore: Array<{ current: unknown }> = [];
const effectDeps: Array<readonly unknown[] | undefined> = [];
const effectCleanups: Array<(() => void) | undefined> = [];
const memoDeps: Array<readonly unknown[] | undefined> = [];
const memoValues: unknown[] = [];

// Reset the hook cursors so a hook can be re-invoked as if React re-rendered the
// component. Persisted state/ref/effect/memo stores survive across renders, which
// lets these tests exercise dependency-array behavior (e.g. an effect that should
// only re-run when its deps actually change).
const resetCursors = () => {
  stateCursor = 0;
  refCursor = 0;
  effectCursor = 0;
  memoCursor = 0;
};

const depsChanged = (next: readonly unknown[] | undefined, previous: readonly unknown[] | undefined) => {
  if (!next || !previous) return true;
  if (next.length !== previous.length) return true;
  return next.some((value, index) => !Object.is(value, previous[index]));
};

// Run any pending effect cleanups registered by the React mock.
const runEffectCleanups = () => {
  for (const cleanup of effectCleanups) {
    cleanup?.();
  }
};

// Allow the async fetch effect to settle (request + json + setState).
const flushMicrotasks = async () => {
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
};

const useActionDataMock = mock(() => ({ ok: true, status: 201, message: 'saved' }));
const useLoaderDataMock = mock(() => ({ page: 'docs' }));
const useRouteLoaderDataMock = mock((id: string) => ({ id, title: 'root-data' }));
const useFormActionMock = mock((action?: string) => (action ? `/resolved${action}` : '/resolved/current'));
const fetcherLoadMock = mock(() => undefined);
const useFetcherMock = mock(() => ({
  state: 'idle',
  data: undefined,
  load: fetcherLoadMock,
}));
const useResolvedPathMock = mock((to: string | { pathname?: string }) => ({
  pathname: typeof to === 'string' ? to : (to.pathname ?? '/resolved'),
}));

mock.module('react', () => ({
  useState<T>(initial: T | (() => T)) {
    const index = stateCursor++;
    if (!(index in stateStore)) {
      stateStore[index] = typeof initial === 'function' ? (initial as () => T)() : initial;
    }
    if (!stateSetters[index]) {
      stateSetters[index] = mock((value: unknown) => {
        stateStore[index] =
          typeof value === 'function' ? (value as (previous: unknown) => unknown)(stateStore[index]) : value;
      });
    }
    return [stateStore[index] as T, stateSetters[index]!];
  },
  useEffect(effect: () => undefined | (() => void), deps?: readonly unknown[]) {
    const index = effectCursor++;
    if (depsChanged(deps, effectDeps[index])) {
      effectCleanups[index]?.();
      effectDeps[index] = deps;
      effectCleanups[index] = effect() ?? undefined;
    }
  },
  useMemo<T>(factory: () => T, deps?: readonly unknown[]) {
    const index = memoCursor++;
    if (!(index in memoValues) || depsChanged(deps, memoDeps[index])) {
      memoDeps[index] = deps;
      memoValues[index] = factory();
    }
    return memoValues[index] as T;
  },
  useCallback<T extends (...args: never[]) => unknown>(callback: T) {
    return callback;
  },
  useRef<T>(initialValue: T) {
    const index = refCursor++;
    refStore[index] ||= { current: initialValue };
    return refStore[index] as { current: T };
  },
}));

mock.module('react-router', () => ({
  useActionData: () => useActionDataMock(),
  useLoaderData: () => useLoaderDataMock(),
  useRouteLoaderData: (id: string) => useRouteLoaderDataMock(id),
  useFormAction: (action?: string) => useFormActionMock(action),
  useFetcher: () => useFetcherMock(),
  useResolvedPath: (to: string | { pathname?: string }) => useResolvedPathMock(to),
}));

const hooks = await import('../../src/client/hooks');
const { useFetch } = await import('../../src/client/hooks/use-fetch.hook');
const { usePrefetch } = await import('../../src/client/hooks/use-prefetch.hook');

type BrowserGlobals = typeof globalThis & { window?: { __reactClientFetchTimeoutMs?: number } };
type UseFetchStoreState = { loading: boolean; error?: Error; data?: unknown };

// A hung server: the returned promise only settles when the request's signal
// aborts — without a timeout signal such a request would stay pending forever.
const makeHangingFetchMock = () =>
  mock(
    (_input: RequestInfo | URL, init?: RequestInit) =>
      new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener('abort', () => reject(init?.signal?.reason));
      }),
  );

afterAll(() => {
  mock.module('react', () => realReact);
  mock.module('react-router', () => realReactRouter);
});

describe('hook runtime behavior', () => {
  beforeEach(() => {
    resetCursors();
    runEffectCleanups();
    stateStore.length = 0;
    stateSetters.length = 0;
    refStore.length = 0;
    effectDeps.length = 0;
    effectCleanups.length = 0;
    memoDeps.length = 0;
    memoValues.length = 0;
    useActionDataMock.mockClear();
    useLoaderDataMock.mockClear();
    useRouteLoaderDataMock.mockClear();
    useFormActionMock.mockClear();
    useFetcherMock.mockClear();
    useResolvedPathMock.mockClear();
    fetcherLoadMock.mockClear();
    (globalThis as typeof globalThis & { document?: FakeDocument; fetch?: typeof fetch }).document = new FakeDocument();
  });

  afterEach(() => {
    runEffectCleanups();
    (globalThis as typeof globalThis & { document?: FakeDocument; fetch?: typeof fetch }).document = undefined;
    globalThis.fetch = originalFetch;
    (globalThis as BrowserGlobals).window = undefined;
  });

  it('proxies typed router hooks and form actions', () => {
    expect(hooks.useLoaderData()).toEqual({ page: 'docs' });
    expect(hooks.useActionData()).toEqual({ ok: true, status: 201, message: 'saved' });
    expect(hooks.useRouteLoaderData('root')).toEqual({ id: 'root', title: 'root-data' });
    expect(hooks.useFormAction('/users')).toBe('/resolved/users');
  });

  it('fetches JSON data and re-fetches on explicit refetch', async () => {
    const fetchMock = mock(
      async () =>
        new Response(JSON.stringify({ ok: true }), { status: 200, headers: { 'content-type': 'application/json' } }),
    );
    (globalThis as typeof globalThis & { fetch?: typeof fetch }).fetch = fetchMock as typeof fetch;

    const result = useFetch<{ ok: boolean }>('/api/data');
    await flushMicrotasks();

    const fetchArgs = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(fetchArgs[0]).toBe('/api/data');
    const dataHeaders = new Headers(fetchArgs[1]?.headers);
    expect([...dataHeaders]).toEqual([]);
    expect((stateStore[0] as Record<string, unknown>).loading).toBe(false);
    expect((stateStore[0] as Record<string, unknown>).data).toEqual({ ok: true });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // refetch() must bump the reload token *and* the effect must re-run on the
    // next render, issuing a second request. The previous test only checked that
    // a state setter was invoked, which masked the broken refetch contract.
    result.refetch();
    resetCursors();
    useFetch<{ ok: boolean }>('/api/data');
    await flushMicrotasks();

    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('does not re-fetch when an inline init object keeps the same contents across renders', async () => {
    const fetchMock = mock(
      async () =>
        new Response(JSON.stringify({ ok: true }), { status: 200, headers: { 'content-type': 'application/json' } }),
    );
    (globalThis as typeof globalThis & { fetch?: typeof fetch }).fetch = fetchMock as typeof fetch;

    // First render with an inline init object.
    useFetch('/api/data', { method: 'GET' });
    await flushMicrotasks();
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Re-render with a brand-new inline object of identical contents — the common
    // case that previously changed the dependency identity every render and drove
    // a fetch loop. The serialized init key keeps the effect from re-running.
    resetCursors();
    useFetch('/api/data', { method: 'GET' });
    await flushMicrotasks();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('adds CSRF protection to state-changing requests and surfaces HTTP failures', async () => {
    const fakeDocument = (globalThis as typeof globalThis & { document?: FakeDocument }).document!;
    fakeDocument.cookie = '_csrf=csrf-token';

    const fetchMock = mock(async () => new Response(JSON.stringify({ ok: false }), { status: 403 }));
    (globalThis as typeof globalThis & { fetch?: typeof fetch }).fetch = fetchMock as typeof fetch;

    useFetch('/api/save', {
      method: 'POST',
      headers: { 'X-Test': '1' },
    });
    await flushMicrotasks();

    const [, requestInit] = fetchMock.mock.calls[0] as [string, RequestInit];
    const sentHeaders = new Headers(requestInit.headers);
    expect(sentHeaders.get('X-CSRF-Token')).toBe('csrf-token');
    expect(sentHeaders.get('X-Test')).toBe('1');
    expect(((stateStore[0] as Record<string, unknown>).error as Error).message).toContain('403');
  });

  it('aborts a request that exceeds the client fetch timeout and surfaces a timeout error', async () => {
    // Shrink the configurable timeout (normally SSR-injected, default 30s) so
    // the test observes the abort quickly.
    (globalThis as BrowserGlobals).window = { __reactClientFetchTimeoutMs: 20 };
    const fetchMock = makeHangingFetchMock();
    (globalThis as typeof globalThis & { fetch?: typeof fetch }).fetch = fetchMock as unknown as typeof fetch;

    useFetch('/api/slow');

    // Wait (bounded) for the timeout signal to abort the hung request and the
    // error state to land — real timers drive AbortSignal.timeout.
    const deadline = Date.now() + 2000;
    while (Date.now() < deadline && (stateStore[0] as UseFetchStoreState | undefined)?.loading !== false) {
      await new Promise((resolve) => setTimeout(resolve, 5));
    }

    const [, requestInit] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(requestInit.signal?.aborted).toBe(true);
    const state = stateStore[0] as UseFetchStoreState;
    expect(state.loading).toBe(false);
    expect(state.error?.message).toBe('HTTP request timed out after 20ms');
  });

  it('does not surface an error when a pending request is aborted by unmount', async () => {
    const fetchMock = makeHangingFetchMock();
    (globalThis as typeof globalThis & { fetch?: typeof fetch }).fetch = fetchMock as unknown as typeof fetch;

    useFetch('/api/slow');
    // Unmount while the request is still pending.
    runEffectCleanups();
    await flushMicrotasks();

    const [, requestInit] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(requestInit.signal?.aborted).toBe(true);
    const state = stateStore[0] as UseFetchStoreState;
    expect(state.error).toBeUndefined();
    expect(state.loading).toBe(true);
  });

  it('preserves caller headers passed as a Headers instance and keeps the CSRF token authoritative', async () => {
    const fakeDocument = (globalThis as typeof globalThis & { document?: FakeDocument }).document!;
    fakeDocument.cookie = '_csrf=real-csrf';

    const fetchMock = mock(
      async () =>
        new Response(JSON.stringify({ ok: true }), { status: 200, headers: { 'content-type': 'application/json' } }),
    );
    (globalThis as typeof globalThis & { fetch?: typeof fetch }).fetch = fetchMock as typeof fetch;

    // Caller passes headers as a `Headers` instance (the form that the old
    // record-spread silently dropped) and also tries to override the CSRF token.
    const callerHeaders = new Headers();
    callerHeaders.set('Authorization', 'Bearer abc123');
    callerHeaders.set('X-CSRF-Token', 'attacker-supplied');

    useFetch('/api/save', {
      method: 'POST',
      headers: callerHeaders,
    });
    await flushMicrotasks();

    const [, requestInit] = fetchMock.mock.calls[0] as [string, RequestInit];
    const sentHeaders = new Headers(requestInit.headers);
    // Caller's Authorization header survives (record-spread would have dropped it).
    expect(sentHeaders.get('Authorization')).toBe('Bearer abc123');
    // Framework CSRF token wins over the caller-supplied value.
    expect(sentHeaders.get('X-CSRF-Token')).toBe('real-csrf');
  });

  it('falls back for unserializable init objects and prefetches on render', () => {
    const fetchMock = mock(async () => new Response(JSON.stringify({ ok: true }), { status: 200 }));
    (globalThis as typeof globalThis & { fetch?: typeof fetch }).fetch = fetchMock as typeof fetch;

    const circularInit = { method: 'GET' } as RequestInit & { self?: unknown };
    circularInit.self = circularInit;
    useFetch('/api/circular', circularInit);

    const fakeDocument = (globalThis as typeof globalThis & { document?: FakeDocument }).document!;
    const appendedLinks: Record<string, string>[] = [];
    fakeDocument.head.appendChild = mock((element: Record<string, string>) => {
      appendedLinks.push(element);
      return element;
    });

    const prefetch = usePrefetch('/docs', 'render');
    prefetch.load();

    expect(fetcherLoadMock).toHaveBeenCalledWith('/docs');
    expect(appendedLinks).toHaveLength(1);
    expect(appendedLinks[0]?.href).toBe('/docs.json');
  });
});
