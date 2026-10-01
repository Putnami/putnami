import { afterEach, describe, expect } from 'bun:test';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { application } from '../../src/application';
import { createCapabilitiesProducer } from '../../src/capabilities/capabilities.producer';
import type { ManifestV2 } from '../../src/capabilities/manifest.types';
import { Command } from '../../src/darc/command';
import { DarcPlugin, evidence } from '../../src/darc/evidence';
import { Projection } from '../../src/darc/projection';
import { Reference } from '../../src/darc/reference';
import { COMMAND_CONTRACT, PROJECTION_CONTRACT, REFERENCE_CONTRACT } from './fixtures/contracts';

/**
 * The evidence half, end to end: a TypeScript workload that enforces a contract
 * emits the row `putnami architecture validate` reads.
 *
 * The detector is `tooling/sdd-extension/internal/sdd/architecture_engine_evidence.go`.
 * It reads `<project>/schema/capabilities.json` for every mapped project and
 * names no language, so what makes a TypeScript project visible to the gate is
 * exactly this: a `domainAccess` collection in the committed v2 manifest.
 */

const SOURCE_BINDING = `source-v1:sha256:${'0'.repeat(64)}`;
const CROSS_LANGUAGE_FIXTURE = join(
  __dirname,
  '../../../../../protocols/architecture/fixtures/conformance/typescript-emitted-capabilities.json',
);
const roots: string[] = [];

afterEach(() => {
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true });
});

function projectRoot(label: string): string {
  const root = mkdtempSync(join(tmpdir(), `putnami-darc-${label}-`));
  roots.push(root);
  return root;
}

function contractsPlugin(): DarcPlugin {
  return new DarcPlugin(
    'consumer-contracts',
    new Reference(REFERENCE_CONTRACT),
    new Command<string>(COMMAND_CONTRACT, () => {}),
    new Projection<string>(PROJECTION_CONTRACT, { bootstrap: () => [] }),
  );
}

/** Emit the committed manifest a scheduler-driven build would write. */
async function emit(root: string, plugin?: DarcPlugin, project = 'example/workload'): Promise<ManifestV2> {
  const producer = createCapabilitiesProducer({
    projectRoot: root,
    project,
    capabilityPackages: [
      {
        package: project,
        version: '1.0.0',
        evidencePath: 'package.json',
        sourceRoot: '.',
        sourceBinding: SOURCE_BINDING,
      },
    ],
    registeredConfigDefinitions: () => [],
  });
  const app = application();
  if (plugin) app.use(plugin);
  await producer.postGenerate?.(app, {});
  return JSON.parse(readFileSync(join(root, '.gen', 'schema', 'capabilities.json'), 'utf8')) as ManifestV2;
}

