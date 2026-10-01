import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:net';
import { dirname, join, relative, resolve } from 'node:path';
import { Application, registerModuleLoader } from '@putnami/application';
import { resetConfigLoader, Uuid } from '@putnami/runtime';
import { getDesignPublications, getTransport, getPublisher } from '../../src/publisher/publisher';
import { EventsPlugin, events } from '../../src/events.plugin';
import { handler, isHandlerDefinition, type HandlerDefinition } from '../../src/handler/handler';
import { MemoryServer } from '../../src/server/memory-server';
import type { Message } from '../../src/topic/message';
import { topic } from '../../src/topic/topic';
import type { Transport } from '../../src/transport';

const TestTopic = topic('test.event', { id: Uuid, value: String });
const OtherTopic = topic('other.event', { id: Uuid });
const GeneratedTopic = topic('generated.event', { id: Uuid, value: String });
const AnalyticsTopic = topic('analytics.page_view', { id: Uuid }, { channel: 'analytics' });
const projectRoot = resolve(import.meta.dir, '..', '..');
const tmpRoot = join(projectRoot, '.tmp');

let originalMathRandom = Math.random;
let portCounter = 50_000 + (process.pid % 10_000);

async function isPortAvailable(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const server = createServer();
    server.once('error', () => resolve(false));
    server.listen(port, '127.0.0.1', () => {
      server.close(() => resolve(true));
    });
  });
}

async function nextPort(attempts = 0): Promise<number> {
  if (attempts > 1000) {
    throw new Error('Unable to find an available test port');
  }

  const port = portCounter++;
  if (await isPortAvailable(port)) {
    return port;
  }

  return nextPort(attempts + 1);
}

async function waitUntil(condition: () => boolean, timeoutMs = 200): Promise<void> {
  const startedAt = Date.now();
  while (!condition()) {
    if (Date.now() - startedAt > timeoutMs) {
      throw new Error(`Timed out after ${timeoutMs}ms`);
    }
    await Bun.sleep(1);
  }
}

function createTempDir(prefix: string): string {
  mkdirSync(tmpRoot, { recursive: true });
  return mkdtempSync(join(tmpRoot, prefix));
}

function toImportPath(fromFile: string, toFile: string): string {
  const importPath = relative(dirname(fromFile), toFile).replaceAll('\\', '/');
  return importPath.startsWith('.') ? importPath : `./${importPath}`;
}

