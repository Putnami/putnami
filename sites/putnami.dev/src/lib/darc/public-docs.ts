/**
 * The runtime half of the `public-docs` domain's declared access.
 *
 * Each declared documentation import becomes one `Snapshot` from
 * `@putnami/application`'s DARC runtime. The build OBSERVES the import's source
 * roots — reads their markdown, digests it, and attaches that state under its
 * exact version — and then RESOLVES each section through `Snapshot.latest()`.
 * An import that governs several copies holds them as one set, attached only
 * when every member was read. Everything the docs pipeline publishes for a
 * section is the file inventory `latest()` handed back, which is what makes the
 * contract enforcement rather than description:
 *
 * - a source that is gone is never attached — nor is the set it belongs to — so
 *   `latest()` takes the declared `onMissing: fail-closed` and throws
 *   `DarcError('missing')`;
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
import {
  APPLICATION_RUNTIME_FACT,
  APPLICATION_RUNTIME_IMPORT,
  DOCUMENTATION_SOURCES,
  type DocumentationSourceContract,
  PROTOCOL_CONTRACTS_IMPORT,
} from './documentation-contracts';
import {
  composeDocumentationSet,
  type DocumentationTree,
  type DocumentationTreeReader,
  documentationSetMember,
  type ObservedDocumentationTree,
  readDocumentationTree,
} from './documentation-tree';

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
 * The `public-docs` domain's enforced contracts: one snapshot per documentation
 * import, one framework-runtime reference and one protocol-contract reference.
 */
export class PublicDocumentationAccess {
  /** One snapshot per documentation import, keyed by the import id. */
  private readonly snapshots = new Map<string, Snapshot<DocumentationTree>>();
  /** The import that governs each declared source root. */
  private readonly governing = new Map<string, DocumentationSourceContract>();
  private readonly readTree: DocumentationTreeReader;
  /** The stable handle on the TypeScript framework runtime this site is built on. */
  readonly runtime: Reference;
  /** The stable handle on the content-bundle protocol this site implements. */
  readonly protocols: Reference;

  constructor(options: PublicDocumentationAccessOptions = {}) {
    const clock = options.clock;
    for (const source of DOCUMENTATION_SOURCES) {
      this.snapshots.set(source.contract.id, new Snapshot<DocumentationTree>(source.contract, clock ? { clock } : {}));
      for (const copy of source.copies) this.governing.set(copy.sourceRoot, source);
    }
    // Resolved on first read, not in the constructor: `getWorkspaceRoot()`
    // walks the filesystem, and a component built to inspect its contracts
    // should not need a workspace to exist.
    this.readTree =
      options.readTree ?? ((root: string) => readDocumentationTree(options.workspaceRoot ?? getWorkspaceRoot(), root));
    this.runtime = new Reference(APPLICATION_RUNTIME_IMPORT);
    this.protocols = new Reference(PROTOCOL_CONTRACTS_IMPORT);
  }

  /** Every component this domain enforces, snapshots first, in declaration order. */
  components(): DarcComponent[] {
    return [...DOCUMENTATION_SOURCES.map((source) => this.snapshotOf(source)), this.runtime, this.protocols];
  }

  /** Whether a `generate.assets` source root is governed by a declared import. */
  declares(sourceRoot: string): boolean {
    return this.governing.has(sourceRoot);
  }

  /**
   * Read every source root of the import that governs `sourceRoot`, and attach
   * what is there.
   *
   * An import with one copy attaches that copy's tree. An import with several
   * attaches their composed set, and only when the reader found every member. A
   * root the reader reports absent therefore attaches nothing. That is the whole
   * mechanism: the refusal is produced by the declared contract at read time,
   * not by an `if` here.
   */
  observe(sourceRoot: string): void {
    this.observeSource(this.sourceFor(sourceRoot));
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
    for (const source of DOCUMENTATION_SOURCES) this.observeSource(source);
  }

  /**
   * The section's current state, under the declared contract.
   *
   * Throws `DarcError('missing')` when the source was never attached, because
   * the import declares `onMissing: fail-closed`. The record it returns carries
   * the file inventory the docs pipeline publishes for this one root, plus the
   * provenance, observation time and freshness verdict the contract requires.
   */
  resolve(sourceRoot: string): DarcRecord<DocumentationTree> {
    const source = this.sourceFor(sourceRoot);
    const record = this.snapshotOf(source).latest();
    if (!record) {
      // Unreachable while the contract declares fail-closed: latest() throws
      // before returning undefined. Stated rather than asserted so a future
      // contract edit to fail-open surfaces here instead of as `undefined`
      // reaching the publisher.
      throw new Error(
        `darc: documentation source ${sourceRoot} resolved to nothing; its import no longer declares onMissing fail-closed`,
      );
    }
    if (source.copies.length === 1) return record;
    return { ...record, value: documentationSetMember(record.value, sourceRoot) };
  }

  /** The framework runtime fact this site consumes, and the export that owns it. */
  runtimeProvenance(): string {
    return this.runtime.fact(APPLICATION_RUNTIME_FACT);
  }

  private observeSource(source: DocumentationSourceContract): void {
    const members: ObservedDocumentationTree[] = [];
    for (const copy of source.copies) {
      const observed = this.readTree(copy.sourceRoot);
      if (!observed) return;
      members.push(observed);
    }
    const observed =
      members.length === 1 ? (members[0] as ObservedDocumentationTree) : composeDocumentationSet(members);
    this.snapshotOf(source).attach(observed.version, observed.tree);
  }

  private sourceFor(sourceRoot: string): DocumentationSourceContract {
    const source = this.governing.get(sourceRoot);
    if (!source) {
      throw new Error(`darc: ${sourceRoot} is not a declared public-docs documentation source`);
    }
    return source;
  }

  private snapshotOf(source: DocumentationSourceContract): Snapshot<DocumentationTree> {
    return this.snapshots.get(source.contract.id) as Snapshot<DocumentationTree>;
  }
}

/** Build the domain's enforced contracts for one checkout. */
export function publicDocumentationAccess(options: PublicDocumentationAccessOptions = {}): PublicDocumentationAccess {
  return new PublicDocumentationAccess(options);
}
