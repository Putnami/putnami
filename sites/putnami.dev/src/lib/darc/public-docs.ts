/**
 * The runtime half of the `public-docs` domain's declared access.
 *
 * Each of the five repository-owned documentation sources becomes one
 * `Snapshot` from `@putnami/application`'s DARC runtime. The build OBSERVES a
 * source root — reads its markdown tree, digests it, and attaches that state
 * under its exact version — and then RESOLVES the section through
 * `Snapshot.latest()`. Everything the docs pipeline publishes for a section is
 * the file inventory `latest()` handed back, which is what makes the contract
 * enforcement rather than description:
 *
 * - a source that is gone is never attached, so `latest()` takes the declared
 *   `onMissing: fail-closed` and throws `DarcError('missing')`;
 * - a source whose bytes changed is a new version, and re-attaching an existing
 *   version with different content throws `DarcError('immutable')`;
 * - the record carries provenance (the producer export), the observation time,
 *   and the freshness verdict against the declared 720h bound. Nothing here
 *   keeps a second, parallel provenance channel — those three fields are the
 *   `DarcRecord`'s.
 *
 * The clock is injectable because a freshness verdict measured against the wall
 * clock would make a test's outcome depend on when it ran.
 */
import { type DarcComponent, type DarcRecord, Reference, Snapshot } from '@putnami/application';
import { getWorkspaceRoot } from '@putnami/utils';
import { APPLICATION_RUNTIME_FACT, APPLICATION_RUNTIME_IMPORT, DOCUMENTATION_SOURCES } from './documentation-contracts';
import { type DocumentationTree, type DocumentationTreeReader, readDocumentationTree } from './documentation-tree';

/** Construction options. */
export interface PublicDocumentationAccessOptions {
  /** The checkout the default reader reads source roots from. */
  readonly workspaceRoot?: string;
  /** Replaces the source reader — a test simulates a deleted source through this. */
  readonly readTree?: DocumentationTreeReader;
  /** Replaces the clock the declared freshness bound is measured against. */
  readonly clock?: () => Date;
}

/**
 * The `public-docs` domain's enforced contracts: five documentation snapshots
 * and one framework-runtime reference.
 */
export class PublicDocumentationAccess {
  private readonly snapshots = new Map<string, Snapshot<DocumentationTree>>();
  private readonly declaredRoots = new Set<string>();
  private readonly readTree: DocumentationTreeReader;
  /** The stable handle on the TypeScript framework runtime this site is built on. */
  readonly runtime: Reference;

  constructor(options: PublicDocumentationAccessOptions = {}) {
    const clock = options.clock;
    for (const source of DOCUMENTATION_SOURCES) {
      this.declaredRoots.add(source.sourceRoot);
      this.snapshots.set(source.sourceRoot, new Snapshot<DocumentationTree>(source.contract, clock ? { clock } : {}));
    }
    // Resolved on first read, not in the constructor: `getWorkspaceRoot()`
    // walks the filesystem, and a component built to inspect its contracts
    // should not need a workspace to exist.
    this.readTree =
      options.readTree ?? ((root: string) => readDocumentationTree(options.workspaceRoot ?? getWorkspaceRoot(), root));
    this.runtime = new Reference(APPLICATION_RUNTIME_IMPORT);
  }

  /** Every component this domain enforces, snapshots first, in declaration order. */
  components(): DarcComponent[] {
    return [...DOCUMENTATION_SOURCES.map((source) => this.snapshotFor(source.sourceRoot)), this.runtime];
  }

  /** Whether a `generate.assets` source root is governed by a declared import. */
  declares(sourceRoot: string): boolean {
    return this.declaredRoots.has(sourceRoot);
  }

  /**
   * Read one source root and attach what is there.
   *
   * A root the reader reports absent attaches nothing. That is the whole
   * mechanism: the refusal is produced by the declared contract at read time,
   * not by an `if` here.
   */
  observe(sourceRoot: string): void {
    const snapshot = this.snapshotFor(sourceRoot);
    const observed = this.readTree(sourceRoot);
    if (!observed) return;
    snapshot.attach(observed.version, observed.tree);
  }

  /**
   * Read every declared source root as one authoritative filesystem scan.
   *
   * A documentation tree's version is a content digest, not a sequence number.
   * Resetting the snapshots prevents a later full scan whose digest happens to
   * sort below a prior one from resolving the old tree, and makes a source that
   * vanished since the last scan fail closed instead of retaining its old state.
   */
  observeAll(): void {
    for (const snapshot of this.snapshots.values()) snapshot.reset();
    for (const source of DOCUMENTATION_SOURCES) this.observe(source.sourceRoot);
  }

  /**
   * The section's current state, under the declared contract.
   *
   * Throws `DarcError('missing')` when the source was never attached, because
   * the import declares `onMissing: fail-closed`. The record it returns carries
   * the file inventory the docs pipeline publishes, plus the provenance,
   * observation time and freshness verdict the contract requires.
   */
  resolve(sourceRoot: string): DarcRecord<DocumentationTree> {
    const record = this.snapshotFor(sourceRoot).latest();
    if (!record) {
      // Unreachable while the contract declares fail-closed: latest() throws
      // before returning undefined. Stated rather than asserted so a future
      // contract edit to fail-open surfaces here instead of as `undefined`
      // reaching the publisher.
      throw new Error(
        `darc: documentation source ${sourceRoot} resolved to nothing; its import no longer declares onMissing fail-closed`,
      );
    }
    return record;
  }

  /** The framework runtime fact this site consumes, and the export that owns it. */
  runtimeProvenance(): string {
    return this.runtime.fact(APPLICATION_RUNTIME_FACT);
  }

  private snapshotFor(sourceRoot: string): Snapshot<DocumentationTree> {
    const snapshot = this.snapshots.get(sourceRoot);
    if (!snapshot) {
      throw new Error(`darc: ${sourceRoot} is not a declared public-docs documentation source`);
    }
    return snapshot;
  }
}

/** Build the domain's enforced contracts for one checkout. */
export function publicDocumentationAccess(options: PublicDocumentationAccessOptions = {}): PublicDocumentationAccess {
  return new PublicDocumentationAccess(options);
}
