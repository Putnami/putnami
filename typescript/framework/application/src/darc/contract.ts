/**
 * The pieces every DARC component shares: the refusal vocabulary, the record
 * shape a copied fact carries, and the construction-time verdicts.
 *
 * The Go twin is `go.putnami.dev/protocol/architecture/darc/contract.go`, and the two are held to
 * `protocols/architecture/fixtures/conformance/*-behavior.json` — one corpus of
 * contracts and ordered operations that both runtimes execute.
 */

import { validateArchitectureImport } from '../architecture/contract';
import type {
  AccessMode,
  ArchitectureDiagnostic,
  ArchitectureImport,
  Consistency,
  FailureMode,
  Transport,
} from '../architecture/contract.types';
import { parseDuration } from '../architecture/contract';

/**
 * Which clause of the contract refused.
 *
 * A caller branches on this rather than on a message, the same way the Go twin
 * branches on a sentinel error: the clause is the frozen vocabulary and the
 * wording is not.
 */
export type DarcErrorCode =
  /** The projected fact is not available, under `onMissing` fail-closed or unavailable. */
  | 'missing'
  /** The copy is older than `maxStaleness` and `onStale` is fail-closed. */
  | 'stale'
  /** The update is older than the local source version and `lateEvents` is reject. */
  | 'late-update'
  /** A second component asked for the writer handle the local model hands out once. */
  | 'writer-claimed'
  /** The claim used a name the local model does not name as its writer. */
  | 'not-the-writer'
  /** The contract's deletion strategy is not-applicable. */
  | 'deletion-not-applicable'
  /** A snapshot version was re-attached with different content. */
  | 'immutable'
  /** The contract, or a carrier it depends on, is not active. */
  | 'not-active'
  /** The import does not name that fact. */
  | 'fact-not-imported';

/** A refusal that names the clause of the contract that produced it. */
export class DarcError extends Error {
  readonly code: DarcErrorCode;
  /**
   * The record the read refused, when there is one.
   *
   * A fail-closed stale read still returns what it refused, so a caller that
   * wants to log the copy it would not serve can.
   */
  readonly record?: DarcRecord<unknown>;

  constructor(code: DarcErrorCode, message: string, record?: DarcRecord<unknown>) {
    super(message);
    this.name = 'DarcError';
    this.code = code;
    this.record = record;
  }
}

/** A contract a component refuses to run at all. */
export class ContractError extends Error {
  /** The contract's ID, empty when the contract does not have one. */
  readonly importId: string;
  /** The protocol's own findings, when the contract failed protocol validation. */
  readonly diagnostics: readonly ArchitectureDiagnostic[];
  /** A refusal the protocol cannot express — a mode mismatch, or a rebuild strategy with no hook. */
  readonly reason: string;

  constructor(importId: string, reason: string, diagnostics: readonly ArchitectureDiagnostic[] = []) {
    const name = importId === '' ? '<unnamed>' : importId;
    super(
      diagnostics.length === 0
        ? `darc: contract ${name} cannot be enforced: ${reason}`
        : [
            `darc: contract ${name} has ${diagnostics.length} protocol violation(s):`,
            ...diagnostics.map((diagnostic) => `  - ${diagnostic.field}: ${diagnostic.message} [${diagnostic.code}]`),
          ].join('\n'),
    );
    this.name = 'ContractError';
    this.importId = importId;
    this.diagnostics = diagnostics;
    this.reason = reason;
  }
}

/**
 * How a read describes the copy it returned.
 *
 * The vocabulary is this module's, not the protocol's: a manifest names the
 * FIELD that carries freshness (`localModel.freshnessField`) and deliberately
 * leaves its values to the consumer, because what counts as usable differs per
 * contract. These two are the only distinction a staleness bound can support.
 */
export type Freshness = 'fresh' | 'stale';

/**
 * One copied fact with the three metadata fields the local model is required to
 * declare. They are structure rather than convention on purpose: a projection
 * whose provenance or observation time lived in a comment could not be checked.
 */
