import { useLogger } from '@putnami/runtime';
import { buildInfraManifest, type PerProjectInfraManifest } from './infra';
import { MIGRATION_LOGGER, migrationFields } from './logging';
import type { ApplyOpts, DriftReport, Kind, MigrationRecord, MigrationRunner, MigrationSource } from './types';

/**
 * The per-application collection of contributed Sources and registered
 * Runners. One Registry is held in DI by the application; it is built
 * fresh at every boot — there is no module-level singleton.
 */
export class MigrationRegistry {
  private readonly sources = new Map<Kind, MigrationSource[]>();
  private readonly runners = new Map<Kind, MigrationRunner>();
  private readonly logger = useLogger(MIGRATION_LOGGER);

  /**
   * Record a feature-plugin contribution. Per-(namespace, name)
   * duplicate detection is delegated to the Runner once the source has
   * been expanded into definitions.
   */
  addSource(source: MigrationSource): void {
    if (!source) {
      throw new MigrationConfigError('nil migration source');
    }
    if (!source.kind) {
      throw new MigrationConfigError('migration source has empty kind');
    }
    if (!source.namespace || source.namespace.trim() === '') {
      throw new MigrationConfigError(`migration source has empty namespace (kind=${source.kind})`);
    }
    const list = this.sources.get(source.kind) ?? [];
    list.push(source);
    this.sources.set(source.kind, list);
    this.logger.debug(
      'plugin contributed source',
      migrationFields({ kind: source.kind, namespace: source.namespace, sourcesForKind: list.length }),
    );
  }

  /**
   * Install the Runner for one Kind. Exactly one Runner may register
   * per Kind; a second call for the same Kind throws.
   */
  registerRunner(runner: MigrationRunner): void {
    if (!runner) {
      throw new MigrationConfigError('nil migration runner');
    }
    if (!runner.kind) {
      throw new MigrationConfigError('migration runner has empty kind');
    }
    if (this.runners.has(runner.kind)) {
      throw new MigrationConfigError(`a runner for kind "${runner.kind}" is already registered`);
    }
    this.runners.set(runner.kind, runner);
    this.logger.debug(
      'runner registered',
      migrationFields({ kind: runner.kind, sourcesForKind: this.sources.get(runner.kind)?.length ?? 0 }),
    );
  }

  /** Sources for one kind, in registration order. Returns a defensive copy. */
  sourcesFor(k: Kind): MigrationSource[] {
    const src = this.sources.get(k);
    return src ? [...src] : [];
  }

  /** The Runner registered for one Kind, if any. */
  runnerFor(k: Kind): MigrationRunner | undefined {
    return this.runners.get(k);
  }

  /**
   * The per-project infra manifest derived from every contributed source's
   * `infraDatabase()`, or undefined when nothing is contributed. This is the
   * "walk the registry" entry point: it aggregates schemas across all kinds
   * into databases keyed by `(name, engine)`. See ./infra.
   */
  infraManifest(): PerProjectInfraManifest | undefined {
    const sources: MigrationSource[] = [];
    for (const k of this.kinds()) {
      sources.push(...this.sourcesFor(k));
    }
    return buildInfraManifest(sources);
  }

  /**
   * Every Kind with either a registered Runner or contributed Sources,
   * lexicographically sorted. ApplyAll / StatusAll / VerifyAll iterate
   * in this order so cross-kind output is reproducible.
   */
  kinds(): Kind[] {
    const seen = new Set<Kind>();
    for (const k of this.runners.keys()) seen.add(k);
    for (const k of this.sources.keys()) seen.add(k);
    return [...seen].sort();
  }

  /**
   * Run Runner.apply for every Kind with a registered Runner, in
   * lexicographic order. By default, sources contributed for a kind with no
   * runner produce an UnknownKindError; callers must explicitly set
   * `allowSourceOnly` for the automatic lifecycle apply that tolerates
   * source-only static metadata.
   */
  async applyAll(opts: ApplyOpts = {}): Promise<MigrationRecord[]> {
    if (!opts.allowSourceOnly) {
      this.assertNoOrphanSources();
    }
    const out: MigrationRecord[] = [];
    for (const k of this.kinds()) {
      const runner = this.runners.get(k);
      if (!runner) continue;
      const records = await runner.apply(opts);
      out.push(...records);
    }
    return out;
  }

  /** Aggregated Runner.status across every registered kind. */
  async statusAll(): Promise<MigrationRecord[]> {
    this.assertNoOrphanSources();
    const out: MigrationRecord[] = [];
    for (const k of this.kinds()) {
      const runner = this.runners.get(k);
      if (!runner) continue;
      const records = await runner.status();
      out.push(...records);
    }
    return out;
  }

  /** Per-kind drift reports, one entry per kind (including empties). */
  async verifyAll(): Promise<DriftReport[]> {
    this.assertNoOrphanSources();
    const out: DriftReport[] = [];
    for (const k of this.kinds()) {
      const runner = this.runners.get(k);
      if (!runner) continue;
      out.push(await runner.verify());
    }
    return out;
  }

  /**
   * Catches the common "plugin ships SQL migrations but the app forgot
   * to `.use(databasePlugin())`" mistake for explicit migration operations,
   * while Start opts into source-only static metadata during the automatic
   * lifecycle apply. Naming the contributing plugin in the error message is
   * the §15.6 discoverability contract.
   */
  private assertNoOrphanSources(): void {
    const missing: string[] = [];
    for (const [kind, srcs] of this.sources) {
      if (this.runners.has(kind) || srcs.length === 0) continue;
      const namespaces = srcs.map((s) => s.namespace).join(', ');
      missing.push(`${kind} (contributed by: ${namespaces})`);
    }
    if (missing.length === 0) return;
    missing.sort();
    throw new UnknownKindError(
      `migration sources contributed for kinds with no registered Runner: ${missing.join('; ')}`,
    );
  }
}

/** Thrown when a source / runner contribution is structurally invalid. */
export class MigrationConfigError extends Error {
  override readonly name = 'MigrationConfigError';
}

/**
 * Thrown when a Source has been contributed for a Kind with no
 * registered Runner. Matches Go's `migration.CodeUnknownKind`.
 */
export class UnknownKindError extends Error {
  override readonly name = 'UnknownKindError';
}