describe('DARC evidence', () => {
  specTest(
    'projects the declared contract onto the row verbatim',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'darc-runtime-evidence',
      check: 'an-evidence-row-carries-the-declared-contract-verbatim',
    },
    () => {
      const row = evidence(new Projection<string>(PROJECTION_CONTRACT, { bootstrap: () => [] }));
      expect(row).toEqual({
        import: 'consumer.producer-projection.v1',
        mode: 'projection',
        status: 'active',
        transports: [
          { role: 'bootstrap', kind: 'in-process', contract: 'putnami.probe.v1', availability: 'active' },
          { role: 'updates', kind: 'in-process', contract: 'putnami.probe.v1', availability: 'active' },
        ],
        enforced: {
          maxStaleness: '24h',
          onMissing: 'fail-closed',
          onStale: 'use-stale',
          ordering: 'source-version',
          lateEvents: 'ignore-older',
          deletion: 'not-applicable',
          writer: 'consumer.loader',
          rebuild: 'bootstrap',
        },
      });

      // A reference enforces none of the consistency parameters, and saying so
      // with an absent member is honest. An object of empty strings would claim
      // the component applies behaviors the contract never declared.
      const bare = evidence(new Reference(REFERENCE_CONTRACT));
      expect(bare).toEqual({
        import: 'consumer.producer-reference.v1',
        mode: 'reference',
        status: 'active',
      });
    },
  );

  specTest(
    'registers one row per component however often it is registered',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'darc-runtime-evidence',
      check: 'a-registered-component-contributes-exactly-one-row',
    },
    () => {
      const reference = new Reference(REFERENCE_CONTRACT);
      const plugin = new DarcPlugin('consumer-contracts', reference, reference);
      plugin.register(reference, new Reference(REFERENCE_CONTRACT));
      const rows = plugin.domainAccessContracts();
      expect(rows.map((row) => `${row.import} ${row.mode}`)).toEqual(['consumer.producer-reference.v1 reference']);
    },
  );

  specTest(
    'reaches the committed capability manifest the architecture gate reads',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'darc-runtime-evidence',
      check: 'the-emitted-manifest-carries-the-domain-access-rows',
    },
    async () => {
      const manifest = await emit(projectRoot('emitted'), contractsPlugin());
      const rows = manifest.domainAccess ?? [];
      // Ordered by import then mode, so a reader and a diff see a stable list.
      expect(rows.map((row) => row.import)).toEqual([
        'consumer.producer-command.v1',
        'consumer.producer-projection.v1',
        'consumer.producer-reference.v1',
      ]);

      // The mode is the subkind, so one project enforcing two modes of one
      // import keeps two distinct identities instead of colliding.
      const projection = rows.find((row) => row.mode === 'projection');
      expect(projection?.identity).toEqual({
        ownerProject: 'example/workload',
        kind: 'domainAccess',
        subkind: 'projection',
        key: 'consumer.producer-projection.v1',
      });
      // Provenance is stamped by the producer, never by the contributor: a row a
      // reader cannot locate is worse than no row.
      expect(projection?.provenance.project).toBe('example/workload');
      expect(projection?.provenance.sourceKind).toBe('framework');
    },
  );

  specTest(
    'emits nothing at all when no component enforces a contract',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'darc-runtime-evidence',
      check: 'a-project-that-enforces-nothing-emits-no-rows',
    },
    async () => {
      const manifest = await emit(projectRoot('empty'));
      // Absent rather than an empty array: "this project implements nothing" and
      // "this project implements zero contracts" must not look alike to a gate
      // that reports an implementation nobody declared.
      expect(manifest.domainAccess).toBeUndefined();
    },
  );

  specTest(
    'emits the same bytes for the same contracts',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'darc-runtime-evidence',
      check: 'the-emitted-evidence-rows-are-deterministic-bytes',
    },
    async () => {
      const first = projectRoot('deterministic-first');
      const second = projectRoot('deterministic-second');
      await emit(first, contractsPlugin());
      await emit(second, contractsPlugin());
      const read = (root: string) => readFileSync(join(root, '.gen', 'schema', 'capabilities.json'), 'utf8');
      expect(read(first)).toBe(read(second));
      // The assertion is only worth something if the bytes carry the rows.
      expect(read(first)).toContain('"domainAccess"');
    },
  );

  specTest(
    'emits the exact bytes the Go detector is tested against',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'darc-runtime-evidence',
      check: 'the-emitted-manifest-is-the-one-the-go-detector-reads-back',
    },
    async () => {
      // The Go side of this fixture is
      // `tooling/sdd-extension/internal/sdd/architecture_engine_evidence_test.go`,
      // which feeds these exact bytes to the real detector and asserts the rows
      // land in the right domain. Regenerating one side without the other is the
      // drift the fixture exists to catch, so the comparison is byte-for-byte.
      const root = projectRoot('cross-language');
      const manifest = await emit(root, contractsPlugin(), 'consumer');
      expect(manifest).toBeDefined();
      expect(readFileSync(join(root, '.gen', 'schema', 'capabilities.json'), 'utf8')).toBe(
        readFileSync(CROSS_LANGUAGE_FIXTURE, 'utf8'),
      );
    },
  );
});
