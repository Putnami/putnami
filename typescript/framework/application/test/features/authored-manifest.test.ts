import { afterEach, describe, expect, it } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
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

  function writeDocuments(documents: Record<string, unknown>, root?: string): string {
    const target = root ?? mkdtempSync(join(tmpdir(), 'authored-manifest-'));
    if (!root) roots.push(target);
    mkdirSync(join(target, 'features'), { recursive: true });
    for (const [name, document] of Object.entries(documents)) {
      writeFileSync(join(target, 'features', name), JSON.stringify(document));
    }
    return target;
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

  it('merges the feature documents under features/ in filename order', () => {
    const root = writeDocuments({
      'invoice-refund.json': {
        protocolVersion: 2,
        namespace: 'billing',
        feature: { ...feature, id: 'billing/invoice-refund' },
      },
      'invoice-export.json': { protocolVersion: 2, namespace: 'billing', feature },
    });

    const catalog = readAuthoredFeatureManifest(root);

    expect(catalog?.features.map((entry) => entry.id)).toEqual(['billing/invoice-export', 'billing/invoice-refund']);
  });

  it('names the feature document a malformed requirement comes from', () => {
    const root = writeDocuments({
      'invoice-export.json': { protocolVersion: 2, namespace: 'billing', feature: { ...feature, requirements: [{}] } },
    });

    expect(() => readAuthoredFeatureManifest(root)).toThrow('features/invoice-export.json feature.requirements[0].id');
  });

  it('refuses a root that holds both putnami.features.json and features/', () => {
    const root = write({ protocolVersion: 2, namespace: 'billing', features: [feature] });
    writeDocuments({ 'invoice-export.json': { protocolVersion: 2, namespace: 'billing', feature } }, root);

    expect(() => readAuthoredFeatureManifest(root)).toThrow('never both');
  });

  it('reads no feature from a features/ directory that is a nested project', () => {
    const root = writeDocuments({ 'putnami.json': { name: 'features' }, 'package.json': {} });

    expect(readAuthoredFeatureManifest(root)).toBeUndefined();
  });
});
