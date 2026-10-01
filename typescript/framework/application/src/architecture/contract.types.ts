/**
 * The Architecture Rules as Code (ARC) import subtree, mirrored for TypeScript.
 *
 * This is the TypeScript twin of `go.putnami.dev/protocol/architecture`. It
 * covers exactly the half a running workload needs — one import contract and
 * everything under it — because that is what `@putnami/application`'s DARC
 * components are configured with. The manifest, graph, ratchet, and canonical
 * writer halves stay Go-only: they belong to `putnami architecture validate`,
 * which is a Go tool, and a second implementation of them would have no reader.
 *
 * The wire shape is owned by the protocol, not by this file. Every member name
 * and every enum value here is the JSON the Go package emits and accepts, and
 * `protocols/architecture/fixtures/conformance/contract-validation.json` is the
 * corpus both languages run to prove they still agree.
 */

/** Where an import sits in its lifecycle. */
export type LifecycleStatus = 'planned' | 'active' | 'legacy' | 'deprecated';

/** Why the consumer crosses the domain boundary. */
export type AccessMode = 'reference' | 'query' | 'snapshot' | 'projection' | 'command';

/** The carrier category a transport names. */
export type TransportKind = 'none' | 'in-process' | 'api' | 'event' | 'file';

/** What a consumer does when data is missing or past its freshness bound. */
export type FailureMode = 'fail-open' | 'fail-closed' | 'use-stale' | 'unavailable';

/** How concurrent or late updates are sequenced. */
export type OrderingStrategy = 'none' | 'source-version' | 'source-sequence' | 'event-time';

/** What happens to an update older than the copy already held. */
export type LateEventStrategy = 'ignore-older' | 'reject' | 'apply';

/** How a producer deletion reaches a local copy. */
export type DeletionStrategy = 'tombstone' | 'hard-delete' | 'retain' | 'not-applicable';

/** What kind of local representation the consumer keeps. */
export type LocalModelKind = 'projection' | 'snapshot' | 'reference';

/** How a local model is reconstructed from its sources. */
export type RebuildStrategy = 'bootstrap' | 'replay' | 'bootstrap-and-replay' | 'not-applicable';

/** The detector category a binding authorizes. */
export type BindingKind = 'project-dependency';

/** The producer domain and export an import consumes. */
export interface ExportReference {
  readonly domain: string;
  readonly export: string;
}

/** One carrier: its category, the concrete contract it names, and whether that carrier is usable yet. */
export interface Transport {
  readonly kind: TransportKind;
  readonly contract?: string;
  readonly availability: LifecycleStatus;
}

/** The freshness, ordering, and failure behavior a consumer promises. */
export interface Consistency {
  readonly maxStaleness: string;
  readonly onMissing: FailureMode;
  readonly onStale: FailureMode;
  readonly ordering: OrderingStrategy;
  readonly sourceVersion: string;
  readonly idempotencyKey: string;
  readonly lateEvents: LateEventStrategy;
}

/** How a producer deletion propagates to the local copy. */
export interface Deletion {
  readonly strategy: DeletionStrategy;
  readonly tombstoneField?: string;
}

/** The consumer-owned representation of copied facts. */
export interface LocalModel {
  readonly name: string;
  readonly kind: LocalModelKind;
  readonly sourceIdentity: string;
  readonly projectedFields: readonly string[];
  readonly localFields?: readonly string[];
  readonly provenanceField: string;
  readonly observedAtField: string;
  readonly freshnessField: string;
  readonly writer: string;
  readonly rebuildable: boolean;
  readonly rebuild: RebuildStrategy;
}

/**
 * One authorized observable implementation dependency.
 *
 * A binding names two exact project IDs and nothing else. It is the reviewed
 * half of the system: no code and no generator may create one, because a
 * permission that a program can grant itself is not a permission (ADR 0001 of
 * `protocols/architecture`).
 */
export interface Binding {
  readonly kind: BindingKind;
  readonly consumerProject: string;
  readonly producerProject: string;
}

/** One Domain Access & Replication Contract, as declared in a manifest. */
export interface ArchitectureImport {
  readonly id: string;
  readonly version: number;
  readonly from: ExportReference;
  readonly as: string;
  readonly mode: AccessMode;
  readonly status: LifecycleStatus;
  readonly facts: readonly string[];
  readonly transport?: Transport;
  readonly bootstrap?: Transport;
  readonly updates?: Transport;
  readonly consistency?: Consistency;
  readonly deletion?: Deletion;
  readonly localModel?: LocalModel;
  readonly bindings?: readonly Binding[];
  readonly justification: string;
}

/**
 * One finding, in the shape `go.putnami.dev/protocol/diagnostic` carries.
 *
 * A consumer branches on `code`, never on `message`: the code is the frozen
 * vocabulary and the message is written for a person reading a terminal.
 */
export interface ArchitectureDiagnostic {
  readonly severity: 'error' | 'warning' | 'info';
  readonly code: string;
  readonly field: string;
  readonly message: string;
}

/**
 * The closed set of codes the import validator emits, mirroring the constants
 * in `protocols/architecture/strict.go`.
 */
export const ARCHITECTURE_DIAGNOSTIC_CODES = {
  invalidId: 'architecture.invalid_id',
  invalidDomain: 'architecture.invalid_domain',
  invalidProject: 'architecture.invalid_project',
  invalidStatus: 'architecture.invalid_status',
  invalidMode: 'architecture.invalid_mode',
  invalidTransport: 'architecture.invalid_transport',
  invalidFact: 'architecture.invalid_fact',
  invalidConsistency: 'architecture.invalid_consistency',
  invalidProjection: 'architecture.invalid_projection',
  invalidDeletion: 'architecture.invalid_deletion',
  invalidBinding: 'architecture.invalid_binding',
  duplicateBinding: 'architecture.duplicate_binding',
} as const;
