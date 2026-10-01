/**
 * The snapshot mode: an immutable, version-addressed copy of another domain's
 * facts.
 *
 * The Go twin is `go.putnami.dev/protocol/architecture/darc/snapshot.go`, and both are held to
 * `protocols/architecture/fixtures/conformance/snapshot-behavior.json`.
 */

import { isDeepStrictEqual } from 'node:util';
import type { ArchitectureImport } from '../architecture/contract.types';
import {
  cloneDate,
  cloneRecord,
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

/** Construction options. */
export interface SnapshotOptions<T = unknown> {
  /** Replaces the comparator that decides which attached version is the latest. */
  readonly versionOrder?: VersionOrder;
  /** Replaces the clock the freshness bound is measured against. */
  readonly clock?: () => Date;
  /**
   * Copies values at storage and read boundaries. Defaults to `structuredClone`;
   * supply this for unsupported values or application-specific prototypes.
   */
  readonly cloneValue?: ValueCloner<T>;
}

/**
 * An immutable, version-addressed copy.
 *
 * It is the mode between a reference and a projection: the consumer attaches a
 * whole producer state under an exact version and reads it back by that version.
 * Nothing is edited in place — a new producer state is a new version — which is
 * what makes a snapshot safe to hold across a request without a freshness race
 * inside it.
 *
 * The declared consistency block still governs the LATEST read: a snapshot older
 * than `maxStaleness` takes the declared stale behavior, and an absent one takes
 * the declared missing behavior. Attaching is the consumer's job, over whichever
 * transport the contract declares.
 */
export class Snapshot<T> {
  private readonly declaration: ArchitectureImport;
  private readonly bound: number;
  private readonly order: VersionOrder;
  private readonly clock: () => Date;
  private readonly cloneValue: ValueCloner<T>;
  private readonly versionMap = new Map<string, DarcRecord<T>>();
  private latestVersion = '';

  /**
   * Build a snapshot from its declared contract. The protocol requires a
   * snapshot import to name one transport and an explicit consistency block. The
   * import and transport must both be active; refusing a planned contract here
   * keeps a target design from becoming runtime state.
   */
  constructor(contract: ArchitectureImport, options: SnapshotOptions<T> = {}) {
    validateContract(contract, 'snapshot');
    requireActive(contract, contract.transport);
    this.declaration = contract;
    this.bound = staleness(contract.consistency);
    this.order = options.versionOrder ?? lexicalVersionOrder;
    this.clock = options.clock ?? (() => new Date());
    this.cloneValue = resolveValueCloner(options.cloneValue, 'SnapshotOptions.cloneValue');
  }

  /** The declaration this snapshot enforces. */
  contract(): ArchitectureImport {
    return this.declaration;
  }

  /**
   * Discard every attached state.
   *
   * Consumers that perform a new authoritative full scan use this before
   * attaching that scan's states, so `latest()` cannot retain a version that
   * disappeared between scans. It deliberately preserves the contract and
   * component identity: reset changes the observed state, not what the
   * component enforces.
   */
  reset(): void {
    this.versionMap.clear();
    this.latestVersion = '';
  }

  /**
   * Record one producer state under its exact version.
   *
   * Re-attaching a version with identical content is accepted and does nothing —
   * a transport that redelivers is not an error. Re-attaching one with DIFFERENT
   * content throws `DarcError('immutable')`: the version is the identity of the
   * state, and letting it change would make every reader that already resolved
   * it wrong.
   */
  attach(version: string, value: T, observedAt?: Date): void {
    if (version === '') {
      throw new Error(`darc: snapshot ${this.declaration.id} cannot attach an unversioned state`);
    }
    const storedValue = this.cloneValue(value);
    const existing = this.versionMap.get(version);
    if (existing) {
      if (!isDeepStrictEqual(existing.value, storedValue)) {
        throw new DarcError(
          'immutable',
          `darc: a snapshot version is immutable: ${this.declaration.id} version ${version} is already attached with different content`,
        );
      }
      return;
    }
    this.versionMap.set(version, {
      id: version,
      value: storedValue,
      provenance: this.declaration.from.export,
      observedAt: cloneDate(observedAt ?? this.clock()),
      freshness: 'fresh',
      sourceVersion: version,
      deleted: false,
    });
    if (this.latestVersion === '' || this.order(version, this.latestVersion) > 0) this.latestVersion = version;
  }

  /**
   * The state attached under an exact version.
   *
   * A version-addressed read carries no staleness verdict, and that is the point
   * of the mode: the caller named the state it wants, so "this state is old" is
   * not news. The record still reports its observation time, and `freshness` is
   * stamped against the declared bound so a caller that cares can see it.
   */
  at(version: string): DarcRecord<T> | undefined {
    const record = this.versionMap.get(version);
    if (!record) return undefined;
    return cloneRecord(stampFreshness(record, this.clock(), this.bound), this.cloneValue);
  }

  /**
   * The newest attached state and the verdict the contract calls for, with the
   * same three answers as {@link Projection.get}: absent under fail-open is
   * `undefined`, absent otherwise throws `DarcError('missing')`, and a state past
   * the declared bound is stamped stale or refused with `DarcError('stale')`.
   */
  latest(): DarcRecord<T> | undefined {
    const record = this.versionMap.get(this.latestVersion);
    if (!record) {
      missingVerdict(this.declaration);
      return undefined;
    }
    const stamped = cloneRecord(stampFreshness(record, this.clock(), this.bound), this.cloneValue);
    staleVerdict(stamped, this.bound, this.declaration.consistency?.onStale ?? 'fail-open');
    return stamped;
  }

  /** Every attached version, oldest first under the configured comparator. */
  versions(): string[] {
    return [...this.versionMap.keys()].sort(this.order);
  }
}
