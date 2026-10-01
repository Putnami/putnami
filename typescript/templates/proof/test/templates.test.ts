/**
 * The TypeScript templates, rendered and run against this repository's
 * framework sources.
 *
 * For each template beside this project, the proof renders it into a throwaway
 * workspace, runs the preBuild hooks, lints it with the Biome check
 * `putnami lint` runs, type-checks it the way `build~types` does and runs the
 * tests the template ships. The static checks below keep this project's cache
 * key complete: every file a rendered project reads is a declared input, so a
 * change to a template or to the framework reruns it.
 */
import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import { existsSync, readFileSync, realpathSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import {
  BIOME_CONFIG,
  declaredDependencies,
  discoverTemplates,
  disposeProject,
  findWorkspaceRoot,
  INIT_SOURCE,
  lint,
  prepareProject,
  type RenderedProject,
  readPackageJson,
  runPreBuildHooks,
  runTests,
  type StepResult,
  TYPESCRIPT_EXTENSION,
  typeCheck,
  WORKSPACE_GITIGNORE,
} from './harness';

const proofDir = dirname(import.meta.dir);
const root = findWorkspaceRoot(proofDir);
const templatesDir = dirname(proofDir);
const templates = discoverTemplates(templatesDir, TYPESCRIPT_EXTENSION);

/** A step can compile the whole framework from source on a loaded host. */
const STEP_TIMEOUT_MS = 300_000;

const proofConfig = JSON.parse(readFileSync(join(proofDir, 'putnami.json'), 'utf8')) as {
  options?: { test?: { filePatterns?: string[] } };
};
const filePatterns = new Set(proofConfig.options?.test?.filePatterns ?? []);
const proofPackage = readPackageJson(proofDir);

function expectSuccess(step: string, result: StepResult): void {
  if (result.exitCode !== 0) {
    throw new Error(`${step} exited ${result.exitCode}:\n${result.output.slice(-8000)}`);
  }
}

/** The proof-relative path of a directory, with forward slashes. */
function fromProof(dir: string): string {
  return relative(proofDir, dir).replaceAll('\\', '/');
}

describe('the proof keeps its cache key complete', () => {
  test('it finds the TypeScript templates beside it', () => {
    expect(templates.length).toBeGreaterThan(0);
  });

  test('every template beside it is a declared input', () => {
    expect(filePatterns.has('../*/putnami.template.json')).toBe(true);
    for (const template of templates) {
      expect(filePatterns.has(`../${template.name}/**`)).toBe(true);
    }
  });

  test('the workspace tsconfig a rendered project extends is a declared input', () => {
    expect(filePatterns.has(fromProof(join(root, 'typescript', 'extension', 'config', 'tsconfig.json')))).toBe(true);
  });

  test('the Biome configuration a rendered project is linted with is a declared input', () => {
    expect(filePatterns.has(fromProof(join(root, BIOME_CONFIG)))).toBe(true);
  });

  test('the .gitignore a rendered workspace holds is the one putnami init writes', () => {
    expect(filePatterns.has(fromProof(join(root, INIT_SOURCE)))).toBe(true);
    const source = readFileSync(join(root, INIT_SOURCE), 'utf8');
    expect(source.match(/const defaultGitignore = `([^`]*)`/)?.[1]).toBe(WORKSPACE_GITIGNORE);
  });

  test('every dependency a template declares is installed here under the same specifier', () => {
    const declared = declaredDependencies(proofPackage);
    for (const template of templates) {
      const templatePackage = JSON.parse(readFileSync(join(template.dir, 'package.json.template'), 'utf8'));
      for (const [name, specifier] of Object.entries(declaredDependencies(templatePackage))) {
        // A template names a framework package through the workspace catalog;
        // here it is this repository's own package.
        const expected = name.startsWith('@putnami/') ? 'workspace:*' : specifier;
        expect({ template: template.name, name, specifier: declared[name] }).toEqual({
          template: template.name,
          name,
          specifier: expected,
        });
      }
    }
  });

  test('the framework files a rendered project reads are declared inputs', () => {
    for (const [name, specifier] of Object.entries(declaredDependencies(proofPackage))) {
      if (!specifier.startsWith('workspace:')) {
        continue;
      }
      const dir = fromProof(realpathSync(join(proofDir, 'node_modules', name)));
      const expected = [`${dir}/package.json`, `${dir}/src/**`];
      const manifest = join(proofDir, 'node_modules', name, 'putnami.extension.json');
      if (existsSync(manifest) && JSON.parse(readFileSync(manifest, 'utf8')).hooks) {
        expected.push(`${dir}/bin/**`, `${dir}/putnami.extension.json`);
      }
      for (const pattern of expected) {
        expect({ name, pattern, declared: filePatterns.has(pattern) }).toEqual({ name, pattern, declared: true });
      }
    }
  });
});

for (const template of templates) {
  describe(`the ${template.name} template`, () => {
    let rendered: RenderedProject | undefined;
    const current = (): RenderedProject => {
      if (!rendered) {
        throw new Error(`the ${template.name} template was not rendered`);
      }
      return rendered;
    };

    beforeAll(() => {
      rendered = prepareProject(root, proofDir, template);
    });

    afterAll(() => {
      disposeProject(rendered);
    });

    test(
      'runs its preBuild hooks',
      async () => {
        expectSuccess('the preBuild hooks', await runPreBuildHooks(current(), 'test'));
      },
      STEP_TIMEOUT_MS,
    );

    // After the hooks, so the files they write are linted too.
    test(
      'passes the Biome check of putnami lint',
      async () => {
        expectSuccess('the Biome check', await lint(current()));
      },
      STEP_TIMEOUT_MS,
    );

    test(
      'type-checks the way build~types does',
      async () => {
        expectSuccess('the type-check', await typeCheck(current()));
      },
      STEP_TIMEOUT_MS,
    );

    test(
      'passes the tests it ships',
      async () => {
        expectSuccess('bun test', await runTests(current()));
      },
      STEP_TIMEOUT_MS,
    );
  });
}
