import { afterEach, beforeEach, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { application, HttpPlugin } from '@putnami/application';
import { resolve } from 'node:path';
import { loader } from '../../src/ssr/loader';
import { ReactApplication } from '../../src/ssr/react-application';
import { createMockReactConfig } from '../fixtures/route.fixtures';

/**
 * A layout under a dynamic segment serves its loader at
 * `<layout path>-layout.json`. The server must answer that endpoint with the
 * same loader and params as the server render of a page under the layout,
 * because client navigation fetches it in place of the render.
 */
describe('dynamic layout loader endpoint', () => {
  const packageRoot = resolve(import.meta.dir, '..', '..');
  const workspaceRoot = resolve(packageRoot, '..', '..', '..');
  const sameParams = {
    feature: 'typescript/web-application-delivery',
    requirement: 'layout-loader-endpoint',
    check: 'the-server-answers-a-dynamic-layout-endpoint-with-the-render-params',
  };

  type Call = { layout: string; params: Record<string, string> };
  let calls: Call[];
  let stop: () => Promise<void>;
  let origin: string;

  beforeEach(async () => {
    process.env['PUTNAMI_WORKSPACE_ROOT'] = workspaceRoot;
    process.env['PUTNAMI_PROJECT_ROOT'] = packageRoot;
    process.env['PWD'] = packageRoot;
    calls = [];

    const recordingLoader = (layout: string) => ({
      loader: loader((ctx) => {
        const params = { ...(ctx.params ?? {}) };
        calls.push({ layout, params });
        return { layout, params };
      }),
    });

    const rootPlugin = new HttpPlugin({ port: 0, secure: false });
    const reactApp = new ReactApplication(createMockReactConfig(), new HttpPlugin());
    reactApp.reactLayout('/w/:workspace', { layout: { default: () => null }, loader: recordingLoader('workspace') });
    reactApp.reactLayout('/w/:workspace/p/:project', {
      layout: { default: () => null },
      loader: recordingLoader('project'),
    });
    reactApp.reactPage('/w/:workspace/settings', { page: { default: () => 'settings' } });
    reactApp.reactPage('/w/:workspace/p/:project/runs', { page: { default: () => 'runs' } });
    rootPlugin.merge(reactApp.getHttpPlugin());

    const app = application().use(rootPlugin);
    await app.start();
    origin = `http://localhost:${rootPlugin.getServer()?.port}`;
    stop = () => app.stop();
  });

  afterEach(async () => {
    await stop();
  });

  const fetchLayoutData = async (path: string) => {
    const response = await fetch(`${origin}${path}`, {
      headers: { Accept: 'application/json', 'X-Putnami-Loader': '1' },
    });
    expect(response.status).toBe(200);
    return response.json();
  };

  const renderedParams = async (path: string, layout: string) => {
    calls = [];
    const response = await fetch(`${origin}${path}`, { headers: { Accept: 'text/html' } });
    expect(response.status).toBe(200);
    await response.text();
    return calls.find((call) => call.layout === layout)?.params;
  };

  specTest('answers /w/acme-layout.json with the layout loader and the render params', sameParams, async () => {
    const data = await fetchLayoutData('/w/acme-layout.json');

    expect(data).toEqual({ layout: 'workspace', params: { workspace: 'acme' } });
    expect(await renderedParams('/w/acme/settings', 'workspace')).toEqual({ workspace: 'acme' });
  });

  specTest('keeps a value that contains a hyphen whole', sameParams, async () => {
    const data = await fetchLayoutData('/w/team-a-layout.json');

    expect(data).toEqual({ layout: 'workspace', params: { workspace: 'team-a' } });
  });

  specTest('passes an encoded value as the render receives it', sameParams, async () => {
    const data = await fetchLayoutData('/w/a%20b-layout.json');

    expect(data).toEqual({ layout: 'workspace', params: { workspace: 'a%20b' } });
    expect(await renderedParams('/w/a%20b/settings', 'workspace')).toEqual({ workspace: 'a%20b' });
  });

  specTest('answers a nested dynamic layout with every ancestor param', sameParams, async () => {
    const data = await fetchLayoutData('/w/acme/p/web-layout.json');

    expect(data).toEqual({ layout: 'project', params: { workspace: 'acme', project: 'web' } });
    expect(await renderedParams('/w/acme/p/web/runs', 'project')).toEqual({ workspace: 'acme', project: 'web' });
  });
});
