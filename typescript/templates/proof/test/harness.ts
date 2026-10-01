/**
 * The harness behind the TypeScript template proof.
 *
 * It renders a committed template into a throwaway workspace wired to this
 * repository's framework sources, then replays what `putnami lint`,
 * `putnami build` and `putnami test` run on a freshly scaffolded project: the
 * Biome check, the preBuild hooks, the `build~types` type-check and `bun test`.
 * Nothing reaches a registry or the network, and everything it writes lives
 * under one temporary directory.
 *
 * Each step replays code that lives elsewhere, and names it, so a change there
 * can be followed here. The design and its limits are recorded in
 * tooling/scaffold/doc/adr/0006-templates-run-against-the-workspace-framework.md.
 */
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  realpathSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, isAbsolute, join, relative, resolve } from 'node:path';

/** The TypeScript extension every TypeScript template names. */
export const TYPESCRIPT_EXTENSION = '@putnami/typescript';

/** The Biome configuration the TypeScript extension ships, relative to the workspace root. */
export const BIOME_CONFIG = 'typescript/extension/config/biome.json';

/** The package `putnami lint` runs Biome from, one of the extension's workspaceDevDependencies. */
const BIOME_PACKAGE = '@biomejs/biome';

/** The source of the files `putnami init` writes, relative to the workspace root. */
export const INIT_SOURCE = 'tooling/cli/internal/commands/lifecycle/workspace_init.go';

/**
 * The .gitignore `putnami init` writes: defaultGitignore in INIT_SOURCE, which
 * a static test compares with this copy. The extension's Biome configuration
 * reads it, and fails without one.
 */
export const WORKSPACE_GITIGNORE = `node_modules
dist
.putnami
coverage
.gen
.generated
.DS_Store
.env.local.yaml
.env.prod.yaml
`;

/** A committed template, as its `putnami.template.json` declares it. */
export interface Template {
  /** The template directory name. */
  name: string;
  /** The absolute template directory. */
  dir: string;
  manifest: {
    name: string;
    extension?: string;
    testVariables?: Record<string, string>;
  };
}

/** The render variables of the CLI (tooling/cli/internal/template/render.go). */
export interface RenderVars {
  projectName: string;
  projectPath: string;
  projectModule: string;
  putnamiVersion: string;
  workspaceRelativePath: string;
  goFrameworkVersion: string;
}

/** A template rendered into its own throwaway workspace. */
export interface RenderedProject {
  template: Template;
  workspace: string;
  project: string;
  vars: RenderVars;
}

/** What a child process did. */
export interface StepResult {
  exitCode: number;
  output: string;
}

interface PackageJson {
  name?: string;
  dependencies?: Record<string, string>;
  devDependencies?: Record<string, string>;
}

interface HookDefinition {
  command: string;
  args?: string[];
  timeoutMs?: number;
  order?: number;
}

/** Returns the nearest directory at or above `start` that holds putnami.workspace.json. */
export function findWorkspaceRoot(start: string): string {
  let dir = resolve(start);
  for (;;) {
    if (existsSync(join(dir, 'putnami.workspace.json'))) {
      return dir;
    }
    const parent = dirname(dir);
    if (parent === dir) {
      throw new Error(`no putnami.workspace.json above ${start}`);
    }
    dir = parent;
  }
}

/**
 * Lists the templates directly under `templatesDir` whose manifest names
 * `extension`, in byte order of their directory names.
 */
export function discoverTemplates(templatesDir: string, extension: string): Template[] {
  const templates: Template[] = [];
  for (const name of readdirSync(templatesDir).sort(byteOrder)) {
    const manifestPath = join(templatesDir, name, 'putnami.template.json');
    if (!existsSync(manifestPath)) {
      continue;
    }
    const manifest = JSON.parse(readFileSync(manifestPath, 'utf8')) as Template['manifest'];
    if (manifest.extension === extension) {
      templates.push({ name, dir: join(templatesDir, name), manifest });
    }
  }
  return templates;
}

/**
 * The variables `putnami dev template test` renders with: DefaultTestVars, with
 * the manifest's `testVariables` overriding `projectName` and `projectModule`
 * only (tooling/cli/internal/commands/extensions/templates.go).
 */
