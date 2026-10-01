import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'bun:test';
import { ApiPlugin, application, HttpPlugin, http } from '@putnami/application';
import releaseEndpoint from '../src/api/release.json/get';
import {
  type PointerFetch,
  resetLatestVersionForTest,
  resolveLatestVersion,
  LATEST_VERSION_TTL_MS,
} from '../src/lib/release/latest-version';

const REGISTRY = 'https://test-registry.example.com';

function pointer(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
}

describe('latest version resolver', () => {
  beforeEach(() => resetLatestVersionForTest());

  it('reads the version the CLI latest channel points at', async () => {
    const urls: string[] = [];
    const fetchPointer: PointerFetch = async (url) => {
      urls.push(url);
      return pointer({ digest: '', version: '0.2.0' });
    };
    expect(await resolveLatestVersion({ registryUrl: `${REGISTRY}/`, fetch: fetchPointer })).toBe('0.2.0');
    expect(urls).toEqual([`${REGISTRY}/putnami/cli/channels/latest`]);
  });

  it('serves the cached version within the TTL and looks up again after it', async () => {
    let calls = 0;
    let clock = 1000;
    const versions = ['0.2.0', '0.3.0'];
    const options = {
      registryUrl: REGISTRY,
      now: () => clock,
      fetch: (async () => pointer({ version: versions[calls++] })) as PointerFetch,
    };
    expect(await resolveLatestVersion(options)).toBe('0.2.0');
    clock += LATEST_VERSION_TTL_MS - 1;
    expect(await resolveLatestVersion(options)).toBe('0.2.0');
    expect(calls).toBe(1);
    clock += 1;
    expect(await resolveLatestVersion(options)).toBe('0.3.0');
    expect(calls).toBe(2);
  });

  it('shares one lookup between concurrent callers', async () => {
    let calls = 0;
    const options = {
      registryUrl: REGISTRY,
      fetch: (async () => {
        calls++;
        return pointer({ version: '0.2.0' });
      }) as PointerFetch,
    };
    const results = await Promise.all([resolveLatestVersion(options), resolveLatestVersion(options)]);
    expect(results).toEqual(['0.2.0', '0.2.0']);
    expect(calls).toBe(1);
  });

  it('keeps the last version when a later lookup fails', async () => {
    let clock = 0;
    let fail = false;
    const options = {
      registryUrl: REGISTRY,
      now: () => clock,
      fetch: (async () => {
        if (fail) throw new Error('registry unreachable');
        return pointer({ version: '0.2.0' });
      }) as PointerFetch,
    };
    expect(await resolveLatestVersion(options)).toBe('0.2.0');
    fail = true;
    clock += LATEST_VERSION_TTL_MS;
    expect(await resolveLatestVersion(options)).toBe('0.2.0');
  });

  it.each([
    ['an HTTP error', () => pointer({ version: '0.2.0' }, 404)],
    ['no version', () => pointer({ digest: '' })],
    ['a release id instead of a version', () => pointer({ version: 'cm_5b1535f0a503e86c848e2cc16ebb37a54c9b12eb' })],
    ['a body that is not JSON', () => new Response('<html>', { status: 200 })],
  ])('reports no version for %s', async (_label, respond) => {
    const fetchPointer: PointerFetch = async () => respond();
    expect(await resolveLatestVersion({ registryUrl: REGISTRY, fetch: fetchPointer })).toBeUndefined();
  });
});

describe('GET /release.json', () => {
  let app: Application;
  let baseUrl: string;
  let previousRegistryUrl: string | undefined;
  let registry: ReturnType<typeof Bun.serve>;
  let advertised: unknown;

  beforeAll(async () => {
    registry = Bun.serve({
      port: 0,
      fetch: (request) =>
        new URL(request.url).pathname === '/putnami/cli/channels/latest'
          ? pointer(advertised)
          : new Response('not found', { status: 404 }),
    });
    previousRegistryUrl = process.env['PUTNAMI_REGISTRY_URL'];
    process.env['PUTNAMI_REGISTRY_URL'] = `http://localhost:${registry.port}`;

    const apiPlugin = new ApiPlugin({ autoScan: false });
    apiPlugin.register('/release.json', { default: releaseEndpoint }, 'GET');
    app = application()
      .use(http({ port: 0 }))
      .use(apiPlugin);
    // See dl.test.ts: a test-local Application must not overwrite the site's
    // generated capability manifest.
    await app.build({ publishCapabilityManifest: false });
    await app.start();
    baseUrl = `http://localhost:${app.getPlugin(HttpPlugin).getServer()?.port ?? 3001}`;
  });

  beforeEach(() => resetLatestVersionForTest());

  afterAll(async () => {
    await app.stop();
    registry.stop(true);
    if (previousRegistryUrl === undefined) {
      process.env['PUTNAMI_REGISTRY_URL'] = undefined;
    } else {
      process.env['PUTNAMI_REGISTRY_URL'] = previousRegistryUrl;
    }
  });

  it('returns the latest version with a five-minute cache', async () => {
    advertised = { digest: '', version: '0.2.0' };
    const res = await fetch(`${baseUrl}/release.json`);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ channel: 'latest', version: '0.2.0' });
    expect(res.headers.get('cache-control')).toBe('public, max-age=300, s-maxage=300, stale-while-revalidate=300');
  });

  it('omits the version when the registry advertises none', async () => {
    advertised = { digest: '' };
    const res = await fetch(`${baseUrl}/release.json`);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ channel: 'latest' });
  });
});
