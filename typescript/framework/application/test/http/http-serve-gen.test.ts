import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { generateServeFiles } from '../../src/http/http-serve-gen';

describe('generateServeFiles', () => {
  let projectRoot: string;
  let previousRoot: string | undefined;

  beforeEach(async () => {
    projectRoot = await mkdtemp(join(tmpdir(), 'serve-gen-'));
    previousRoot = process.env.PUTNAMI_PROJECT_ROOT;
    process.env.PUTNAMI_PROJECT_ROOT = projectRoot;
  });

  afterEach(async () => {
    if (previousRoot === undefined) {
      delete process.env.PUTNAMI_PROJECT_ROOT;
    } else {
      process.env.PUTNAMI_PROJECT_ROOT = previousRoot;
    }
    await rm(projectRoot, { recursive: true, force: true });
  });

  it('generates a serve entrypoint that routes through bootstrapServe', async () => {
    await mkdir(join(projectRoot, 'src'), { recursive: true });
    await writeFile(join(projectRoot, 'src', 'main.ts'), 'export const app = () => ({});');
    // No ./serve export and no checked-in src/serve.ts → generation path.
    await writeFile(join(projectRoot, 'package.json'), JSON.stringify({ name: 'demo', main: 'src/main.ts' }));

    const result = generateServeFiles();

    const servePath = result.exports?.['./serve'];
    expect(servePath).toBeDefined();
    const contents = await readFile(servePath as string, 'utf-8');

    expect(contents).toContain("import { bootstrapServe } from '@putnami/application';");
    expect(contents).toContain('await bootstrapServe(app);');
    // The bare start() escape hatch must be gone so failures can't reach Bun's
    // default multi-line printer.
    expect(contents).not.toContain('app().start()');
  });
});
