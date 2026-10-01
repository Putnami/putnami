import { describe, expect, it } from 'bun:test';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { type ContractManifest, emitTypeScript, serializeJSONSchema } from '../../src/contracts';

const here = dirname(fileURLToPath(import.meta.url));
// The contract IR module owns the shared cross-language corpus. The equivalence
// golden covers every node kind and is the single input both emitters consume.
const contractsRoot = resolve(here, '../../../../../protocols/contracts');
const goldenInput = resolve(contractsRoot, 'fixtures/equivalence/contracts.golden.json');
// Emitter goldens live under the contract module's testdata/ so Go tooling
// ignores them and the TS project's biome pass never reaches them.
const testdataDir = resolve(contractsRoot, 'testdata');
const schemaGolden = resolve(testdataDir, 'contracts.schema.json');
const tsGolden = resolve(testdataDir, 'contracts.golden.ts');

const update = process.env.PUTNAMI_UPDATE_GOLDEN === '1';

function loadManifest(): ContractManifest {
  return JSON.parse(readFileSync(goldenInput, 'utf8')) as ContractManifest;
}

describe('contract emitters', () => {
  it('reproduces the Go-emitted JSON Schema byte-for-byte', () => {
    // The schema golden is written by the Go emitter's TestEmitJSONSchema_Golden
    // (json.MarshalIndent + "\n"). The TS serializer must reproduce those exact
    // bytes — this is the cross-language byte-parity guarantee.
    const got = serializeJSONSchema(loadManifest());
    const want = readFileSync(schemaGolden, 'utf8');
    expect(got).toBe(want);
  });

  it('serializes the JSON Schema deterministically', () => {
    const manifest = loadManifest();
    const first = serializeJSONSchema(manifest);
    for (let i = 0; i < 20; i++) {
      expect(serializeJSONSchema(manifest)).toBe(first);
    }
  });

  it('emits deterministic TypeScript types matching the golden', () => {
    const got = emitTypeScript(loadManifest());
    if (update) {
      if (!existsSync(testdataDir)) mkdirSync(testdataDir, { recursive: true });
      writeFileSync(tsGolden, got);
      return;
    }
    const want = readFileSync(tsGolden, 'utf8');
    expect(got).toBe(want);
  });
});
