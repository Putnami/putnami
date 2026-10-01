import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { type ContractManifest, emitTypeScript } from '../../src/contracts';
import { AuthDecision, ClaimName, PrincipalKind } from '../../src/security/identity.constants';

const here = dirname(fileURLToPath(import.meta.url));
// `protocols/identity` authors the canonical IR; the committed `.gen.ts` twin
// next to the Go twin is a pure function of it, and the in-project constants the
// framework adopts mirror the same vocabulary.
const schemaDir = resolve(here, '../../../../../protocols/identity/schema');
const manifestPath = resolve(schemaDir, 'contracts.json');
const twinPath = resolve(schemaDir, 'contracts.gen.ts');

function loadManifest(): ContractManifest {
  return JSON.parse(readFileSync(manifestPath, 'utf8')) as ContractManifest;
}

function enumPairs(manifest: ContractManifest, name: string): Record<string, string> {
  const found = (manifest.enums ?? []).find((e) => e.name === name);
  if (!found) throw new Error(`identity contract declares no enum ${name}`);
  return Object.fromEntries(found.values.map((v) => [v.name, v.value]));
}

describe('identity contract twin', () => {
  it('committed contracts.gen.ts is byte-identical to a fresh emitTypeScript', () => {
    // Fails if the committed twin drifts from the IR — regenerate with the
    // ContractTwinPlugin (`emitTypeScript` is golden-pinned, never reimplemented).
    const got = emitTypeScript(loadManifest());
    const want = readFileSync(twinPath, 'utf8');
    expect(got).toBe(want);
  });

  it('emits the twin deterministically', () => {
    const manifest = loadManifest();
    const first = emitTypeScript(manifest);
    for (let i = 0; i < 20; i++) {
      expect(emitTypeScript(manifest)).toBe(first);
    }
  });

  it('adopted ClaimName/PrincipalKind/AuthDecision constants mirror the contract enums', () => {
    // Pins the in-project vocabulary to the contract: a rename or wire-value
    // change in the manifest fails here instead of silently diverging from the
    // Go framework's adopted constants.
    const manifest = loadManifest();
    expect({ ...ClaimName }).toEqual(enumPairs(manifest, 'ClaimName'));
    expect({ ...PrincipalKind }).toEqual(enumPairs(manifest, 'PrincipalKind'));
    expect({ ...AuthDecision }).toEqual(enumPairs(manifest, 'AuthDecision'));
  });
});
