import type { ArchitectureImport } from '../../../src/architecture/contract.types';

/**
 * The three contracts the DARC evidence tests enforce.
 *
 * They are shared because two tests need the exact same workload: the evidence
 * test, which asserts what the producer emits, and the cross-language fixture in
 * `protocols/architecture/fixtures/conformance/typescript-emitted-capabilities.json`,
 * which the Go detector reads back. If those two drifted apart the fixture would
 * stop describing anything.
 */

export const REFERENCE_CONTRACT: ArchitectureImport = {
  id: 'consumer.producer-reference.v1',
  version: 1,
  from: { domain: 'producer', export: 'producer.contracts.v1' },
  as: 'consumer.producer-contracts',
  mode: 'reference',
  status: 'active',
  facts: ['wire_contract_definitions'],
  justification: 'The consumer holds a stable handle and resolves it at the owner.',
};

export const COMMAND_CONTRACT: ArchitectureImport = {
  id: 'consumer.producer-command.v1',
  version: 1,
  from: { domain: 'producer', export: 'producer.ingest.v1' },
  as: 'consumer.producer-ingest',
  mode: 'command',
  status: 'active',
  facts: ['usage_ingest'],
  transport: { kind: 'event', contract: 'putnami.usage.v1', availability: 'active' },
  justification: 'The consumer may ask the owner to ingest a record; the owner decides whether it happens.',
};

export const PROJECTION_CONTRACT: ArchitectureImport = {
  id: 'consumer.producer-projection.v1',
  version: 1,
  from: { domain: 'producer', export: 'producer.facts.v1' },
  as: 'consumer.producer-copy',
  mode: 'projection',
  status: 'active',
  facts: ['project_identity', 'dependency_edges'],
  bootstrap: { kind: 'in-process', contract: 'putnami.probe.v1', availability: 'active' },
  updates: { kind: 'in-process', contract: 'putnami.probe.v1', availability: 'active' },
  consistency: {
    maxStaleness: '24h',
    onMissing: 'fail-closed',
    onStale: 'use-stale',
    ordering: 'source-version',
    sourceVersion: 'probe_digest',
    idempotencyKey: 'identity_digest',
    lateEvents: 'ignore-older',
  },
  localModel: {
    name: 'consumer.producer-copy',
    kind: 'projection',
    sourceIdentity: 'project_identity',
    projectedFields: ['project_identity', 'dependency_edges'],
    localFields: [],
    provenanceField: 'probe_digest',
    observedAtField: 'observed_at',
    freshnessField: 'freshness_state',
    writer: 'consumer.loader',
    rebuildable: true,
    rebuild: 'bootstrap',
  },
  deletion: { strategy: 'not-applicable' },
  justification: 'The consumer keeps a rebuildable local copy so read-only paths answer without paying a probe.',
};
