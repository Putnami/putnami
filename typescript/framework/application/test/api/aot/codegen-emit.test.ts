import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { generateApiLoader } from '../../../src/api/api-codegen';

// Write generated artifacts under the package's (gitignored) .gen, and import the
// fixture from the package tree so its `@putnami/runtime` / src imports resolve.
const packageRoot = join(import.meta.dir, '..', '..', '..');
const fixtures = join(import.meta.dir, 'fixtures');
const original = process.env['PUTNAMI_PROJECT_ROOT'];

describe('generateApiLoader — build-time AOT emission', () => {
  beforeAll(() => {
    process.env['PUTNAMI_PROJECT_ROOT'] = packageRoot;
  });
  afterAll(() => {
    if (original === undefined) {
      delete process.env.PUTNAMI_PROJECT_ROOT;
    } else {
      process.env['PUTNAMI_PROJECT_ROOT'] = original;
    }
    rmSync(join(packageRoot, '.gen', 'test', 'api', 'aot'), { recursive: true, force: true });
  });

  it('inlines validators for a simple endpoint when aot is enabled', async () => {
    const { apiLoaderPath } = await generateApiLoader(fixtures, undefined, false, true);
    const source = readFileSync(apiLoaderPath, 'utf8');
    expect(source).toContain('query:');
    expect(source).toContain('Number.isInteger'); // the inlined Int check
    expect(source).toContain('fallback(raw)'); // anomalies delegate to the generic validator
  });

  it('emits no validators when aot is disabled (default)', async () => {
    const { apiLoaderPath } = await generateApiLoader(fixtures, undefined, false, false);
    const source = readFileSync(apiLoaderPath, 'utf8');
    expect(source).not.toContain('Number.isInteger');
    expect(source).not.toContain('fallback(raw)');
  });

  it('introspects the method export, not default, to match runtime register()', async () => {
    // fixtures-both exports default (Uuid, not inlineable) AND GET (Int, inlineable).
    // Runtime register() uses the GET export, so the codegen must compile GET's schema.
    const both = join(import.meta.dir, 'fixtures-both');
    const { apiLoaderPath } = await generateApiLoader(both, undefined, false, true);
    const source = readFileSync(apiLoaderPath, 'utf8');
    expect(source).toContain('Number.isInteger'); // GET's Int validator → resolved GET, not default
  });
});
