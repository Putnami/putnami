import { afterAll, describe, expect, it } from 'bun:test';
import { mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, relative } from 'node:path';
import type { GenerateResult } from '@putnami/application';
import { ConflictingDeliveryError, buildEventsInfraManifestFromSubscriptions } from '../../src/infra/requirements';

/**
 * A workload with several events() plugins: one with explicit handlers only and
 * two that each scan their own handler folder.
 *
 * Every scanning plugin used to export `events-loader`. The build keeps the last
 * export under a key, so the packaged binary registered one folder's loader and
 * every plugin, the explicit-only one included, subscribed that folder's
 * handlers. The plugins also each rewrote the project's infra sidecar, so the
 * last writer dropped the other folder's topics from provisioning.
 */

const EVENTS_ROOT = join(import.meta.dir, '..', '..');
const APPLICATION_ROOT = join(EVENTS_ROOT, '..', 'application');
const PACKAGED_TIMEOUT_MS = 60_000;
const REPORT_MARKER = 'events-loader-slots-report:';
const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
const temporaryRoots: string[] = [];

const MAIN = `import { resolve } from 'node:path';
import { application } from '@putnami/application';
import { events } from '@putnami/events';

export const app = () =>
  application()
    // Explicit handlers only: owns no generated loader.
    .use(events({ autoScan: false }))
    .use(events({ scanPath: resolve(import.meta.dir, 'orders') }))
    .use(events({ scanPath: resolve(import.meta.dir, 'billing') }));
`;

function handlerModule(topicName: string): string {
  return [
    "import { handler, topic } from '@putnami/events';",
    `export const Subject = topic(${JSON.stringify(topicName)}, { value: String });`,
    'export default handler(Subject).handle(async () => {});',
    '',
  ].join('\n');
}

afterAll(() => {
  if (originalProjectRoot === undefined) delete process.env.PUTNAMI_PROJECT_ROOT;
  else process.env['PUTNAMI_PROJECT_ROOT'] = originalProjectRoot;
  for (const root of temporaryRoots.splice(0)) rmSync(root, { recursive: true, force: true });
});

function temporaryRoot(prefix: string): string {
  const root = realpathSync(mkdtempSync(join(tmpdir(), prefix)));
  temporaryRoots.push(root);
  return root;
}

interface BuiltWorkload {
  projectRoot: string;
  mainPath: string;
  loaders: Record<string, string>;
}

let built: Promise<BuiltWorkload> | undefined;

function builtWorkload(): Promise<BuiltWorkload> {
  built ??= (async () => {
    const projectRoot = temporaryRoot('putnami-events-loader-slots-');
    const mainPath = join(projectRoot, 'src', 'main.ts');
    mkdirSync(join(projectRoot, 'node_modules', '@putnami'), { recursive: true });
    symlinkSync(EVENTS_ROOT, join(projectRoot, 'node_modules', '@putnami', 'events'), 'dir');
    symlinkSync(APPLICATION_ROOT, join(projectRoot, 'node_modules', '@putnami', 'application'), 'dir');
    mkdirSync(join(projectRoot, 'src', 'orders'), { recursive: true });
    mkdirSync(join(projectRoot, 'src', 'billing'), { recursive: true });
    writeFileSync(mainPath, MAIN);
    writeFileSync(join(projectRoot, 'src', 'orders', 'created.on.ts'), handlerModule('orders.created'));
    writeFileSync(join(projectRoot, 'src', 'billing', 'paid.on.ts'), handlerModule('billing.paid'));

    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;
    const { app } = (await import(mainPath)) as {
      app: () => { build(options?: object): Promise<GenerateResult> };
    };
    const result = await app().build({ publishCapabilityManifest: false, publishDesignGraph: false });
    const loaders = Object.fromEntries(
      Object.entries(result.exports ?? {})
        .filter(([key]) => key.endsWith('-loader') && !key.endsWith('client-loader'))
        .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)),
    );
    return { projectRoot, mainPath, loaders };
  })();
  return built;
}

/**
 * The packaged serve entrypoint (GenerateBundledServe): the app, then one static
 * import and one registration per loader key. Instead of starting transports it
 * loads each plugin's discovered handlers and reports their topics.
 */
