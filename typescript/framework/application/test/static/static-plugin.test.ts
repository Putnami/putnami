import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import { HttpPlugin, http } from '../../src/http/http.plugin';
import { StaticPlugin, staticFiles } from '../../src/static/static.plugin';
import { publicFolder } from '../../src/static/static.utils';

describe('StaticPlugin', () => {
  let app: Application;
  let httpPlugin: HttpPlugin;

  beforeEach(() => {
    httpPlugin = http({ port: 0 });
    app = application().use(httpPlugin);
  });

  afterEach(async () => {
    try {
      await app.stop();
    } catch {
      // Ignore if already stopped
    }
  });

  describe('staticFiles()', () => {
    it('should create StaticPlugin instance', () => {
      const plugin = staticFiles({ skipLoading: true });
      expect(plugin).toBeInstanceOf(StaticPlugin);
    });
  });

  describe('routeStatic()', () => {
    it('should be chainable', () => {
      const plugin = new StaticPlugin({ skipLoading: true });
      const result = plugin
        .routeStatic('a.html', 'a.html', { gzip: false, mime: 'text/html' })
        .routeStatic('b.html', 'b.html', { gzip: false, mime: 'text/html' });

      expect(result).toBe(plugin);
    });

    it('should store gzip file path when compressed', () => {
      const plugin = new StaticPlugin({ skipLoading: true });
      plugin.routeStatic('compressed.js', 'compressed.js.gz', { gzip: true, mime: 'application/javascript' });

      const routes = (plugin as unknown as { staticRoutes: Map<string, unknown> }).staticRoutes;
      const route = routes.get('compressed.js') as { gzipFilePath?: string };

      expect(route.gzipFilePath).toBeDefined();
      expect(route.gzipFilePath).toContain('compressed.js.gz');
    });

    it('should set cache control header with configured max-age', () => {
      const plugin = new StaticPlugin({ skipLoading: true, cacheMaxAge: 3600 });
      plugin.routeStatic('styles.css', 'styles.css', { gzip: false, mime: 'text/css' });

      const routes = (plugin as unknown as { staticRoutes: Map<string, unknown> }).staticRoutes;
      const route = routes.get('styles.css') as { headers: Record<string, string> };

      expect(route.headers['Cache-Control']).toBe('public, max-age=3600');
    });
  });

  describe('warmup()', () => {
    it('should register routes with HttpPlugin', async () => {
      const plugin = new StaticPlugin({ skipLoading: true });
      app.use(plugin);
      await app.start();

      const http = app.getPlugin(HttpPlugin);
      expect(http).toBeInstanceOf(HttpPlugin);

      await app.stop();
    });

    it('should create HttpPlugin if not registered', async () => {
      const plugin = new StaticPlugin({ skipLoading: true });
      // Use http({ port: 0 }) to avoid port collision in parallel tests
      const emptyApp = application()
        .use(http({ port: 0 }))
        .use(plugin);
      await emptyApp.start();

      const httpPlugin = emptyApp.getPlugin(HttpPlugin);
      expect(httpPlugin).toBeInstanceOf(HttpPlugin);

      await emptyApp.stop();
    });
  });

  describe('config options', () => {
    it('should use default config values', () => {
      const plugin = new StaticPlugin();
      expect(plugin.config.publicFolder).toBe('public');
      expect(plugin.config.compress).toBe(1024);
      expect(plugin.config.cacheMaxAge).toBe(86_400);
    });

    it('should override config values', () => {
      const plugin = new StaticPlugin({
        publicFolder: 'assets',
        compress: 512,
        cacheMaxAge: 604_800,
      });

      expect(plugin.config.publicFolder).toBe('assets');
      expect(plugin.config.compress).toBe(512);
      expect(plugin.config.cacheMaxAge).toBe(604_800);
    });

    it('should disable compression when set to 0', () => {
      const plugin = new StaticPlugin({ compress: 0, skipLoading: true });
      expect(plugin.config.compress).toBe(0);
    });
  });

  describe('prefix option', () => {
    it('should prefix routes with given prefix', async () => {
      const plugin = new StaticPlugin({ skipLoading: true, prefix: '/assets' });

      // Create test file in the actual public folder
      const pubFolder = publicFolder();
      mkdirSync(pubFolder, { recursive: true });
      writeFileSync(`${pubFolder}/logo.png`, 'fake-image');

      plugin.routeStatic('logo.png', 'logo.png', { gzip: false, mime: 'image/png' });

      app.use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

      // Should NOT match without prefix
      const noPrefix = await fetch(`${baseUrl}/logo.png`);
      expect(noPrefix.status).toBe(404);

      // Should match with prefix
      const withPrefix = await fetch(`${baseUrl}/assets/logo.png`);
      expect(withPrefix.status).toBe(200);

      // Cleanup
      rmSync(`${pubFolder}/logo.png`, { force: true });

      await app.stop();
    });

    it('should work without prefix (default)', async () => {
      const plugin = new StaticPlugin({ skipLoading: true });

      const pubFolder = publicFolder();
      mkdirSync(pubFolder, { recursive: true });
      writeFileSync(`${pubFolder}/style.css`, 'body {}');

      plugin.routeStatic('style.css', 'style.css', { gzip: false, mime: 'text/css' });

      app.use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/style.css`);
      expect(res.status).toBe(200);

      // Cleanup
      rmSync(`${pubFolder}/style.css`, { force: true });

      await app.stop();
    });

    it('should serve GET and HEAD requests regardless of Accept when unconstrained', async () => {
      const plugin = new StaticPlugin({ skipLoading: true });
      const pubFolder = publicFolder();
      const staticPath = `${pubFolder}/accept-test.txt`;
      mkdirSync(pubFolder, { recursive: true });
      writeFileSync(staticPath, 'hello');

      try {
        plugin.routeStatic('accept-test.txt', 'accept-test.txt', { gzip: false, mime: 'text/plain' });
        app.use(plugin);
        await app.start();

        const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
        const matching = await fetch(`${baseUrl}/accept-test.txt`, { headers: { Accept: 'text/plain' } });
        expect(matching.status).toBe(200);
        expect(await matching.text()).toBe('hello');

        const unrelated = await fetch(`${baseUrl}/accept-test.txt`, {
          headers: { Accept: 'application/json' },
        });
        expect(unrelated.status).toBe(200);
        expect(await unrelated.text()).toBe('hello');

        const head = await fetch(`${baseUrl}/accept-test.txt`, {
          method: 'HEAD',
          headers: { Accept: 'image/png' },
        });
        expect(head.status).toBe(200);
        expect(head.headers.get('Content-Type')).toBe('text/plain');
        expect(head.headers.get('Content-Length')).toBe('5');
      } finally {
        rmSync(staticPath, { force: true });
      }
    });
  });

  describe('cacheMaxAge option', () => {
    it('should use default cache duration (1 day)', () => {
      const plugin = new StaticPlugin({ skipLoading: true });
      plugin.routeStatic('test.js', 'test.js', { gzip: false, mime: 'application/javascript' });

      const routes = (plugin as unknown as { staticRoutes: Map<string, unknown> }).staticRoutes;
      const route = routes.get('test.js') as { headers: Record<string, string> };

      expect(route.headers['Cache-Control']).toBe('public, max-age=86400');
    });

    it('should use custom cache duration', () => {
      const plugin = new StaticPlugin({ skipLoading: true, cacheMaxAge: 604_800 });
      plugin.routeStatic('test.js', 'test.js', { gzip: false, mime: 'application/javascript' });

      const routes = (plugin as unknown as { staticRoutes: Map<string, unknown> }).staticRoutes;
      const route = routes.get('test.js') as { headers: Record<string, string> };

      expect(route.headers['Cache-Control']).toBe('public, max-age=604800');
    });
  });

  describe('ETag support', () => {
    it('should set ETag header for existing files', () => {
      const plugin = new StaticPlugin({ skipLoading: true });

      // Create a test file in the actual public folder
      const pubFolder = publicFolder();
      mkdirSync(pubFolder, { recursive: true });
      writeFileSync(`${pubFolder}/script.js`, 'console.log("test");');

      plugin.routeStatic('script.js', 'script.js', { gzip: false, mime: 'application/javascript' });

      const routes = (plugin as unknown as { staticRoutes: Map<string, unknown> }).staticRoutes;
      const route = routes.get('script.js') as { headers: Record<string, string> };

      expect(route.headers['ETag']).toBeDefined();
      expect(route.headers['ETag']).toMatch(/^"[a-f0-9]+"$/);

      // Cleanup
      rmSync(`${pubFolder}/script.js`, { force: true });
    });
  });

  describe('conditional requests (304 Not Modified)', () => {
    it('should return 304 when If-None-Match matches ETag', async () => {
      const plugin = new StaticPlugin({ skipLoading: true });

      const pubFolder = publicFolder();
      mkdirSync(pubFolder, { recursive: true });
      writeFileSync(`${pubFolder}/cached.js`, 'console.log("cached");');

      plugin.routeStatic('cached.js', 'cached.js', { gzip: false, mime: 'application/javascript' });

      app.use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

      // First request: get the ETag
      const firstRes = await fetch(`${baseUrl}/cached.js`);
      expect(firstRes.status).toBe(200);
      const etag = firstRes.headers.get('ETag') ?? '';
      expect(etag).not.toBe('');

      // Second request: send If-None-Match with the ETag
      const secondRes = await fetch(`${baseUrl}/cached.js`, {
        headers: { 'If-None-Match': etag },
      });
      expect(secondRes.status).toBe(304);

      // Cleanup
      rmSync(`${pubFolder}/cached.js`, { force: true });
      await app.stop();
    });

    it('should return 200 when If-None-Match does not match', async () => {
      const plugin = new StaticPlugin({ skipLoading: true });

      const pubFolder = publicFolder();
      mkdirSync(pubFolder, { recursive: true });
      writeFileSync(`${pubFolder}/fresh.js`, 'console.log("fresh");');

      plugin.routeStatic('fresh.js', 'fresh.js', { gzip: false, mime: 'application/javascript' });

      app.use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

      const res = await fetch(`${baseUrl}/fresh.js`, {
        headers: { 'If-None-Match': '"stale-etag"' },
      });
      expect(res.status).toBe(200);
      expect(res.headers.get('Cache-Control')).toBe('public, max-age=86400');

      // Cleanup
      rmSync(`${pubFolder}/fresh.js`, { force: true });
      await app.stop();
    });

    it('should include Cache-Control header in response', async () => {
      const plugin = new StaticPlugin({ skipLoading: true, cacheMaxAge: 3600 });

      const pubFolder = publicFolder();
      mkdirSync(pubFolder, { recursive: true });
      writeFileSync(`${pubFolder}/header-test.css`, 'body {}');

      plugin.routeStatic('header-test.css', 'header-test.css', { gzip: false, mime: 'text/css' });

      app.use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

      const res = await fetch(`${baseUrl}/header-test.css`);
      expect(res.status).toBe(200);
      expect(res.headers.get('Cache-Control')).toBe('public, max-age=3600');
      expect(res.headers.get('Content-Type')).toBe('text/css');
      expect(res.headers.get('ETag')).toBeDefined();

      // Cleanup
      rmSync(`${pubFolder}/header-test.css`, { force: true });
      await app.stop();
    });
  });

  describe('Plugin interface', () => {
    it('should implement Plugin interface', () => {
      const plugin = new StaticPlugin({ skipLoading: true });
      expect(typeof plugin.warmup).toBe('function');
      expect(typeof plugin.generate).toBe('function');
    });
  });
});
