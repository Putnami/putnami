import { afterEach, beforeEach, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { StaticPlugin } from '../../src/static/static.plugin';

/**
 * The build-generate key reads the public/ folder of every project in the
 * workload's dependency closure, and nothing else a scan root could name. So
 * generate stages a scan root only from such a folder, and refuses one
 * elsewhere instead of staging bytes a cache hit would not refresh.
 *
 * The temporary tree is a workspace: `workload/` is the project being built,
 * whose root carries no putnami.json, as in the framework's other generate
 * tests, and `libs/fonts/` is a library project that carries one.
 */
describe('static scan roots', () => {
  const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
  let workspace: string;
  let workload: string;
  let library: string;

  beforeEach(() => {
    workspace = realpathSync(mkdtempSync(join(tmpdir(), 'putnami-static-scan-root-')));
    workload = join(workspace, 'workload');
    library = join(workspace, 'libs', 'fonts');
    mkdirSync(workload, { recursive: true });
    mkdirSync(library, { recursive: true });
    writeFileSync(join(library, 'putnami.json'), '{}');
    process.env['PUTNAMI_PROJECT_ROOT'] = workload;
  });

  afterEach(() => {
    if (originalProjectRoot === undefined) delete process.env.PUTNAMI_PROJECT_ROOT;
    else process.env['PUTNAMI_PROJECT_ROOT'] = originalProjectRoot;
    rmSync(workspace, { recursive: true, force: true });
  });

  function writeFile(path: string, content: string): void {
    mkdirSync(join(path, '..'), { recursive: true });
    writeFileSync(path, content);
  }

  const staged = (relative: string) => join(workload, '.gen', 'public', relative);

  specTest(
    'stages a scan root under the public/ folder of the workload or of a library project',
    {
      feature: 'typescript/http-services',
      requirement: 'static-scan-root',
      check: 'a-scan-root-under-a-project-public-folder-is-staged',
    },
    async () => {
      writeFile(join(workload, 'public', 'index.html'), 'home');
      writeFile(join(library, 'public', 'fonts', 'inter.woff2'), 'inter');

      await new StaticPlugin({ scanPath: join(workload, 'public'), compress: 0 }).generate();
      // A sub-folder of a library's public/ is under it, so it is keyed too.
      await new StaticPlugin({ scanPath: join(library, 'public', 'fonts'), compress: 0 }).generate();

      expect(readFileSync(staged('index.html'), 'utf8')).toBe('home');
      expect(readFileSync(staged('inter.woff2'), 'utf8')).toBe('inter');
    },
  );

  specTest(
    'fails generate for a scan root outside every public/ folder, naming the folder to move it under',
    {
      feature: 'typescript/http-services',
      requirement: 'static-scan-root',
      check: 'a-scan-root-outside-every-public-folder-fails-generate',
    },
    async () => {
      writeFile(join(library, 'fonts', 'inter.woff2'), 'inter');
      writeFile(join(workload, 'assets', 'logo.svg'), '<svg/>');
      // A folder named public/ whose parent is no project is not keyed either.
      writeFile(join(workspace, 'shared', 'public', 'robots.txt'), 'robots');

      const generate = (scanPath: string) => new StaticPlugin({ scanPath, compress: 0 }).generate();

      await expect(generate(join(library, 'fonts'))).rejects.toThrow(
        "StaticPlugin: the scan root '../libs/fonts/fonts' lies outside every project's public/ folder",
      );
      await expect(generate(join(library, 'fonts'))).rejects.toThrow("Move it under '../libs/fonts/public'.");
      await expect(generate(join(workload, 'assets'))).rejects.toThrow("Move it under 'public'.");
      await expect(generate(join(workload, 'assets', 'logo.svg'))).rejects.toThrow(
        "the scan root 'assets/logo.svg' lies outside",
      );
      await expect(generate(join(workspace, 'shared', 'public'))).rejects.toThrow(
        "the scan root '../shared/public' lies outside",
      );
      expect(existsSync(staged('inter.woff2'))).toBe(false);
      expect(existsSync(staged('logo.svg'))).toBe(false);
      expect(existsSync(staged('robots.txt'))).toBe(false);
    },
  );

  specTest(
    'stages nothing and fails nothing for a scan root that does not exist',
    {
      feature: 'typescript/http-services',
      requirement: 'static-scan-root',
      check: 'a-missing-scan-root-stages-nothing',
    },
    async () => {
      const result = await new StaticPlugin({ scanPath: join(library, 'fonts'), compress: 0 }).generate();

      expect(result.assets).toEqual({});
      expect(existsSync(join(workload, '.gen', 'src', 'static', '.static.gen.ts'))).toBe(true);
    },
  );
});
