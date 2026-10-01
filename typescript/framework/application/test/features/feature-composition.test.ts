import { afterEach, describe, expect, it } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  api,
  buildDesignGraph,
  composeModules,
  type DesignBuilder,
  type DesignContributor,
  module,
  type Module,
} from '../../src';

const applicationRoot = join(import.meta.dir, '..', '..');
const routeModule = join(applicationRoot, 'src', 'api', 'route', 'index.ts');
const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
const tempDirs: string[] = [];

afterEach(() => {
  if (originalProjectRoot === undefined) {
    delete process.env.PUTNAMI_PROJECT_ROOT;
  } else {
    process.env['PUTNAMI_PROJECT_ROOT'] = originalProjectRoot;
  }
  while (tempDirs.length > 0) {
    rmSync(tempDirs.pop() as string, { recursive: true, force: true });
  }
});

/** A workload project whose root `api()` plugin scans real route files. */
function createWorkload(): string {
  const projectRoot = mkdtempSync(join(tmpdir(), 'putnami-feature-composition-'));
  tempDirs.push(projectRoot);
  // The generated api loader imports '@putnami/application' by name, so the
  // temporary workload resolves that name back to this project.
  mkdirSync(join(projectRoot, 'node_modules', '@putnami'), { recursive: true });
  symlinkSync(applicationRoot, join(projectRoot, 'node_modules', '@putnami', 'application'), 'dir');

  writeRoute(
    projectRoot,
    join('src', 'api', 'auth', 'token', 'post.ts'),
    `export default endpoint().returns({ token: String }).handle(() => ({ token: 'opaque' }));\n`,
  );
  writeRoute(
    projectRoot,
    join('src', 'api', 'auth', 'token', 'delete.ts'),
    `export default endpoint().handle(() => ({ revoked: true }));\n`,
  );
  writeRoute(
    projectRoot,
    join('src', 'api', 'tasks', 'get.ts'),
    `export default endpoint().handle(() => ({ tasks: [] }));\n`,
  );
  return projectRoot;
}

function writeRoute(projectRoot: string, relative: string, body: string): void {
  const file = join(projectRoot, relative);
  mkdirSync(join(file, '..'), { recursive: true });
  writeFileSync(file, `import { endpoint } from ${JSON.stringify(routeModule)};\n${body}`);
}

class OauthContributor implements DesignContributor {
  constructor(private readonly provider: string) {}

  contributeDesign(builder: DesignBuilder): void {
    const id = `service:${this.provider}-oauth-client`;
    builder.addNode({ id, kind: 'service', name: `${this.provider}-oauth-client` });
    builder.relateFromModule(id, 'injects');
  }
}

/**
 * The shape a Cloud workload needs: one `auth/opaque-tokens`
 * declaration spanning selected sibling modules of a library composer and
 * selected file routes owned by the workload root.
 */
async function buildCloudShapedGraph(
  projectRoot: string,
  selectSources = true,
): Promise<{
  graph: Awaited<ReturnType<typeof buildDesignGraph>>;
  workload: Module;
  routes: ReturnType<typeof api>;
}> {
  const googleOauth = module('google-oauth').use(new OauthContributor('google'));
  const githubOauth = module('github-oauth').use(new OauthContributor('github'));
  const opaqueTokens = module('opaque-tokens')
    .feature(
      {
        id: 'auth/opaque-tokens',
        name: 'Opaque tokens',
        outcome: 'Clients exchange credentials for revocable opaque tokens',
        owner: 'auth',
      },
      { modules: [googleOauth], ...(selectSources ? { sources: ['src/api/auth/token'] } : {}) },
    )
    .use(new OauthContributor('token'));
  // The selected sibling is composed before the module that selects it, so
  // claims have to be collected before module identities are assigned.
  const auth = composeModules([googleOauth, opaqueTokens, githubOauth], { name: 'auth' });

  const routes = api({ scanPath: join(projectRoot, 'src', 'api') });
  const workload = module('app').use(auth).use(routes);
  await routes.generate(workload);

  return { graph: await buildDesignGraph(workload, '@cloud/workload', projectRoot), workload, routes };
}

