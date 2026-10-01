/**
 * The projection mode: a rebuildable local copy of another domain's facts,
 * governed at run time by the import contract that declares it.
 *
 * The Go twin is `go.putnami.dev/protocol/architecture/darc/projection.go`, and both are held to
 * `protocols/architecture/fixtures/conformance/projection-behavior.json`.
 */

import type { ArchitectureImport, LocalModel } from '../architecture/contract.types';
import {
  cloneDate,
  cloneRecord,
  ContractError,
  type DarcRecord,
  DarcError,
  type ValueCloner,
  type VersionOrder,
  lexicalVersionOrder,
  missingVerdict,
  requireActive,
  resolveValueCloner,
  staleVerdict,
  staleness,
  stampFreshness,
  validateContract,
} from './contract';

/**
 * One delivery of producer state to a projection.
 *
 * It carries what the contract's consistency block names: the source identity,
 * the producer state version the ordering rule compares, and the idempotency key
 * that makes a repeated delivery a no-op. `observedAt` is when the producer
 * state was seen — not when it arrived here — because that is what the declared
 * freshness bound is measured against.
 */
export interface Update<T> {
  /** The value of the declared source identity for this record. */
  readonly id: string;
  /** The projected copy. */
  readonly value: T;
  /** The producer state version, compared by the ordering rule. */
  readonly sourceVersion: string;
  /** Identifies this exact delivery; a repeat of the key last applied to the record is dropped. */
  readonly idempotencyKey?: string;
  /** When the producer state was observed. Absent means now, which is only correct for a delivery just fetched. */
  readonly observedAt?: Date;
}

/** How a projection obtains its initial state, over whichever carrier the contract declares. */
export type BootstrapSource<T> = () => Promise<readonly Update<T>[]> | readonly Update<T>[];

/**
 * How a projection resumes.
 *
 * It receives the newest source version the projection currently holds, or the
 * empty string when it holds nothing, so a replay resumes rather than restarts.
 */
export type ReplaySource<T> = (since: string) => Promise<readonly Update<T>[]> | readonly Update<T>[];

/** Construction options. */
export interface ProjectionOptions<T> {
  /** Required exactly when the declared rebuild strategy names bootstrap, and refused otherwise. */
  readonly bootstrap?: BootstrapSource<T>;
  /** Required exactly when the declared rebuild strategy names replay, and refused otherwise. */
  readonly replay?: ReplaySource<T>;
  /** Replaces the source-version comparator; pass one whenever the declared version is not lexically ordered. */
  readonly versionOrder?: VersionOrder;
  /** Replaces the clock the freshness bound is measured against. Tests use it; production does not need it. */
  readonly clock?: () => Date;
  /**
   * Copies values at storage and read boundaries. Defaults to `structuredClone`;
   * supply this for unsupported values or application-specific prototypes.
   */
  readonly cloneValue?: ValueCloner<T>;
}

/**
 * A rebuildable local copy, governed by its contract.
 *
 * Reads apply the declared freshness bound and missing/stale behavior. Writes go
 * through the single writer the local model names, in the declared order, with
 * the declared late-event and deletion rules. Nothing here fetches: the consumer
 * hands the projection its bootstrap and its updates over whichever carrier the
 * contract declares, and this class governs what happens to them.
 *
 * The Go twin guards its state with a mutex. This one holds no lock because
 * every mutation below completes without yielding, and JavaScript runs one task
 * at a time — the only suspension point is inside `rebuild`, where the Go twin
 * also calls its sources outside the lock. Two overlapping rebuilds race in both
 * languages; a caller that can issue one must serialize it.
 */
export class Projection<T> {
  private readonly declaration: ArchitectureImport;
  private readonly model: LocalModel;
  private readonly bound: number;
  private readonly order: VersionOrder;
  private readonly clock: () => Date;
  private readonly cloneValue: ValueCloner<T>;
  private readonly bootstrapSource?: BootstrapSource<T>;
  private readonly replaySource?: ReplaySource<T>;

  private records = new Map<string, DarcRecord<T>>();
  private lastKey = new Map<string, string>();
  private deleteVersion = new Map<string, string>();
  private writerTaken = false;
  private bootstrapped = false;