async function packagedHandlerTopics(): Promise<string[][]> {
  const { projectRoot, mainPath, loaders } = await builtWorkload();
  const entryDir = join(projectRoot, '.gen', 'src');
  const entry = join(entryDir, 'serve.bundled.ts');
  const importPath = (path: string) => {
    const withoutExtension = relative(entryDir, path).replace(/\.ts$/, '');
    return withoutExtension.startsWith('.') ? withoutExtension : `./${withoutExtension}`;
  };
  const entries = Object.entries(loaders);
  writeFileSync(
    entry,
    [
      `import { registerModuleLoader } from '@putnami/application';`,
      `import { EventsPlugin } from '@putnami/events';`,
      `import { app } from ${JSON.stringify(importPath(mainPath))};`,
      ...entries.map(([, path], index) => `import * as loaderModule${index} from ${JSON.stringify(importPath(path))};`),
      ...entries.map(
        ([key], index) => `registerModuleLoader(${JSON.stringify(key)}, () => Promise.resolve(loaderModule${index}));`,
      ),
      'const workload = app();',
      'const report: string[][] = [];',
      'for (const { plugin } of workload.collectPlugins()) {',
      '  if (!(plugin instanceof EventsPlugin)) continue;',
      '  const internals = plugin as unknown as {',
      '    loadDiscoveredHandlers(app: unknown, logger: unknown): Promise<void>;',
      '    handlers: Array<{ topic: { name: string } }>;',
      '  };',
      '  await internals.loadDiscoveredHandlers(workload, { debug() {} });',
      '  report.push(internals.handlers.map((definition) => definition.topic.name).sort());',
      '}',
      `console.log(${JSON.stringify(REPORT_MARKER)} + JSON.stringify(report));`,
      '',
    ].join('\n'),
  );

  const packagedRoot = temporaryRoot('putnami-events-loader-slots-packaged-');
  const build = await Bun.build({ entrypoints: [entry], outdir: packagedRoot, target: 'bun', naming: 'server.js' });
  if (!build.success) throw new Error(build.logs.map((log) => log.message).join('\n'));
  const output = build.outputs.find((artifact) => artifact.kind === 'entry-point');
  if (!output) throw new Error('packaged entrypoint output was not emitted');
  const child = Bun.spawn([process.execPath, output.path], {
    cwd: packagedRoot,
    env: { ...process.env, PWD: packagedRoot, PUTNAMI_PROJECT_ROOT: packagedRoot, NODE_ENV: 'test' },
    stdout: 'pipe',
    stderr: 'pipe',
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (exitCode !== 0) throw new Error(`packaged workload exited ${exitCode}:\n${stderr}`);
  const line = stdout.split('\n').find((candidate) => candidate.startsWith(REPORT_MARKER));
  if (!line) throw new Error(`packaged workload printed no report:\n${stdout}\n${stderr}`);
  return JSON.parse(line.slice(REPORT_MARKER.length)) as string[][];
}

describe('events() handler loaders in a packaged workload', () => {
  it(
    'every scanning events() plugin exports its own handler loader',
    async () => {
      const { projectRoot, loaders } = await builtWorkload();
      // The explicit-only plugin takes no slot; the others follow registration order.
      expect(loaders).toEqual({
        'events-1-loader': join(projectRoot, '.gen', 'src', 'billing', '.events-application.gen.ts'),
        'events-loader': join(projectRoot, '.gen', 'src', 'orders', '.events-application.gen.ts'),
      });
    },
    PACKAGED_TIMEOUT_MS,
  );

  it(
    'writes one infra sidecar carrying the topics of every scan folder',
    async () => {
      const { projectRoot } = await builtWorkload();
      const sidecar = JSON.parse(readFileSync(join(projectRoot, '.gen', 'infra', 'events.json'), 'utf8'));
      expect(sidecar.events).toEqual({ publishes: [], subscribes: ['billing.paid', 'orders.created'] });
    },
    PACKAGED_TIMEOUT_MS,
  );

  it(
    'each plugin loads only its own handlers in the packaged entrypoint',
    async () => {
      // Registration order: explicit-only, orders, billing.
      expect(await packagedHandlerTopics()).toEqual([[], ['orders.created'], ['billing.paid']]);
    },
    PACKAGED_TIMEOUT_MS,
  );
});

describe('events infra manifest from several plugins', () => {
  it('keeps each subscription delivery and the bare form for pull', () => {
    const manifest = buildEventsInfraManifestFromSubscriptions(
      ['audit.logged'],
      [
        { topic: 'orders.created', delivery: 'pull' },
        { topic: 'billing.paid', delivery: 'push' },
        { topic: 'orders.created', delivery: 'pull' },
      ],
    );
    expect(manifest?.events).toEqual({
      publishes: ['audit.logged'],
      subscribes: [{ topic: 'billing.paid', delivery: 'push' }, 'orders.created'],
    });
  });

  it('refuses one topic subscribed with two deliveries', () => {
    expect(() =>
      buildEventsInfraManifestFromSubscriptions(
        [],
        [
          { topic: 'orders.created', delivery: 'pull' },
          { topic: 'orders.created', delivery: 'push' },
        ],
      ),
    ).toThrow(ConflictingDeliveryError);
  });
});
