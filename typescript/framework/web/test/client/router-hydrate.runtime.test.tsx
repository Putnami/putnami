import { afterAll, afterEach, beforeEach, describe, expect, mock } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import React from 'react';

// Capture the real modules so the mocks can be restored after this file. bun's
// `mock.restore()` does not undo `mock.module()`, so without this the stubs
// would leak into other suites that import the real modules.
const realReactDomClient = { ...(await import('react-dom/client')) };
const realReactRouter = { ...(await import('react-router')) };

/**
 * The subscriber `hydratePage` installs to announce client navigations. Kept on
 * the fake so a test can assert the wiring without a real router.
 */
const routerSubscribers: Array<(state: unknown) => void> = [];

/** Stands in for a real router: `hydratePage` reads `state` and calls `subscribe`. */
const makeFakeRouter = () => ({
  id: 'router',
  state: { location: { pathname: '/docs', search: '', hash: '' } },
  subscribe: (listener: (state: unknown) => void) => {
    routerSubscribers.push(listener);
    return () => routerSubscribers.splice(routerSubscribers.indexOf(listener), 1);
  },
});

let lastRouter: ReturnType<typeof makeFakeRouter> | undefined;
const createBrowserRouterMock = mock(() => {
  lastRouter = makeFakeRouter();
  return lastRouter;
});
const matchRoutesMock = mock(() => null);
const hydrateRootMock = mock((_root: unknown, _element: unknown, _options: unknown) => ({ hydrated: true }));

mock.module('react-dom/client', () => ({
  hydrateRoot: (...args: [unknown, unknown, unknown]) => hydrateRootMock(...args),
}));

mock.module('react-router', () => ({
  createBrowserRouter: (...args: [unknown, unknown]) => createBrowserRouterMock(...args),
  matchRoutes: (...args: [unknown, unknown, unknown]) => matchRoutesMock(...args),
  RouterProvider: ({ router }: { router: unknown }) => React.createElement('router-provider', { router }),
}));

const { hydratePage } = await import('../../src/client/router');
const { ANONYMOUS_SECURITY_CONTEXT } = await import('../../src/shared/security.types');
const { getSecurityContext, setSecurityContext } = await import('../../src/client/security/security-context');

afterAll(() => {
  mock.module('react-dom/client', () => realReactDomClient);
  mock.module('react-router', () => realReactRouter);
});

