/**
 * The TypeScript templates, rendered and run against this repository's
 * framework sources.
 *
 * For each template beside this project, the proof renders it into a throwaway
 * workspace, runs the preBuild hooks, lints it with the Biome check
 * `putnami lint` runs, type-checks it the way `build~types` does and runs the
 * tests the template ships. Each step observes a check of the feature the
 * template declares about itself (tooling/scaffold ADR 0003), so the spec gate
 * sees each template keep its promise. The static checks below keep this
 * project's cache key complete: every file a rendered project reads is a
 * declared input, so a change to a template or to the framework reruns it.
 */
import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import { existsSync, readFileSync, realpathSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
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
  TEMPLATE_PROTOCOL_SOURCE,
  TEMPLATE_ROOT_EXCLUSIONS,
  type Template,
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

/** A check of a template's feature, which one step observes. */
interface StepCheck {
  requirement: string;
  check: string;
}

/**
 * The check each step observes. A template whose packages declare no preBuild
 * hook declares no hook check, so its hook step stays a plain test.
 */
const STEPS = {
  hooks: { requirement: 'a-rendered-project-type-checks', check: 'its-prebuild-hooks-certify-their-routes' },
  lint: { requirement: 'a-rendered-project-passes-lint', check: 'the-biome-check-reports-nothing' },
  types: { requirement: 'a-rendered-project-type-checks', check: 'it-type-checks-as-build-types-does' },
  tests: { requirement: 'a-rendered-project-passes-its-tests', check: 'bun-test-passes' },
} as const satisfies Record<string, StepCheck>;

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

/** Every check the template's features declare, as `requirement#check`. */
function declaredChecks(template: Template): string[] {
  return template.features.flatMap((feature) =>
    feature.requirements.flatMap((requirement) =>
      (requirement.verification?.checks ?? []).map((check) => `${requirement.id}#${check}`),
    ),
  );
}

/** Whether a package the template declares runs a preBuild hook. */
function hasPreBuildHook(template: Template): boolean {
  const templatePackage = JSON.parse(readFileSync(join(template.dir, 'package.json.template'), 'utf8'));
  return Object.keys(declaredDependencies(templatePackage)).some((name) => {
    const manifest = join(proofDir, 'node_modules', name, 'putnami.extension.json');
    return existsSync(manifest) && JSON.parse(readFileSync(manifest, 'utf8')).hooks?.preBuild !== undefined;
  });
}

/**
 * Registers one step of a template's proof: a specTest observing `observes`
 * when the template's feature declares that check, and a plain test otherwise.
 */
function step(template: Template, name: string, observes: StepCheck, body: () => Promise<void>): void {
  const feature = template.features[0];
  if (feature && declaredChecks(template).includes(`${observes.requirement}#${observes.check}`)) {
    specTest(name, { feature: feature.id, ...observes }, body, STEP_TIMEOUT_MS);
    return;
  }
  test(name, body, STEP_TIMEOUT_MS);
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

  test('a render leaves out the root entries a template archive leaves out', () => {
    expect(filePatterns.has(fromProof(join(root, TEMPLATE_PROTOCOL_SOURCE)))).toBe(true);
    const source = readFileSync(join(root, TEMPLATE_PROTOCOL_SOURCE), 'utf8');
    const body = source.split('func IsTemplateRootPackagingExclusion(name string) bool {')[1]?.split('\n}\n')[0] ?? '';
    expect(body).toContain('strings.HasPrefix(name, ".")');
    const names = [...(body.match(/case ([^:]*):/)?.[1] ?? '').matchAll(/"([^"]*)"/g)].map((match) => match[1]);
    expect(names).toEqual([...TEMPLATE_ROOT_EXCLUSIONS]);
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

describe('every template declares the promise this proof observes', () => {
  test('every template declares one feature of its own, under tooling/project-scaffolding', () => {
    for (const template of templates) {
      expect({
        template: template.name,
        features: template.features.length,
        relations: template.features[0]?.relations,
      }).toEqual({
        template: template.name,
        features: 1,
        relations: [{ kind: 'parent', target: 'tooling/project-scaffolding' }],
      });
    }
  });

  test('every template declares the check of each step it runs, and only those', () => {
    for (const template of templates) {
      const expected = Object.entries(STEPS)
        .filter(([name]) => name !== 'hooks' || hasPreBuildHook(template))
        .map(([, { requirement, check }]) => `${requirement}#${check}`)
        .sort();
      expect({ template: template.name, checks: declaredChecks(template).sort() }).toEqual({
        template: template.name,
        checks: expected,
      });
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

    step(template, 'runs its preBuild hooks', STEPS.hooks, async () => {
      expectSuccess('the preBuild hooks', await runPreBuildHooks(current(), 'test'));
    });

    // After the hooks, so the files they write are linted too.
    step(template, 'passes the Biome check of putnami lint', STEPS.lint, async () => {
      expectSuccess('the Biome check', await lint(current()));
    });

    step(template, 'type-checks the way build~types does', STEPS.types, async () => {
      expectSuccess('the type-check', await typeCheck(current()));
    });

    step(template, 'passes the tests it ships', STEPS.tests, async () => {
      expectSuccess('bun test', await runTests(current()));
    });
  });
}