  /**
   * Build a projection from its declared contract.
   *
   * The import and both of its carriers must be active: a planned projection is
   * still a target design and cannot expose a runtime copy. The contract is
   * validated by the protocol itself, so every projection invariant the manifest
   * must state is checked rather than assumed. On top of that the rebuild
   * strategy must have the hooks it names — a projection that declares
   * `bootstrap` and is handed none would fail at the moment it was needed, which
   * is the moment it is least able to.
   */
  constructor(contract: ArchitectureImport, options: ProjectionOptions<T> = {}) {
    validateContract(contract, 'projection');
    requireActive(contract, contract.bootstrap, contract.updates);
    this.declaration = contract;
    // Protocol validation already refused a projection without a local model.
    this.model = contract.localModel as LocalModel;
    this.bound = staleness(contract.consistency);
    this.order = options.versionOrder ?? lexicalVersionOrder;
    this.clock = options.clock ?? (() => new Date());
    this.cloneValue = resolveValueCloner(options.cloneValue, 'ProjectionOptions.cloneValue');
    this.bootstrapSource = options.bootstrap;
    this.replaySource = options.replay;
    this.checkRebuildHooks();
  }

  /** Hold the rebuild strategy to the hooks it needs. */
  private checkRebuildHooks(): void {
    const rebuild = this.model.rebuild;
    const needsBootstrap = rebuild === 'bootstrap' || rebuild === 'bootstrap-and-replay';
    const needsReplay = rebuild === 'replay' || rebuild === 'bootstrap-and-replay';
    if (needsBootstrap && !this.bootstrapSource) {
      throw new ContractError(
        this.declaration.id,
        `declares rebuild "${rebuild}" but no bootstrap source was supplied (see ProjectionOptions.bootstrap)`,
      );
    }
    if (needsReplay && !this.replaySource) {
      throw new ContractError(
        this.declaration.id,
        `declares rebuild "${rebuild}" but no replay source was supplied (see ProjectionOptions.replay)`,
      );
    }
    if (!needsBootstrap && this.bootstrapSource) {
      throw new ContractError(
        this.declaration.id,
        `declares rebuild "${rebuild}", which does not bootstrap, but a bootstrap source was supplied`,
      );
    }
    if (!needsReplay && this.replaySource) {
      throw new ContractError(
        this.declaration.id,
        `declares rebuild "${rebuild}", which does not replay, but a replay source was supplied`,
      );
    }
  }

  /** The declaration this projection enforces. */
  contract(): ArchitectureImport {
    return this.declaration;
  }

  /**
   * Return one projected record and the verdict the contract calls for.
   *
   * The three answers are distinct on purpose:
   *
   * - `undefined` — the fact is absent and the contract declares `onMissing`
   *   fail-open, so the caller continues without it;
   * - throws `DarcError('missing')` — absent under fail-closed or unavailable;
   * - returns a record whose `freshness` is `stale` — present but past the
   *   declared bound, returned because `onStale` is use-stale or fail-open.
   *   Under fail-closed the same read throws `DarcError('stale')`, and that
   *   error carries the record, so a caller that wants to log what it refused can.
   *
   * A tombstoned record is reported as absent: the producer deleted the fact,
   * and the tombstone strategy is about retaining the marker, not the value.
   */
  get(id: string): DarcRecord<T> | undefined {
    const record = this.records.get(id);
    if (!record || record.deleted) {
      // A projection that was never rebuilt holds nothing, and reporting that as
      // an ordinary miss would hide the difference between "the producer has no
      // such fact" and "this copy was never built".
      missingVerdict(this.declaration, this.bootstrapped ? '' : ' (the projection has not been rebuilt yet)');
      return undefined;
    }
    const stamped = cloneRecord(stampFreshness(record, this.clock(), this.bound), this.cloneValue);
    staleVerdict(stamped, this.bound, this.declaration.consistency?.onStale ?? 'fail-open');
    return stamped;
  }

  /**
   * Every live record, ordered by source identity, each stamped with its
   * freshness. No missing or stale verdict applies: a caller enumerating a
   * projection is asking what it holds, and the per-record stamp is the answer.
   */
  all(): DarcRecord<T>[] {
    const now = this.clock();
    return [...this.records.values()]
      .filter((record) => !record.deleted)
      .map((record) => cloneRecord(stampFreshness(record, now, this.bound), this.cloneValue))
      .sort((left, right) => lexicalVersionOrder(left.id, right.id));
  }