export function testVars(template: Template): RenderVars {
  const vars: RenderVars = {
    projectName: 'test-project',
    projectPath: 'test-project',
    projectModule: 'test_project',
    putnamiVersion: 'latest',
    workspaceRelativePath: '..',
    goFrameworkVersion: 'v0.0.0',
  };
  const overrides = template.manifest.testVariables ?? {};
  if (overrides['projectName'] !== undefined) {
    vars.projectName = overrides['projectName'];
  }
  if (overrides['projectModule'] !== undefined) {
    vars.projectModule = overrides['projectModule'];
  }
  return vars;
}

/**
 * Replays template.RenderDir (tooling/cli/internal/template/render.go), whose
 * rules protocols/template/doc/02-variables.md documents: the walk is
 * depth-first in byte order, the root manifest is skipped, `__module__` in a
 * path becomes `projectModule`, and a `.template` file gets `<%= name %>`
 * substitution and loses its suffix. Unlike the CLI, a placeholder left after
 * rendering fails here: it would reach the user as literal text.
 */
export function renderTemplate(src: string, dst: string, vars: RenderVars): void {
  const walk = (rel: string) => {
    const entries = readdirSync(join(src, rel), { withFileTypes: true }).sort((a, b) => byteOrder(a.name, b.name));
    for (const entry of entries) {
      const childRel = rel === '' ? entry.name : `${rel}/${entry.name}`;
      if (childRel === 'putnami.template.json') {
        continue;
      }
      const target = join(dst, childRel.replaceAll('__module__', vars.projectModule));
      if (entry.isDirectory()) {
        mkdirSync(target, { recursive: true });
        walk(childRel);
      } else {
        renderFile(join(src, childRel), target, childRel, vars);
      }
    }
  };
  mkdirSync(dst, { recursive: true });
  walk('');
}

function renderFile(source: string, target: string, rel: string, vars: RenderVars): void {
  mkdirSync(dirname(target), { recursive: true });
  const data = readFileSync(source);
  if (!target.endsWith('.template')) {
    writeFileSync(target, data);
    return;
  }
  const content = evalTemplate(data.toString('utf8'), vars);
  const left = content.match(/<%=[^%]*%>/);
  if (left) {
    throw new Error(`${rel} still carries the placeholder ${left[0]} after rendering`);
  }
  writeFileSync(target.slice(0, -'.template'.length), content);
}

/** Replays template.EvalTemplate: an empty value leaves its placeholder in place. */
export function evalTemplate(content: string, vars: RenderVars): string {
  let out = content;
  for (const [key, value] of Object.entries(vars)) {
    if (value !== '') {
      out = out.replaceAll(`<%= ${key} %>`, value);
    }
  }
  return out;
}

/**
 * Renders `template` into a new throwaway workspace and wires it the way a
 * consumer workspace is wired after `putnami install`:
 *
 * - putnami.workspace.json is the one `putnami dev template test` writes;
 * - .gitignore is the one `putnami init` writes, which Biome reads;
 * - tsconfig.json extends the TypeScript extension's config, as
 *   ensureWorkspaceTsConfig writes it;
 * - biome.json is the TypeScript extension's config marked as the root one, as
 *   ensureWorkspaceBiomeConfig writes it;
 * - node_modules holds the extension's workspaceDevDependencies that the lint
 *   and the type-check read (@biomejs/biome, typescript and @types/bun);
 * - the project's node_modules links every dependency the rendered package.json
 *   declares to the package this proof project installed under the same name.
 *   A `@putnami/*` package therefore resolves to this repository's framework
 *   source, and every other package to the version bun.lock pins.
 */
