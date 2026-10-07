import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { ApiPlugin, application, HttpPlugin, http } from '@putnami/application';
import agentReadinessMethod from '../src/api/agent-readiness/method/get';
import { getDocsPaths } from '../src/lib/docs/navigation.server';
import { discoverSchemaSources, planSchemaPublications, SCHEMA_ORIGIN } from '../src/lib/schemas/publish';

// The workspace root is three levels up from sites/putnami.dev/test/.
const WORKSPACE_ROOT = join(import.meta.dir, '..', '..', '..');
const HOSTED_SCHEMAS = 'sites/putnami.dev/hosted-schemas/*/*.json';
const HOSTED_ROOT = join(import.meta.dir, '..', 'hosted-schemas', 'agent-readiness');

describe('agent-readiness method link', () => {
  let app: Application;
  let baseUrl: string;

  beforeAll(async () => {
    const apiPlugin = new ApiPlugin({ autoScan: false });
    apiPlugin.register('/agent-readiness/method', { default: agentReadinessMethod }, 'GET');
    app = application()
      .use(http({ port: 0 }))
      .use(apiPlugin);
    // A test-local Application must never publish a capability manifest (see dl.test.ts).
    await app.build({ publishCapabilityManifest: false });
    await app.start();
    const port = app.getPlugin(HttpPlugin).getServer()?.port ?? 3001;
    baseUrl = `http://localhost:${port}`;
  });

  afterAll(async () => {
    await app.stop();
  });

  it('redirects to the method documentation page', async () => {
    const res = await fetch(`${baseUrl}/agent-readiness/method`, { redirect: 'manual' });
    expect(res.status).toBe(302);
    expect(res.headers.get('location')).toBe('/docs/platform/intelligence/agent-readiness-method');
  });

  it('points at a page the pinned docs bundle serves', async () => {
    // The page ships in the platform content bundle. A content.lock.json pin
    // older than the page turns the redirect into a 404.
    const paths = new Set(await getDocsPaths());
    expect(paths.has('platform/intelligence/agent-readiness-method')).toBe(true);
  });
});

describe('agent-readiness hosted schemas', () => {
  it('keeps hosted schemas identical to the public collector contract', () => {
    const collectorRoot = join(WORKSPACE_ROOT, 'intelligence', 'agent-readiness', 'schema');
    for (const name of readdirSync(HOSTED_ROOT)) {
      expect(readFileSync(join(HOSTED_ROOT, name), 'utf8')).toBe(readFileSync(join(collectorRoot, name), 'utf8'));
    }
  });

  it('publishes the payload and report schemas at their $id', () => {
    const plan = planSchemaPublications(discoverSchemaSources(WORKSPACE_ROOT, { sources: [HOSTED_SCHEMAS] }));
    expect(plan.map((p) => p.urlPath).sort()).toEqual([
      'agent-readiness/payload.v1.json',
      'agent-readiness/report.v1.json',
    ]);
  });

  it('keeps each file where its $id says, so a copy cannot land under the wrong name', () => {
    for (const name of readdirSync(HOSTED_ROOT)) {
      const schema = JSON.parse(readFileSync(join(HOSTED_ROOT, name), 'utf8')) as { $id?: string; $schema?: string };
      expect(schema.$id).toBe(`${SCHEMA_ORIGIN}agent-readiness/${name}`);
      expect(schema.$schema).toBe('https://json-schema.org/draft/2020-12/schema');
    }
  });

  it('keeps the bytes the collector emits: two-space indent and a final newline', () => {
    for (const name of readdirSync(HOSTED_ROOT)) {
      const text = readFileSync(join(HOSTED_ROOT, name), 'utf8');
      expect(text.endsWith('}\n')).toBe(true);
      expect(`${JSON.stringify(JSON.parse(text), null, 2)}\n`).toBe(text);
    }
  });
});