export interface DarcRecord<T> {
  /** The value of the declared source identity. */
  readonly id: string;
  /** The projected copy. */
  readonly value: T;
  /** The producer contract this copy came from — the export the import names. */
  readonly provenance: string;
  /** When the source state this copy reflects was observed. */
  readonly observedAt: Date;
  /** The read's verdict against the declared staleness bound. */
  readonly freshness: Freshness;
  /** The producer state version this copy carries. */
  readonly sourceVersion: string;
  /** True for a tombstoned record: the producer removed the fact and the contract retains the marker. */
  readonly deleted: boolean;
  /** When the tombstone was applied, absent for a live record. */
  readonly deletedAt?: Date;
}

/**
 * Makes one independent value for DARC storage or a read result.
 *
 * The default is the platform structured-clone algorithm. Supply one for values
 * it cannot clone (such as functions) or whose application-specific prototype
 * must be retained. A custom cloner owns the promise that the returned mutable
 * graph does not alias its input.
 */
export type ValueCloner<T> = (value: T) => T;

/** Resolve the public cloning option and make the default failure actionable. */
export function resolveValueCloner<T>(cloner: ValueCloner<T> | undefined, option: string): ValueCloner<T> {
  if (cloner) return cloner;
  return (value: T): T => {
    try {
      return structuredClone(value);
    } catch (cause) {
      throw new TypeError(
        `darc: the value cannot be cloned by the structured-clone algorithm; supply ${option} for this value type`,
        { cause },
      );
    }
  };
}

/** Copy one mutable timestamp at an ingress or egress boundary. */
export function cloneDate(value: Date): Date {
  return new Date(value.getTime());
}

/** Copy every caller-mutable member of a record before exposing it. */
export function cloneRecord<T>(record: DarcRecord<T>, cloner: ValueCloner<T>): DarcRecord<T> {
  return {
    ...record,
    value: cloner(record.value),
    observedAt: cloneDate(record.observedAt),
    ...(record.deletedAt === undefined ? {} : { deletedAt: cloneDate(record.deletedAt) }),
  };
}

/** Compares two producer state versions: negative when left is older, zero when equal. */
export type VersionOrder = (left: string, right: string) => number;

/**
 * The default comparator: code point order.
 *
 * It is correct for every version shape whose order IS its time order — RFC 3339
 * timestamps, ULIDs, ordered UUIDs, zero-padded counters — and WRONG for
 * unpadded decimal integers, where "10" sorts before "9". A contract using those
 * must pass its own comparator; the default is stated rather than guessed because
 * a comparator that silently mis-orders makes the late-event rule report the
 * opposite of what happened.
 *
 * Code points rather than JavaScript's `<`, which compares UTF-16 code units and
 * would order a supplementary character before U+E000 where the Go twin's byte
 * comparison orders it after.
 */
export function lexicalVersionOrder(left: string, right: string): number {
  const leftPoints = Array.from(left);
  const rightPoints = Array.from(right);
  const shared = Math.min(leftPoints.length, rightPoints.length);
  for (let index = 0; index < shared; index += 1) {
    const leftPoint = leftPoints[index]?.codePointAt(0) ?? 0;
    const rightPoint = rightPoints[index]?.codePointAt(0) ?? 0;
    if (leftPoint !== rightPoint) return leftPoint < rightPoint ? -1 : 1;
  }
  if (leftPoints.length === rightPoints.length) return 0;
  return leftPoints.length < rightPoints.length ? -1 : 1;
}

/**
 * Apply the protocol's own verdict to one import, then the mode-specific
 * requirement of the component being built.
 *
 * The protocol's rules are not restated here. `validateArchitectureImport` is
 * the single interpretation this package defers to, exactly as the Go twin
 * defers to `archproto.ValidateManifest`.
 */
