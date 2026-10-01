import { beforeEach, describe, expect, it, mock } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import {
  application,
  cache,
  clearCache,
  CSP_NONCE_CONTEXT_KEY,
  type CsrfOptions,
  type HttpMiddleware,
  HttpPlugin,
  HttpResponse,
} from '@putnami/application';
import { resolve } from 'node:path';
import React from 'react';
import { runInContext } from '../../../runtime/src/context/context.utils';
import { CsrfInput } from '../../src/client/form/csrf-input';
import { action as actionBuilder } from '../../src/ssr/action';
import { layout } from '../../src/ssr/layout';
import { loader as loaderBuilder } from '../../src/ssr/loader';
import { notFound } from '../../src/ssr/not-found';
import { page } from '../../src/ssr/page';
import { ReactApplication } from '../../src/ssr/react-application';
import { createMockReactConfig } from '../fixtures/route.fixtures';

// Mock useConfig to avoid config loading side effects
const _mockGetConfig = mock(() =>
  createMockReactConfig({
    scanFolder: 'app',
    publicFolder: 'public',
    ssrTimeout: 3000,
    page: undefined,
    isDevelopment: true,
  }),
);

describe('ReactApplication', () => {
  const packageRoot = resolve(import.meta.dir, '..', '..');
  const workspaceRoot = resolve(packageRoot, '..', '..', '..');
  let app: ReactApplication;

  beforeEach(() => {
    process.env['PUTNAMI_WORKSPACE_ROOT'] = workspaceRoot;
    process.env['PUTNAMI_PROJECT_ROOT'] = packageRoot;
    process.env['PWD'] = packageRoot;
    app = new ReactApplication();
  });

  const createHttpContext = (url: string, overrides: Record<string, unknown> = {}) =>
    ({
      req: new Request(url),
      method: 'GET',
      headers: new Headers(),
      params: {},
      queryParams: () => ({}),
      body: async () => undefined,
      path: () => new URL(url).pathname,
      statusCode: 0,
      ...overrides,
    }) as never;

  describe('constructor', () => {
    it('creates a new instance', () => {
      expect(app).toBeInstanceOf(ReactApplication);
    });

    it('initializes with root route', () => {
      const httpPlugin = app.getHttpPlugin();
      expect(httpPlugin).toBeDefined();
    });

    it('updates and returns the basename', () => {
      app.setBasename('/docs');

      expect(app.getBasename()).toBe('/docs');
      expect((app as unknown as { reactRouter: Array<{ path?: string }> }).reactRouter[0]?.path).toBe('/docs');
    });
  });

  describe('getHttpPlugin', () => {
    it('returns the internal HttpPlugin', () => {
      const plugin = app.getHttpPlugin();
      expect(plugin).toBeDefined();
    });
  });

  describe('module caching', () => {
    it('caches successful module loads', async () => {
      const cache = (
        app as unknown as { cache: { loadModule: <T>(key: string, loader: () => Promise<T>) => Promise<T> } }
      ).cache;
      const loader = mock(async () => ({ ok: true }));

      const first = await cache.loadModule('page:/cached', loader);
      const second = await cache.loadModule('page:/cached', loader);

      expect(first).toEqual({ ok: true });
      expect(second).toEqual({ ok: true });
      expect(loader).toHaveBeenCalledTimes(1);
    });

    it('clears failed module loads so they can be retried', async () => {
      const cache = (
        app as unknown as { cache: { loadModule: <T>(key: string, loader: () => Promise<T>) => Promise<T> } }
      ).cache;
      const failing = mock(async () => {
        throw new Error('boom');
      });
      const succeeding = mock(async () => ({ ok: true }));

      await expect(cache.loadModule('page:/retry', failing)).rejects.toThrow('boom');
      await expect(cache.loadModule('page:/retry', succeeding)).resolves.toEqual({ ok: true });
      expect(failing).toHaveBeenCalledTimes(1);
      expect(succeeding).toHaveBeenCalledTimes(1);
    });
  });

  describe('findNearestLayout', () => {
    specTest(
      'converts dynamic segments written with brackets to React Router params',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'route-graph',
        check: 'a-bracketed-dynamic-segment-becomes-a-router-param',
      },
      () => {
        const result = (
          app as unknown as { findNearestLayout: (path: string) => { layout: unknown; remainingPath: string } }
        ).findNearestLayout('/users/[id]');

        expect(result.remainingPath).toBe('users/:id');
      },
    );

    specTest(
      'handles multiple dynamic segments',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'route-graph',
        check: 'several-dynamic-segments-are-mapped-together',
      },
      () => {
        const result = (
          app as unknown as { findNearestLayout: (path: string) => { layout: unknown; remainingPath: string } }
        ).findNearestLayout('/users/[userId]/posts/[postId]');

        expect(result.remainingPath).toBe('users/:userId/posts/:postId');
      },
    );

    it('handles root route', () => {
      const result = (
        app as unknown as { findNearestLayout: (path: string) => { layout: unknown; remainingPath: string } }
      ).findNearestLayout('/');

      expect(result.remainingPath).toBe('');
    });

    specTest(
      'filters route groups from path',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'route-graph',
        check: 'a-route-group-is-filtered-out-of-the-path',
      },
      () => {
        const result = (
          app as unknown as { findNearestLayout: (path: string) => { layout: unknown; remainingPath: string } }
        ).findNearestLayout('/(auth)/login');

        expect(result.remainingPath).toBe('login');
      },
    );
  });

  describe('ensureLayoutRoute', () => {
    it('creates layout node for simple path', () => {
      const ensureLayoutRoute = (app as unknown as { ensureLayoutRoute: (path: string) => unknown }).ensureLayoutRoute;
      const result = ensureLayoutRoute.call(app, '/dashboard');

      expect(result).toMatchObject({ path: 'dashboard', id: 'root-dashboard' });
    });

    specTest(
      'creates nested layout nodes',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'route-graph',
        check: 'a-nested-layout-produces-nested-nodes',
      },
      () => {
        const ensureLayoutRoute = (app as unknown as { ensureLayoutRoute: (path: string) => unknown })
          .ensureLayoutRoute;

        // Create parent first
        ensureLayoutRoute.call(app, '/admin');
        const result = ensureLayoutRoute.call(app, '/admin/settings');

        expect(result).toBeDefined();
        expect((result as { path: string }).path).toBe('settings');
      },
    );
  });

  describe('chaining', () => {
    it('returns this from reactLayout for chaining', () => {
      const layoutMock = { default: () => null };
      const result = app.reactLayout('/', { layout: layoutMock });

      expect(result).toBe(app);
    });

    it('returns this from reactPage for chaining', () => {
      const pageMock = { default: () => null };
      const result = app.reactPage('/', { page: pageMock });

      expect(result).toBe(app);
    });

    it('returns this from reactScript for chaining', () => {
      const result = app.reactScript('/hydrate.main.js', 'hydrate.main.js');

      expect(result).toBe(app);
    });
  });

  describe('route ID generation', () => {
    it('generates correct ID for root layout', () => {
      const layoutMock = { default: () => null };
      app.reactLayout('/', { layout: layoutMock });

      const router = (app as unknown as { reactRouter: unknown[] }).reactRouter;
      expect(router[0]).toBeDefined();
    });

    it('generates correct ID for nested routes', () => {
      const layoutMock = { default: () => null };
      app.reactLayout('/dashboard', { layout: layoutMock });
      app.reactLayout('/dashboard/settings', { layout: layoutMock });

      const router = (app as unknown as { reactRouter: unknown[] }).reactRouter;
      expect(router[0]).toBeDefined();
    });
  });

  describe('reactPage', () => {
    specTest(
      'registers page with correct route ID',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'route-graph',
        check: 'a-page-registers-under-its-route-id',
      },
      () => {
        const pageMock = { default: () => null };
        app.reactPage('/users', { page: pageMock });

        const router = (app as unknown as { reactRouter: unknown[] }).reactRouter;
        const root = router[0] as { children: { id: string }[] };
        const pageNode = root.children.find((c) => c.id === 'root-users-page');

        expect(pageNode).toMatchObject({ path: 'users', id: 'root-users-page' });
      },
    );

    specTest(
      'handles index pages correctly',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'route-graph',
        check: 'an-index-page-is-placed-at-its-parent',
      },
      () => {
        const pageMock = { default: () => null };
        app.reactPage('/', { page: pageMock });

        const router = (app as unknown as { reactRouter: unknown[] }).reactRouter;
        const root = router[0] as { children: { index: boolean }[] };
        const pageNode = root.children.find((c) => c.index === true);

        expect(pageNode).toBeDefined();
      },
    );

    specTest(
      'registers loader endpoint when provided',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'route-graph',
        check: 'a-loader-endpoint-is-registered-when-declared',
      },
      () => {
        // Create a spy-like mock for HttpPlugin
        const httpPluginMock = {
          get: mock(),
          post: mock(),
        };
        // Re-instantiate app with mock
        const appWithMock = new ReactApplication(
          undefined,
          httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
        );

        const pageMock = { default: () => null };
        const loaderMock = { loader: loaderBuilder(() => ({ data: 'test' })) };

        appWithMock.reactPage('/users', { page: pageMock, loader: loaderMock });

        expect(httpPluginMock.get).toHaveBeenCalled();
        // Verify JSON endpoint registration
        const calls = httpPluginMock.get.mock.calls;
        const jsonCall = calls.find((c) => c[0] === '/users.json');
        expect(jsonCall).toBeDefined();
      },
    );

    specTest(
      'registers action endpoint when provided',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'route-graph',
        check: 'an-action-endpoint-is-registered-when-declared',
      },
      () => {
        const httpPluginMock = {
          get: mock(),
          post: mock(),
        };
        const appWithMock = new ReactApplication(
          undefined,
          httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
        );

        const pageMock = { default: () => null };
        const actionMock = { action: actionBuilder(() => ({ success: true })) };

        appWithMock.reactPage('/contact', { page: pageMock, action: actionMock });

        expect(httpPluginMock.post).toHaveBeenCalled();
        expect(httpPluginMock.post).toHaveBeenCalledWith(
          '/contact',
          expect.any(Function),
          expect.objectContaining({ accept: expect.arrayContaining(['application/json']) }),
        );
      },
    );
  });

  describe('reactLayout', () => {
    it('moves existing page element to children', () => {
      const pageMock = { default: () => 'Page' };
      const LayoutComp = () => 'Layout';
      const layoutMock = { default: layout().render(LayoutComp) };

      // First register a page at /dashboard
      app.reactPage('/dashboard', { page: pageMock });

      // Then register a layout at /dashboard
      app.reactLayout('/dashboard', { layout: layoutMock });

      const router = (app as unknown as { reactRouter: unknown[] }).reactRouter;
      const root = router[0] as { children: any[] };
      const dashboardNode = root.children.find((c) => c.path === 'dashboard');

      expect(dashboardNode).toBeDefined();
      expect(dashboardNode.element).toBeDefined();
      expect(dashboardNode.children).toHaveLength(1);
      expect(dashboardNode.children[0].element).toBeDefined();
    });

    it('registers layout loader JSON endpoint', () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      const layoutMock = { default: () => null };
      const loaderMock = { loader: loaderBuilder(() => ({ user: 'admin' })) };

      appWithMock.reactLayout('/admin', { layout: layoutMock, loader: loaderMock });

      const calls = httpPluginMock.get.mock.calls;
      const jsonCall = calls.find((c) => c[0] === '/admin-layout.json');
      expect(jsonCall).toBeDefined();
    });
  });

  describe('reactError', () => {
    it('adds error element to nearest layout', () => {
      const errorMock = { default: () => 'Error' };
      const layoutMock = { default: () => 'Layout' };

      app.reactLayout('/dashboard', { layout: layoutMock });
      app.reactError('/dashboard', { error: errorMock });

      const router = (app as unknown as { reactRouter: unknown[] }).reactRouter;
      const root = router[0] as { children: any[] };
      const dashboardNode = root.children.find((c) => c.path === 'dashboard');

      // reactError creates a React element from the component
      expect(dashboardNode.errorElement).toBeDefined();
      expect(dashboardNode.errorElement.type).toBe(errorMock.default);
    });
  });

  describe('reactPage with page().render()', () => {
    it('applies statusCode from page definition', () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      const pageMod = {
        default: page()
          .status(403)
          .render(() => null),
      };

      appWithMock.reactPage('/forbidden', { page: pageMod });

      const calls = httpPluginMock.get.mock.calls;
      const pageCall = calls.find((c: unknown[]) => c[0] === '/forbidden');
      expect(pageCall).toBeDefined();
      expect(pageCall[2]).toMatchObject({ statusCode: 403 });
    });

    it('wraps page handler with middleware from page definition (enforces 401 unauthenticated)', async () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      const pageMod = {
        default: page()
          .secure({ roles: ['admin'] })
          .render(() => 'secret content'),
      };

      appWithMock.reactPage('/admin', { page: pageMod });

      const calls = httpPluginMock.get.mock.calls;
      const pageCall = calls.find((c: unknown[]) => c[0] === '/admin');
      expect(pageCall).toBeDefined();
      // ENFORCEMENT: the page's own .secure() must reject an unauthenticated
      // request with 401 instead of rendering the page.
      const ctx = createHttpContext('http://localhost/admin');
      const response = await runInContext(ctx, async () => pageCall[1](ctx));
      expect(response.status).toBe(401);
    });

    it('wraps loader JSON endpoint with middleware from page definition (enforces 401 unauthenticated)', async () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      const pageMod = {
        default: page()
          .secure({ roles: ['user'] })
          .render(() => null),
      };
      const loaderMock = { loader: loaderBuilder(() => ({ data: 'test' })) };

      appWithMock.reactPage('/secure', { page: pageMod, loader: loaderMock });

      const calls = httpPluginMock.get.mock.calls;
      const jsonCall = calls.find((c: unknown[]) => c[0] === '/secure.json');
      expect(jsonCall).toBeDefined();
      // ENFORCEMENT: the loader JSON endpoint inherits the page's .secure() and
      // must reject an unauthenticated request with 401, not leak loader data.
      const ctx = createHttpContext('http://localhost/secure.json');
      const response = await runInContext(ctx, async () => jsonCall[1](ctx));
      expect(response.status).toBe(401);
    });

    it('wraps action endpoint with middleware from page definition', () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      const pageMod = {
        default: page().render(() => null),
      };
      const actionMock = { action: actionBuilder(() => ({ success: true })) };

      appWithMock.reactPage('/form', { page: pageMod, action: actionMock });

      expect(httpPluginMock.post).toHaveBeenCalled();
      expect(httpPluginMock.post).toHaveBeenCalledWith(
        '/form',
        expect.any(Function),
        expect.objectContaining({ accept: expect.arrayContaining(['application/json']) }),
      );
    });

    it('handles plain component module without page definition', () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      const pageMock = { default: () => null };

      appWithMock.reactPage('/normal', { page: pageMock });

      const calls = httpPluginMock.get.mock.calls;
      const pageCall = calls.find((c: unknown[]) => c[0] === '/normal');
      expect(pageCall).toBeDefined();
      expect(pageCall[2]).toMatchObject({ statusCode: 200 });
    });

    it('renders lazy pages and reuses the loaded module across requests', async () => {
      let lazyPageLoads = 0;
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      appWithMock.reactPage('/lazy', {
        page: async () => {
          lazyPageLoads++;
          return {
            default: page()
              .status(202)
              .render(() => 'Lazy page'),
          };
        },
      });

      const pageCall = httpPluginMock.get.mock.calls.find((call: unknown[]) => call[0] === '/lazy');
      expect(pageCall).toBeDefined();

      const firstCtx = createHttpContext('http://localhost/lazy');
      const secondCtx = createHttpContext('http://localhost/lazy');
      const first = await runInContext(firstCtx, async () => pageCall[1](firstCtx));
      const second = await runInContext(secondCtx, async () => pageCall[1](secondCtx));

      expect(lazyPageLoads).toBe(1);
      expect(first.status).toBe(202);
      expect(await first.get().text()).toContain('Lazy page');
      expect(await second.get().text()).toContain('Lazy page');
    });

    it('serves lazy loader JSON endpoints and caches the lazy module', async () => {
      let lazyLoaderLoads = 0;
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      appWithMock.reactLayout('/admin', {
        layout: {
          default: layout()
            .secure({ roles: ['admin'] })
            .render(() => null),
        },
      });
      appWithMock.reactPage('/admin/reports', {
        page: { default: () => null },
        loader: async () => {
          lazyLoaderLoads++;
          return { default: loaderBuilder(() => ({ section: 'reports' })) };
        },
      });

      const jsonCall = httpPluginMock.get.mock.calls.find((call: unknown[]) => call[0] === '/admin/reports.json');
      expect(jsonCall).toBeDefined();

      const first = await jsonCall[1](
        createHttpContext('http://localhost/admin/reports', { user: { roles: ['admin'] } }),
      );
      const second = await jsonCall[1](
        createHttpContext('http://localhost/admin/reports', { user: { roles: ['admin'] } }),
      );

      expect(lazyLoaderLoads).toBe(1);
      expect(await first.get().json()).toEqual({ section: 'reports' });
      expect(await second.get().json()).toEqual({ section: 'reports' });
    });

    it('returns null JSON payloads when lazy loader modules do not export a loader', async () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      appWithMock.reactPage('/empty', {
        page: { default: () => null },
        loader: async () => ({}) as never,
      });

      const jsonCall = httpPluginMock.get.mock.calls.find((call: unknown[]) => call[0] === '/empty.json');
      const response = await jsonCall[1](createHttpContext('http://localhost/empty'));

      expect(response.status).toBe(200);
      expect(await response.get().json()).toBeNull();
    });

    it('evicts cache patterns after lazy actions succeed', async () => {
      await clearCache();
      await cache('loader')
        .ttl(60_000)
        .for('users')
        .fetch(() => 'stale');

      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      appWithMock.reactPage('/save', {
        page: { default: () => null },
        action: async () => ({
          default: actionBuilder()
            .evict('loader:users')
            .handle(() => ({ saved: true })),
        }),
      });

      const actionCall = httpPluginMock.post.mock.calls.find((call: unknown[]) => call[0] === '/save');
      const result = await actionCall[1](createHttpContext('http://localhost/save'));
      const after = await cache('loader')
        .ttl(60_000)
        .for('users')
        .fetch(() => 'fresh');

      expect(result).toEqual({ saved: true });
      expect(after).toBe('fresh');
    });
  });

  describe('reactLayout with inline middleware', () => {
    it('wraps layout loader with middleware from layout definition', () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      const layoutMod = {
        default: layout()
          .secure({ roles: ['admin'] })
          .render(() => null),
      };
      const loaderMock = { loader: loaderBuilder(() => ({ user: 'admin' })) };

      appWithMock.reactLayout('/admin', { layout: layoutMod, loader: loaderMock });

      const calls = httpPluginMock.get.mock.calls;
      const jsonCall = calls.find((c: unknown[]) => c[0] === '/admin-layout.json');
      expect(jsonCall).toBeDefined();
      // Handler should be wrapped with middleware
      expect(typeof jsonCall[1]).toBe('function');
    });

    it('propagates layout middleware to child pages (enforces 401 when unauthenticated)', async () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      const layoutMod = {
        default: layout()
          .secure({ roles: ['admin'] })
          .render(() => null),
      };
      const pageMock = { default: page().render(() => 'secret content') };

      appWithMock.reactLayout('/admin', { layout: layoutMod });
      appWithMock.reactPage('/admin/settings', { page: pageMock });

      const calls = httpPluginMock.get.mock.calls;
      const pageCall = calls.find((c: unknown[]) => c[0] === '/admin/settings');
      expect(pageCall).toBeDefined();

      // ENFORCEMENT (not function-ness): invoking the child page handler with an
      // unauthenticated context must return 401 from the layout's SecurityMiddleware,
      // never the rendered page. A bare (unwrapped) renderer would 200 here.
      const ctx = createHttpContext('http://localhost/admin/settings');
      const response = await runInContext(ctx, async () => pageCall[1](ctx));
      expect(response.status).toBe(401);
    });

    it('composes layout middleware (outer) with page middleware (inner)', () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      const executionOrder: string[] = [];
      const layoutMw = async (_ctx: unknown, next: () => Promise<unknown>) => {
        executionOrder.push('layout');
        return next();
      };
      const pageMw = async (_ctx: unknown, next: () => Promise<unknown>) => {
        executionOrder.push('page');
        return next();
      };

      const layoutMod = {
        default: layout()
          .use(layoutMw as never)
          .render(() => null),
      };
      const pageMod = {
        default: page()
          .use(pageMw as never)
          .render(() => null),
      };

      appWithMock.reactLayout('/admin', { layout: layoutMod });
      appWithMock.reactPage('/admin/dashboard', { page: pageMod });

      // The page handler was registered with composed middleware
      const calls = httpPluginMock.get.mock.calls;
      const pageCall = calls.find((c: unknown[]) => c[0] === '/admin/dashboard');
      expect(pageCall).toBeDefined();
      expect(typeof pageCall[1]).toBe('function');
    });

    it('propagates nested layout middleware in correct order (admin .secure() enforces 401)', async () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      const rootLayout = {
        default: layout()
          .rateLimit({ max: 100 })
          .render(() => null),
      };
      const adminLayout = {
        default: layout()
          .secure({ roles: ['admin'] })
          .render(() => null),
      };
      const pageMock = { default: page().render(() => 'secret content') };

      appWithMock.reactLayout('/', { layout: rootLayout });
      appWithMock.reactLayout('/admin', { layout: adminLayout });
      appWithMock.reactPage('/admin/users', { page: pageMock });

      // Page under /admin should get both root (rate-limit) + admin (secure)
      // middleware. ENFORCEMENT: an unauthenticated request must hit the admin
      // layout's SecurityMiddleware and return 401, proving the nested-layout
      // chain actually wraps the child handler (not merely that it is a function).
      const calls = httpPluginMock.get.mock.calls;
      const pageCall = calls.find((c: unknown[]) => c[0] === '/admin/users');
      expect(pageCall).toBeDefined();
      const ctx = createHttpContext('http://localhost/admin/users');
      const response = await runInContext(ctx, async () => pageCall[1](ctx));
      expect(response.status).toBe(401);
    });

    it('does not apply layout middleware to pages outside the layout (public route stays unguarded)', async () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      const layoutMod = {
        default: layout()
          .secure({ roles: ['admin'] })
          .render(() => null),
      };
      const pageMock = { default: page().render(() => 'public content') };

      appWithMock.reactLayout('/admin', { layout: layoutMod });
      appWithMock.reactPage('/public', { page: pageMock });

      const calls = httpPluginMock.get.mock.calls;
      const pageCall = calls.find((c: unknown[]) => c[0] === '/public');
      expect(pageCall).toBeDefined();
      // ENFORCEMENT (negative): the /admin layout's SecurityMiddleware must NOT
      // leak onto a sibling /public route. An unauthenticated request renders
      // normally (200) rather than 401 — confirming scoping, not just function-ness.
      const ctx = createHttpContext('http://localhost/public');
      const response = await runInContext(ctx, async () => pageCall[1](ctx));
      expect(response.status).toBe(200);
    });

    it('propagates layout middleware to page loader and action (both enforce 401 unauthenticated)', async () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      const layoutMod = {
        default: layout()
          .secure({ roles: ['admin'] })
          .render(() => null),
      };
      const pageMock = { default: () => null };
      const loaderMock = { loader: loaderBuilder(() => ({ data: 'test' })) };
      const actionMock = { action: actionBuilder(() => ({ success: true })) };

      appWithMock.reactLayout('/admin', { layout: layoutMod });
      appWithMock.reactPage('/admin/settings', {
        page: pageMock,
        loader: loaderMock,
        action: actionMock,
      });

      // ENFORCEMENT: the loader JSON endpoint must reject unauthenticated requests
      // with 401 (layout SecurityMiddleware), never leak loader data.
      const getCalls = httpPluginMock.get.mock.calls;
      const jsonCall = getCalls.find((c: unknown[]) => c[0] === '/admin/settings.json');
      expect(jsonCall).toBeDefined();
      const loaderCtx = createHttpContext('http://localhost/admin/settings.json');
      const loaderResponse = await runInContext(loaderCtx, async () => jsonCall[1](loaderCtx));
      expect(loaderResponse.status).toBe(401);

      // ENFORCEMENT: the action POST endpoint must reject unauthenticated requests
      // with 401, never run the mutation.
      const postCalls = httpPluginMock.post.mock.calls;
      const actionCall = postCalls.find((c: unknown[]) => c[0] === '/admin/settings');
      expect(actionCall).toBeDefined();
      const actionCtx = createHttpContext('http://localhost/admin/settings');
      const actionResponse = await runInContext(actionCtx, async () => actionCall[1](actionCtx));
      expect(actionResponse.status).toBe(401);
    });
  });

  // ---------------------------------------------------------------------------
  // LAZY secured layouts — production routes layouts EAGERLY (the SSR generator
  // always emits `addImportModule(...)` for layouts before any page), so these
  // hand-written lazy-layout cases are the only path that hits the lazy branch
  // in `reactLayout`. These enforcement tests INVOKE the registered child
  // handler with an UNAUTHENTICATED context and assert 401 — i.e. they assert
  // real auth ENFORCEMENT, not handler function-ness.
  //
  // Previously a lazy layout populated middleware only when its React component
  // first rendered, while child routes read it eagerly at registration. A child
  // loader/page/action under a lazy `.secure()` layout could therefore ship with
  // no server-side enforcement. RouteMiddlewareResolver records lazy layouts
  // and resolves their security at request time before any child handler runs.
  // Each assertion invokes a registered child handler with an unauthenticated
  // context and demands 401, proving enforcement rather than handler shape.
  describe('lazy secured layout fail-closed enforcement', () => {
    const lazySecuredLayout = () => ({
      layout: async () => ({
        default: layout()
          .secure({ roles: ['admin'] })
          .render(() => null),
      }),
    });

    it('enforces 401 on an eager child page under a lazy secured layout', async () => {
      const httpPluginMock = { get: mock(), post: mock() };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      appWithMock.reactLayout('/admin', lazySecuredLayout());
      appWithMock.reactPage('/admin/settings', { page: { default: page().render(() => 'secret content') } });

      const pageCall = httpPluginMock.get.mock.calls.find((c: unknown[]) => c[0] === '/admin/settings');
      expect(pageCall).toBeDefined();
      const ctx = createHttpContext('http://localhost/admin/settings');
      const response = await runInContext(ctx, async () => pageCall[1](ctx));
      expect(response.status).toBe(401);
    });

    it('enforces 401 on an eager child loader under a lazy secured layout', async () => {
      const httpPluginMock = { get: mock(), post: mock() };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      appWithMock.reactLayout('/admin', lazySecuredLayout());
      appWithMock.reactPage('/admin/reports', {
        page: { default: () => null },
        loader: { loader: loaderBuilder(() => ({ section: 'reports' })) },
      });

      const jsonCall = httpPluginMock.get.mock.calls.find((c: unknown[]) => c[0] === '/admin/reports.json');
      expect(jsonCall).toBeDefined();
      const ctx = createHttpContext('http://localhost/admin/reports.json');
      const response = await runInContext(ctx, async () => jsonCall[1](ctx));
      expect(response.status).toBe(401);
    });

    it('enforces 401 on a lazy child loader under a lazy secured layout', async () => {
      const httpPluginMock = { get: mock(), post: mock() };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      appWithMock.reactLayout('/admin', lazySecuredLayout());
      appWithMock.reactPage('/admin/reports', {
        page: { default: () => null },
        loader: async () => ({ default: loaderBuilder(() => ({ section: 'reports' })) }),
      });

      const jsonCall = httpPluginMock.get.mock.calls.find((c: unknown[]) => c[0] === '/admin/reports.json');
      expect(jsonCall).toBeDefined();
      const ctx = createHttpContext('http://localhost/admin/reports.json');
      const response = await runInContext(ctx, async () => jsonCall[1](ctx));
      expect(response.status).toBe(401);
    });

    it('enforces 401 on an eager child action under a lazy secured layout', async () => {
      const httpPluginMock = { get: mock(), post: mock() };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      appWithMock.reactLayout('/admin', lazySecuredLayout());
      appWithMock.reactPage('/admin/save', {
        page: { default: () => null },
        action: { action: actionBuilder(() => ({ saved: true })) },
      });

      const actionCall = httpPluginMock.post.mock.calls.find((c: unknown[]) => c[0] === '/admin/save');
      expect(actionCall).toBeDefined();
      const ctx = createHttpContext('http://localhost/admin/save');
      const response = await runInContext(ctx, async () => actionCall[1](ctx));
      expect((response as { status?: number }).status).toBe(401);
    });

    it('enforces 401 on a not-found catch-all under a lazy secured layout', async () => {
      const httpPluginMock = { get: mock(), post: mock() };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      appWithMock.reactLayout('/admin', lazySecuredLayout());
      appWithMock.reactNotFound('/admin', {
        notFound: { default: notFound().render(() => 'missing admin page') },
      });

      const pageCall = httpPluginMock.get.mock.calls.find((c: unknown[]) => c[0] === '/admin*');
      expect(pageCall).toBeDefined();
      const ctx = createHttpContext('http://localhost/admin/missing');
      const response = await runInContext(ctx, async () => pageCall[1](ctx));
      expect(response.status).toBe(401);
    });
  });

  describe('not found handling', () => {
    it('renders eager not-found components for 404 loader errors', async () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      appWithMock.reactLayout('/docs', {
        layout: { default: layout().render(() => 'Docs layout') },
      });
      appWithMock.reactPage('/docs/item', {
        page: { default: () => 'Item page' },
        loader: {
          loader: loaderBuilder(() => {
            throw new Response('missing', { status: 404 });
          }),
        },
      });
      appWithMock.reactNotFound('/docs', {
        notFound: { default: notFound().render(() => 'Docs not found') },
      });

      const pageCall = httpPluginMock.get.mock.calls.find((call: unknown[]) => call[0] === '/docs/item');
      const ctx = createHttpContext('http://localhost/docs/item');
      const response = await runInContext(ctx, async () => pageCall[1](ctx));
      const html = await response.get().text();

      expect(response.status).toBe(404);
      expect(html).toContain('Docs not found');
    });

    it('renders lazy not-found components for 404 loader errors', async () => {
      const httpPluginMock = {
        get: mock(),
        post: mock(),
      };
      const appWithMock = new ReactApplication(
        undefined,
        httpPluginMock as unknown as import('@putnami/application').HttpPlugin,
      );

      appWithMock.reactLayout('/guides', {
        layout: { default: layout().render(() => 'Guides layout') },
      });
      appWithMock.reactPage('/guides/item', {
        page: { default: () => 'Guide page' },
        loader: {
          loader: loaderBuilder(() => {
            throw new Response('missing', { status: 404 });
          }),
        },
      });
      appWithMock.reactNotFound('/guides', {
        notFound: async () => ({ default: notFound().render(() => 'Lazy guides not found') }),
      });

      const pageCall = httpPluginMock.get.mock.calls.find((call: unknown[]) => call[0] === '/guides/item');
      const ctx = createHttpContext('http://localhost/guides/item');
      const response = await runInContext(ctx, async () => pageCall[1](ctx));
      const html = await response.get().text();

      expect(response.status).toBe(404);
      expect(html).toContain('Lazy guides not found');
    });
  });

  // ---------------------------------------------------------------------------
  // SSR ships secure-by-default response headers. ReactApplication prepends
  // SecurityHeadersMiddleware to its internal HttpPlugin, which is carried to
  // the app on merge, so every SSR/loader/action response gets a nonce-based
  // CSP, X-Content-Type-Options: nosniff, and Referrer-Policy by default.
  describe('default security headers', () => {
    // Run the plugin's registered middleware chain around a representative SSR
    // response, mirroring the per-request nonce the page renderer publishes on
    // the context (CSP_NONCE_CONTEXT_KEY). `secured()` is provided for the HSTS
    // gate. Returns the final response after the header middleware runs.
    const runThroughMiddleware = async (
      plugin: HttpPlugin,
      ctxOverrides: Record<string, unknown> = {},
    ): Promise<HttpResponse | undefined> => {
      const middlewares = (plugin as unknown as { middlewares: HttpMiddleware[] }).middlewares;
      const ctx = {
        req: new Request('http://localhost/'),
        method: 'GET',
        secured: () => true,
        [CSP_NONCE_CONTEXT_KEY]: 'test-nonce-value',
        ...ctxOverrides,
      } as never;
      // Inner handler stands in for the SSR page renderer's HTML response.
      const dispatch = async (): Promise<HttpResponse | undefined> =>
        new HttpResponse('<!DOCTYPE html><html></html>', {
          status: 200,
          headers: { 'content-type': 'text/html;charset=utf-8' },
        });
      const chain = middlewares.reduceRight<() => Promise<HttpResponse | undefined>>(
        (next, mw) => async () => (await mw(ctx, next)) as HttpResponse | undefined,
        dispatch,
      );
      return chain();
    };

    const runThroughRootPipeline = async (
      plugin: HttpPlugin,
      url: string,
      ctxOverrides: Record<string, unknown> = {},
    ): Promise<HttpResponse | undefined> => {
      type MatchedRoute = {
        route: string;
        params?: Record<string, string>;
        statusCode: number;
      };
      type HttpPluginInternals = {
        router: { find: (ctx: never) => MatchedRoute[] };
        buildMiddlewareChain: () => (
          ctx: never,
          dispatch: () => Promise<HttpResponse | undefined>,
        ) => Promise<HttpResponse | undefined>;
      };
      const parsed = new URL(url);
      const headers = new Headers({ Accept: 'text/html' });
      const ctx = createHttpContext(url, {
        method: 'GET',
        headers,
        secured: () => true,
        host: () => parsed.host,
        domain: () => parsed.origin,
        path: () => parsed.pathname,
        query: () => parsed.search.slice(1),
        [CSP_NONCE_CONTEXT_KEY]: 'test-nonce-value',
        ...ctxOverrides,
      });
      const internals = plugin as unknown as HttpPluginInternals;
      const matched = internals.router.find(ctx);
      const first = matched[0];
      Object.assign(ctx as Record<string, unknown>, {
        __matchedHandlers: matched,
        route: first?.route,
        params: first?.params ?? {},
        statusCode: first?.statusCode ?? 200,
      });
      const chain = internals.buildMiddlewareChain();
      return chain(
        ctx,
        async () =>
          new HttpResponse('<!DOCTYPE html><html></html>', {
            status: 200,
            headers: { 'content-type': 'text/html;charset=utf-8' },
          }),
      );
    };

    it('sets CSP (with hydration nonce), nosniff, and Referrer-Policy by default', async () => {
      const realPlugin = new HttpPlugin();
      // securityHeaders defaults to true (omitted here → undefined, treated as on).
      const securedApp = new ReactApplication(createMockReactConfig(), realPlugin);
      securedApp.reactPage('/', { page: { default: () => null } });

      const response = await runThroughMiddleware(realPlugin);
      expect(response).toBeDefined();
      expect(response?.getHeader('X-Content-Type-Options')).toBe('nosniff');
      expect(response?.getHeader('Referrer-Policy')).toBe('strict-origin-when-cross-origin');

      const csp = response?.getHeader('Content-Security-Policy');
      expect(csp).toBeDefined();
      expect(csp).toContain("default-src 'self'");
      // Nonce-based, NOT unsafe-inline, for scripts — the per-request nonce is
      // woven into script-src so the framework's inline hydration script runs.
      expect(csp).toContain("script-src 'self' 'nonce-test-nonce-value'");
      expect(csp).not.toContain("script-src 'self' 'unsafe-inline'");
    });

    it('does NOT clobber a Content-Type set by the SSR response', async () => {
      const realPlugin = new HttpPlugin();
      const securedApp = new ReactApplication(createMockReactConfig(), realPlugin);
      securedApp.reactPage('/', { page: { default: () => null } });

      const response = await runThroughMiddleware(realPlugin);
      expect(response?.getHeader('content-type')).toBe('text/html;charset=utf-8');
    });

    it('opt-out: react({ securityHeaders: false }) ships no security headers', async () => {
      const realPlugin = new HttpPlugin();
      const openApp = new ReactApplication(createMockReactConfig({ securityHeaders: false }), realPlugin);
      openApp.reactPage('/', { page: { default: () => null } });

      const response = await runThroughMiddleware(realPlugin);
      expect(response).toBeDefined();
      // No security headers were prepended.
      expect(response?.getHeader('X-Content-Type-Options')).toBeUndefined();
      expect(response?.getHeader('Referrer-Policy')).toBeUndefined();
      expect(response?.getHeader('Content-Security-Policy')).toBeUndefined();
    });

    it('opt-out survives merging React routes into the root HttpPlugin', async () => {
      const rootPlugin = new HttpPlugin();
      const openApp = new ReactApplication(createMockReactConfig({ securityHeaders: false }), new HttpPlugin());
      openApp.reactPage('/', { page: { default: () => null } });
      rootPlugin.merge(openApp.getHttpPlugin());

      const response = await runThroughRootPipeline(rootPlugin, 'http://localhost/');
      expect(response).toBeDefined();
      expect(response?.getHeader('X-Content-Type-Options')).toBeUndefined();
      expect(response?.getHeader('Referrer-Policy')).toBeUndefined();
      expect(response?.getHeader('Content-Security-Policy')).toBeUndefined();
    });
  });

  // ---------------------------------------------------------------------------
  // action() POSTs are CSRF-validated by default. The ReactApplication
  // prepends CsrfMiddleware onto its internal HttpPlugin (carried to the app on
  // merge): page GETs issue the double-submit `_csrf` cookie, and action POSTs
  // must echo the token via the X-CSRF-Token header (the client action handler
  // does this automatically) or a `_csrf` form field (<CsrfInput />), or they
  // are rejected with 403. Opt out with `react({ csrf: false })`.
  describe('default CSRF validation on action() POSTs', () => {
    type PluginInternals = {
      middlewares: HttpMiddleware[];
      router: {
        find: (ctx: never) => Array<{
          handler: (ctx: never) => unknown;
          route: string;
          params?: Record<string, string>;
        }>;
      };
      _routeEntries: Array<{ method: string; path: string; options?: { csrfExempt?: boolean } }>;
    };

    // Build a real app with one action route on a real internal HttpPlugin.
    const buildActionApp = (config: Parameters<typeof createMockReactConfig>[0] = {}): HttpPlugin => {
      const plugin = new HttpPlugin();
      const reactApp = new ReactApplication(createMockReactConfig(config), plugin);
      reactApp.reactPage('/submit', {
        page: { default: () => null },
        action: { action: actionBuilder(async () => HttpResponse.json({ submitted: true })) },
      });
      return plugin;
    };

    const startComposedActionApp = async (
      rootCsrf: true | CsrfOptions,
      {
        fieldName = '_csrf',
        reactConfig = {},
      }: {
        fieldName?: string;
        reactConfig?: Parameters<typeof createMockReactConfig>[0];
      } = {},
    ) => {
      const rootPlugin = new HttpPlugin({ port: 0, secure: false, csrf: rootCsrf });
      const reactApp = new ReactApplication(createMockReactConfig(reactConfig), new HttpPlugin());
      reactApp.reactPage('/submit', {
        page: {
          default: () =>
            React.createElement('form', { method: 'post' }, React.createElement(CsrfInput, { name: fieldName })),
        },
        action: { action: actionBuilder(async () => HttpResponse.json({ submitted: true })) },
      });
      rootPlugin.merge(reactApp.getHttpPlugin());

      const composedApp = application().use(rootPlugin);
      await composedApp.start();
      return {
        app: composedApp,
        url: `http://localhost:${rootPlugin.getServer()?.port}/submit`,
      };
    };

    const readBrowserCookieToken = (setCookie: string, cookieName: string): string | undefined =>
      new RegExp(`(?:^|\\s)${cookieName}=([^;]+)`).exec(setCookie)?.[1];

    const readHiddenToken = (html: string, fieldName: string): string | undefined =>
      new RegExp(`name="${fieldName}" value="([^"]+)"`).exec(html)?.[1];

    // Run a real Request through the plugin's registered middleware chain and
    // into the route handler the router matches for it — the request-level
    // equivalent of `runThroughMiddleware` above.
    const runRequest = async (plugin: HttpPlugin, req: Request): Promise<HttpResponse | undefined> => {
      const parsed = new URL(req.url);
      const ctx = createHttpContext(req.url, {
        req,
        method: req.method,
        headers: req.headers,
        secured: () => true,
        host: () => parsed.host,
        domain: () => parsed.origin,
        path: () => parsed.pathname,
        query: () => '',
        [CSP_NONCE_CONTEXT_KEY]: 'test-nonce-value',
      });
      const internals = plugin as unknown as PluginInternals;
      const matched = internals.router.find(ctx);
      Object.assign(ctx as unknown as Record<string, unknown>, {
        __matchedHandlers: matched,
        route: matched[0]?.route,
        params: matched[0]?.params ?? {},
      });
      const dispatch = async (): Promise<HttpResponse | undefined> =>
        (await matched[0]?.handler(ctx)) as HttpResponse | undefined;
      const chain = (internals.middlewares as HttpMiddleware[]).reduceRight<() => Promise<HttpResponse | undefined>>(
        (next, mw) => async () => (await mw(ctx, next)) as HttpResponse | undefined,
        dispatch,
      );
      return runInContext(ctx, async () => chain());
    };

    // GET any page once and harvest the `_csrf` token the middleware issues via
    // Set-Cookie (the middleware runs around a stand-in HTML response, exactly
    // like `runThroughMiddleware`).
    const issueCsrfToken = async (plugin: HttpPlugin): Promise<string> => {
      const middlewares = (plugin as unknown as PluginInternals).middlewares;
      const ctx = {
        req: new Request('http://localhost/submit'),
        method: 'GET',
        secured: () => true,
        [CSP_NONCE_CONTEXT_KEY]: 'test-nonce-value',
      } as never;
      const dispatch = async (): Promise<HttpResponse | undefined> =>
        new HttpResponse('<!DOCTYPE html><html></html>', {
          status: 200,
          headers: { 'content-type': 'text/html;charset=utf-8' },
        });
      const chain = middlewares.reduceRight<() => Promise<HttpResponse | undefined>>(
        (next, mw) => async () => (await mw(ctx, next)) as HttpResponse | undefined,
        dispatch,
      );
      const response = await chain();
      const setCookie = response?.getHeader('Set-Cookie') ?? '';
      const match = /_csrf=([^;]+)/.exec(setCookie);
      expect(match).toBeTruthy();
      return match?.[1] as string;
    };

    it('rejects an action POST without a CSRF token (403 by default)', async () => {
      const plugin = buildActionApp();
      const response = await runRequest(
        plugin,
        new Request('http://localhost/submit', { method: 'POST', headers: { Accept: 'application/json' } }),
      );
      expect(response?.status).toBe(403);
      expect(await response?.get().json()).toEqual({ error: 'CSRF token mismatch' });
    });

    it('issues the _csrf double-submit cookie on page GETs', async () => {
      const plugin = buildActionApp();
      const token = await issueCsrfToken(plugin);
      expect(token.length).toBeGreaterThan(0);
    });

    it('accepts an action POST echoing the cookie token via X-CSRF-Token (client round-trip)', async () => {
      const plugin = buildActionApp();
      const token = await issueCsrfToken(plugin);
      const response = await runRequest(
        plugin,
        new Request('http://localhost/submit', {
          method: 'POST',
          headers: {
            Accept: 'application/json',
            Cookie: `_csrf=${token}`,
            'X-CSRF-Token': token,
          },
        }),
      );
      expect(response?.status).toBe(200);
      expect(await response?.get().json()).toEqual({ submitted: true });
    });

    it('accepts an action POST echoing the cookie token via the _csrf form field (no-JS <CsrfInput />)', async () => {
      const plugin = buildActionApp();
      const token = await issueCsrfToken(plugin);
      const form = new FormData();
      form.set('_csrf', token);
      form.set('name', 'Ada');
      const response = await runRequest(
        plugin,
        new Request('http://localhost/submit', {
          method: 'POST',
          headers: { Accept: 'application/json', Cookie: `_csrf=${token}` },
          body: form,
        }),
      );
      expect(response?.status).toBe(200);
      expect(await response?.get().json()).toEqual({ submitted: true });
    });

    it('keeps one CSRF token authority when http({ csrf: true }) and react() are composed', async () => {
      const composed = await startComposedActionApp(true);
      try {
        const pageResponse = await fetch(composed.url);
        expect(pageResponse.status).toBe(200);
        const setCookies = pageResponse.headers.getSetCookie();
        const html = await pageResponse.text();

        expect(setCookies).toHaveLength(1);
        const cookieToken = readBrowserCookieToken(setCookies[0] ?? '', '_csrf');
        const renderedToken = readHiddenToken(html, '_csrf');
        expect(cookieToken).toBeTruthy();
        expect(renderedToken).toBe(cookieToken);

        const form = new FormData();
        form.set('_csrf', renderedToken as string);
        form.set('name', 'Ada');
        const actionResponse = await fetch(composed.url, {
          method: 'POST',
          headers: {
            Accept: 'application/json',
            Cookie: `_csrf=${cookieToken}`,
          },
          body: form,
        });

        expect(actionResponse.status).toBe(200);
        expect(await actionResponse.json()).toEqual({ submitted: true });
      } finally {
        await composed.app.stop();
      }
    });

    it('preserves the root http() CSRF options instead of installing React defaults', async () => {
      const composed = await startComposedActionApp(
        {
          cookieName: 'http-csrf',
          fieldName: 'http_csrf',
          headerName: 'X-Http-CSRF',
          sameSite: 'Lax',
        },
        { fieldName: 'http_csrf' },
      );
      try {
        const pageResponse = await fetch(composed.url);
        expect(pageResponse.status).toBe(200);
        const setCookies = pageResponse.headers.getSetCookie();
        const html = await pageResponse.text();

        expect(setCookies).toHaveLength(1);
        expect(setCookies[0]).toContain('http-csrf=');
        expect(setCookies[0]).toContain('SameSite=Lax');
        expect(setCookies[0]).not.toContain('_csrf=');
        const cookieToken = readBrowserCookieToken(setCookies[0] ?? '', 'http-csrf');
        const renderedToken = readHiddenToken(html, 'http_csrf');
        expect(cookieToken).toBeTruthy();
        expect(renderedToken).toBe(cookieToken);

        const form = new FormData();
        form.set('http_csrf', renderedToken as string);
        const actionResponse = await fetch(composed.url, {
          method: 'POST',
          headers: {
            Accept: 'application/json',
            Cookie: `http-csrf=${cookieToken}`,
          },
          body: form,
        });

        expect(actionResponse.status).toBe(200);
        expect(await actionResponse.json()).toEqual({ submitted: true });
      } finally {
        await composed.app.stop();
      }
    });

    it('rejects an action POST whose submitted token does not match the cookie', async () => {
      const plugin = buildActionApp();
      const token = await issueCsrfToken(plugin);
      const response = await runRequest(
        plugin,
        new Request('http://localhost/submit', {
          method: 'POST',
          headers: {
            Accept: 'application/json',
            Cookie: `_csrf=${token}`,
            'X-CSRF-Token': 'forged-token',
          },
        }),
      );
      expect(response?.status).toBe(403);
    });

    it('opt-out: react({ csrf: false }) does not validate action POSTs', async () => {
      const plugin = buildActionApp({ csrf: false });
      const response = await runRequest(
        plugin,
        new Request('http://localhost/submit', { method: 'POST', headers: { Accept: 'application/json' } }),
      );
      expect(response?.status).toBe(200);
      expect(await response?.get().json()).toEqual({ submitted: true });
    });

    it('opt-out marks the app routes csrfExempt so app-level CSRF middleware skips them after merge', () => {
      const plugin = buildActionApp({ csrf: false });
      const entries = (plugin as unknown as PluginInternals)._routeEntries;
      expect(entries.length).toBeGreaterThan(0);
      for (const entry of entries) {
        expect(entry.options?.csrfExempt).toBe(true);
      }
    });

    it('react({ csrf: false }) remains exempt when the root http() plugin enables CSRF', async () => {
      const composed = await startComposedActionApp(true, { reactConfig: { csrf: false } });
      try {
        const response = await fetch(composed.url, {
          method: 'POST',
          headers: { Accept: 'application/json' },
        });
        expect(response.status).toBe(200);
        expect(await response.json()).toEqual({ submitted: true });
      } finally {
        await composed.app.stop();
      }
    });
  });
});
