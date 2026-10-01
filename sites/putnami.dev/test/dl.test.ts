import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { ApiPlugin, application, HttpPlugin, http } from '@putnami/application';
import downloadResolver from '../src/api/dl/[artifact]/get';

describe('putnami.dev download resolver', () => {
  let app: Application;
  let baseUrl: string;
  let registryBaseUrl: string;
  let previousRegistryUrl: string | undefined;

  beforeAll(async () => {
    registryBaseUrl = 'https://test-registry.example.com';
    previousRegistryUrl = process.env['PUTNAMI_REGISTRY_URL'];
    process.env['PUTNAMI_REGISTRY_URL'] = registryBaseUrl;

    const apiPlugin = new ApiPlugin({ autoScan: false });
    apiPlugin.register('/dl/[artifact]', { default: downloadResolver }, 'GET');

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

    const port = app.getPlugin(HttpPlugin).getServer()?.port ?? 3001;
    baseUrl = `http://localhost:${port}`;
  });

  afterAll(async () => {
    await app.stop();
    if (previousRegistryUrl === undefined) {
      delete process.env.PUTNAMI_REGISTRY_URL;
    } else {
      process.env['PUTNAMI_REGISTRY_URL'] = previousRegistryUrl;
    }
  });

  it('redirects an explicit version to registry download endpoint', async () => {
    const res = await fetch(`${baseUrl}/dl/putnami?version=v1.2.3&platform=darwin&target=arm64`, {
      redirect: 'manual',
    });
    expect(res.status).toBe(302);
    expect(res.headers.get('location')).toBe(
      `${registryBaseUrl}/putnami/cli/download?channel=v1.2.3&os=darwin&arch=arm64`,
    );
    expect(res.headers.get('cache-control')).toBe('public, max-age=60, s-maxage=60, stale-while-revalidate=60');
  });

  it('redirects putnami latest to registry download endpoint', async () => {
    const res = await fetch(`${baseUrl}/dl/putnami?version=latest&platform=linux&target=x64`, {
      redirect: 'manual',
    });
    expect(res.status).toBe(302);
    expect(res.headers.get('location')).toBe(
      `${registryBaseUrl}/putnami/cli/download?channel=latest&os=linux&arch=x64`,
    );
  });

  it('maps putnami-go artifact to putnami/go namespace', async () => {
    const res = await fetch(`${baseUrl}/dl/putnami-go?version=latest&platform=darwin&target=arm64`, {
      redirect: 'manual',
    });
    expect(res.status).toBe(302);
    expect(res.headers.get('location')).toBe(
      `${registryBaseUrl}/putnami/go/download?channel=latest&os=darwin&arch=arm64`,
    );
  });

  it('maps putnami-ruby artifact to putnami/ruby namespace', async () => {
    const res = await fetch(`${baseUrl}/dl/putnami-ruby?version=latest&platform=linux&target=arm64`, {
      redirect: 'manual',
    });
    expect(res.status).toBe(302);
    expect(res.headers.get('location')).toBe(
      `${registryBaseUrl}/putnami/ruby/download?channel=latest&os=linux&arch=arm64`,
    );
  });

  it('maps putnami-python artifact to putnami/python namespace', async () => {
    const res = await fetch(`${baseUrl}/dl/putnami-python?version=latest&platform=linux&target=arm64`, {
      redirect: 'manual',
    });
    expect(res.status).toBe(302);
    expect(res.headers.get('location')).toBe(
      `${registryBaseUrl}/putnami/python/download?channel=latest&os=linux&arch=arm64`,
    );
  });

  it('returns 400 when platform/target is missing', async () => {
    const res = await fetch(`${baseUrl}/dl/putnami?version=latest`, { redirect: 'manual' });
    expect(res.status).toBe(400);
  });

  it('rejects the removed misspelled taget query alias', async () => {
    const res = await fetch(`${baseUrl}/dl/putnami?version=latest&platform=linux&taget=arm64`, {
      redirect: 'manual',
    });
    expect(res.status).toBe(400);
  });

  it('publishes request/response metadata for OpenAPI generation', () => {
    const routeDef = downloadResolver as {
      schemas?: { params?: Record<string, unknown>; query?: Record<string, unknown> };
      responses?: { returns?: Array<{ status: number }>; throws?: Array<{ status: number }> };
      meta?: { cache?: unknown };
    };

    expect(routeDef.schemas?.params?.['artifact']).toBeDefined();
    expect(routeDef.schemas?.query?.['platform']).toBeDefined();
    expect(routeDef.schemas?.query?.['target']).toBeDefined();
    expect(routeDef.responses?.returns?.some((r) => r.status === 302)).toBe(true);
    expect(routeDef.responses?.throws?.some((r) => r.status === 400)).toBe(true);
    expect(routeDef.meta?.cache).toBeDefined();
  });
});
