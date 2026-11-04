import { afterEach, describe, expect, it } from 'bun:test';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { readAuthoredFeatureManifest } from '../../src/features/authored-manifest';

describe('readAuthoredFeatureManifest', () => {
  const roots: string[] = [];

  afterEach(() => {
    for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true });
  });

  function write(manifest: unknown): string {
    const root = mkdtempSync(join(tmpdir(), 'authored-manifest-'));
    roots.push(root);
    writeFileSync(join(root, 'putnami.features.json'), JSON.stringify(manifest));
    return root;
  }

  const feature = {
    id: 'billing/invoice-export',
    type: 'feature',
    name: 'Export',
    outcome: 'O',
    owner: 'o',
    target: 'modeled',
  };

  it('reads a version 3 manifest whose feature names an external spec', () => {
    const root = write({
      protocolVersion: 3,
      namespace: 'billing',
      features: [{ ...feature, spec: { format: 'openspec', path: 'openspec/specs/invoice' } }],
    });

    const catalog = readAuthoredFeatureManifest(root);

    expect(catalog?.features.map((entry) => entry.id)).toEqual(['billing/invoice-export']);
  });

  it('refuses a protocol version it does not know', () => {
    const root = write({ protocolVersion: 4, namespace: 'billing', features: [feature] });

    expect(() => readAuthoredFeatureManifest(root)).toThrow('unsupported protocolVersion 4');
  });
});