export function prepareProject(root: string, proofDir: string, template: Template): RenderedProject {
  const vars = testVars(template);
  const workspace = realpathSync(mkdtempSync(join(tmpdir(), 'putnami-template-proof-')));
  try {
    writeJson(join(workspace, 'putnami.workspace.json'), {
      name: 'template-test-workspace',
      workspaces: [vars.projectPath],
    });
    writeFileSync(join(workspace, '.gitignore'), WORKSPACE_GITIGNORE);
    const extensionConfig = join(root, 'typescript', 'extension', 'config', 'tsconfig.json');
    const relativeConfig = relative(workspace, extensionConfig);
    writeJson(join(workspace, 'tsconfig.json'), {
      extends: isAbsolute(relativeConfig) ? toSlash(relativeConfig) : `./${toSlash(relativeConfig)}`,
    });
    const biomeConfig = JSON.parse(readFileSync(join(root, BIOME_CONFIG), 'utf8')) as Record<string, unknown>;
    writeJson(join(workspace, 'biome.json'), { ...biomeConfig, root: true });
    for (const name of [BIOME_PACKAGE, 'typescript', '@types/bun']) {
      linkDirectory(resolveWorkspaceTool(proofDir, name), join(workspace, 'node_modules', name));
    }

    const project = join(workspace, vars.projectPath);
    renderTemplate(template.dir, project, vars);
    for (const name of Object.keys(declaredDependencies(readPackageJson(project)))) {
      linkDirectory(resolvePackageDir(proofDir, name), join(project, 'node_modules', name));
    }
    return { template, workspace, project, vars };
  } catch (err) {
    rmSync(workspace, { recursive: true, force: true });
    throw err;
  }
}

/** Removes the throwaway workspace. */
export function disposeProject(rendered: RenderedProject | undefined): void {
  if (rendered) {
    rmSync(rendered.workspace, { recursive: true, force: true });
  }
}

/**
 * Replays hooks.RunHooks for the `preBuild` kind
 * (typescript/extension/internal/hooks/hooks.go): discovery reads the project's
 * dependencies and devDependencies in byte order, keeps those whose
 * putnami.extension.json declares the hook, and runs them by declared `order`.
 * Each hook gets the context file and environment the extension gives it, and
 * must exit 0 with a summary event.
 */
export function runPreBuildHooks(rendered: RenderedProject, mode: string): Promise<StepResult> {
  return invokeInOrder(rendered, mode, discoverPreBuildHooks(rendered.project), 0, '');
}

interface DiscoveredHook {
  name: string;
  root: string;
  hook: HookDefinition;
}

function discoverPreBuildHooks(project: string): DiscoveredHook[] {
  const deps = declaredDependencies(readPackageJson(project));
  const hooks: DiscoveredHook[] = [];
  for (const name of Object.keys(deps).sort(byteOrder)) {
    const link = join(project, 'node_modules', name);
    const root = deps[name]?.startsWith('workspace:') ? realpathSync(link) : link;
    const manifestPath = join(root, 'putnami.extension.json');
    if (!existsSync(manifestPath)) {
      continue;
    }
    const manifest = JSON.parse(readFileSync(manifestPath, 'utf8')) as { hooks?: Record<string, HookDefinition> };
    const hook = manifest.hooks?.['preBuild'];
    if (hook) {
      hooks.push({ name, root, hook });
    }
  }
  // Array.prototype.sort is stable, so the byte order of names holds within one rank.
  return hooks.sort((a, b) => (a.hook.order ?? 0) - (b.hook.order ?? 0));
}

/** Runs hooks[index] and the hooks after it, one at a time: each sees what the ones before it wrote. */
async function invokeInOrder(
  rendered: RenderedProject,
  mode: string,
  hooks: DiscoveredHook[],
  index: number,
  output: string,
): Promise<StepResult> {
  const current = hooks[index];
  if (!current) {
    return { exitCode: 0, output };
  }
  const result = await invokeHook(rendered, mode, current, index);
  const transcript = `${output}--- preBuild hook ${current.name}\n${result.output}\n`;
  if (result.exitCode !== 0) {
    return { exitCode: result.exitCode, output: transcript };
  }
  if (!result.output.split('\n').some((line) => line.includes('"type":"summary"'))) {
    return { exitCode: 1, output: `${transcript}hook ${current.name} exited 0 but emitted no summary event\n` };
  }
  return invokeInOrder(rendered, mode, hooks, index + 1, transcript);
}