describe('hydratePage', () => {
  const rootElement = { id: 'root', innerHTML: '<main>server rendered</main>' };
  const consoleErrorMock = mock(() => undefined);

  // The post-commit safety net in hydratePage schedules its blank-outlet check
  // via a double `requestAnimationFrame`. The stub queues callbacks so a test can
  // deterministically drain them (mimicking React committing asynchronously)
  // instead of relying on real animation frames.
  let animationFrameQueue: Array<() => void> = [];
  const flushAnimationFrames = () => {
    let guard = 0;
    while (animationFrameQueue.length > 0 && guard < 100) {
      const batch = animationFrameQueue;
      animationFrameQueue = [];
      for (const cb of batch) {
        cb();
      }
      guard += 1;
    }
  };

  beforeEach(() => {
    createBrowserRouterMock.mockClear();
    matchRoutesMock.mockClear();
    hydrateRootMock.mockClear();
    consoleErrorMock.mockClear();
    console.error = consoleErrorMock as typeof console.error;
    setSecurityContext(ANONYMOUS_SECURITY_CONTEXT);
    rootElement.innerHTML = '<main>server rendered</main>';
    animationFrameQueue = [];
    routerSubscribers.length = 0;
    lastRouter = undefined;

    (
      globalThis as typeof globalThis & {
        document?: { getElementById: (id: string) => unknown };
        window?: Record<string, unknown>;
      }
    ).document = {
      getElementById: (id: string) => (id === 'root' ? rootElement : null),
    };
    (
      globalThis as typeof globalThis & {
        document?: { getElementById: (id: string) => unknown };
        window?: Record<string, unknown>;
      }
    ).window = {
      location: { pathname: '/docs' },
      __basename: '/docs',
      requestAnimationFrame: (cb: () => void) => {
        animationFrameQueue.push(cb);
        return animationFrameQueue.length;
      },
      __staticRouterHydrationData: {
        loaderData: { root: { title: 'Docs' } },
        actionData: null,
        errors: null,
      },
    };
  });

  afterEach(() => {
    (globalThis as typeof globalThis & { document?: unknown; window?: unknown }).document = undefined;
    (globalThis as typeof globalThis & { document?: unknown; window?: unknown }).window = undefined;
    // Drop any callbacks left over from the scheduled guard so they cannot leak
    // into (or fire during) another test.
    animationFrameQueue = [];
  });

  specTest(
    'hydrates lazy routes and applies serialized security context',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'hydration-containment',
      check: 'lazy-routes-hydrate-with-the-serialized-security-context',
    },
    async () => {
      const lazyLoad = mock(async () => ({ element: React.createElement('div', null, 'Docs page') }));
      const routes = [{ path: '/docs', lazy: lazyLoad }];
      const securityContext = {
        authenticated: true,
        roles: ['admin'],
        scopes: ['docs:read'],
      };

      matchRoutesMock.mockReturnValue([{ route: routes[0] }]);
      (globalThis as typeof globalThis & { window?: Record<string, unknown> }).window!.__securityContext =
        securityContext;

      const result = await hydratePage('root', routes as never);

      expect(result).toEqual({ hydrated: true });
      expect(matchRoutesMock).toHaveBeenCalledWith(routes, '/docs', '/docs');
      expect(lazyLoad).toHaveBeenCalledTimes(1);
      expect(routes[0]).toMatchObject({
        path: '/docs',
        lazy: undefined,
      });
      expect(createBrowserRouterMock).toHaveBeenCalledWith(routes, {
        basename: '/docs',
        hydrationData: (globalThis as typeof globalThis & { window?: Record<string, unknown> }).window
          ?.__staticRouterHydrationData,
      });
      expect(getSecurityContext()).toEqual(securityContext);

      const [, element, options] = hydrateRootMock.mock.calls[0] as [
        unknown,
        React.ReactElement,
        Record<string, unknown>,
      ];
      expect(element.props.value).toEqual(securityContext);
      expect(element.props.children.props.router).toBe(lastRouter);
      // Navigation announcements are wired once, on the router that was handed
      // to RouterProvider.
      expect(routerSubscribers).toHaveLength(1);
      (options.onRecoverableError as (error: Error, info: { componentStack: string }) => void)(
        new Error('recoverable'),
        {
          componentStack: 'stack',
        },
      );
      (options.onCaughtError as (error: Error, info: { componentStack: string }) => void)(new Error('caught'), {
        componentStack: 'stack',
      });
      (options.onUncaughtError as (error: Error, info: { componentStack: string }) => void)(new Error('uncaught'), {
        componentStack: 'stack',
      });
      expect(consoleErrorMock).toHaveBeenCalledTimes(3);
    },
  );

  specTest(
    'throws when the target root element cannot be found and falls back to anonymous security',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'hydration-containment',
      check: 'a-missing-root-element-falls-back-to-anonymous-security',
    },
    async () => {
      (
        globalThis as typeof globalThis & {
          document?: { getElementById: (id: string) => unknown };
          window?: Record<string, unknown>;
        }
      ).document = {
        getElementById: () => null,
      };

      await expect(hydratePage('missing-root', [])).rejects.toThrow(
        'Hydration failed: root element with id "missing-root" not found',
      );
      expect(getSecurityContext()).toEqual(ANONYMOUS_SECURITY_CONTEXT);
    },
  );

  specTest(
    'keeps the static server HTML and skips hydration when a matched lazy chunk fails to load',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'hydration-containment',
      check: 'a-failed-lazy-chunk-preserves-the-server-html',
    },
    async () => {
      // Reproduces the "client chunk throws at module load" failure: the matched
      // route's lazy import rejects. Hydration must degrade to the server content
      // instead of rejecting out as an unhandled rejection.
      const lazyLoad = mock(async () => {
        throw new Error('d is not a function');
      });
      const routes = [{ path: '/docs', lazy: lazyLoad }];
      matchRoutesMock.mockReturnValue([{ route: routes[0] }]);

      const result = await hydratePage('root', routes as never);

      expect(result).toBeUndefined();
      expect(lazyLoad).toHaveBeenCalledTimes(1);
      // Hydration is skipped entirely, so the server HTML is never touched.
      expect(createBrowserRouterMock).not.toHaveBeenCalled();
      expect(hydrateRootMock).not.toHaveBeenCalled();
      expect(rootElement.innerHTML).toBe('<main>server rendered</main>');
      // The failing route is reported (not swallowed) and left unresolved.
      expect(consoleErrorMock).toHaveBeenCalledTimes(1);
      expect(routes[0].lazy).toBe(lazyLoad);
    },
  );

  specTest(
    'restores the server HTML when React reports an uncaught hydration error',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'hydration-containment',
      check: 'an-uncaught-hydration-error-restores-the-server-html',
    },
    async () => {
      const lazyLoad = mock(async () => ({ element: React.createElement('div', null, 'Docs page') }));
      const routes = [{ path: '/docs', lazy: lazyLoad }];
      matchRoutesMock.mockReturnValue([{ route: routes[0] }]);

      await hydratePage('root', routes as never);

      const [, , options] = hydrateRootMock.mock.calls[0] as [unknown, unknown, Record<string, unknown>];
      // Simulate React tearing the SSR tree down while unwinding a failed hydration.
      rootElement.innerHTML = '';
      (options.onUncaughtError as (error: Error, info: { componentStack: string }) => void)(new Error('boom'), {
        componentStack: 'stack',
      });

      // The server-rendered markup is put back so the page is not left blank.
      expect(rootElement.innerHTML).toBe('<main>server rendered</main>');
      expect(consoleErrorMock).toHaveBeenCalledTimes(1);
    },
  );

  specTest(
    'restores the server HTML and reports when hydration silently blanks the outlet',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'hydration-containment',
      check: 'a-silently-blanked-outlet-is-restored-and-reported',
    },
    async () => {
      // React 19's hydration-recovery path can discard the SSR
      // subtree and commit an EMPTY outlet without firing onUncaughtError /
      // onCaughtError / onRecoverableError. Before the fix this was the worst case:
      // blank page AND a clean console. The container-level safety net must catch
      // it after commit.
      const lazyLoad = mock(async () => ({ element: React.createElement('div', null, 'Docs page') }));
      const routes = [{ path: '/docs', lazy: lazyLoad }];
      matchRoutesMock.mockReturnValue([{ route: routes[0] }]);

      await hydratePage('root', routes as never);

      // React commits an empty outlet asynchronously and none of the three error
      // callbacks fire, so nothing has been logged yet.
      rootElement.innerHTML = '';
      expect(consoleErrorMock).not.toHaveBeenCalled();

      // Drain the double requestAnimationFrame that the guard scheduled.
      flushAnimationFrames();

      // The SSR markup is restored so the page degrades to static content...
      expect(rootElement.innerHTML).toBe('<main>server rendered</main>');
      // ...and a structured diagnostic is emitted so the blanking is never silent.
      expect(consoleErrorMock).toHaveBeenCalledTimes(1);
      const [prefix, kind, detail] = consoleErrorMock.mock.calls[0] as unknown as [
        string,
        string,
        { message: string; route: string },
      ];
      expect(prefix).toBe('[putnami:hydration]');
      expect(kind).toBe('blanked');
      expect(detail.route).toBe('/docs');
      expect(detail.message).toContain('restored server-rendered HTML');
    },
  );

  specTest(
    'leaves a healthy client render untouched and stays silent',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'hydration-containment',
      check: 'a-healthy-client-render-is-left-untouched',
    },
    async () => {
      // The safety net must not clobber a successful client render nor log a false
      // positive: it only fires when the outlet was actually left blank.
      const lazyLoad = mock(async () => ({ element: React.createElement('div', null, 'Docs page') }));
      const routes = [{ path: '/docs', lazy: lazyLoad }];
      matchRoutesMock.mockReturnValue([{ route: routes[0] }]);

      await hydratePage('root', routes as never);

      // Hydration succeeded and produced fresh client-rendered content.
      rootElement.innerHTML = '<main>client rendered</main>';

      flushAnimationFrames();

      expect(rootElement.innerHTML).toBe('<main>client rendered</main>');
      expect(consoleErrorMock).not.toHaveBeenCalled();
    },
  );
});