  /**
   * The one handle allowed to update this projection.
   *
   * The local model names a single writer, and this is where that stops being a
   * sentence in a manifest: the name must match, and the handle is handed out
   * once. A second component that wants to write has to change the declaration
   * first, which is the review the invariant exists to force.
   */
  writer(name: string): ProjectionWriter<T> {
    if (name !== this.model.writer) {
      throw new DarcError(
        'not-the-writer',
        `darc: the local model names a different writer: import ${this.declaration.id} names "${this.model.writer}", not "${name}"`,
      );
    }
    if (this.writerTaken) {
      throw new DarcError(
        'writer-claimed',
        `darc: the projection writer is already claimed: "${this.model.writer}" already holds it for import ${this.declaration.id}`,
      );
    }
    this.writerTaken = true;
    return new ProjectionWriter<T>(this);
  }

  /**
   * Reconstruct the projection through the declared strategy: bootstrap replaces
   * what is held, replay resumes from the newest source version held, and
   * bootstrap-and-replay does both in that order.
   *
   * It does not need the writer handle: a rebuild is the projector re-deriving
   * its own state from the producer, not a second component writing into it.
   */
  async rebuild(): Promise<void> {
    switch (this.model.rebuild) {
      case 'bootstrap':
        await this.runBootstrap();
        return;
      case 'replay':
        await this.runReplay();
        return;
      case 'bootstrap-and-replay':
        await this.runBootstrap();
        await this.runReplay();
        return;
      default:
        throw new ContractError(
          this.declaration.id,
          `declares rebuild "${this.model.rebuild}", so it cannot be rebuilt`,
        );
    }
  }

  private async runBootstrap(): Promise<void> {
    const updates = await (this.bootstrapSource as BootstrapSource<T>)();
    this.records = new Map();
    this.lastKey = new Map();
    this.deleteVersion = new Map();
    this.bootstrapped = true;
    for (const update of updates) this.apply(update);
  }

  private async runReplay(): Promise<void> {
    const updates = await (this.replaySource as ReplaySource<T>)(this.newestVersion());
    this.bootstrapped = true;
    for (const update of updates) this.apply(update);
  }

  /** The newest source version held, so a replay resumes instead of restarting. */
  private newestVersion(): string {
    let newest = '';
    for (const record of this.records.values()) {
      if (newest === '' || this.order(record.sourceVersion, newest) > 0) newest = record.sourceVersion;
    }
    for (const version of this.deleteVersion.values()) {
      if (newest === '' || this.order(version, newest) > 0) newest = version;
    }
    return newest;
  }

  /** The ordered, idempotent write the contract describes. Returns whether the local model changed. */
  apply(update: Update<T>): boolean {
    const observedAt = cloneDate(update.observedAt ?? this.clock());

    // Idempotency first: a repeated delivery of the update already applied is
    // not a late event, it is the same event, and reporting it as late would
    // make a retrying transport look like a broken producer.
    const key = update.idempotencyKey ?? '';
    if (key !== '' && this.lastKey.get(update.id) === key) return false;

    // A deletion already accepted at this exact producer state wins over a
    // delayed live delivery of the same state. Re-applying it would make the
    // meaning of one source version depend on transport arrival order.
    const deletedVersion = this.deletionVersion(update.id);
    if (deletedVersion !== undefined && this.order(update.sourceVersion, deletedVersion) === 0) return false;

    const currentVersion = this.currentVersion(update.id);
    if (currentVersion !== undefined && !this.orderVerdict(currentVersion, update.sourceVersion, update.id)) {
      return false;
    }

    this.records.set(update.id, {
      id: update.id,
      value: this.cloneValue(update.value),
      provenance: this.declaration.from.export,
      observedAt,
      freshness: 'fresh',
      sourceVersion: update.sourceVersion,
      deleted: false,
    });
    this.deleteVersion.delete(update.id);
    if (key !== '') this.lastKey.set(update.id, key);
    return true;
  }

  /** What an update older than the local copy means, per the declared late-event strategy. */
  private orderVerdict(existingVersion: string, incomingVersion: string, id: string): boolean {
    if (this.declaration.consistency?.ordering === 'none') return true;
    if (this.order(incomingVersion, existingVersion) >= 0) return true;
    switch (this.declaration.consistency?.lateEvents) {
      case 'apply':
        return true;
      case 'reject':
        throw new DarcError(
          'late-update',
          `darc: the update is older than the local source version: ${incomingVersion} is older than the local ${existingVersion} for ${id}`,
        );
      default:
        // ignore-older: the update is dropped and the caller is told nothing
        // changed. It is not an error — an out-of-order delivery is what the
        // strategy exists to absorb.
        return false;
    }
  }

