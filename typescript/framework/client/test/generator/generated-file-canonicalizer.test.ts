import { afterEach, describe, expect, test } from 'bun:test';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import {
  canonicalizeGeneratedFiles,
  resolveBiomeCli,
  resolveBiomeEnvironment,
} from '../../src/generator/generated-file-canonicalizer';
import type { GeneratedFile } from '../../src/generator/ts/ts-generator';

const tempRoots: string[] = [];

function setupProject(files: Record<string, string>): string {
  const root = mkdtempSync(join(tmpdir(), 'clientgen-canonicalizer-'));
  tempRoots.push(root);
  for (const [relativePath, content] of Object.entries(files)) {
    const path = join(root, relativePath);
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, content);
  }
  return root;
}

afterEach(() => {
  for (const root of tempRoots.splice(0)) rmSync(root, { recursive: true, force: true });
});

describe('canonicalizeGeneratedFiles', () => {
  test('uses a provider-local Biome package before the client dependency', () => {
    const root = setupProject({ 'package.json': JSON.stringify({ private: true }) });
    const biomeCli = join(root, 'node_modules/@biomejs/biome/bin/biome');
    mkdirSync(dirname(biomeCli), { recursive: true });
    writeFileSync(
      join(root, 'node_modules/@biomejs/biome/package.json'),
      JSON.stringify({
        name: '@biomejs/biome',
        version: '99.0.0-provider-fixture',
        exports: { './bin/biome': './bin/biome' },
      }),
    );
    writeFileSync(biomeCli, '// provider-local resolver fixture\n');

    expect(resolveBiomeCli(root)).toBe(biomeCli);
  });

  test('requires a project or workspace Biome configuration', () => {
    const root = setupProject({ 'package.json': JSON.stringify({ private: true }) });

    expect(() =>
      canonicalizeGeneratedFiles(root, join(root, 'clients/ts'), [{ path: 'src/client.ts', content: 'export {}\n' }]),
    ).toThrow(/^clientgen_format_config_missing:/);
  });

  test('maps a positive scheduler budget without overriding an explicit Rayon setting', () => {
    expect(resolveBiomeEnvironment({ PUTNAMI_CPU_BUDGET: '3' })).toEqual({
      PUTNAMI_CPU_BUDGET: '3',
      RAYON_NUM_THREADS: '3',
    });
    expect(resolveBiomeEnvironment({ PUTNAMI_CPU_BUDGET: '3', RAYON_NUM_THREADS: '7' })).toEqual({
      PUTNAMI_CPU_BUDGET: '3',
      RAYON_NUM_THREADS: '7',
    });
    expect(resolveBiomeEnvironment({ PUTNAMI_CPU_BUDGET: '3.5' })).toEqual({ PUTNAMI_CPU_BUDGET: '3.5' });
    expect(resolveBiomeEnvironment({ PUTNAMI_CPU_BUDGET: '0' })).toEqual({ PUTNAMI_CPU_BUDGET: '0' });
  });

  test('does not expose earlier files when a later file cannot be formatted', () => {
    const root = setupProject({
      'biome.json': JSON.stringify({
        root: true,
        vcs: { enabled: false },
        formatter: { enabled: true },
        linter: { enabled: true, rules: { recommended: false } },
      }),
      'clients/ts/src/first.ts': '// existing first\n',
      'clients/ts/src/second.ts': '// existing second\n',
    });

    expect(() =>
      canonicalizeGeneratedFiles(root, join(root, 'clients/ts'), [
        { path: 'src/first.ts', content: 'export const first = true;\n' },
        { path: 'src/second.ts', content: 'export const second = ;\n' },
      ]),
    ).toThrow(/^clientgen_format_failed: Biome format failed for src\/second\.ts/);
    expect(readFileSync(join(root, 'clients/ts/src/first.ts'), 'utf8')).toBe('// existing first\n');
    expect(readFileSync(join(root, 'clients/ts/src/second.ts'), 'utf8')).toBe('// existing second\n');
  });

  test("reports Biome's own diagnostic instead of blaming the provider configuration", () => {
    const root = setupProject({
      'biome.json': JSON.stringify({
        root: true,
        vcs: { enabled: false },
        formatter: { enabled: true },
        linter: { enabled: true, rules: { recommended: false } },
      }),
    });

    // A syntax error in emitted bytes is an emitter defect, not a consumer
    // configuration defect, and the message has to be able to say so.
    let thrown: unknown;
    try {
      canonicalizeGeneratedFiles(root, join(root, 'clients/ts'), [
        { path: 'src/broken.ts', content: 'export const value = input.query?["x"];\n' },
      ]);
    } catch (error) {
      thrown = error;
    }
    expect(thrown).toBeInstanceOf(Error);
    const message = (thrown as Error).message;
    expect(message).toContain('clientgen_format_failed');
    expect(message).not.toContain('fix the provider Biome configuration');
    // Biome's own summary says the bytes did not parse.
    expect(message).toContain('parsing errors');
    // Its code frame quotes the offending source, and this message is printed by
    // the standalone CLI, so the frame never reaches it.
    expect(message).not.toContain('input.query');
    expect(message.split('\n')).toHaveLength(1);
  });

  test('uses the installed Biome with an empty PATH and input larger than a pipe buffer', () => {
    const root = setupProject({
      'package.json': JSON.stringify({ name: 'provider', private: true }),
      'biome.json': JSON.stringify({
        root: true,
        vcs: { enabled: false },
        formatter: { enabled: true },
        linter: {
          enabled: true,
          rules: { recommended: false, style: { useConsistentArrayType: 'error' } },
        },
        javascript: { formatter: { quoteStyle: 'single' } },
      }),
    });
    const source = `const values: Array<string> = ["one"];\nconst payload = "${'x'.repeat(128 * 1024)}";\n`;
    const originalPath = process.env.PATH;

    let generated: GeneratedFile[];
    try {
      process.env.PATH = '';
      generated = canonicalizeGeneratedFiles(root, join(root, 'clients/ts'), [
        { path: 'src/client.ts', content: source },
      ]);
    } finally {
      if (originalPath === undefined) delete process.env.PATH;
      else process.env.PATH = originalPath;
    }

    expect(generated[0].content).toContain("const values: string[] = ['one'];");
    expect(generated[0].content).toContain('x'.repeat(128 * 1024));
    expect(canonicalizeGeneratedFiles(root, join(root, 'clients/ts'), generated)).toEqual(generated);
  });
});
