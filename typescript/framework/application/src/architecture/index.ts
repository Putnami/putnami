/**
 * Architecture Rules as Code (ARC), for TypeScript workloads.
 *
 * The types and the import validator mirror the import half of
 * `go.putnami.dev/protocol/architecture`. They exist so `../darc` can take a
 * declared contract as its configuration and refuse one the gate would reject,
 * rather than trusting that whoever wrote the manifest and whoever wrote the code
 * happened to agree.
 *
 * The manifest, graph, ratchet, and canonical-writer halves stay Go-only: they
 * belong to `putnami architecture validate`, and a second implementation of them
 * would have no reader.
 */

export { parseDuration, validateArchitectureImport } from './contract';
export {
  type AccessMode,
  ARCHITECTURE_DIAGNOSTIC_CODES,
  type ArchitectureDiagnostic,
  type ArchitectureImport,
  type Binding,
  type BindingKind,
  type Consistency,
  type Deletion,
  type DeletionStrategy,
  type ExportReference,
  type FailureMode,
  type LateEventStrategy,
  type LifecycleStatus,
  type LocalModel,
  type LocalModelKind,
  type OrderingStrategy,
  type RebuildStrategy,
  type Transport,
  type TransportKind,
} from './contract.types';
