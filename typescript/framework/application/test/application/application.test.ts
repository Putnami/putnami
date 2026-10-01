import { existsSync } from 'node:fs';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { beforeEach, describe, expect, it, mock, spyOn } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { type BundleOperation, type BundlePayload, computePayloadHash, type MigrationSource } from '@putnami/migration';
import { Application, application, composeModules, module, type Plugin } from '../../src/application';
import { SHUTDOWN_TIMEOUT_MS, installSignalHandlers } from '../../src/application/app-signals';
import {
  ContainerContext,
  RequirementNotMetError,
  named,
  resetDefaultLogger,
  setRootLogger,
  useLogger,
} from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import { restoreEnv } from '@putnami/utils';

describe('Application', () => {
  let app: Application;

  beforeEach(() => {
    app = application();
  });

  describe('use()', () => {
    it('should add a plugin', () => {
      const plugin: Plugin = {};
      app.use(plugin);
      expect(app.getPlugins()).toHaveLength(1);
      expect(app.getPlugins()[0]).toBe(plugin);
    });

    it('should chain multiple plugins', () => {
      const plugin1: Plugin = {};
      const plugin2: Plugin = {};
      app.use(plugin1).use(plugin2);
      expect(app.getPlugins()).toHaveLength(2);
    });

    it('should throw if plugin is null', () => {
      // biome-ignore lint/suspicious/noExplicitAny: Testing error case
      expect(() => app.use(null as any)).toThrow('Plugin or module not provided');
    });

    it('should throw if plugin is undefined', () => {
      // biome-ignore lint/suspicious/noExplicitAny: Testing error case
      expect(() => app.use(undefined as any)).toThrow('Plugin or module not provided');
    });
  });

  describe('getPlugin()', () => {
    class TestPlugin implements Plugin {
      warmup = mock(() => Promise.resolve());
    }

    it('should return registered plugin by type', () => {
      const plugin = new TestPlugin();
      app.use(plugin);
      const result = app.getPlugin(TestPlugin);
      expect(result).toBe(plugin);
    });

    it('should throw if plugin not found', () => {
      expect(() => app.getPlugin(TestPlugin)).toThrow('Plugin TestPlugin not found');
    });
  });

  describe('ensurePlugin()', () => {
    class TestPlugin implements Plugin {
      warmup = mock(() => Promise.resolve());
    }

    it('should create plugin if not registered', async () => {
      expect(app.getPlugins()).toHaveLength(0);
      const plugin = await app.ensurePlugin(TestPlugin);
      expect(plugin).toBeInstanceOf(TestPlugin);
      expect(app.getPlugins()).toHaveLength(1);
    });

    it('should warmup created plugin', async () => {
      const plugin = await app.ensurePlugin(TestPlugin);
      expect(plugin.warmup).toHaveBeenCalled();
    });

    it('should insert plugin at beginning', async () => {
      const existingPlugin: Plugin = {};
      app.use(existingPlugin);
      await app.ensurePlugin(TestPlugin);
      // Ensured plugin should be first
      expect(app.getPlugins()[0]).toBeInstanceOf(TestPlugin);
      expect(app.getPlugins()[1]).toBe(existingPlugin);
    });

    it('should return existing plugin without creating new one', async () => {
      const plugin = new TestPlugin();
      app.use(plugin);
      const result = await app.ensurePlugin(TestPlugin);
      expect(result).toBe(plugin);
      expect(app.getPlugins()).toHaveLength(1);
    });
  });

  describe('run()', () => {
    it('should register a runner', async () => {
      const runner = mock(() => Promise.resolve());
      app.run(runner);
      await app.start();
      expect(runner).toHaveBeenCalled();
      await app.stop();
    });
  });

  describe('onStop()', () => {
    it('should register shutdown hooks', async () => {
      const hook = mock(() => Promise.resolve());
      app.onStop(hook);
      await app.start();
      await app.stop();
      expect(hook).toHaveBeenCalled();
    });

    specTest(
      'should call hooks in reverse order',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'shutdown-order',
        check: 'shutdown-hooks-run-in-reverse-registration-order',
      },
      async () => {
        const order: number[] = [];
        app.onStop(async () => {
          order.push(1);
        });
        app.onStop(async () => {
          order.push(2);
        });
        await app.start();
        await app.stop();
        expect(order).toEqual([2, 1]);
      },
    );
  });

  describe('isRunning()', () => {
    it('should return false initially', () => {
      expect(app.isRunning()).toBe(false);
    });

    it('should return true after start', async () => {
      await app.start();
      expect(app.isRunning()).toBe(true);
      await app.stop();
    });

    it('should return false after stop', async () => {
      await app.start();
      await app.stop();
      expect(app.isRunning()).toBe(false);
    });
  });

  describe('build()', () => {
    specTest(
      'should call generate on all plugins',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'build-only-generation',
        check: 'build-runs-generate-on-every-plugin',
      },
      async () => {
        const plugin1: Plugin = { generate: mock(() => Promise.resolve({})) };
        const plugin2: Plugin = { generate: mock(() => Promise.resolve({})) };
        app.use(plugin1).use(plugin2);
        await app.build();
        expect(plugin1.generate).toHaveBeenCalledWith(app);
        expect(plugin2.generate).toHaveBeenCalledWith(app);
      },
    );

    specTest(
      'should skip plugins without generate',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'build-only-generation',
        check: 'a-plugin-without-generate-is-skipped',
      },
      async () => {
        const plugin: Plugin = {};
        app.use(plugin);
        await expect(app.build()).resolves.toBeDefined();
      },
    );

    specTest(
      'should run postGenerate only after every plugin generate() has completed',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'build-only-generation',
        check: 'post-generate-runs-only-after-the-generate-barrier',
      },
      async () => {
        const order: string[] = [];
        let specWritten = false;
        // generate() yields and delays — if postGenerate raced inside the parallel
        // generate() pass (the old bug), the reader below would observe specWritten=false.
        const writer: Plugin = {
          generate: async () => {
            await new Promise((resolve) => setTimeout(resolve, 5));
            specWritten = true;
            order.push('generate');
            return {};
          },
        };
        let observedSpec = false;
        const reader: Plugin = {
          postGenerate: async () => {
            observedSpec = specWritten;
            order.push('postGenerate');
            return {};
          },
        };

        app.use(writer).use(reader);
        await app.build();

        expect(order).toEqual(['generate', 'postGenerate']);
        expect(observedSpec).toBe(true);
      },
    );

    specTest(
      'should merge postGenerate assets and exports into the build result',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'build-only-generation',
        check: 'post-generate-assets-and-exports-reach-the-build-result',
      },
      async () => {
        const plugin: Plugin = {
          postGenerate: async () => ({
            assets: { 'clientgen/config.json': '/tmp/config.json' },
            exports: { 'client-loader': '/tmp/loader.ts' },
          }),
        };
        app.use(plugin);

        const result = await app.build();

        expect(result.assets?.['clientgen/config.json']).toBe('/tmp/config.json');
        expect(result.exports?.['client-loader']).toBe('/tmp/loader.ts');
      },
    );

    specTest(
      'should fail when migration artifact emission fails in a resolved project context',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'capability-publication',
        check: 'a-failed-migration-artifact-emission-fails-the-build',
      },
      async () => {
        const projectRoot = await mkdtemp(join(tmpdir(), 'putnami-app-build-'));
        const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
        const originalPwd = process.env['PWD'];

        try {
          await writeFile(join(projectRoot, 'package.json'), `${JSON.stringify({ name: 'migration-test-app' })}\n`);
          process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;
          process.env['PWD'] = projectRoot;

          const up = 'CREATE TABLE app_users (id uuid primary key);';
          const upPath = '../escape.sql';
          const op: BundleOperation = {
            kind: 'sql',
            target: 'default',
            namespace: 'app',
            name: 'app/0001_init',
            up: { path: upPath, hash: computePayloadHash(up) },
            safety: 'safe-online',
          };
          const source: MigrationSource & {
            migrationBundleOperations(): { operations: BundleOperation[]; payloads: BundlePayload[] };
          } = {
            kind: 'sql',
            namespace: 'app',
            migrationBundleOperations: () => ({ operations: [op], payloads: [{ path: upPath, bytes: up }] }),
          };
          const migrations: Plugin & { migrationSources(): MigrationSource[] } = {
            migrationSources: () => [source],
          };

          app.use(migrations);

          // Emission only runs for a build that carries the project identity
          // (the build-generate hook); a malformed emission then fails it.
          await expect(app.build({ projectName: 'migration-test-app' })).rejects.toThrow(/clean relative path/);
        } finally {
          restoreEnv('PUTNAMI_PROJECT_ROOT', originalProjectRoot);
          restoreEnv('PWD', originalPwd);
          await rm(projectRoot, { recursive: true, force: true });
        }
      },
    );

    specTest(
      'should key the migration bundle on the putnami project identity, not the npm package name',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'capability-publication',
        check: 'the-migration-bundle-keys-on-the-putnami-project-identity',
      },
      async () => {
        // The publisher matches the bundle's appName against the putnami
        // project (the Go describer keys on ctx.Project.Name). A workload whose
        // package.json name differs, e.g. a domain name, must still match.
        const projectRoot = await mkdtemp(join(tmpdir(), 'putnami-app-identity-'));
        const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
        const originalPwd = process.env['PWD'];

        try {
          await writeFile(join(projectRoot, 'package.json'), `${JSON.stringify({ name: 'auth.example.test' })}\n`);
          process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;
          process.env['PWD'] = projectRoot;

          const up = 'CREATE TABLE app_users (id uuid primary key);';
          const upPath = 'app/0001_init.sql';
          const op: BundleOperation = {
            kind: 'sql',
            target: 'default',
            namespace: 'app',
            name: 'app/0001_init',
            up: { path: upPath, hash: computePayloadHash(up) },
            safety: 'safe-online',
          };
          const source: MigrationSource & {
            migrationBundleOperations(): { operations: BundleOperation[]; payloads: BundlePayload[] };
          } = {
            kind: 'sql',
            namespace: 'app',
            migrationBundleOperations: () => ({ operations: [op], payloads: [{ path: upPath, bytes: up }] }),
          };
          app.use({ migrationSources: () => [source] } satisfies Plugin & { migrationSources(): MigrationSource[] });

          await app.build({ publishCapabilityManifest: false, projectName: 'identity/workloads/auth-server' });

          const bundle = await Bun.file(join(projectRoot, '.gen', 'migration-bundle', 'bundle.json')).json();
          expect(bundle.appName).toBe('identity/workloads/auth-server');
        } finally {
          restoreEnv('PUTNAMI_PROJECT_ROOT', originalProjectRoot);
          restoreEnv('PWD', originalPwd);
          await rm(projectRoot, { recursive: true, force: true });
        }
      },
    );

    specTest(
      'should leave the hook-written bundle untouched when build() runs without a project identity',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'capability-publication',
        check: 'a-build-without-a-project-identity-leaves-the-hook-bundle-untouched',
      },
      async () => {
        // The CI failure this pins: build~generate (the hook, which passes the
        // putnami project name) wrote the bundle; then test~test ran a workload
        // test that composes the same migration sources into an app and calls
        // build() with no projectName. The fallback to the npm package.json name
        // overwrote the manifest, and `publish-migration` refused it ("migration
        // bundle manifest identity does not match the selected application").
        // Only the hook is a writer: a build without an identity emits nothing.
        const projectRoot = await mkdtemp(join(tmpdir(), 'putnami-app-second-writer-'));
        const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
        const originalPwd = process.env['PWD'];

        try {
          await writeFile(join(projectRoot, 'package.json'), `${JSON.stringify({ name: 'auth.example.test' })}\n`);
          process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;
          process.env['PWD'] = projectRoot;

          const up = 'CREATE TABLE app_users (id uuid primary key);';
          const upPath = 'app/0001_init.sql';
          const op: BundleOperation = {
            kind: 'sql',
            target: 'default',
            namespace: 'app',
            name: 'app/0001_init',
            up: { path: upPath, hash: computePayloadHash(up) },
            safety: 'safe-online',
          };
          const source: MigrationSource & {
            migrationBundleOperations(): { operations: BundleOperation[]; payloads: BundlePayload[] };
          } = {
            kind: 'sql',
            namespace: 'app',
            migrationBundleOperations: () => ({ operations: [op], payloads: [{ path: upPath, bytes: up }] }),
          };
          const migrations = { migrationSources: () => [source] } satisfies Plugin & {
            migrationSources(): MigrationSource[];
          };
          const bundlePath = join(projectRoot, '.gen', 'migration-bundle', 'bundle.json');

          // 1. The build-generate hook: carries the putnami project identity.
          await app
            .use(migrations)
            .build({ publishCapabilityManifest: false, projectName: 'identity/workloads/auth-server' });
          expect((await Bun.file(bundlePath).json()).appName).toBe('identity/workloads/auth-server');

          // 2. A workload test: same sources, no identity.
          await application().use(migrations).build({ publishCapabilityManifest: false });

          expect((await Bun.file(bundlePath).json()).appName).toBe('identity/workloads/auth-server');
          // 3. Without a bundle on disk, the identity-less build writes none.
          await rm(join(projectRoot, '.gen'), { recursive: true, force: true });
          await application().use(migrations).build({ publishCapabilityManifest: false });
          expect(existsSync(join(projectRoot, '.gen', 'migration-bundle'))).toBe(false);
          expect(existsSync(join(projectRoot, '.gen', 'infra', 'migration.json'))).toBe(false);
        } finally {
          restoreEnv('PUTNAMI_PROJECT_ROOT', originalProjectRoot);
          restoreEnv('PWD', originalPwd);
          await rm(projectRoot, { recursive: true, force: true });
        }
      },
    );

    specTest(
      'should emit no migration artifacts when publication is disabled',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'capability-publication',
        check: 'migration-artifact-publication-can-be-disabled',
      },
      async () => {
        // The config-extract hook carries the project identity (the config
        // schema keys on it) but is not a writer of the migration artifacts:
        // build-generate owns them, exactly like the capability manifest.
        const projectRoot = await mkdtemp(join(tmpdir(), 'putnami-app-migration-off-'));
        const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
        const originalPwd = process.env['PWD'];

        try {
          await writeFile(join(projectRoot, 'package.json'), `${JSON.stringify({ name: 'auth.example.test' })}\n`);
          process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;
          process.env['PWD'] = projectRoot;

          const up = 'CREATE TABLE app_users (id uuid primary key);';
          const upPath = 'app/0001_init.sql';
          const op: BundleOperation = {
            kind: 'sql',
            target: 'default',
            namespace: 'app',
            name: 'app/0001_init',
            up: { path: upPath, hash: computePayloadHash(up) },
            safety: 'safe-online',
          };
          const source: MigrationSource & {
            migrationBundleOperations(): { operations: BundleOperation[]; payloads: BundlePayload[] };
          } = {
            kind: 'sql',
            namespace: 'app',
            migrationBundleOperations: () => ({ operations: [op], payloads: [{ path: upPath, bytes: up }] }),
          };
          app.use({ migrationSources: () => [source] } satisfies Plugin & { migrationSources(): MigrationSource[] });

          await app.build({
            publishCapabilityManifest: false,
            projectName: 'identity/workloads/auth-server',
            publishMigrationArtifacts: false,
          });

          expect(existsSync(join(projectRoot, '.gen', 'migration-bundle'))).toBe(false);
          expect(existsSync(join(projectRoot, '.gen', 'infra', 'migration.json'))).toBe(false);
        } finally {
          restoreEnv('PUTNAMI_PROJECT_ROOT', originalProjectRoot);
          restoreEnv('PWD', originalPwd);
          await rm(projectRoot, { recursive: true, force: true });
        }
      },
    );

    // Last in this block on purpose: it redirects the resolved project root, and
    // the case above already opened that window. Adding a second, earlier
    // redirect would move global config and project resolution for every later
    // case in the process.
    specTest(
      'should drop a stale capability manifest when generation fails',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'capability-publication',
        check: 'a-failed-generation-leaves-no-stale-capability-manifest',
      },
      async () => {
        // The manifest is invalidated BEFORE generate and written AFTER it
        // succeeds. Bytes surviving a failed build would claim to describe it.
        const projectRoot = await mkdtemp(join(tmpdir(), 'putnami-app-capability-'));
        const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
        const originalPwd = process.env['PWD'];
        const manifest = join(projectRoot, '.gen', 'schema', 'capabilities.json');

        try {
          await writeFile(join(projectRoot, 'package.json'), `${JSON.stringify({ name: 'capability-test-app' })}\n`);
          await mkdir(join(projectRoot, '.gen', 'schema'), { recursive: true });
          await writeFile(manifest, 'stale-manifest-from-a-previous-build\n');
          process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;
          process.env['PWD'] = projectRoot;

          app.use({
            generate: async () => {
              throw new Error('generate boom');
            },
          } satisfies Plugin);

          await expect(app.build()).rejects.toThrow('generate boom');
          expect(existsSync(manifest)).toBe(false);
        } finally {
          // Restore by deleting rather than assigning undefined: an absent
          // variable and one holding the string "undefined" resolve differently.
          restoreEnv('PUTNAMI_PROJECT_ROOT', originalProjectRoot);
          restoreEnv('PWD', originalPwd);
          await rm(projectRoot, { recursive: true, force: true });
        }
      },
    );
  });

  describe('start()', () => {
    specTest(
      'should call warmup on all plugins',
      { feature: 'typescript/application-lifecycle', requirement: 'phase-order', check: 'every-plugin-is-warmed-up' },
      async () => {
        const plugin: Plugin = { warmup: mock(() => Promise.resolve()) };
        app.use(plugin);
        await app.start();
        expect(plugin.warmup).toHaveBeenCalledWith(app);
        await app.stop();
      },
    );

    specTest(
      'should call start on all plugins',
      { feature: 'typescript/application-lifecycle', requirement: 'phase-order', check: 'every-plugin-is-started' },
      async () => {
        const plugin: Plugin = { start: mock(() => Promise.resolve()) };
        app.use(plugin);
        await app.start();
        expect(plugin.start).toHaveBeenCalledWith(app);
        await app.stop();
      },
    );

    specTest(
      'should call plugins in order: warmup then start',
      { feature: 'typescript/application-lifecycle', requirement: 'phase-order', check: 'warmup-runs-before-start' },
      async () => {
        const order: string[] = [];
        const plugin: Plugin = {
          warmup: mock(async () => {
            order.push('warmup');
          }),
          start: mock(async () => {
            order.push('start');
          }),
        };
        app.use(plugin);
        await app.start();
        expect(order).toEqual(['warmup', 'start']);
        await app.stop();
      },
    );

    specTest(
      'should run runner after plugins start',
      { feature: 'typescript/application-lifecycle', requirement: 'phase-order', check: 'the-runner-goes-last' },
      async () => {
        const order: string[] = [];
        const plugin: Plugin = {
          start: mock(async () => {
            order.push('plugin');
          }),
        };
        app.use(plugin);
        app.run(async () => {
          order.push('runner');
        });
        await app.start();
        expect(order).toEqual(['plugin', 'runner']);
        await app.stop();
      },
    );

    specTest(
      'should re-throw startup errors instead of terminating the process',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'failed-start-unwind',
        check: 'a-startup-failure-is-re-thrown-to-the-caller',
      },
      async () => {
        const failure = new Error('boom');
        const plugin: Plugin = {
          start: mock(async () => {
            throw failure;
          }),
        };
        app.use(plugin);
        await expect(app.start()).rejects.toThrow('boom');
        expect(app.isRunning()).toBe(false);
      },
    );

    specTest(
      'should clean up started resources when startup fails',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'failed-start-unwind',
        check: 'a-failed-start-stops-started-plugins-closes-the-container-and-leaves-it-not-running',
      },
      async () => {
        let resourceClosed = false;
        class StartupResource {}
        const plugin: Plugin = {
          start: mock(async () => {
            throw new Error('boom');
          }),
          stop: mock(async () => {}),
        };
        app.provide(StartupResource, () => new StartupResource(), {
          onClose: () => {
            resourceClosed = true;
          },
        });
        app.use(plugin);

        await expect(app.start()).rejects.toThrow('boom');

        expect(resourceClosed).toBe(true);
        expect(app.getActiveContext()).toBeUndefined();
        expect(app.isRunning()).toBe(false);
      },
    );

    specTest(
      'should not call stop() on a plugin whose start() rejected',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'failed-start-unwind',
        check: 'a-plugin-whose-start-rejected-is-not-stopped',
      },
      async () => {
        // The plugin that fails to start must not be torn down — closing a
        // connection it never opened raises a secondary error that masks the
        // real startup failure. Plugins that did start are still stopped.
        const startedPlugin: Plugin = {
          start: mock(async () => {}),
          stop: mock(async () => {}),
        };
        const failingPlugin: Plugin = {
          start: mock(async () => {
            throw new Error('boom');
          }),
          stop: mock(async () => {}),
        };
        app.use(startedPlugin);
        app.use(failingPlugin);

        await expect(app.start()).rejects.toThrow('boom');

        expect(failingPlugin.stop).not.toHaveBeenCalled();
        expect(startedPlugin.stop).toHaveBeenCalledWith(app);
        expect(app.isRunning()).toBe(false);
      },
    );

    specTest(
      'should log every additional concurrent start failure and throw the first',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'failed-start-unwind',
        check: 'every-additional-concurrent-failure-is-logged-and-the-first-is-thrown',
      },
      async () => {
        // Starters run under Promise.allSettled, so several can reject in one
        // pass. Only the first is thrown; the others must still reach the log or
        // an operator diagnoses the same outage twice.
        const logger = new MemoryLogger();
        setRootLogger(logger);
        try {
          const first: Plugin = {
            start: mock(async () => {
              throw new Error('first failure');
            }),
          };
          const second: Plugin = {
            start: mock(async () => {
              throw new Error('second failure');
            }),
          };
          app.use(first).use(second);

          await expect(app.start()).rejects.toThrow('first failure');

          const messages = logger.entries.map((entry) => entry.message);
          expect(messages.some((message) => message.includes('second failure'))).toBe(true);
        } finally {
          resetDefaultLogger();
        }
      },
    );

    specTest(
      'should run migrate hooks after warmup and before plugin start',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'phase-order',
        check: 'migrate-hooks-run-after-warmup-and-before-plugin-start',
      },
      async () => {
        const order: string[] = [];
        const plugin: Plugin = {
          warmup: mock(async () => {
            order.push('warmup');
          }),
          migrate: mock(async () => {
            order.push('migrate');
          }),
          start: mock(async () => {
            order.push('start');
          }),
        };
        app.use(plugin);
        app.run(async () => {
          order.push('runner');
        });

        await app.start();

        expect(order).toEqual(['warmup', 'migrate', 'start', 'runner']);
        await app.stop();
      },
    );
  });

  describe('stop()', () => {
    specTest(
      'should call stop on all plugins',
      { feature: 'typescript/application-lifecycle', requirement: 'shutdown-order', check: 'every-plugin-is-stopped' },
      async () => {
        const plugin: Plugin = { stop: mock(() => Promise.resolve()) };
        app.use(plugin);
        await app.start();
        await app.stop();
        expect(plugin.stop).toHaveBeenCalledWith(app);
      },
    );

    it('should call shutdown hooks', async () => {
      const hook = mock(() => Promise.resolve());
      app.onStop(hook);
      await app.start();
      await app.stop();
      expect(hook).toHaveBeenCalled();
    });

    specTest(
      'should not stop if not running',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'shutdown-order',
        check: 'stopping-a-stopped-application-does-nothing',
      },
      async () => {
        const hook = mock(() => Promise.resolve());
        app.onStop(hook);
        await app.stop();
        expect(hook).not.toHaveBeenCalled();
      },
    );
  });

  describe('lifecycle integration', () => {
    it('should run full runtime lifecycle (warmup, start, stop)', async () => {
      const order: string[] = [];
      const plugin: Plugin = {
        warmup: mock(async () => {
          order.push('warmup');
        }),
        start: mock(async () => {
          order.push('start');
        }),
        stop: mock(async () => {
          order.push('stop');
        }),
      };
      app.use(plugin);
      await app.start();
      await app.stop();
      expect(order).toEqual(['warmup', 'start', 'stop']);
    });

    specTest(
      'should not call generate during start (generate is build-time only)',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'build-only-generation',
        check: 'start-never-invokes-generate',
      },
      async () => {
        const generateMock = mock(async () => ({}));
        const plugin: Plugin = {
          generate: generateMock,
        };
        app.use(plugin);
        await app.start();
        expect(generateMock).not.toHaveBeenCalled();
        await app.stop();
      },
    );

    specTest(
      'should run generate during build phase',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'build-only-generation',
        check: 'generate-runs-during-build',
      },
      async () => {
        const order: string[] = [];
        const plugin: Plugin = {
          generate: mock(async () => {
            order.push('generate');
            return {};
          }),
        };
        app.use(plugin);
        await app.build();
        expect(order).toEqual(['generate']);
      },
    );
  });

  describe('module composition', () => {
    specTest(
      'should warmup plugins from sub-modules',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'phase-order',
        check: 'sub-module-plugins-are-warmed-up-too',
      },
      async () => {
        const order: string[] = [];
        const appPlugin: Plugin = { warmup: mock(async () => order.push('app-plugin')) };
        const modPlugin: Plugin = { warmup: mock(async () => order.push('mod-plugin')) };

        const mod = module('auth').use(modPlugin);
        app.use(appPlugin).use(mod);

        await app.start();
        expect(order).toEqual(['app-plugin', 'mod-plugin']);
        await app.stop();
      },
    );

    specTest(
      'should start plugins from sub-modules',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'phase-order',
        check: 'sub-module-plugins-are-started-too',
      },
      async () => {
        const order: string[] = [];
        const appPlugin: Plugin = { start: mock(async () => order.push('app')) };
        const modPlugin: Plugin = { start: mock(async () => order.push('mod')) };

        const mod = module('auth').use(modPlugin);
        app.use(appPlugin).use(mod);

        await app.start();
        // start runs in parallel, so check both ran
        expect(order).toContain('app');
        expect(order).toContain('mod');
        await app.stop();
      },
    );

    specTest(
      'should stop plugins from sub-modules in reverse order',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'shutdown-order',
        check: 'sub-module-plugins-stop-in-reverse-module-tree-order',
      },
      async () => {
        const order: string[] = [];
        const p1: Plugin = { stop: mock(async () => order.push('p1')) };
        const p2: Plugin = { stop: mock(async () => order.push('p2')) };
        const p3: Plugin = { stop: mock(async () => order.push('p3')) };

        const mod = module('auth').use(p2);
        app.use(p1).use(mod).use(p3);

        await app.start();
        await app.stop();
        // Reverse of [p1, p2 (from mod), p3] = [p3, p2, p1]
        expect(order).toEqual(['p3', 'p2', 'p1']);
      },
    );

    specTest(
      'should collect shutdown hooks from sub-modules',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'shutdown-order',
        check: 'shutdown-hooks-are-collected-from-sub-modules',
      },
      async () => {
        const order: string[] = [];
        const mod = module('auth');
        mod.onStop(async () => order.push('mod-hook'));
        app.onStop(async () => order.push('app-hook'));
        app.use(mod);

        await app.start();
        await app.stop();
        // Hooks are collected then reversed: [mod-hook, app-hook] -> reversed = [app-hook, mod-hook]
        expect(order).toEqual(['app-hook', 'mod-hook']);
      },
    );

    it('should run generate on module plugins during build', async () => {
      const modPlugin: Plugin = {
        generate: mock(async () => ({ assets: { 'test.ts': '/path/test.ts' } })),
      };
      const mod = module('auth').use(modPlugin);
      app.use(mod);

      const result = await app.build();
      expect(modPlugin.generate).toHaveBeenCalled();
      expect(result.assets?.['test.ts']).toBe('/path/test.ts');
    });

    specTest(
      'should pass the owning module to plugin lifecycle methods',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'phase-order',
        check: 'the-owning-module-is-passed-to-each-lifecycle-method',
      },
      async () => {
        let receivedOwner: unknown;
        const modPlugin: Plugin = {
          warmup: mock(async (owner) => {
            receivedOwner = owner;
          }),
        };
        const mod = module('auth').use(modPlugin);
        app.use(mod);

        await app.start();
        // Plugin should receive its owning module, not the application
        expect(receivedOwner).toBe(mod);
        await app.stop();
      },
    );

    it('should support nested modules', async () => {
      const order: string[] = [];
      const p1: Plugin = { warmup: mock(async () => order.push('p1')) };
      const p2: Plugin = { warmup: mock(async () => order.push('p2')) };
      const p3: Plugin = { warmup: mock(async () => order.push('p3')) };

      const inner = module('inner').use(p2);
      const outer = module('outer').use(inner).use(p3);
      app.use(p1).use(outer);

      await app.start();
      expect(order).toEqual(['p1', 'p2', 'p3']);
      await app.stop();
    });
  });

  describe('application() factory', () => {
    it('should create an Application instance', () => {
      const app = application();
      expect(app).toBeInstanceOf(Application);
    });

    it('should support fluent chaining', async () => {
      const plugin: Plugin = { warmup: mock(() => Promise.resolve()) };
      const app = application().use(plugin);
      await app.start();
      expect(plugin.warmup).toHaveBeenCalled();
      await app.stop();
    });
  });

  describe('ensurePlugin() lifecycle', () => {
    specTest(
      'should start plugins added during warmup via ensurePlugin()',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'composition-invalidation',
        check: 'a-plugin-added-during-warmup-is-still-started',
      },
      async () => {
        const started: string[] = [];

        class PluginB implements Plugin {
          async start() {
            started.push('B');
          }
        }

        class PluginA implements Plugin {
          async warmup(owner: any) {
            await owner.ensurePlugin(PluginB);
          }
          async start() {
            started.push('A');
          }
        }

        app.use(new PluginA());
        await app.start();

        // PluginB was created during warmup — it must still be started
        expect(started).toContain('A');
        expect(started).toContain('B');

        await app.stop();
      },
    );

    specTest(
      'should not double-warmup plugins added via ensurePlugin()',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'composition-invalidation',
        check: 'a-plugin-added-via-ensure-is-not-warmed-up-twice',
      },
      async () => {
        let warmupCount = 0;

        class LazyPlugin implements Plugin {
          async warmup() {
            warmupCount++;
          }
        }

        class ConsumerPlugin implements Plugin {
          async warmup(owner: any) {
            await owner.ensurePlugin(LazyPlugin);
          }
        }

        app.use(new ConsumerPlugin());
        await app.start();

        // ensurePlugin warms up LazyPlugin inline; start() should not re-warm it
        expect(warmupCount).toBe(1);

        await app.stop();
      },
    );
  });

  describe('context staleness', () => {
    specTest(
      'should include providers added after early context access',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'composition-invalidation',
        check: 'a-provider-registered-after-the-context-was-read-is-still-present',
      },
      async () => {
        class ServiceA {}
        class ServiceB {}

        app.provide(ServiceA);
        // Access context early — builds a cached version
        const _earlyCtx = app.context;

        // Add more composition after the early access
        app.provide(ServiceB);

        await app.start();

        // ServiceB must be available despite the early context access
        expect(app.context.has(ServiceA)).toBe(true);
        expect(app.context.has(ServiceB)).toBe(true);
      },
    );

    specTest(
      'should include modules added after early context access',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'composition-invalidation',
        check: 'a-module-mounted-after-the-context-was-read-is-still-present',
      },
      async () => {
        class Database {}

        // Access context early
        const _earlyCtx = app.context;

        const mod = module('data').provide(Database);
        app.use(mod);

        await app.start();

        expect(app.context.has(Database)).toBe(true);
      },
    );

    specTest(
      'should invalidate context when provide() is called',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'composition-invalidation',
        check: 'provide-invalidates-the-context',
      },
      () => {
        class ServiceA {}
        class ServiceB {}

        app.provide(ServiceA);
        const ctx1 = app.context;

        app.provide(ServiceB);
        const ctx2 = app.context;

        // Context should have been rebuilt
        expect(ctx1).not.toBe(ctx2);
      },
    );

    specTest(
      'should invalidate context when use() is called',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'composition-invalidation',
        check: 'use-invalidates-the-context',
      },
      () => {
        class ServiceA {}

        app.provide(ServiceA);
        const ctx1 = app.context;

        app.use(module('extra'));
        const ctx2 = app.context;

        expect(ctx1).not.toBe(ctx2);
      },
    );
  });

  describe('DI integration', () => {
    it('should resolve providers after start', async () => {
      class Database {
        query() {
          return 'data';
        }
      }

      app.provide(Database);
      await app.start();

      const db = app.context.get(Database);
      expect(db).toBeInstanceOf(Database);
      expect(db.query()).toBe('data');

      await app.stop();
    });

    it('should resolve providers across modules', async () => {
      class Database {
        query() {
          return 'data';
        }
      }
      class AuthService {
        constructor(public db: Database) {}
      }

      const authModule = module('auth')
        .require(Database)
        .provide(AuthService, { deps: [Database] });

      app.provide(Database).use(authModule);
      await app.start();

      const auth = app.context.get(AuthService);
      expect(auth).toBeInstanceOf(AuthService);
      expect(auth.db).toBeInstanceOf(Database);
      expect(auth.db.query()).toBe('data');

      await app.stop();
    });

    specTest(
      'should throw RequirementNotMetError when module requirements are not met',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'requirement-validation',
        check: 'an-unsatisfied-module-requirement-fails-at-composition',
      },
      () => {
        class Database {}
        class AuthService {}

        const authModule = module('auth').require(Database).provide(AuthService);

        app.use(authModule);

        // Accessing .context triggers buildContainerContext which mounts modules.
        // The mount step validates requirements.
        expect(() => app.context).toThrow(RequirementNotMetError);
      },
    );

    specTest(
      'should enforce private visibility across modules',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'requirement-validation',
        check: 'private-visibility-is-enforced-across-modules',
      },
      async () => {
        class InternalHelper {}
        class PublicService {}

        const modA = module('modA').provide(InternalHelper, { visibility: 'private' }).provide(PublicService);

        app.use(modA);
        await app.start();

        // PublicService should be accessible from the app context
        expect(app.context.get(PublicService)).toBeInstanceOf(PublicService);

        // InternalHelper is private to modA and is not visible from the app context root
        expect(app.context.has(InternalHelper)).toBe(false);

        await app.stop();
      },
    );

    it('should make context available after start', async () => {
      class Service {}
      app.provide(Service);
      await app.start();

      expect(app.context).toBeInstanceOf(ContainerContext);
      expect(app.context.has(Service)).toBe(true);

      await app.stop();
    });

    specTest(
      'should clean up context after stop',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'shutdown-order',
        check: 'the-container-is-cleaned-up-after-stop',
      },
      async () => {
        class Service {}
        app.provide(Service);
        await app.start();

        const ctx = app.context;
        expect(ctx).toBeInstanceOf(ContainerContext);

        await app.stop();

        // After stop, the internal _context is cleared.
        // Accessing .context again builds a fresh one (lazy getter).
        // The old context should be closed.
        expect(() => ctx.get(Service)).toThrow();
      },
    );

    it('should lazily build context on first access before start', () => {
      class Service {}
      app.provide(Service);

      // Accessing .context before start should build the ContainerContext lazily
      const ctx = app.context;
      expect(ctx).toBeInstanceOf(ContainerContext);
    });

    it('should always build a context (framework registers MigrationRegistry by default)', async () => {
      const plugin: Plugin = { warmup: mock(() => Promise.resolve()) };
      app.use(plugin);

      // No user-side providers registered, but the framework always
      // wires a per-app MigrationRegistry into DI so the container is
      // never empty.
      await app.start();
      expect(app.isRunning()).toBe(true);
      expect(app.describeContainer()).toBeDefined();

      await app.stop();
    });

    it('should support scoped resolution through context.scope()', async () => {
      class RequestId {
        id = crypto.randomUUID();
      }

      app.provide(RequestId, { scope: 'scoped' });
      await app.start();

      let id1: string | undefined;
      let id2: string | undefined;

      await app.context.scope(async (scope) => {
        id1 = scope.get(RequestId).id;
        // Same instance within the same scope
        expect(scope.get(RequestId).id).toBe(id1);
      });

      await app.context.scope(async (scope) => {
        id2 = scope.get(RequestId).id;
      });

      // Different scopes should produce different instances
      expect(id1).toBeDefined();
      expect(id2).toBeDefined();
      expect(id1).not.toBe(id2);

      await app.stop();
    });

    it('should list providers by tags via context.list()', async () => {
      class MiddlewareA {
        name = 'A';
      }
      class MiddlewareB {
        name = 'B';
      }
      class ServiceC {
        name = 'C';
      }

      app
        .provide(MiddlewareA, { tags: ['middleware'] })
        .provide(MiddlewareB, { tags: ['middleware'] })
        .provide(ServiceC, { tags: ['service'] });

      await app.start();

      const middlewares = app.context.list<{ name: string }>({ tags: 'middleware' });
      expect(middlewares).toHaveLength(2);
      const names = middlewares.map((m) => m.name).sort();
      expect(names).toEqual(['A', 'B']);

      const services = app.context.list<{ name: string }>({ tags: 'service' });
      expect(services).toHaveLength(1);
      expect(services[0].name).toBe('C');

      await app.stop();
    });

    it('should resolve named tokens', async () => {
      const AppVersion = named<string>('app-version');
      app.provide(AppVersion, () => '1.0.0');
      await app.start();

      expect(app.context.get(AppVersion)).toBe('1.0.0');

      await app.stop();
    });

    it('should resolve dependencies across multiple modules', async () => {
      class Database {
        query() {
          return 'data';
        }
      }
      class AuthService {
        constructor(public db: Database) {}
      }
      class StoreService {
        constructor(public db: Database) {}
      }

      const authModule = module('auth')
        .require(Database)
        .provide(AuthService, { deps: [Database] });

      const storeModule = module('store')
        .require(Database)
        .provide(StoreService, { deps: [Database] });

      app.provide(Database).use(authModule).use(storeModule);
      await app.start();

      const auth = app.context.get(AuthService);
      const store = app.context.get(StoreService);

      expect(auth).toBeInstanceOf(AuthService);
      expect(store).toBeInstanceOf(StoreService);

      // Both modules should share the same Database singleton
      expect(auth.db).toBe(store.db);

      await app.stop();
    });

    it('should resolve dependencies across composed sibling modules', async () => {
      class Database {
        query() {
          return 'data';
        }
      }
      class AuthService {
        constructor(public db: Database) {}
      }

      const databaseModule = module('database').provide(Database);
      const authModule = module('auth')
        .require(Database)
        .provide(AuthService, { deps: [Database] });

      app.use(composeModules([databaseModule, authModule], { name: 'auth.server' }));
      await app.start();

      const auth = app.context.get(AuthService);
      expect(auth).toBeInstanceOf(AuthService);
      expect(auth.db).toBeInstanceOf(Database);
      expect(auth.db.query()).toBe('data');
      expect(app.context.getModuleContainers().map((container) => container.name)).toContain('auth.server');
      expect(app.context.getModuleContainers().map((container) => container.name)).not.toContain('database');
      expect(app.context.getModuleContainers().map((container) => container.name)).not.toContain('auth');

      await app.stop();
    });

    it('should start context when only a sub-module has registrations', async () => {
      class AuthService {}

      const authModule = module('auth').provide(AuthService);
      app.use(authModule);

      await app.start();

      expect(app.context.get(AuthService)).toBeInstanceOf(AuthService);

      await app.stop();
    });

    it('should describe container after start', async () => {
      class Service {}
      app.provide(Service);
      await app.start();

      const description = app.describeContainer();
      expect(description).toBeDefined();
      expect(description?.name).toBe('app');
      expect(description?.providers.length).toBeGreaterThanOrEqual(1);

      await app.stop();
    });
  });

  describe('signal handlers (graceful shutdown exit code)', () => {
    // Invoke only the listener this call installed (diff before/after) rather
    // than process.emit(), so we don't trip other tests' signal handlers.
    const fireOwnSignal = (signal: 'SIGTERM' | 'SIGINT', install: () => () => void): (() => void) => {
      const before = new Set(process.listeners(signal));
      const cleanup = install();
      const added = process.listeners(signal).filter((l) => !before.has(l));
      for (const listener of added) {
        (listener as (sig: string) => void)(signal);
      }
      return cleanup;
    };

    specTest(
      'exits 0 when stop() resolves',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'signal-shutdown',
        check: 'a-resolved-drain-exits-zero',
      },
      async () => {
        const exitSpy = spyOn(process, 'exit').mockImplementation((() => undefined) as never);
        const cleanup = fireOwnSignal('SIGTERM', () =>
          installSignalHandlers(() => Promise.resolve(), useLogger('test')),
        );
        try {
          // Let the stop() promise settle and the .then run.
          await new Promise((r) => setTimeout(r, 10));
          expect(exitSpy).toHaveBeenCalledWith(0);
        } finally {
          cleanup();
          exitSpy.mockRestore();
        }
      },
    );

    specTest(
      'exits non-zero when stop() rejects',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'signal-shutdown',
        check: 'a-rejected-drain-exits-non-zero',
      },
      async () => {
        const exitSpy = spyOn(process, 'exit').mockImplementation((() => undefined) as never);
        const cleanup = fireOwnSignal('SIGINT', () =>
          installSignalHandlers(() => Promise.reject(new Error('drain failed')), useLogger('test')),
        );
        try {
          await new Promise((r) => setTimeout(r, 10));
          expect(exitSpy).toHaveBeenCalledWith(1);
        } finally {
          cleanup();
          exitSpy.mockRestore();
        }
      },
    );

    specTest(
      'force-exits 1 when stop() hangs past SHUTDOWN_TIMEOUT_MS',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'signal-shutdown',
        check: 'a-hung-drain-force-exits-non-zero-at-the-deadline',
      },
      () => {
        const exitSpy = spyOn(process, 'exit').mockImplementation((() => undefined) as never);
        // Capture the force-exit timer instead of waiting the real 10s. Mock
        // setTimeout to record the (callback, delay) the handler schedules, and
        // return a handle with unref() so the unref branch runs unchanged.
        let scheduled: { cb: () => void; delay?: number } | undefined;
        const setTimeoutSpy = spyOn(globalThis, 'setTimeout').mockImplementation(((cb: () => void, delay?: number) => {
          scheduled = { cb, delay };
          return { unref() {} } as unknown as ReturnType<typeof setTimeout>;
        }) as never);
        // stop() never resolves — only the force-exit timer can end the process.
        const cleanup = fireOwnSignal('SIGTERM', () =>
          installSignalHandlers(() => new Promise<void>(() => {}), useLogger('test')),
        );
        try {
          expect(scheduled?.delay).toBe(SHUTDOWN_TIMEOUT_MS);
          expect(exitSpy).not.toHaveBeenCalled();
          // Fire the force-exit timer as if the timeout elapsed.
          scheduled?.cb();
          expect(exitSpy).toHaveBeenCalledWith(1);
        } finally {
          cleanup();
          setTimeoutSpy.mockRestore();
          exitSpy.mockRestore();
        }
      },
    );

    specTest(
      'ignores a second signal while already stopping (no double stop)',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'signal-shutdown',
        check: 'a-second-signal-does-not-start-a-second-stop',
      },
      async () => {
        const exitSpy = spyOn(process, 'exit').mockImplementation((() => undefined) as never);
        let stopCalls = 0;
        let resolveStop: (() => void) | undefined;
        // Keep shutdown pending across both signals, then settle it during cleanup
        // so the handler clears its real force-exit timer before this test returns.
        const stop = () => {
          stopCalls++;
          return new Promise<void>((resolve) => {
            resolveStop = resolve;
          });
        };
        const before = new Set(process.listeners('SIGTERM'));
        const cleanup = installSignalHandlers(stop, useLogger('test'));
        const added = process.listeners('SIGTERM').filter((l) => !before.has(l));
        try {
          for (const listener of added) (listener as (sig: string) => void)('SIGTERM');
          for (const listener of added) (listener as (sig: string) => void)('SIGTERM');
          expect(stopCalls).toBe(1);
        } finally {
          resolveStop?.();
          await Promise.resolve();
          cleanup();
          exitSpy.mockRestore();
        }
      },
    );
  });
});
