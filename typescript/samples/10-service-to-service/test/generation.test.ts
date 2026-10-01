import { generateTypeScriptClient, readOpenApiSource } from '@putnami/client/generator';
import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

// Generation cell of the cross-language interop matrix, provider side: what the committed
// clients are worth as evidence. The Go sample proves the same two properties
// in service/clientsync_test.go; this is the TypeScript half.

const PROJECT = join(import.meta.dir, '..');
const CONTRACT = join(PROJECT, 'schema', 'openapi.json');
const TS_CLIENT = join(PROJECT, 'clients', 'ts');

interface GeneratedManifest {
  readonly contractSha256: string;
  readonly files: ReadonlyArray<{ readonly path: string; readonly sha256: string }>;
  readonly operations: ReadonlyArray<{ readonly operationId: string }>;
}

function committedManifest(): GeneratedManifest {
  return JSON.parse(readFileSync(join(TS_CLIENT, 'client.putnami.json'), 'utf8')) as GeneratedManifest;
}

function sha256(content: string): string {
  return createHash('sha256').update(content).digest('hex');
}

/** The feature this sample owns in putnami.features.json. */
const FEATURE = 'samples/ts-first-party-client-matrix';

describe('generated client', () => {
  specTest(
    'renders the same file set and the same operations as what is committed',
    {
      feature: FEATURE,
      requirement: 'generation-is-deterministic-and-a-break-is-detected',
      check: 'the-committed-surface-is-what-the-generator-renders',
    },
    () => {
      // The same emitter the workspace clientgen command runs, on the same
      // committed contract. Byte equality belongs to that command — the
      // committed bytes are the formatter's output, and the workspace guard
      // compares them after it runs — so what this level owns is the surface:
      // the files that exist and the operations they carry.
      const contract = readFileSync(CONTRACT, 'utf8');
      const files = generateTypeScriptClient(readOpenApiSource(contract, { mode: 'firstParty' }), {
        packageName: '@example/items-client',
      });
      expect(files.length).toBeGreaterThan(0);

      const manifest = committedManifest();
      expect(files.map((file) => file.path).sort()).toEqual([...manifest.files.map((file) => file.path)].sort());

      // Every declared operation is rendered as a method a consumer can call. An
      // operation the emitter dropped would leave the manifest owning a method
      // that no longer exists.
      const rendered = files.map((file) => file.content).join('\n');
      for (const operation of manifest.operations) {
        expect(rendered).toContain(operation.operationId);
      }
    },
  );

  specTest(
    'binds the committed bytes to the contract they came from',
    {
      feature: FEATURE,
      requirement: 'generation-is-deterministic-and-a-break-is-detected',
      check: 'the-manifest-owns-the-current-contract-and-bytes',
    },
    () => {
      const contract = readFileSync(CONTRACT, 'utf8');
      const manifest = committedManifest();

      // The manifest owns one exact contract and one exact set of bytes. That
      // pairing is what makes a contract change detectable rather than silent:
      // regenerating from a changed contract moves the hash, and the workspace
      // clientgen guard compares it with the tree.
      expect(manifest.contractSha256).toBe(sha256(contract));
      expect(manifest.files.length).toBeGreaterThan(0);
      for (const file of manifest.files) {
        expect({ path: file.path, sha256: file.sha256 }).toEqual({
          path: file.path,
          sha256: sha256(readFileSync(join(TS_CLIENT, file.path), 'utf8')),
        });
      }
    },
  );

  specTest(
    'detects a contract break instead of rendering through it',
    {
      feature: FEATURE,
      requirement: 'generation-is-deterministic-and-a-break-is-detected',
      check: 'a-removed-operation-is-detected-rather-than-rendered-through',
    },
    () => {
      // A removed operation is the break a consumer must not discover at run
      // time. Rendering the amputated contract produces different bytes and a
      // different contract hash, so both halves of the committed pair stop
      // matching — which is exactly what the guard reads.
      const contract = readFileSync(CONTRACT, 'utf8');
      const amputated = readOpenApiSource(contract, { mode: 'firstParty' });
      for (const service of amputated.services) {
        service.methods = service.methods.filter((method) => method.path !== '/whoami');
      }
      amputated.services = amputated.services.filter((service) => service.methods.length > 0);

      const files = generateTypeScriptClient(amputated, { packageName: '@example/items-client' });
      const manifest = committedManifest();

      // The client of the removed operation is gone from the render, while the
      // committed manifest still owns it.
      expect(files.some((file) => file.path.endsWith('whoami-client.ts'))).toBe(false);
      expect(manifest.files.some((file) => file.path.endsWith('whoami-client.ts'))).toBe(true);
      expect(manifest.operations.some((operation) => operation.operationId === 'getWhoami')).toBe(true);

      // Every file the break touched now hashes differently from the committed
      // one, so no path through the guard leaves the tree looking current.
      const index = files.find((file) => file.path === 'src/index.ts');
      expect(index).toBeDefined();
      const committedIndex = manifest.files.find((file) => file.path === 'src/index.ts');
      expect(sha256(index?.content ?? '')).not.toBe(committedIndex?.sha256);
    },
  );
});