describe('feature composition across native owners', () => {
  it('spans a selected sibling module and selected workload file routes', async () => {
    const projectRoot = createWorkload();
    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;

    const { graph } = await buildCloudShapedGraph(projectRoot);

    expect(graph?.nodes.map(({ id }) => id)).toEqual([
      'api.operation:DELETE:/auth/token',
      'api.operation:POST:/auth/token',
      'api.schema:POST:/auth/token:response',
      'feature:auth/opaque-tokens',
      'module:@cloud/workload/google-oauth',
      'module:@cloud/workload/opaque-tokens',
      'service:google-oauth-client',
      'service:token-oauth-client',
    ]);
    expect(graph?.edges).toEqual([
      {
        from: 'api.operation:POST:/auth/token',
        to: 'api.schema:POST:/auth/token:response',
        kind: 'returns',
        authority: 'exact',
      },
      // A selected module is named by identity in the declaration, so the
      // containment is exact; the property keeps it distinguishable from the
      // module that carries the declaration.
      {
        from: 'feature:auth/opaque-tokens',
        to: 'module:@cloud/workload/google-oauth',
        kind: 'implementedBy',
        authority: 'exact',
        properties: { association: 'selected' },
      },
      {
        from: 'feature:auth/opaque-tokens',
        to: 'module:@cloud/workload/opaque-tokens',
        kind: 'implementedBy',
        authority: 'exact',
      },
      {
        from: 'module:@cloud/workload/google-oauth',
        to: 'service:google-oauth-client',
        kind: 'injects',
        authority: 'exact',
      },
      // Selecting a source path leaves the framework to resolve which routes
      // live there, so route association is derived and carries the matched
      // declaration source.
      {
        from: 'module:@cloud/workload/opaque-tokens',
        to: 'api.operation:DELETE:/auth/token',
        kind: 'exposes',
        authority: 'derived',
        properties: { association: 'selected' },
        provenance: { path: 'src/api/auth/token/delete.ts' },
      },
      {
        from: 'module:@cloud/workload/opaque-tokens',
        to: 'api.operation:POST:/auth/token',
        kind: 'exposes',
        authority: 'derived',
        properties: { association: 'selected' },
        provenance: { path: 'src/api/auth/token/post.ts' },
      },
      {
        from: 'module:@cloud/workload/opaque-tokens',
        to: 'service:token-oauth-client',
        kind: 'injects',
        authority: 'exact',
      },
    ]);
  });

  it('leaves unselected sibling modules and unselected routes outside the feature', async () => {
    const projectRoot = createWorkload();
    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;

    const { graph } = await buildCloudShapedGraph(projectRoot);

    expect(graph?.nodes.map(({ id }) => id)).not.toContain('module:@cloud/workload/github-oauth');
    expect(graph?.nodes.map(({ id }) => id)).not.toContain('service:github-oauth-client');
    expect(graph?.nodes.map(({ id }) => id)).not.toContain('api.operation:GET:/tasks');
    // The composer itself is not a broad ancestor claim either.
    expect(graph?.nodes.map(({ id }) => id)).not.toContain('module:@cloud/workload/auth');
  });

  // A feature declared elsewhere that selected nothing from this scan can never
  // see these routes, so importing the loader to register them all is pure waste
  // — the graph builder discards every one. The previous condition ("any feature
  // anywhere") did that work for every unscoped scan in the application.
  it('does not import the generated loader when no selection reaches the scan', async () => {
    const projectRoot = createWorkload();
    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;

    const { graph, routes } = await buildCloudShapedGraph(projectRoot, false);

    expect(routes.generatedRoutesForDesign).toEqual([]);
    expect(graph?.nodes.some(({ kind }) => kind === 'api.operation')).toBe(false);
    // The rest of the declaration is unaffected: the selected sibling is still in.
    expect(graph?.nodes.map(({ id }) => id)).toContain('module:@cloud/workload/google-oauth');
  });

  it('imports the generated loader once a selection overlaps the scan', async () => {
    const projectRoot = createWorkload();
    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;

    const { routes } = await buildCloudShapedGraph(projectRoot);

    // The whole scan is read back; the selection narrows attribution, not discovery.
    expect(routes.generatedRoutesForDesign.map(({ method, path }) => `${method} ${path}`).sort()).toEqual([
      'DELETE /auth/token',
      'GET /tasks',
      'POST /auth/token',
    ]);
  });

  it('keeps every association provenance project-relative', async () => {
    const projectRoot = createWorkload();
    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;

    const { graph } = await buildCloudShapedGraph(projectRoot);

    const paths = [
      ...(graph?.nodes ?? []).map(({ provenance }) => provenance?.path),
      ...(graph?.edges ?? []).map(({ provenance }) => provenance?.path),
    ].filter((path): path is string => Boolean(path));
    expect(paths.length).toBeGreaterThan(0);
    expect(paths.some((path) => path.startsWith('/') || path.includes(projectRoot))).toBe(false);
  });

  it('does not change runtime composition, provider order, or lifecycle', async () => {
    const projectRoot = createWorkload();
    process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;

    const { workload } = await buildCloudShapedGraph(projectRoot);

    // The selected module is mounted exactly once, where its composer put it,
    // and the declaration adds no module, plugin, or container of its own.
    expect(workload.collectModules().map(({ name }) => name)).toEqual([
      'auth',
      'google-oauth',
      'opaque-tokens',
      'github-oauth',
    ]);
    // The composer still mounts its children as one DI unit: selection did not
    // promote the selected module to a container of its own.
    expect(workload.collectContainerModules().map(({ name }) => name)).toEqual(['auth']);
    expect(workload.getRegistrations()).toEqual([]);
  });
});
