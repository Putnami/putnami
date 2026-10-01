import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { cpSync, mkdirSync } from 'node:fs';
import { ApiPlugin, application, HttpPlugin, http } from '@putnami/application';
import { specTest } from '@putnami/spectest';
import { fileExists, joinPath } from '@putnami/utils';
import docMarkdown from '../src/api/doc-markdown/get';
import llmsTxt from '../src/api/llms.txt/get';

/**
 * The agent-facing surfaces: a machine-readable `/llms.txt` index and the
 * `/doc-markdown` raw-source endpoint behind "Copy as Markdown". Both resolve
 * against the generated docs tree, so the suite seeds `.gen/public/docs` the
 * same way the integration suite does when the build hasn't populated it.
 */
describe('putnami.dev agent surfaces', () => {
  let app: Application;
  let baseUrl: string;

  beforeAll(async () => {
    const projectRoot = joinPath(import.meta.dir, '..');
    const generatedDocsDir = joinPath(projectRoot, '.gen', 'public', 'docs');
    if (!fileExists(generatedDocsDir)) {
      mkdirSync(generatedDocsDir, { recursive: true });
      cpSync(joinPath(projectRoot, 'doc'), generatedDocsDir, { recursive: true });
    }

    const apiPlugin = new ApiPlugin({ autoScan: false });
    apiPlugin.register('/llms.txt', { default: llmsTxt }, 'GET');
    apiPlugin.register('/doc-markdown', { default: docMarkdown }, 'GET');

    app = application()
      .use(http({ port: 0 }))
      .use(apiPlugin);
    // A test-local Application must never publish a capability manifest: the
    // producer writes .gen/schema/capabilities.json, which the scheduler's
    // generate step owns. Publishing here overwrote the site's manifest with
    // this two-plugin app's, erasing the schemas and the domainAccess evidence
    // rows the build had just emitted.
    await app.build({ publishCapabilityManifest: false });
    await app.start();

    baseUrl = `http://localhost:${app.getPlugin(HttpPlugin).getServer()?.port ?? 3001}`;
  });

  afterAll(async () => {
    await app.stop();
  });

  it('serves /llms.txt as a markdown index of the docs tree', async () => {
    const res = await fetch(`${baseUrl}/llms.txt`);
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toContain('text/plain');

    const body = await res.text();
    expect(body).toContain('# Putnami');
    expect(body).toContain('## Start');
    expect(body).toContain('https://putnami.dev/docs/getting-started');
  });

  it('serves a doc page as raw source markdown', async () => {
    const res = await fetch(`${baseUrl}/doc-markdown?path=/docs/getting-started`);
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toContain('markdown');

    const body = await res.text();
    expect(body.length).toBeGreaterThan(0);
    expect(body).toContain('#');
  });

  specTest(
    'indexes the same generated documentation route the raw-markdown endpoint serves',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'agent-surfaces-track-the-docs-tree',
      check: 'index-and-raw-markdown-share-a-doc-route',
    },
    async () => {
      const index = await fetch(`${baseUrl}/llms.txt`);
      expect(index.status).toBe(200);
      expect(await index.text()).toContain('https://putnami.dev/docs/getting-started');

      const source = await fetch(`${baseUrl}/doc-markdown?path=/docs/getting-started`);
      expect(source.status).toBe(200);
      expect(source.headers.get('content-type')).toContain('markdown');
      expect((await source.text()).length).toBeGreaterThan(0);
    },
  );

  specTest(
    'publishes the complete Python boundary in the machine-readable documentation index',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'python-is-marked-experimental',
      check: 'llms-index-publishes-full-python-boundary',
    },
    async () => {
      const response = await fetch(`${baseUrl}/llms.txt`);
      expect(response.status).toBe(200);
      const body = await response.text();

      expect(body).toContain('Python is experimental');
      expect(body).toContain('requires explicit opt-in');
      expect(body).toContain('is never enabled by default');
      expect(body).toContain('carries no Go or TypeScript parity promise');
    },
  );

  it('accepts a path without the /docs prefix', async () => {
    const res = await fetch(`${baseUrl}/doc-markdown?path=getting-started`);
    expect(res.status).toBe(200);
  });

  it('returns 404 for an unknown doc-markdown path', async () => {
    const res = await fetch(`${baseUrl}/doc-markdown?path=/docs/definitely-not-real`);
    expect(res.status).toBe(404);
  });
});