function invokeHook(
  rendered: RenderedProject,
  mode: string,
  { name, root, hook }: DiscoveredHook,
  index: number,
): Promise<StepResult> {
  const { project, workspace } = rendered;
  const contextPath = join(workspace, `.hook-context-${index}.json`);
  writeJson(contextPath, {
    workspaceRoot: workspace,
    projectRoot: project,
    extensionRoot: root,
    outputRoot: join(project, '.gen'),
    cacheRoot: join(workspace, '.putnami', 'hooks-cache', name),
    debug: false,
    hook: 'preBuild',
    extension: name,
    projectName: rendered.vars.projectName,
    mode,
  });
  const expand = (arg: string) =>
    arg
      .replaceAll('{extensionRoot}', root)
      .replaceAll('{projectRoot}', project)
      .replaceAll('{workspaceRoot}', workspace);
  const command = hook.command === 'bun' ? process.execPath : hook.command;
  return run([command, ...(hook.args ?? []).map(expand), '--putnami-context', contextPath], {
    cwd: project,
    env: { ...childEnv(rendered), PUTNAMI_PREBUILD_CONTEXT: 'true' },
    timeoutMs: hook.timeoutMs && hook.timeoutMs > 0 ? hook.timeoutMs : 120_000,
  });
}

/**
 * Replays build~types (typescript/extension/internal/build/types.go): a scratch
 * tsconfig that extends the project's own, restricted to src, bin and .gen and
 * without test files, compiled with the extension's flags. The compiler is the
 * TypeScript the workspace root links, run by the bun running this proof.
 */
export async function typeCheck(rendered: RenderedProject): Promise<StepResult> {
  const { project, workspace } = rendered;
  const scratch = join(workspace, '.types');
  mkdirSync(scratch, { recursive: true });
  const inProject = (pattern: string) => toSlash(join(project, pattern));
  const tsconfigPath = join(scratch, 'tsconfig.types.json');
  writeJson(tsconfigPath, {
    extends: toSlash(join(project, 'tsconfig.json')),
    compilerOptions: { typeRoots: defaultTypeRoots(project) },
    include: [
      inProject('src/**/*.ts'),
      inProject('src/**/*.tsx'),
      inProject('bin/**/*.ts'),
      inProject('.gen/**/*.ts'),
      inProject('.gen/**/*.tsx'),
    ],
    exclude: [
      inProject('**/*.test.ts'),
      inProject('**/*.spec.ts'),
      inProject('**/*.test.tsx'),
      inProject('**/*.spec.tsx'),
    ],
  });
  const typescriptDir = join(workspace, 'node_modules', 'typescript');
  const major = Number((readPackageJson(typescriptDir) as { version?: string }).version?.split('.')[0]);
  const ignoreDeprecations = ({ 5: '5.0', 6: '6.0' } as Record<number, string>)[major];
  const args = [
    join(typescriptDir, 'bin', 'tsc'),
    '--rootDir',
    '.',
    '--outDir',
    join(scratch, 'out'),
    '--declaration',
    '--emitDeclarationOnly',
    '--declarationMap',
    '--experimentalDecorators',
    '--strict',
    'true',
    '--target',
    'esnext',
    '--module',
    'esnext',
    '--types',
    'bun',
    '--moduleResolution',
    'bundler',
    '--jsx',
    'react-jsx',
    '--lib',
    'ESNext,DOM',
    '--skipLibCheck',
    'true',
    '--skipDefaultLibCheck',
    'true',
    '--forceConsistentCasingInFileNames',
    'true',
    ...(ignoreDeprecations ? ['--ignoreDeprecations', ignoreDeprecations] : []),
    '--pretty',
    'false',
    '--project',
    tsconfigPath,
  ];
  return run([process.execPath, ...args], { cwd: project, env: childEnv(rendered), timeoutMs: 300_000 });
}

/**
 * Replays the read-only lint~check-only task (typescript/extension/internal/lint,
 * CheckAll): one `biome check` with the workspace biome.json, from the
 * directory that holds it, over the project. Unlike the task, a warning fails
 * it too: a new project's `putnami lint` must report nothing. Biome is the one
 * the workspace links; when it is not installed, prepareProject has already
 * failed and named it.
 */
export function lint(rendered: RenderedProject): Promise<StepResult> {
  const { project, workspace } = rendered;
  const biome = join(workspace, 'node_modules', BIOME_PACKAGE, 'bin', 'biome');
  const args = [
    biome,
    'check',
    `--config-path=${join(workspace, 'biome.json')}`,
    '--assist-enabled=true',
    '--enforce-assist=false',
    '--diagnostic-level=warn',
    '--error-on-warnings',
    '--max-diagnostics=none',
    toSlash(relative(workspace, project)),
  ];
  return run([process.execPath, ...args], { cwd: workspace, env: childEnv(rendered), timeoutMs: 120_000 });
}