  /**
   * The ordering watermark for one source identity.
   *
   * A hard delete has no record to carry it, and retain deliberately keeps the
   * older value, so accepted deletions are tracked separately from the copy.
   */
  private currentVersion(id: string): string | undefined {
    const deleted = this.deleteVersion.get(id);
    if (deleted !== undefined) return deleted;
    return this.records.get(id)?.sourceVersion;
  }

  /**
   * The producer state at which this identity was deleted, if it was. Tombstone
   * carries it in the visible marker; hard-delete and retain carry it separately.
   */
  private deletionVersion(id: string): string | undefined {
    const deleted = this.deleteVersion.get(id);
    if (deleted !== undefined) return deleted;
    const record = this.records.get(id);
    return record?.deleted ? record.sourceVersion : undefined;
  }

  /** Apply the declared deletion strategy. Called through the writer handle only. */
  remove(id: string, sourceVersion: string): void {
    // Protocol validation already refused a projection without a deletion block.
    const strategy = this.declaration.deletion?.strategy;
    if (strategy === 'not-applicable' || strategy === undefined) {
      throw new DarcError(
        'deletion-not-applicable',
        `darc: the contract declares no deletion strategy: import ${this.declaration.id}`,
      );
    }
    const deletedVersion = this.deletionVersion(id);
    if (deletedVersion !== undefined && this.order(sourceVersion, deletedVersion) === 0) return;
    const currentVersion = this.currentVersion(id);
    if (currentVersion !== undefined && !this.orderVerdict(currentVersion, sourceVersion, id)) return;

    const existing = this.records.get(id);
    switch (strategy) {
      case 'hard-delete':
        this.records.delete(id);
        this.lastKey.delete(id);
        this.deleteVersion.set(id, sourceVersion);
        return;
      case 'tombstone': {
        const now = cloneDate(this.clock());
        const base: DarcRecord<T> = existing ?? {
          id,
          value: undefined as unknown as T,
          provenance: this.declaration.from.export,
          observedAt: now,
          freshness: 'fresh',
          sourceVersion,
          deleted: false,
        };
        this.records.set(id, { ...base, deleted: true, deletedAt: now, sourceVersion });
        return;
      }
      default:
        // retain: the visible copy stays exactly as it was, while the deletion
        // version advances the ordering watermark. A delayed pre-deletion write
        // therefore cannot mutate the retained value under ignore-older or reject.
        this.deleteVersion.set(id, sourceVersion);
    }
  }
}

/**
 * The single component the local model names as allowed to update the
 * projection. It is obtained once, from {@link Projection.writer}.
 */
export class ProjectionWriter<T> {
  constructor(private readonly projection: Projection<T>) {}

  /** The writer identity the local model declares. */
  name(): string {
    return this.projection.contract().localModel?.writer ?? '';
  }

  /**
   * Record one update and report whether it changed the local model.
   *
   * It returns false for the two deliveries the contract says to absorb: a
   * repeat of the update already applied, and — under an `ignore-older`
   * late-event strategy — an update older than the copy held. Under `reject` the
   * same older update throws `DarcError('late-update')` instead.
   */
  apply(update: Update<T>): boolean {
    return this.projection.apply(update);
  }

  /**
   * Propagate a producer deletion through the declared strategy:
   *
   * - tombstone retains the record and marks it, so a later reader can tell
   *   "deleted" from "never seen";
   * - hard-delete removes it;
   * - retain leaves the copy in place, which is what the strategy means — the
   *   consumer keeps its own copy after the producer drops the fact;
   * - not-applicable refuses, because the contract says deletion does not happen
   *   for these facts.
   *
   * The source version is compared exactly as an update is, so a late deletion
   * obeys the same late-event rule. Once accepted, the deletion version is an
   * ordering watermark: replaying that deletion or a live delivery at the same
   * producer state is an idempotent no-op, while a newer state may recreate it.
   */
  delete(id: string, sourceVersion: string): void {
    this.projection.remove(id, sourceVersion);
  }
}
