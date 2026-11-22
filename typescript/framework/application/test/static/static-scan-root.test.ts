import { afterEach, beforeEach, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs';
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
 * tests, `libs/fonts/` is a library project that carries one, and `libs/` is
 * a scope, whose putnami.json configures no project.
 */
describe('static scan roots', () => {
  const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
  const originalWorkspaceRoot = process.env['PUTNAMI_WORKSPACE_ROOT'];
  const originalPrebuildContext = process.env['PUTNAMI_PREBUILD_CONTEXT'];
  let workspace: string;
  let workload: string;
  let library: string;
  const outsideRoots: string[] = [];

  beforeEach(() => {
    workspace = realpathSync(mkdtempSync(join(tmpdir(), 'putnami-static-scan-root-')));
    workload = join(workspace, 'workload');
    library = join(workspace, 'libs', 'fonts');
    mkdirSync(workload, { recursive: true });
    mkdirSync(library, { recursive: true });
    writeFileSync(join(library, 'putnami.json'), '{}');
    writeFileSync(
      join(workspace, 'libs', 'putnami.json'),
      JSON.stringify({ $schema: 'https://putnami.dev/schemas/putnami-scope.json', tags: ['ts'] }),
    );
    process.env['PUTNAMI_PROJECT_ROOT'] = workload;
    // As the pre-build hook of a build sets them.
    process.env['PUTNAMI_WORKSPACE_ROOT'] = workspace;
    process.env['PUTNAMI_PREBUILD_CONTEXT'] = 'true';
  });

  afterEach(() => {
    if (originalProjectRoot === undefined) delete process.env.PUTNAMI_PROJECT_ROOT;
    else process.env['PUTNAMI_PROJECT_ROOT'] = originalProjectRoot;
    if (originalWorkspaceRoot === undefined) delete process.env['PUTNAMI_WORKSPACE_ROOT'];
    else process.env['PUTNAMI_WORKSPACE_ROOT'] = originalWorkspaceRoot;
    if (originalPrebuildContext === undefined) delete process.env['PUTNAMI_PREBUILD_CONTEXT'];
    else process.env['PUTNAMI_PREBUILD_CONTEXT'] = originalPrebuildContext;
    rmSync(workspace, { recursive: true, force: true });
    for (const outside of outsideRoots.splice(0)) rmSync(outside, { recursive: true, force: true });
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
    'stages a scan root inside an installed package or the generated public folder, which the key also reads',
    {
      feature: 'typescript/http-services',
      requirement: 'static-scan-root',
      check: 'a-scan-root-under-a-project-public-folder-is-staged',
    },
    async () => {
      // An installed package is pinned by the workspace lockfiles the key reads.
      writeFile(join(workload, 'node_modules', '@acme', 'fonts', 'fonts', 'mono.woff2'), 'mono');
      // An earlier hook or options.generate.assets wrote it, and the key reads its source.
      writeFile(join(workload, '.gen', 'public', 'vendor', 'chart.js'), 'chart');

      await new StaticPlugin({
        scanPath: join(workload, 'node_modules', '@acme', 'fonts', 'fonts'),
        compress: 0,
      }).generate();
      await new StaticPlugin({ scanPath: '.gen/public', compress: 0 }).generate();

      expect(readFileSync(staged('mono.woff2'), 'utf8')).toBe('mono');
      expect(readFileSync(staged('vendor/chart.js'), 'utf8')).toBe('chart');
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
      // A workspace package linked into node_modules is the library itself,
      // which no lockfile pins: its real path decides.
      mkdirSync(join(workload, 'node_modules', '@acme'), { recursive: true });
      symlinkSync(library, join(workload, 'node_modules', '@acme', 'linked-fonts'), 'dir');
      await expect(generate(join(workload, 'node_modules', '@acme', 'linked-fonts', 'fonts'))).rejects.toThrow(
        "the scan root '../libs/fonts/fonts' lies outside every project's public/ folder",
      );
      // A scope's public/ belongs to no project.
      writeFile(join(workspace, 'libs', 'public', 'shared.css'), 'body{}');
      await expect(generate(join(workspace, 'libs', 'public'))).rejects.toThrow(
        "the scan root '../libs/public' lies outside every project's public/ folder",
      );
      // A folder outside the workspace is read by no key, whatever its name.
      const outside = realpathSync(mkdtempSync(join(tmpdir(), 'putnami-static-scan-root-outside-')));
      outsideRoots.push(outside);
      writeFile(join(outside, 'public', 'outside.txt'), 'outside');
      await expect(generate(join(outside, 'public'))).rejects.toThrow('lies outside the workspace');
      expect(existsSync(staged('inter.woff2'))).toBe(false);
      expect(existsSync(staged('shared.css'))).toBe(false);
      expect(existsSync(staged('outside.txt'))).toBe(false);
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
