/**
 * Shared helper for hook scripts that need to locate the workload's
 * Application instance — used by both `bin/generate.ts` (the preBuild hook)
 * and `bin/config-extract.ts` (the configExtract hook).
 *
 * The duck-typed check avoids tying us to the Application class identity:
 * once a workload bundles a copy of `@putnami/application`, the hook may
 * receive an `Application` from a sibling realm and `instanceof` checks
 * silently fail.
 */
import { fileExists, joinPath, readPackageJson } from '@putnami/utils';
import { emitLog } from '@putnami/utils/hooks';
import type { Application } from '../src/application/application';

/**
 * Check if an object looks like an Application instance using duck typing.
 */
export function isApplicationLike(obj: unknown): obj is Application {
  if (!obj || typeof obj !== 'object') return false;
  const app = obj as Record<string, unknown>;
  return (
    typeof app['build'] === 'function' &&
    typeof app['use'] === 'function' &&
    typeof app['getPlugins'] === 'function' &&
    typeof app['start'] === 'function'
  );
}

/**
 * Try to extract an Application instance from a module's exports.
 *
 * Resolution order matches what `bin/generate.ts` has shipped since the
 * first preBuild hook: `app()` factory, `default` (instance or factory),
 * then `Application` named export.
 */
export function resolveApplicationFromModule(mod: Record<string, unknown>, debug?: boolean): Application | null {
  const log = debug ? (msg: string) => emitLog('debug', msg) : undefined;
  const appExport = mod['app'];
  const defaultExport = mod['default'];
  const applicationExport = mod['Application'];

  // Case 1: app() factory function (most common pattern)
  if (appExport && typeof appExport === 'function') {
    log?.('Found app() factory function, invoking...');
    const app = (appExport as () => unknown)();
    if (isApplicationLike(app)) {
      log?.('Result is Application-like (duck typing)');
      return app;
    }
    log?.('Result is NOT Application-like');
  }

  // Case 2: default export (could be factory or instance)
  if (defaultExport) {
    if (isApplicationLike(defaultExport)) {
      log?.('Found default export as Application-like instance');
      return defaultExport;
    }
    if (typeof defaultExport === 'function') {
      log?.('Found default export as factory function, invoking...');
      const app = (defaultExport as () => unknown)();
      if (isApplicationLike(app)) {
        log?.('Result is Application-like (duck typing)');
        return app;
      }
    }
  }

  // Case 3: Direct Application export
  if (isApplicationLike(applicationExport)) {
    log?.('Found Application export as Application-like instance');
    return applicationExport;
  }

  return null;
}

/**
 * One probed-but-failed entry point.
 *
 * `kind` separates the three very different reasons discovery comes up empty,
 * and the boundaries are load-bearing — a caller that treats them alike either
 * hides a broken build or breaks a working one:
 *
 * - `import-error` — the module could not be LOADED (a broken specifier, or an
 *   import of a module another extension has not generated yet). Never
 *   legitimate for a project that ships that entry point.
 * - `factory-error` — the module loaded; the `app()`/`default` factory threw
 *   while being invoked. Legitimate at build time: a factory is allowed to
 *   demand runtime configuration that only exists at deploy time (e.g.
 *   `typescript/samples/07-authentication` requires `OAUTH_CLIENT_ID`).
 * - `no-application` — the module loaded and simply is not an app (a plain
 *   library that depends on `@putnami/application`). Legitimate.
 */
export interface EntryPointFailure {
  entryPoint: string;
  kind: 'import-error' | 'factory-error' | 'no-application';
  detail: string;
}

/** Render probe failures as the bullet list hook error messages use. */
export function formatEntryPointFailures(failures: EntryPointFailure[]): string {
  return failures.map((f) => `  - ${f.entryPoint}: ${f.detail}`).join('\n');
}

/**
 * Find and instantiate the Application from the project, probing
 * package.json's `main` field plus the conventional entry points.
 *
 * When `diagnostics` is provided, every probed-but-failed entry point is
 * recorded there (import errors, factories that threw, modules without an
 * Application export) so callers that must fail loudly on a missing app can
 * surface WHY discovery came up empty — the debug log alone is invisible in
 * non-debug runs. `failures` carries the same records classified (see
 * {@link EntryPointFailure}), for callers that must distinguish "could not be
 * loaded" from the legitimate misses.
 */
export async function findApplication(
  projectPath: string,
  debug?: boolean,
  diagnostics?: string[],
  failures?: EntryPointFailure[],
): Promise<Application | null> {
  const log = debug ? (msg: string) => emitLog('debug', msg) : undefined;

  // Check package.json main field
  const packageJson = readPackageJson(joinPath(projectPath, 'package.json'));
  const mainEntry = packageJson?.main;

  // Try common entry points (deduped: package.json main usually IS one of
  // the conventional paths, and probing it twice doubles the diagnostics).
  const entryPoints = [
    ...new Set([mainEntry, 'src/main.ts', 'src/app.ts', 'src/index.ts'].filter(Boolean)),
  ] as string[];

  log?.(`Looking for Application in entry points: ${entryPoints.join(', ')}`);

  for (const entryPoint of entryPoints) {
    const entryPath = joinPath(projectPath, entryPoint);

    if (!fileExists(entryPath)) {
      log?.(`Entry point not found: ${entryPoint}`);
      continue;
    }

    log?.(`Trying to import: ${entryPoint}`);

    // Loading the module and invoking its factory are probed in SEPARATE try
    // blocks on purpose. resolveApplicationFromModule CALLS `app()`/`default()`,
    // so sharing one try would report every factory that throws as if the module
    // itself were unloadable — and a factory is entitled to throw at build time
    // (it may demand deploy-time secrets). Keeping them apart is what lets a
    // caller fail closed on a genuinely unloadable module without touching the
    // legitimate cases.
    let mod: Record<string, unknown>;
    try {
      mod = await import(entryPath);
    } catch (error) {
      log?.(`Failed to import ${entryPoint}: ${error}`);
      const detail = error instanceof Error ? error.message : String(error);
      diagnostics?.push(`${entryPoint}: ${detail}`);
      failures?.push({ entryPoint, kind: 'import-error', detail });
      continue;
    }

    try {
      log?.(`Module exports: ${Object.keys(mod).join(', ')}`);

      const app = resolveApplicationFromModule(mod, debug);
      if (app) {
        log?.(`Application found via ${entryPoint}`);
        return app;
      }

      log?.(`No valid Application instance from ${entryPoint}`);
      const detail = 'module loaded but exposed no Application export (app()/default/Application)';
      diagnostics?.push(`${entryPoint}: ${detail}`);
      failures?.push({ entryPoint, kind: 'no-application', detail });
    } catch (error) {
      log?.(`Application factory threw in ${entryPoint}: ${error}`);
      // The diagnostics string stays byte-identical to the pre-split shape:
      // config-extract already surfaces it and its wording is pinned by tests.
      const detail = error instanceof Error ? error.message : String(error);
      diagnostics?.push(`${entryPoint}: ${detail}`);
      failures?.push({ entryPoint, kind: 'factory-error', detail });
    }
  }

  log?.('No Application found in any entry point');
  return null;
}
