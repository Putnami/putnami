/**
 * The docs hub as the deployed image serves it.
 *
 * The image holds the compiled server and its `.gen` output, and nothing else:
 * no `putnami.workspace.json`, no `putnami.support.json`. The hub is an ISR
 * page, so the server renders it live whenever its `docs` tag is revalidated
 * (a content-overlay swap) or its TTL lapses. A live render that reaches for a
 * workspace file fails there, and every `/docs` request answers 500 until the
 * next one succeeds.
 *
 * These tests render the hub live with no workspace in reach: the workspace
 * root points at an empty directory, as it resolves to nothing in the image.
 */
import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { cpSync, mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { type Application, HttpPlugin, application, http, staticFiles } from '@putnami/application';
import { fileExists, joinPath } from '@putnami/utils';
import { ReactPlugin, react, revalidateTag } from '@putnami/web';
import { statusLabel, statusOf } from '../src/lib/support/catalog';
import { readSupportCatalog } from '../src/lib/support/catalog.server';
import { TOOLS } from '../src/lib/tools';

const PROJECT_ROOT = joinPath(import.meta.dir, '..');
const WORKSPACE_ROOT = joinPath(PROJECT_ROOT, '..', '..');
const GENERATED_REACT_APP = '.gen/src/app/.react-application.gen.tsx';

async function start(app: Application): Promise<void> {
  // Same as app.test.ts: `putnami build` already generated the routes, and the
  // generate phase spawns `bun build`, which the test runner may not resolve.
  if (fileExists(joinPath(PROJECT_ROOT, GENERATED_REACT_APP))) {
    const reactPlugin = app.findPlugin(ReactPlugin);
    if (reactPlugin) reactPlugin.reactApplicationPath = GENERATED_REACT_APP;
    const plugins = app.collectPlugins();
    for (const { plugin, owner } of plugins) {
      // biome-ignore lint/performance/noAwaitInLoops: plugins warm up in order
      await plugin.warmup?.(owner);
    }
    await Promise.all(plugins.map(({ plugin, owner }) => plugin.start?.(owner)));
    app.markAsRunning();
    return;
  }
  await app.build({ publishCapabilityManifest: false });
  await app.start();
}

describe('docs hub served without a workspace', () => {
  let app: Application;
  let baseUrl: string;
  let noWorkspace: string;
  const previousWorkspaceRoot = process.env['PUTNAMI_WORKSPACE_ROOT'];

  beforeAll(async () => {
    const generatedDocsDir = joinPath(PROJECT_ROOT, '.gen', 'public', 'docs');
    if (!fileExists(generatedDocsDir)) {
      mkdirSync(generatedDocsDir, { recursive: true });
      cpSync(joinPath(PROJECT_ROOT, 'doc'), generatedDocsDir, { recursive: true });
    }

    app = application()
      .use(http({ port: 0 }))
      .use(react({ scanPath: joinPath(PROJECT_ROOT, 'src', 'app') }))
      .use(staticFiles({ publicFolder: 'public' }));
    await start(app);
    baseUrl = `http://localhost:${app.getPlugin(HttpPlugin).getServer()?.port}`;

    // From here on the process sees what the image sees: no workspace.
    noWorkspace = mkdtempSync(joinPath(tmpdir(), 'putnami-dev-no-workspace-'));
    process.env['PUTNAMI_WORKSPACE_ROOT'] = noWorkspace;
    // A content swap marks every `docs` page stale: the next serve renders live.
    await revalidateTag('docs');
  });

  afterAll(async () => {
    if (previousWorkspaceRoot === undefined) delete process.env['PUTNAMI_WORKSPACE_ROOT'];
    else process.env['PUTNAMI_WORKSPACE_ROOT'] = previousWorkspaceRoot;
    rmSync(noWorkspace, { recursive: true, force: true });
    await app.stop();
  });

  it('renders /docs live', async () => {
    const res = await fetch(`${baseUrl}/docs`);
    const html = await res.text();
    expect(res.status).toBe(200);
    expect(html).toContain('<title>Documentation — Putnami</title>');
  });

  it('shows the reviewed support status on the live hub', async () => {
    const catalog = readSupportCatalog(WORKSPACE_ROOT);
    const classified = Object.values(TOOLS).find(
      (tool) => tool.supportSubject && statusOf(catalog, tool.supportSubject),
    );
    expect(classified).toBeDefined();
    const status = classified?.supportSubject ? statusOf(catalog, classified.supportSubject) : undefined;
    if (!status) throw new Error('no documented surface has a reviewed support status');
    const html = await (await fetch(`${baseUrl}/docs`)).text();
    expect(html).toContain(statusLabel(status));
  });

  it('answers /docs/ with the same hub', async () => {
    const res = await fetch(`${baseUrl}/docs/`);
    expect(res.status).toBe(200);
    expect(await res.text()).toContain('<title>Documentation — Putnami</title>');
  });

  it('redirects /fr/docs to a hub that answers 200', async () => {
    const redirect = await fetch(`${baseUrl}/fr/docs`, { redirect: 'manual' });
    expect(redirect.status).toBe(308);
    const location = redirect.headers.get('location') ?? '';
    expect(new URL(location, baseUrl).pathname).toBe('/docs');

    const res = await fetch(new URL(new URL(location, baseUrl).pathname, baseUrl));
    expect(res.status).toBe(200);
    expect(await res.text()).toContain('<title>Documentation — Putnami</title>');
  });
});