/**
 * Replays test~test (typescript/extension/internal/testjob): `bun test` in the
 * project directory, with the project and workspace roots the CLI exports to
 * every task (tooling/cli/internal/jobs/context.go, BuildEnvVars).
 */
export function runTests(rendered: RenderedProject): Promise<StepResult> {
  return run([process.execPath, 'test'], { cwd: rendered.project, env: childEnv(rendered), timeoutMs: 300_000 });
}

/** Every dependency and devDependency a package.json declares. */
export function declaredDependencies(pkg: PackageJson): Record<string, string> {
  return { ...(pkg.dependencies ?? {}), ...(pkg.devDependencies ?? {}) };
}

/** Reads `<dir>/package.json`. */
export function readPackageJson(dir: string): PackageJson {
  return JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8')) as PackageJson;
}

/**
 * Returns the real directory of `name`, one of the TypeScript extension's
 * workspaceDevDependencies, as seen from `fromDir`. `putnami install` installs
 * those at the workspace root, so a missing one names that command.
 */
function resolveWorkspaceTool(fromDir: string, name: string): string {
  try {
    return resolvePackageDir(fromDir, name);
  } catch {
    throw new Error(
      `${name} is not installed for ${fromDir}: run \`putnami install\`, which installs the TypeScript extension's workspaceDevDependencies`,
    );
  }
}

/**
 * Returns the real directory of package `name` as seen from `fromDir`, walking
 * up node_modules directories the way package resolution does.
 */
export function resolvePackageDir(fromDir: string, name: string): string {
  let dir = resolve(fromDir);
  for (;;) {
    const candidate = join(dir, 'node_modules', name);
    if (existsSync(join(candidate, 'package.json'))) {
      return realpathSync(candidate);
    }
    const parent = dirname(dir);
    if (parent === dir) {
      throw new Error(
        `${name} is not installed for ${fromDir}: declare it in that project's package.json and run the workspace install`,
      );
    }
    dir = parent;
  }
}

/**
 * The environment of a child step. The CLI's own variables that this proof
 * inherited describe the proof project, so every `PUTNAMI_*` variable is
 * dropped and the ones the CLI exports to a task of the rendered project are
 * set in their place. PWD is the workspace root, where a user runs `putnami`.
 */
function childEnv(rendered: RenderedProject): Record<string, string> {
  const env: Record<string, string> = {};
  for (const [key, value] of Object.entries(process.env)) {
    if (value !== undefined && !key.startsWith('PUTNAMI_')) {
      env[key] = value;
    }
  }
  env['PWD'] = rendered.workspace;
  env['PUTNAMI_WORKSPACE_ROOT'] = rendered.workspace;
  env['PUTNAMI_PROJECT_ROOT'] = rendered.project;
  env['PUTNAMI_PROJECT_PATH'] = rendered.project;
  env['PUTNAMI_PROJECT_NAME'] = rendered.vars.projectName;
  return env;
}

async function run(
  cmd: string[],
  options: { cwd: string; env: Record<string, string>; timeoutMs: number },
): Promise<StepResult> {
  const proc = Bun.spawn(cmd, {
    cwd: options.cwd,
    env: options.env,
    stdin: 'ignore',
    stdout: 'pipe',
    stderr: 'pipe',
    timeout: options.timeoutMs,
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ]);
  return { exitCode, output: `${stdout}${stderr}` };
}

/** Replays defaultTypeRoots: node_modules/@types of the project and every ancestor. */
function defaultTypeRoots(dir: string): string[] {
  const roots: string[] = [];
  let current = dir;
  for (;;) {
    roots.push(toSlash(join(current, 'node_modules', '@types')));
    const parent = dirname(current);
    if (parent === current) {
      return roots;
    }
    current = parent;
  }
}

/** Links `target` at `path`: a directory symlink, or a junction on Windows (D-W6). */
function linkDirectory(target: string, path: string): void {
  mkdirSync(dirname(path), { recursive: true });
  symlinkSync(target, path, 'junction');
}

function writeJson(path: string, value: unknown): void {
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`);
}

function toSlash(path: string): string {
  return path.replaceAll('\\', '/');
}

function byteOrder(a: string, b: string): number {
  if (a < b) {
    return -1;
  }
  return a > b ? 1 : 0;
}
