import type { ReadyEndpoint } from '@putnami/runtime/jobs';
import type { Promisable } from '@putnami/utils';
import type { GeneratedHttpRoute } from '../http-routes/generation';
import type { Module } from './module';

export interface BuildOptions {
  /** Enable debug logging during build */
  debug?: boolean;
  /**
   * Publish the application's capability manifest after generation.
   * Defaults to `true`. Test helpers disable this so test execution cannot
   * overwrite the manifest owned by the scheduler's generate step.
   */
  publishCapabilityManifest?: boolean;
  /**
   * Publish the application's disposable design graph after generation.
   * Defaults to the capability-publication setting so unit-test applications
   * cannot erase or replace the graph owned by the scheduler's generate step.
   */
  publishDesignGraph?: boolean;
  /**
   * The putnami project identity (putnami.json name / project path) the build
   * hooks receive from the Go runner. Keys the emitted migration bundle so it
   * matches the Go describer's `ctx.Project.Name`. Absent, no migration
   * artifacts are emitted: the npm package.json name is not a publication
   * identity, and a `build()` outside the build-generate hook (a test, a
   * script) must never overwrite the bundle that hook wrote.
   */
  projectName?: string;
  /**
   * Emit the migration infra fragment (`.gen/infra/migration.json`) and the
   * migration bundle (`.gen/migration-bundle/`) after generation. Defaults to
   * `true` when `projectName` is set. The config-extract hook and the test
   * helpers disable it so the build-generate hook stays the single writer of
   * both artifacts.
   */
  publishMigrationArtifacts?: boolean;
}

export interface GenerateResult {
  assets?: Record<string, string>;
  exports?: Record<string, string>;
  /** Framework route facts aggregated into the build's canonical HTTP inventory. */
  httpRoutes?: GeneratedHttpRoute[];
}

/**
 * Plugin interface for extending Module/Application functionality.
 * Plugins can hook into the application lifecycle.
 *
 * Lifecycle: generate() → postGenerate() → warmup() → migrate() → start() → startupCompleted() → stop()
 */
export interface Plugin {
  /**
   * Optional name identifying the plugin in the module tree.
   *
   * It is declared here because a plugin may legitimately contribute nothing to
   * the lifecycle at all — a contributor that only carries declarations into the
   * capability manifest, such as `darc.DarcPlugin`, implements no hook below.
   * Without one member in common with this interface such a plugin cannot be
   * passed to `use` at all. Go twin: `app.Plugin`'s `Name()`.
   */
  readonly name?: string;

  /**
   * Called during build to generate code (e.g., route loaders).
   *
   * All plugins' `generate()` hooks run concurrently, so a plugin must not
   * depend on an artifact another plugin emits during its own `generate()`.
   * Use `postGenerate()` for that.
   */
  generate?(owner: Module): Promisable<GenerateResult>;

  /**
   * Called after every plugin's `generate()` has resolved — i.e. once all
   * build-time artifacts (OpenAPI/Proto specs, route loaders, …) are written
   * to disk. The second argument is the merged `generate()` result, so derived
   * codegen can consume exact artifact paths emitted by other plugins instead
   * of guessing default locations.
   */
  postGenerate?(owner: Module, generated?: GenerateResult): Promisable<GenerateResult>;

  /**
   * Called during the warmup phase before the application starts.
   * Use this to initialize resources, register routes, etc.
   *
   * Migration runners (`@putnami/database`, future GCS/document) register
   * themselves into the per-app MigrationRegistry from here.
   */
  warmup?(owner: Module): Promisable<void>;

  /**
   * Called between warmup and start, after the framework has collected
   * MigrationContributor sources and the migration runners have been
   * registered. Most plugins do not implement this hook — it exists
   * for runners (or tests) that want to drive a per-kind apply step
   * outside the framework's automatic gating.
   */
  migrate?(owner: Module): Promisable<void>;

  /**
   * Called when the application starts running.
   * Use this to start servers, subscribe to queues, etc.
   */
  start?(owner: Module): Promisable<void>;

  /**
   * Called once the application completed startup: every plugin `start()`
   * resolved. Runs in plugin order, right before the application logs its ready
   * record, and never when startup failed. Must not block. Go twin:
   * `app.StartupObserver`.
   */
  startupCompleted?(): void;

  /**
   * The addresses this plugin accepts traffic on, bound while it started, such
   * as an HTTP server's listener. The application's ready record carries the
   * endpoints of every plugin. Returns only addressable endpoints, and none
   * before `start()` or after `stop()`. Go twin: `app.EndpointReporter`.
   */
  readyEndpoints?(): ReadyEndpoint[];

  /**
   * Called during graceful shutdown.
   * Use this to clean up resources, close connections, etc.
   */
  stop?(owner: Module): Promisable<void>;
}
