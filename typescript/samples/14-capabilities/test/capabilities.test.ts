import { beforeAll, describe, expect, it } from 'bun:test';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

// The sample root and the paths the producer reads (version stamp) and writes
// (the manifest under .gen/schema/, mirroring Go's capabilities.EmitDir).
const SAMPLE_ROOT = join(import.meta.dir, '..');
const VERSION_PATH = join(SAMPLE_ROOT, '.gen', 'version.json');
const EMITTED_PATH = join(SAMPLE_ROOT, '.gen', 'schema', 'capabilities.json');
const EVIDENCE_PATH = join(SAMPLE_ROOT, '.gen', 'schema', 'feature-evidence', 'typescript-framework.json');
const DESIGN_GRAPH_PATH = join(SAMPLE_ROOT, '.gen', 'design', 'graph.json');
const GOLDEN_PATH = join(import.meta.dir, 'capabilities.golden.json');
let emittedManifestAssetPath: string | undefined;

// Point project resolution at this sample and stamp the stable project identity
// the real build scheduler writes before generate. The producer deliberately
// ignores the volatile per-commit version in this file.
process.env.PUTNAMI_PROJECT_ROOT = SAMPLE_ROOT;
process.env.PWD = SAMPLE_ROOT;

describe('capabilities manifest emission', () => {
  beforeAll(async () => {
    mkdirSync(join(SAMPLE_ROOT, '.gen'), { recursive: true });
    writeFileSync(
      VERSION_PATH,
      `${JSON.stringify({
        name: '@example/14-capabilities',
        capabilityRoot: '.',
        capabilityPackages: [
          {
            package: '@example/14-capabilities',
            version: '0.1.0',
            evidencePath: 'putnami.json',
            sourceRoot: '.',
            sourceBinding: `source-v1:sha256:${'0'.repeat(64)}`,
          },
        ],
      })}\n`,
    );
    rmSync(EMITTED_PATH, { force: true });
    rmSync(EVIDENCE_PATH, { force: true });
    rmSync(DESIGN_GRAPH_PATH, { force: true });

    // Import after env is set so getProjectRoot resolves to this sample.
    const { app } = await import('../src/main');
    const result = await app().build();
    emittedManifestAssetPath = result.assets?.['schema/capabilities.json'];
  });

  it('emits the manifest under .gen/schema/ matching the committed golden', () => {
    const emitted = readFileSync(EMITTED_PATH, 'utf8');
    const golden = readFileSync(GOLDEN_PATH, 'utf8');
    expect(emitted).toBe(golden);
    expect(JSON.parse(emitted).packages).toHaveLength(1);
    expect(emitted).not.toContain('packageVersions');
    expect(emitted).not.toContain('"version"');
  });

  it('reports the manifest as a packaged schema asset', () => {
    expect(emittedManifestAssetPath).toBe(EMITTED_PATH);
  });

  it('derives the feature and migration graph from native composition', () => {
    const graph = JSON.parse(readFileSync(DESIGN_GRAPH_PATH, 'utf8')) as {
      nodes: Array<{ id: string; kind: string }>;
    };
    expect(graph.nodes.some(({ id }) => id === 'feature:capabilities/source-bound-manifest')).toBe(true);
    expect(graph.nodes.some(({ kind }) => kind === 'data.migration')).toBe(true);
  });

  it('does not emit the deprecated feature-evidence authoring projection', () => {
    expect(existsSync(EVIDENCE_PATH)).toBe(false);
  });
});