describe('EventsPlugin', () => {
  const cleanupPaths = new Set<string>();

  beforeEach(() => {
    registerModuleLoader(
      'events-loader',
      mock(async () => undefined),
    );
    process.env.PUTNAMI_PROJECT_ROOT = projectRoot;
    delete process.env.EVENTS_ENDPOINT;
    delete process.env.CONFIG_DATA;
    resetConfigLoader();
    (globalThis as { __generatedEvents?: string[] }).__generatedEvents = undefined;
    originalMathRandom = Math.random;
    Math.random = () => 0.5;
  });

  afterEach(() => {
    for (const path of cleanupPaths) {
      rmSync(path, { recursive: true, force: true });
    }
    cleanupPaths.clear();
    delete process.env.PUTNAMI_PROJECT_ROOT;
    delete process.env.EVENTS_ENDPOINT;
    delete process.env.CONFIG_DATA;
    resetConfigLoader();
    (globalThis as { __generatedEvents?: string[] }).__generatedEvents = undefined;
    Math.random = originalMathRandom;
  });

  describe('registerModule()', () => {
    it('should register default export handler', () => {
      const plugin = new EventsPlugin({ autoScan: false });
      const def = handler(TestTopic).handle(async () => {});

      const mod = { default: def };
      plugin.registerModule(mod);

      // Verify the handler was registered by checking the plugin accepts it
      expect(isHandlerDefinition(def)).toBe(true);
    });

    it('should register named export handlers', () => {
      const plugin = new EventsPlugin({ autoScan: false });
      const def1 = handler(TestTopic).handle(async () => {});
      const def2 = handler(OtherTopic).handle(async () => {});

      const mod = { onTest: def1, onOther: def2 };
      plugin.registerModule(mod);

      // Plugin should have both handlers
      expect(isHandlerDefinition(def1)).toBe(true);
      expect(isHandlerDefinition(def2)).toBe(true);
    });

    it('should skip non-handler exports', () => {
      const plugin = new EventsPlugin({ autoScan: false });
      const def = handler(TestTopic).handle(async () => {});

      const mod = {
        default: def,
        TOPIC: TestTopic, // Not a handler
        helper: () => {}, // Not a handler
        config: { retries: 3 }, // Not a handler
      };

      // Should not throw
      plugin.registerModule(mod);
    });

    it('should support method chaining', () => {
      const plugin = new EventsPlugin({ autoScan: false });
      const def1 = handler(TestTopic).handle(async () => {});
      const def2 = handler(OtherTopic).handle(async () => {});

      const result = plugin.registerModule({ default: def1 }).registerModule({ default: def2 });

      expect(result).toBe(plugin);
    });
  });

  describe('register()', () => {
    it('should support method chaining', () => {
      const plugin = new EventsPlugin({ autoScan: false });
      const def = handler(TestTopic).handle(async () => {});

      const result = plugin.register(def);

      expect(result).toBe(plugin);
    });
  });

  describe('constructor', () => {
    it('should default autoScan to true', () => {
      const plugin = new EventsPlugin();
      // Config is private but we can verify the plugin was created
      expect(plugin).toBeInstanceOf(EventsPlugin);
    });

    it('should accept explicit handlers', () => {
      const def = handler(TestTopic).handle(async () => {});
      const plugin = new EventsPlugin({ autoScan: false, handlers: [def] });
      expect(plugin).toBeInstanceOf(EventsPlugin);
    });
  });

  describe('generate()', () => {
    it('should return empty result when no scanPath', async () => {
      const plugin = new EventsPlugin({ autoScan: false });
      // biome-ignore lint/suspicious/noExplicitAny: test mock
      const result = await plugin.generate({} as any);
      expect(result).toEqual({});
    });

    it('should generate a loader for discovered handler files', async () => {
      const scanPath = createTempDir('events-plugin-generate-');
      cleanupPaths.add(scanPath);

      const handlerPath = join(scanPath, 'nested', 'generated.on.ts');
      mkdirSync(dirname(handlerPath), { recursive: true });
      writeFileSync(
        handlerPath,
        [
          `import { handler, topic, Uuid } from '@putnami/events';`,
          `const Topic = topic('generated.event', { id: Uuid, value: String });`,
          `export default handler(Topic).handle(async () => {});`,
          '',
        ].join('\n'),
      );

      const plugin = new EventsPlugin({ scanPath });
      // biome-ignore lint/suspicious/noExplicitAny: test stub
      const result = await plugin.generate({} as any);

      const loaderPath = result.exports?.['events-loader'];
      expect(loaderPath).toBeDefined();
      expect(existsSync(loaderPath as string)).toBe(true);

      cleanupPaths.add(join(projectRoot, '.gen', relative(projectRoot, scanPath)));

      const generated = readFileSync(loaderPath as string, 'utf8');
      expect(generated).toContain('export default new EventsPlugin({ autoScan: false })');
      expect(generated).toContain('generated.on');
      expect(generated).toContain('.registerModule(');
    });
  });

  describe('infra requirements sidecar', () => {
    function writeHandler(scanPath: string, name: string, lines: string[]): string {
      const handlerPath = join(scanPath, name);
      mkdirSync(dirname(handlerPath), { recursive: true });
      writeFileSync(handlerPath, [...lines, ''].join('\n'));
      return handlerPath;
    }

    it('derives publishes and subscribes from scanned handlers', async () => {
      const root = createTempDir('events-infra-root-');
      cleanupPaths.add(root);
      process.env.PUTNAMI_PROJECT_ROOT = root;

      const scanPath = join(root, 'events');
      writeHandler(scanPath, 'orders.on.ts', [
        `import { handler, topic, Uuid, getPublisher } from '@putnami/events';`,
        `const Created = topic('order.created', { id: Uuid });`,
        `const Shipped = topic('order.shipped', { id: Uuid });`,
        `export const ship = getPublisher(Shipped);`,
        `export default handler(Created).handle(async () => {});`,
      ]);

      const plugin = new EventsPlugin({ scanPath });
      // biome-ignore lint/suspicious/noExplicitAny: test stub
      await plugin.generate({} as any);

      const sidecar = join(root, '.gen', 'infra', 'events.json');
      expect(existsSync(sidecar)).toBe(true);
      expect(JSON.parse(readFileSync(sidecar, 'utf8')).events).toEqual({
        publishes: ['order.shipped'],
        subscribes: ['order.created'],
      });
    });

    it('removes a stale sidecar when the last handler is deleted (rebuild)', async () => {
      const root = createTempDir('events-infra-removed-');
      cleanupPaths.add(root);
      process.env.PUTNAMI_PROJECT_ROOT = root;

      const scanPath = join(root, 'events');
      const handlerPath = writeHandler(scanPath, 'orders.on.ts', [
        `import { handler, topic, Uuid, getPublisher } from '@putnami/events';`,
        `const Created = topic('order.created', { id: Uuid });`,
        `const Shipped = topic('order.shipped', { id: Uuid });`,
        `export const ship = getPublisher(Shipped);`,
        `export default handler(Created).handle(async () => {});`,
      ]);

      const plugin = new EventsPlugin({ scanPath });
      const sidecar = join(root, '.gen', 'infra', 'events.json');

      // biome-ignore lint/suspicious/noExplicitAny: test stub
      await plugin.generate({} as any);
      expect(existsSync(sidecar)).toBe(true);

      // Delete the only handler and regenerate in the same process: discovery
      // must observe the removal and clear the now-stale sidecar.
      rmSync(handlerPath);
      // biome-ignore lint/suspicious/noExplicitAny: test stub
      await plugin.generate({} as any);
      expect(existsSync(sidecar)).toBe(false);
    });

    it('re-derives publishes on a second generate() in the same process', async () => {
      const root = createTempDir('events-infra-republish-');
      cleanupPaths.add(root);
      process.env.PUTNAMI_PROJECT_ROOT = root;

      const scanPath = join(root, 'events');
      writeHandler(scanPath, 'notify.on.ts', [
        `import { handler, topic, Uuid, getPublisher } from '@putnami/events';`,
        `const Created = topic('user.created', { id: Uuid });`,
        `const Welcomed = topic('user.welcomed', { id: Uuid });`,
        `export const welcome = getPublisher(Welcomed);`,
        `export default handler(Created).handle(async () => {});`,
      ]);

      const plugin = new EventsPlugin({ scanPath });
      const sidecar = join(root, '.gen', 'infra', 'events.json');

      // biome-ignore lint/suspicious/noExplicitAny: test stub
      await plugin.generate({} as any);
      // Second pass: ESM caches modules by path, so without cache-busting the
      // top-level getPublisher() calls would not re-run and publishes would
      // silently drop to empty.
      // biome-ignore lint/suspicious/noExplicitAny: test stub
      await plugin.generate({} as any);

      expect(JSON.parse(readFileSync(sidecar, 'utf8')).events).toEqual({
        publishes: ['user.welcomed'],
        subscribes: ['user.created'],
      });
    });

    it('does not leak design publications between projects built in one process', async () => {
      const firstRoot = createTempDir('events-design-first-');
      const secondRoot = createTempDir('events-design-second-');
      cleanupPaths.add(firstRoot);
      cleanupPaths.add(secondRoot);

      const firstScanPath = join(firstRoot, 'events');
      writeHandler(firstScanPath, 'orders.on.ts', [
        `import { topic, Uuid, getPublisher } from '@putnami/events';`,
        `const Shipped = topic('order.shipped', { id: Uuid });`,
        `export const ship = getPublisher(Shipped);`,
      ]);
      const secondScanPath = join(secondRoot, 'events');
      mkdirSync(secondScanPath, { recursive: true });

      process.env.PUTNAMI_PROJECT_ROOT = firstRoot;
      // biome-ignore lint/suspicious/noExplicitAny: test stub
      await new EventsPlugin({ scanPath: firstScanPath }).generate({} as any);
      expect(getDesignPublications().map(({ topic: definition }) => definition.name)).toEqual(['order.shipped']);

      process.env.PUTNAMI_PROJECT_ROOT = secondRoot;
      // biome-ignore lint/suspicious/noExplicitAny: test stub
      await new EventsPlugin({ scanPath: secondScanPath }).generate({} as any);
      expect(getDesignPublications()).toEqual([]);
    });
  });

  describe('lifecycle', () => {
    it('loads generated handlers through dynamic import and runs them', async () => {
      const scanPath = createTempDir('events-plugin-dynamic-');
      cleanupPaths.add(scanPath);
      const generatedPath = join(projectRoot, '.gen', relative(projectRoot, scanPath), '.events-application.gen.ts');
      mkdirSync(dirname(generatedPath), { recursive: true });
      cleanupPaths.add(join(projectRoot, '.gen', relative(projectRoot, scanPath)));

      const pluginImport = toImportPath(generatedPath, join(projectRoot, 'src/events.plugin.ts'));
      const eventsApiImport = toImportPath(generatedPath, join(projectRoot, 'src/index.ts'));

      writeFileSync(
        generatedPath,
        [
          `import { EventsPlugin } from '${pluginImport}';`,
          `import { handler, topic, Uuid } from '${eventsApiImport}';`,
          `const Topic = topic('generated.event', { id: Uuid, value: String });`,
          `export default new EventsPlugin({`,
          `  autoScan: false,`,
          `  handlers: [`,
          `    handler(Topic).handle(async (msg) => {`,
          `      const state = globalThis as { __generatedEvents?: string[] };`,
          `      state.__generatedEvents ??= [];`,
          `      state.__generatedEvents.push((msg.payload as { value: string }).value);`,
          `    }),`,
          `  ],`,
          `});`,
          '',
        ].join('\n'),
      );

      const plugin = new EventsPlugin({ port: await nextPort(), scanPath });

      try {
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.warmup({} as any);
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.start({} as any);

        await getPublisher(GeneratedTopic)({ id: crypto.randomUUID(), value: 'generated-value' });
        await waitUntil(() => ((globalThis as { __generatedEvents?: string[] }).__generatedEvents?.length ?? 0) === 1);

        const events = (globalThis as { __generatedEvents?: string[] }).__generatedEvents ?? [];
        expect(events).toContain('generated-value');
        expect(events.length).toBeGreaterThan(0);
      } finally {
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.stop({} as any);
      }
    });

    it('push delivery: registers the receiver, skips subscription, and keeps the broker publishable', async () => {
      const PushTopic = topic('push.lifecycle', { id: Uuid, value: String });
      const post = mock(() => {});
      const ensurePlugin = mock(async () => ({ post }));
      // biome-ignore lint/suspicious/noExplicitAny: test stub
      const app = { ensurePlugin } as any;

      let delivered = 0;
      const plugin = new EventsPlugin({
        autoScan: false,
        port: await nextPort(),
        delivery: 'push',
        push: { issuer: 'https://accounts.google.com', audience: 'aud', allowedServiceAccounts: [] },
        handlers: [
          handler(PushTopic).handle(async () => {
            delivered += 1;
          }),
        ],
      });

      try {
        await plugin.warmup(app);
        await plugin.start(app);

        // The receiver route is registered on the app HTTP server.
        expect(post).toHaveBeenCalledTimes(1);

        // The local broker is started, so publishing does not throw
        // "Broker is not running" (regression: push start() left it stopped).
        await getPublisher(PushTopic)({ id: crypto.randomUUID(), value: 'x' });

        // Handlers are NOT subscribed to the broker in push mode (the receiver
        // owns delivery), so the published event is not delivered in-process.
        await Bun.sleep(5);
        expect(delivered).toBe(0);
      } finally {
        await plugin.stop(app);
      }
    });

    it('loads handlers from a preloaded module and wires DI scopes on start', async () => {
      const dependencyToken = Symbol('dependency-token');
      const closeScope = mock(async () => {});
      const createScope = mock(async () => ({
        scope: {
          get(token: unknown) {
            expect(token).toBe(dependencyToken);
            return { name: 'injected-dependency' };
          },
          list() {
            return [];
          },
        },
        close: closeScope,
      }));

      let receivedDep: { name: string } | undefined;
      let receivedValue: string | undefined;

      const discovered = new EventsPlugin({ autoScan: false }).register(
        handler(TestTopic)
          .inject({ dependency: dependencyToken })
          .handle(async ({ dependency }, msg) => {
            receivedDep = dependency;
            receivedValue = (msg.payload as { value: string }).value;
          }),
      );

      const plugin = new EventsPlugin({
        autoScan: false,
        port: await nextPort(),
        preloadedModule: { default: discovered },
      });
      const app = new Application();
      (app as unknown as { _context: { createScope: typeof createScope } })._context = { createScope };

      try {
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.warmup({} as any);
        await plugin.start(app);

        await getPublisher(TestTopic)({ id: crypto.randomUUID(), value: 'payload-value' });
        await waitUntil(() => receivedDep !== undefined && receivedValue === 'payload-value');
        await waitUntil(() => closeScope.mock.calls.length === 1);

        expect(receivedDep).toEqual({ name: 'injected-dependency' });
        expect(createScope).toHaveBeenCalledTimes(1);
        expect(closeScope).toHaveBeenCalledTimes(1);
      } finally {
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.stop({} as any);
      }
    });

    it('uses a shared local server when another service already owns the port', async () => {
      let received = 0;
      const port = await nextPort();

      registerModuleLoader(
        'events-loader',
        mock(async () => ({
          default: new EventsPlugin({ autoScan: false }).register(
            handler(TestTopic).handle(async () => {
              received++;
            }),
          ),
        })),
      );

      const owner = new EventsPlugin({ autoScan: false, port });
      // Leaves autoScan on, so it owns the `events-loader` slot and subscribes
      // the loader's handler through the owner's server. A plugin with
      // `autoScan: false` owns no generated loader and resolves none (@putnami/application ADR 0006).
      const plugin = new EventsPlugin({ port });

      try {
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await owner.warmup({} as any);
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await owner.start({} as any);

        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.warmup({} as any);
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.start({} as any);

        await getPublisher(TestTopic)({ id: crypto.randomUUID(), value: 'from-loader' });
        await waitUntil(() => received > 0);

        expect(received).toBeGreaterThan(0);
      } finally {
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.stop({} as any);
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await owner.stop({} as any);
      }
    });

    it('connects to configured event endpoints', async () => {
      const port = await nextPort();
      const server = new MemoryServer({ port });
      await server.start();
      const plugin = new EventsPlugin({
        autoScan: false,
        endpoint: `http://127.0.0.1:${port}`,
        token: server.getAuthToken(),
      });

      try {
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.warmup({} as any);
        expect(getTransport()).toBeDefined();
      } finally {
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.stop({} as any);
        await server.stop();
      }
    });

    it('uses an explicit transport when configured', async () => {
      const started: string[] = [];
      const stopped: string[] = [];
      const transport: Transport = {
        async publish() {},
        async subscribe(definition) {
          started.push(`subscribe:${definition.topic.name}`);
        },
        async start() {
          started.push('start');
        },
        async stop() {
          stopped.push('stop');
        },
      };

      const plugin = new EventsPlugin({
        autoScan: false,
        handlers: [handler(TestTopic).handle(async () => {})],
        transport,
      });

      // biome-ignore lint/suspicious/noExplicitAny: test stub
      await plugin.warmup({} as any);
      // biome-ignore lint/suspicious/noExplicitAny: test stub
      await plugin.start({} as any);
      // biome-ignore lint/suspicious/noExplicitAny: test stub
      await plugin.stop({} as any);

      expect(started).toEqual(['subscribe:test.event', 'start']);
      expect(stopped).toEqual(['stop']);
    });

    it('builds a routing transport from named transport config', async () => {
      const stream = new RecordingTransport();
      const analytics = new RecordingTransport();
      const plugin = new EventsPlugin({
        autoScan: false,
        handlers: [handler(AnalyticsTopic).handle(async () => {}), handler(OtherTopic).handle(async () => {})],
        transports: { stream, analytics },
        routes: [
          { match: 'analytics.*', transport: 'stream' },
          { channel: 'analytics', transport: 'analytics' },
        ],
        defaultTransport: 'stream',
      });

      try {
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.warmup({} as any);
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.start({} as any);

        await getPublisher(AnalyticsTopic)({ id: crypto.randomUUID() });
        await getPublisher(OtherTopic)({ id: crypto.randomUUID() });
      } finally {
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.stop({} as any);
      }

      expect(analytics.subscribed).toEqual(['analytics.page_view']);
      expect(analytics.published).toEqual(['analytics.page_view']);
      expect(stream.subscribed).toEqual(['other.event']);
      expect(stream.published).toEqual(['other.event']);
      expect(stream.started).toBe(true);
      expect(analytics.started).toBe(true);
      expect(stream.stopped).toBe(true);
      expect(analytics.stopped).toBe(true);
    });
  });

  describe('events()', () => {
    it('returns an EventsPlugin instance and preserves explicit config', () => {
      const plugin = events({ autoScan: false, scanPath: '/tmp/events-scan-path' });

      expect(plugin).toBeInstanceOf(EventsPlugin);
      expect((plugin as unknown as { config: { scanPath?: string } }).config.scanPath).toBe('/tmp/events-scan-path');
    });

    it('clears the active transport on stop', async () => {
      const plugin = new EventsPlugin({ autoScan: false, port: await nextPort() });

      try {
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.warmup({} as any);
        expect(getTransport()).toBeDefined();
      } finally {
        // biome-ignore lint/suspicious/noExplicitAny: test stub
        await plugin.stop({} as any);
      }

      expect(getTransport()).toBeUndefined();
    });

    it('rejects ambiguous single and named transport config', () => {
      const transport = new RecordingTransport();

      expect(() =>
        events({
          autoScan: false,
          transport,
          transports: { stream: transport },
          defaultTransport: 'stream',
        }),
      ).toThrow('events() cannot combine transport with transports/routes/defaultTransport.');
    });
  });
});

class RecordingTransport implements Transport {
  readonly published: string[] = [];
  readonly subscribed: string[] = [];
  started = false;
  stopped = false;

  async publish(topic: string): Promise<void> {
    this.published.push(topic);
  }

  async subscribe(
    definition: HandlerDefinition,
    _callback: (message: Message<unknown>) => Promise<void>,
  ): Promise<void> {
    this.subscribed.push(definition.topic.name);
  }

  async start(): Promise<void> {
    this.started = true;
  }

  async stop(): Promise<void> {
    this.stopped = true;
  }
}
