import { afterAll, afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { FakeDocument } from '../utils/fake-document';

let currentPathname = '/docs';
let fetcherState: 'idle' | 'loading' = 'idle';
let fetcherData: unknown;
let lastRouterLinkProps: Record<string, unknown> | undefined;

const fetcherLoadMock = mock(() => undefined);
const lazyGuidesMock = mock(async () => ({}));
// The router the Link reads its routes from, without a RouterProvider.
const DataRouterContext = React.createContext<unknown>({
  router: { routes: [{ path: '/guides', lazy: lazyGuidesMock }] },
});

function getLinkHandler<Event>(name: string): (event: Event) => void {
  const handler = lastRouterLinkProps?.[name];
  if (typeof handler !== 'function') {
    throw new TypeError(`Expected Link to provide a ${name} handler`);
  }
  return handler as (event: Event) => void;
}

// Capture the real exports so the module mock can be restored after this file.
// bun's `mock.restore()` does not undo `mock.module()`, so without this the
// partial react-router stub would leak into other suites that import the real
// module.
const realReactRouter = { ...(await import('react-router')) };

mock.module('react-router', () => ({
  Link: (props: Record<string, unknown>) => {
    lastRouterLinkProps = props;
    return React.createElement('a', props);
  },
  useLocation: () => ({ pathname: currentPathname }),
  useFetcher: () => ({
    state: fetcherState,
    data: fetcherData,
    load: fetcherLoadMock,
  }),
  useResolvedPath: (to: string | { pathname?: string }) => ({
    pathname: typeof to === 'string' ? to : (to.pathname ?? currentPathname),
  }),
  useHref: (to: string) => to,
  matchRoutes: realReactRouter.matchRoutes,
  parsePath: realReactRouter.parsePath,
  UNSAFE_DataRouterContext: DataRouterContext,
}));

const { Link } = await import('../../src/client/components');

afterAll(() => {
  mock.module('react-router', () => realReactRouter);
});

describe('Link runtime behavior', () => {
  beforeEach(() => {
    currentPathname = '/docs';
    fetcherState = 'idle';
    fetcherData = undefined;
    lastRouterLinkProps = undefined;
    fetcherLoadMock.mockClear();
    lazyGuidesMock.mockClear();
    const globals = globalThis as typeof globalThis & { document?: FakeDocument; window?: Record<string, unknown> };
    globals.document = new FakeDocument();
    globals.window = {};
  });

  afterEach(() => {
    (globalThis as typeof globalThis & { document?: FakeDocument; window?: Record<string, unknown> }).document =
      undefined;
    (globalThis as typeof globalThis & { document?: FakeDocument; window?: Record<string, unknown> }).window =
      undefined;
  });

  it('keeps its display name for debugging', () => {
    expect(Link.displayName).toBe('Link');
  });

  it('normalizes hash targets to the current pathname', () => {
    renderToStaticMarkup(React.createElement(Link, { to: '#', prefetch: 'none' }, 'Docs'));

    expect(lastRouterLinkProps?.to).toBe('/docs');
  });

  it('prevents navigation when the link targets the current route', () => {
    renderToStaticMarkup(React.createElement(Link, { to: '/docs', prefetch: 'none' }, 'Docs'));

    const preventDefault = mock(() => undefined);
    getLinkHandler<{ preventDefault: () => void }>('onClick')({ preventDefault });

    expect(preventDefault).toHaveBeenCalledTimes(1);
  });

  it('prefetches the route module and data on user intent for internal links and forwards handlers', () => {
    renderToStaticMarkup(
      React.createElement(Link, {
        to: '/guides',
        prefetch: 'intent',
        onMouseEnter: mock(() => undefined),
        onFocus: mock(() => undefined),
        onTouchStart: mock(() => undefined),
      }),
    );

    const mouseEvent = {};
    const focusEvent = {};
    const touchEvent = {};
    getLinkHandler('onMouseEnter')(mouseEvent);
    getLinkHandler('onFocus')(focusEvent);
    getLinkHandler('onTouchStart')(touchEvent);

    expect(lazyGuidesMock).toHaveBeenCalledTimes(3);
    // The loader does not run: a loader that redirects would navigate on a hover.
    expect(fetcherLoadMock).not.toHaveBeenCalled();
    expect((globalThis as typeof globalThis & { document?: FakeDocument }).document?.head.children).toHaveLength(1);
    expect(
      ((globalThis as typeof globalThis & { document?: FakeDocument }).document?.head.children[0] as { href?: string })
        ?.href,
    ).toBe('/guides.json');
  });

  it('skips intent prefetching for external targets', () => {
    renderToStaticMarkup(React.createElement(Link, { to: 'https://putnami.dev', prefetch: 'intent' }));

    getLinkHandler('onMouseEnter')({});

    expect(lazyGuidesMock).not.toHaveBeenCalled();
    expect((globalThis as typeof globalThis & { document?: FakeDocument }).document?.head.children).toHaveLength(0);
  });
});