export function validateContract(contract: ArchitectureImport, mode: AccessMode): void {
  if (contract.mode !== mode) {
    throw new ContractError(
      contract.id ?? '',
      `declares ${contract.mode} access, and this component enforces ${mode} access`,
    );
  }
  const diagnostics = validateArchitectureImport(contract).filter((diagnostic) => diagnostic.severity === 'error');
  if (diagnostics.length > 0) {
    throw new ContractError(contract.id ?? '', 'the contract failed protocol validation', diagnostics);
  }
  const bound = contract.consistency ? parseDuration(contract.consistency.maxStaleness) : undefined;
  const usesDateFreshness = mode === 'projection' || mode === 'snapshot';
  if (usesDateFreshness && bound !== undefined && (!Number.isInteger(bound) || bound < 1)) {
    throw new ContractError(
      contract.id ?? '',
      `declares maxStaleness ${JSON.stringify(contract.consistency?.maxStaleness)}, but the TypeScript runtime requires a positive whole number of milliseconds because Date cannot observe fractional milliseconds`,
    );
  }
}

/**
 * Refuse a contract that is not live.
 *
 * A planned import is a TARGET: the protocol already forbids it from claiming a
 * current project binding, and running it would be the same claim in code. The
 * carriers are checked with it because an active import carried by a planned
 * transport is a design that has not shipped, whichever half is behind.
 */
export function requireActive(contract: ArchitectureImport, ...carriers: (Transport | undefined)[]): void {
  if (contract.status !== 'active') {
    throw new DarcError('not-active', `darc: import ${contract.id} is ${contract.status}`);
  }
  for (const carrier of carriers) {
    if (carrier && carrier.availability !== 'active') {
      throw new DarcError(
        'not-active',
        `darc: import ${contract.id} is carried by a ${carrier.availability} transport`,
      );
    }
  }
}

/**
 * Resolve the declared freshness bound, in milliseconds.
 *
 * The protocol has already accepted it as a positive duration, so a parse
 * failure here is unreachable rather than a default.
 */
export function staleness(consistency: Consistency | undefined): number {
  if (!consistency) return 0;
  return parseDuration(consistency.maxStaleness) ?? 0;
}

/**
 * Record the read's verdict against the declared bound.
 *
 * Separate from the refusal below because two callers need only the stamp: an
 * enumeration is asking what the projection holds, and a version-addressed
 * snapshot read is a caller naming the exact state it wants.
 */
export function stampFreshness<T>(record: DarcRecord<T>, now: Date, bound: number): DarcRecord<T> {
  if (bound <= 0) return { ...record, freshness: 'fresh' };
  const stale = now.getTime() > record.observedAt.getTime() + bound;
  return { ...record, freshness: stale ? 'stale' : 'fresh' };
}

/**
 * Report what a stale copy means under the declared stale behavior.
 *
 * fail-closed refuses. Every other value proceeds, and the record has already
 * been stamped: `use-stale` and `fail-open` differ in intent, not in what the
 * runtime can do, so the difference a consumer acts on is the stamp.
 */
export function staleVerdict<T>(record: DarcRecord<T>, bound: number, onStale: FailureMode): void {
  if (record.freshness !== 'stale' || onStale !== 'fail-closed') return;
  throw new DarcError(
    'stale',
    `darc: the projected fact is older than the declared freshness bound: observed at ${record.observedAt.toISOString()}, bound ${bound}ms`,
    record as DarcRecord<unknown>,
  );
}

/**
 * Report what a read of an absent fact returns under the declared missing
 * behavior. fail-open is the only value that lets a caller continue without an
 * error, which is what it means.
 */
export function missingVerdict(contract: ArchitectureImport, detail = ''): void {
  if (!contract.consistency || contract.consistency.onMissing === 'fail-open') return;
  throw new DarcError(
    'missing',
    `darc: the projected fact is not available: import ${contract.id} declares onMissing ${contract.consistency.onMissing}${detail}`,
  );
}
