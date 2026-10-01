/**
 * Transversal migration framework — kind-agnostic types every backend
 * runner (SQL, GCS, document, …) plugs into.
 *
 * Authoring formats and storage backends live in kind-specific packages
 * (`@putnami/database` for SQL today). This package contains only the
 * shared contracts and the in-process registry that the app lifecycle
 * and migrate CLI orchestrate.
 */

import type { InfraDatabaseRequirement } from './infra';

/**
 * The migration kinds this framework ships intent for. SQL is the only kind
 * with a Runner today (`@putnami/database`); the rest are the planned domains
 * documented across the package. Keeping them in a closed union lets a typo
 * like `'sqll'` fail at compile time while still allowing out-of-tree kinds
 * via the `Kind` escape hatch below.
 */
export type KnownKind = 'sql' | 'gcs' | 'document' | 'cache' | 'event-topic';

/**
 * Kind identifies the migration domain a Source or Runner operates on. The
 * shipped kinds are enumerated by {@link KnownKind} for typo-catching; the
 * `string & {}` escape hatch keeps the contract open for kinds defined in
 * downstream packages without widening literal hints away.
 */
export type Kind = KnownKind | (string & {});

/** Canonical kind for relational-database migrations. */
export const KindSQL: KnownKind = 'sql';

/**
 * Source is what a feature plugin contributes via
 * MigrationContributor.migrationSources(). Concrete Source values are
 * tagged-union-style: each kind defines its own type (e.g.
 * `database.SQLSource`) that satisfies this interface.
 */
export interface MigrationSource {
  /** The migration domain this source belongs to. */
  readonly kind: Kind;
  /**
   * The contributing plugin's identifier — typically `Plugin.name`.
   * Used for diagnostics, logging, and the recommended
   * ${namespace}/${basename} migration naming convention.
   */
  readonly namespace: string;
  /**
   * Optional infra-requirements contribution. Kind-specific sources that map
   * to a deployable database (SQL → a Postgres schema today) return the
   * database `name`, `engine`, and `schemas` they migrate; kinds with no infra
   * footprint leave it undefined. The migration framework aggregates these
   * during generate into the per-project infra scratch fragment. See ./infra.
   */
  infraDatabase?(): InfraDatabaseRequirement | undefined;
}

/** Lifecycle status of a single migration. */
export type RecordStatus = 'applied' | 'rolled-back' | 'pending' | 'failed';

/**
 * Cross-kind result type Runners return. Carries the homogeneous fields
 * the CLI needs to render status/up/down/verify/inspect output the same
 * way across kinds; kind-specific extras can live in `target`.
 */
export interface MigrationRecord {
  kind: Kind;
  namespace?: string;
  name: string;
  status: RecordStatus;
  hash?: string;
  downHash?: string;
  /** RFC3339 timestamp; empty/undefined when not yet executed. */
  executedAt?: string;
  durationMs?: number;
  error?: string;
  /** Diagnostic origin: e.g. "embed:iam/migrations/..." or "inline:iam". */
  source?: string;
  /** Kind-specific scope: datasource for SQL, bucket for GCS, etc. */
  target?: string;
}

/**
 * Input to Runner.apply. The automatic Start lifecycle sets
 * `allowSourceOnly` while leaving `force` false, so source-only migration
 * metadata is tolerated but runners still honor their own auto-apply settings
 * (typical for production services that gate startup migration behind a
 * separate job). Explicit migration calls leave `allowSourceOnly` false so a
 * missing runner fails loudly. `force: true` additionally overrides runner
 * gating — the migrate CLI passes `force` on `up`.
 */
export interface ApplyOpts {
  /**
   * Narrows the apply to migrations up to and including this name.
   * The value may be either the fully namespaced form
   * "iam/20260520120000_create_users" or the bare basename when
   * unambiguous across namespaces.
   */
  to?: string;
  /** Bypass per-runner auto-apply gating. Defaults to false. */
  force?: boolean;
  /**
   * Permit sources whose kind has no registered runner. Set only by the
   * automatic Start lifecycle; explicit migration entry points should leave it
   * false to surface misconfiguration before invoking any runner.
   */
  allowSourceOnly?: boolean;
}

/**
 * Input to Runner.rollback. Empty rolls back only the most recent
 * applied migration; `to` rolls back every migration applied AFTER the
 * named one (the named migration itself remains applied).
 */
export interface RollbackOpts {
  to?: string;
}

/** Single hash drift between applied row and current definition. */
export interface HashDrift {
  namespace?: string;
  name: string;
  storedHash: string;
  currentHash: string;
  target?: string;
}

/** Diff between a Runner's registry view and the persisted state store. */
export interface DriftReport {
  kind: Kind;
  hashDrifts?: HashDrift[];
  /** Rows applied in the state store but absent from the current registry. */
  missingFromRegistry?: MigrationRecord[];
  /** Definitions in the current registry that are not yet applied. */
  missingFromStore?: MigrationRecord[];
}

/** Returns true when registry and state store agree. */
export function isDriftReportEmpty(d: DriftReport): boolean {
  return (
    (d.hashDrifts?.length ?? 0) === 0 &&
    (d.missingFromRegistry?.length ?? 0) === 0 &&
    (d.missingFromStore?.length ?? 0) === 0
  );
}

/**
 * Kind-specific engine. Exactly one Runner registers per Kind into a
 * Registry; that runner consumes every Source of its kind contributed
 * by feature plugins.
 *
 * Implementations live in kind-owning packages — e.g.
 * `@putnami/database` registers a SQL Runner. This package is
 * intentionally backend-free.
 */
export interface MigrationRunner {
  /** The domain this runner serves. Must equal every contributed source's kind. */
  readonly kind: Kind;
  /**
   * Run pending migrations of this kind across all targets. When
   * `opts.force` is false, the runner is free to no-op according to
   * its own auto-apply settings.
   */
  apply(opts?: ApplyOpts): Promise<MigrationRecord[]>;
  /** Recorded state of every migration of this kind. */
  status(): Promise<MigrationRecord[]>;
  /** Reverse applied migrations. */
  rollback(opts?: RollbackOpts): Promise<MigrationRecord[]>;
  /** Compare registered migrations against the state store. */
  verify(): Promise<DriftReport>;
}
