import type { Plugin } from '../application/module';
import type { Module } from '../application/module';
import { type Application, application } from '../application/application';
import { HttpPlugin, http } from '../http/http.plugin';

/**
 * Options for creating a test application.
 */
export interface TestAppOptions {
  /** Plugins to add to the application (an `http({ port: 0 })` is always included) */
  plugins?: (Plugin | Module)[];
  /**
   * Optional callback to configure the Application instance before load/start.
   * Use this to register DI providers, add routes to the HttpPlugin, etc.
   *
   * @example
   * ```typescript
   * await createTestApp({
   *   plugins: [sql(), events()],
   *   configure: (app) => {
   *     app.provide(MyService);
   *   },
   * });
   * ```
   */
  configure?: (app: Application) => void;
}

/**
 * The return value of {@link createTestApp}.
 */
export interface TestApp {
  /** The running application instance */
  app: Application;
  /** The resolved base URL (e.g. `http://localhost:54321`) */
  baseUrl: string;
  /** `fetch` pre-bound to `baseUrl` — call with a path like `fetch('/users')` */
  fetch: (path: string, init?: RequestInit) => Promise<Response>;
  /** Stop the application and release resources */
  stop: () => Promise<void>;
}

/**
 * Create, build, and start a test application with an ephemeral HTTP port.
 * The build runs every plugin generation hook and imports generated loaders,
 * but does not publish the build-owned `.gen/schema/capabilities.json` manifest.
 *
 * Eliminates the boilerplate of `application() → http({ port: 0 }) → build() → start() → getPlugin(HttpPlugin)`.
 *
 * @example
 * ```typescript
 * import { createTestApp } from '@putnami/application/testing';
 * import { api, platform } from '@putnami/application';
 *
 * const { fetch, stop } = await createTestApp({
 *   plugins: [platform(), api({ scanPath: 'src/api' })],
 * });
 *
 * const res = await fetch('/healthz');
 * expect(res.status).toBe(200);
 *
 * await stop();
 * ```
 */
export async function createTestApp(options: TestAppOptions = {}): Promise<TestApp> {
  const app = application().use(http({ port: 0 }));

  for (const plugin of options.plugins ?? []) {
    app.use(plugin);
  }

  options.configure?.(app);

  // Neither the capability manifest nor the migration artifacts belong to a
  // test: both are outputs of the scheduler's build-generate hook, and a test
  // app composing the workload's migration sources would otherwise overwrite
  // the hook's bundle with one keyed on the npm package name.
  await app.build({ publishCapabilityManifest: false, publishMigrationArtifacts: false });
  await app.start();

  const port = app.getPlugin(HttpPlugin).getServer()?.port ?? 3000;
  const baseUrl = `http://localhost:${port}`;

  return {
    app,
    baseUrl,
    fetch: (path: string, init?: RequestInit) => globalThis.fetch(`${baseUrl}${path}`, init),
    stop: () => app.stop(),
  };
}
