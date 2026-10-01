/**
 * Shared build-time config activation for the hook scripts — used by both
 * `bin/generate.ts` (the `preBuild` hook) and `bin/config-extract.ts` (the
 * `configExtract` hook).
 *
 * ## Why both hooks need the same step
 *
 * `emitInfraRequirements()` and `extractConfigSchema()` both read ONE global
 * registry: whatever `configToken()` has registered in this process. Three
 * producers fill it, and they finish at different points:
 *
 *  1. the workload's own module-level `configToken()` calls — during entry-point
 *     import, before `build()`;
 *  2. route-owned `configToken()` calls — only once the generated `*-loader`
 *     modules are imported, which is what makes their handler modules execute;
 *  3. dependency-owned blocks — only once `registerContributedConfigs()` walks
 *     the composed plugin tree for `ConfigContributor`s.
 *
 * `Application.build()` covers (2) on its own — the capability producer imports
 * the loaders in `postGenerate` — but nothing covers (3), so a hook that reads
 * the registry straight after `build()` sees a strict subset. That is not a
 * cosmetic difference for the preBuild hook: `build-generate` is the ONLY
 * task that reconciles `.gen/infra/*.json` into the committed
 * `infra/requirements.json` (`config-extract-exec` writes the scratch fragment
 * but never syncs it, and `putnami build` never schedules it at all). So a
 * preBuild hook that emits its secrets fragment from a partial registry commits
 * a deployability manifest with missing secret NAMES, while `schema/config.json`
 * — written later, by a hook that did activate — is complete. That is exactly
 * the order-dependent output this helper exists to remove.
 *
 * Both callers must therefore run this BEFORE they read the registry.
 */
import { emitLog } from '@putnami/utils/hooks';
import type { Application } from '../src/application/application';
import type { GenerateResult } from '../src/application/module.types';

/**
 * Import every generated server loader the build produced, then register the
 * dependency-contributed config blocks, so the global config registry is
 * complete before the caller reads it.
 *
 * Takes the ALREADY-COMPUTED build result: `Application.build()` writes
 * generated sources, capability manifests and migration artifacts, and running
 * it twice in one hook would republish all of them. The caller owns that call.
 *
 * Only module-level registration effects fire here. Importing a loader executes
 * its handler modules' top level — the `configToken(Config)` calls — and opens
 * no runtime resource: the loader modules are generated re-export barrels, and
 * the application container is never composed by a hook.
 *
 * Degrades instead of throwing when the discovered app predates the
 * `ConfigContributor` walk — see the probe below for why that shape is supported
 * rather than a misconfiguration.
 *
 * @param app          the Application the hook discovered (never rebuilt here)
 * @param buildResult  what `app.build()` returned
 * @param debug        forward the hook's debug flag to per-loader logging
 */
export async function activateBuildTimeConfig(
  app: Application,
  buildResult: GenerateResult | undefined,
  debug?: boolean,
): Promise<void> {
  await importGeneratedLoaders(buildResult, debug);

  // Aggregate dependency-owned config: walk the composed plugin tree for
  // ConfigContributors and register their blocks into the same registry the
  // workload's own configToken() calls populate. This is the config dual of the
  // MigrationContributor walk, so a workload publishes a library's config (and
  // the secrets its sensitive fields declare) transitively. It throws on a
  // genuine path collision — a dependency claiming a path the workload already
  // defines — which is a fail-closed answer both hooks want: the alternative is
  // a manifest that silently drops one of the two blocks.
  //
  // Probed rather than called outright. `isApplicationLike` (bin/_find-application.ts)
  // accepts any object carrying build/use/getPlugins/start, and it is deliberately
  // that narrow: a workload that bundles its own copy of @putnami/application hands
  // the hook an Application from a SIBLING MODULE REALM, where `instanceof` fails and
  // the method set is whatever that copy shipped. `registerContributedConfigs` arrived
  // after those four, so a supported app can legitimately not have it — and this hook
  // runs on every `putnami build`, so calling it blind turns a supported shape into a
  // raw `TypeError: app.registerContributedConfigs is not a function`, thrown AFTER
  // build() already published its generated sources.
  //
  // Skipping is the only thing left to do, not a lesser evil: an @putnami/application
  // without the walk has no ConfigContributor protocol to walk either, so there are no
  // contributed blocks in that realm to lose. What must not happen is losing them
  // QUIETLY — the resulting infra/requirements.json is short exactly the secrets a
  // dependency declares, which is the defect this helper exists to close — so the
  // degraded run says so, names the cause, and gives the remedy.
  // Probed through Partial<Application> because that is the honest type here:
  // `isApplicationLike` returns `obj is Application` off four method checks, so
  // the compiler's view of `app` is a promise the duck check never made.
  const walkContributedConfigs = (app as Partial<Application>).registerContributedConfigs;
  if (typeof walkContributedConfigs !== 'function') {
    emitLog(
      'warn',
      'config activation: this Application exposes no registerContributedConfigs() — ' +
        'dependency-contributed config blocks (and the secrets their sensitive fields declare) ' +
        'will be absent from schema/config.json and infra/requirements.json. ' +
        'The workload is resolving a copy of @putnami/application older than the ConfigContributor walk, ' +
        'usually a duplicate in its own node_modules: dedupe or upgrade it. ' +
        'Config the workload registers itself, and config its generated route loaders register, are unaffected.',
    );
    return;
  }
  app.registerContributedConfigs();
}

/**
 * Import the generated `*-loader` modules so route-owned `configToken()` calls
 * execute.
 *
 * `Application.build()` already imports this same set from the capability
 * producer's `postGenerate` (see `capabilities.producer.ts`
 * `importGeneratedLoaders`), so on a real Application every import below is a
 * module-cache hit and cannot fail a build that is green today. It is repeated
 * here because activation is a contract of THIS helper, not a side effect of
 * capability publication that a future `build()` option could stop performing —
 * and because the two loops must never disagree, the filter, the ordering and
 * the error text are kept identical to that producer's.
 *
 * `client-loader` exports are excluded for the same reason `build()` excludes
 * them from its capability input: a generated client loader is a browser-side
 * OUTPUT, not an importable application module, and it registers no config.
 *
 * The loop is sorted by export key rather than trusting `Object.entries`:
 * the export map is merged from several producers (the generate pass, the
 * postGenerate pass, the capability producer), so insertion order is the
 * scheduler's business, not a contract. Sorting makes both the registration
 * order and the FIRST failure stable across runs and checkouts.
 */
async function importGeneratedLoaders(buildResult: GenerateResult | undefined, debug?: boolean): Promise<void> {
  const loaderEntries = Object.entries(buildResult?.exports ?? {})
    .filter(([key]) => key.endsWith('-loader') && !key.endsWith('client-loader'))
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));

  for (const [key, path] of loaderEntries) {
    if (debug) {
      emitLog('debug', `Importing loader for config registration: ${key} (${path})`);
    }
    try {
      // biome-ignore lint/performance/noAwaitInLoops: deterministic import order keeps registration failures stable
      await import(path);
    } catch (err) {
      throw new Error(`Could not import ${key} loader at ${path}: ${err}`);
    }
  }
}
