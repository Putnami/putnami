import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { rmSync } from 'node:fs';
import { gunzipSync } from 'node:zlib';
import { application, api, Email, endpoint, grpc, http, Optional, proto, Stream, Uuid } from '@putnami/application';
import {
  fileExists,
  getProjectRoot,
  joinPath,
  readFileContent,
  readPackageJson,
  updatePackageJson,
} from '@putnami/utils';
import { Binary } from '../../src/api/route/binary';
import { type OpenApiPlugin, openapi } from '../../src/openapi/openapi.plugin';

const FIXTURES_API_PATH = `${import.meta.dir}/fixtures/api`;

describe('OpenApiPlugin', () => {
  let originalServeExport: unknown;
  beforeAll(() => {
    const exports = readPackageJson(joinPath(getProjectRoot(), 'package.json'))?.exports;
    if (typeof exports === 'object' && exports) originalServeExport = exports['./serve'];
  });

  afterAll(() => {
    const packageJsonPath = joinPath(getProjectRoot(), 'package.json');
    const packageJson = readPackageJson(packageJsonPath);
    const exports = packageJson?.exports;
    // Generation may add this export, but a committed export belongs to the
    // project. Cleanup must never remove a declaration the suite did not add.
    if (originalServeExport !== undefined) {
      expect(typeof exports === 'object' && exports ? exports['./serve'] : undefined).toEqual(originalServeExport);
      return;
    }
    if (typeof exports === 'object' && exports && './serve' in exports) {
      (exports as Record<string, unknown>)['./serve'] = undefined;
      updatePackageJson(packageJsonPath, packageJson);
    }
  });

  describe('runtime spec generation', () => {
    it('uses the installed Proto and Grpc plugins as the Connect route authority', async () => {
      const apiPlugin = api({
        autoScan: false,
        client: {
          service: { id: 'events', audience: 'api://events' },
          credentials: {},
        },
      });
      apiPlugin.register(
        '/events/[topic]',
        endpoint()
          .params({ topic: String })
          .returns(Stream({ sequence: Number }))
          .client({ idempotency: { kind: 'safe' } })
          .handle(async (ctx) => ctx.send({ sequence: 1 })),
        'GET',
      );
      const openapiPlugin = openapi({ title: 'Events', version: '1.0.0' });
      const app = application()
        .use(http({ port: 0 }))
        .use(apiPlugin)
        .use(proto({ packageName: 'acme.events.v1' }))
        .use(grpc())
        .use(openapiPlugin);

      await app.start();
      try {
        expect(specTransport(openapiPlugin.spec(), '/events/{topic}', 'connect')).toEqual({
          protocol: 'connect',
          path: '/acme.events.v1.EventsService/GetEventsByTopic',
          encoding: 'proto',
          protobufMethod: '/acme.events.v1.EventsService/GetEventsByTopic',
        });
      } finally {
        await app.stop();
      }
    });

    it('advertises only Connect encodings accepted by the installed Grpc plugin', async () => {
      const apiPlugin = api({
        autoScan: false,
        client: {
          service: { id: 'events-json', audience: 'api://events-json' },
          credentials: {},
        },
      });
      apiPlugin.register(
        '/events',
        endpoint()
          .returns({ sequence: Number })
          .handle(() => ({ sequence: 1 })),
        'GET',
      );
      const openapiPlugin = openapi({ title: 'Events JSON', version: '1.0.0' });
      const app = application()
        .use(http({ port: 0 }))
        .use(apiPlugin)
        .use(proto({ packageName: 'acme.events.json.v1' }))
        .use(grpc({ acceptContentTypes: ['application/json'] }))
        .use(openapiPlugin);

      await app.start();
      try {
        expect(
          openapiPlugin
            .spec()
            ?.paths['/events']?.get?.['x-putnami-client']?.transports.filter((entry) => entry.protocol === 'connect'),
        ).toEqual([
          {
            protocol: 'connect',
            path: '/acme.events.json.v1.EventsService/ListEvents',
            encoding: 'json',
            protobufMethod: '/acme.events.json.v1.EventsService/ListEvents',
          },
        ]);
      } finally {
        await app.stop();
      }
    });

    it('announces no Connect transport for a raw octet route, like the Go projection', async () => {
      // A Connect envelope carries an encoded message; raw octets would cross
      // re-encoded. With the gRPC plugin mounted, the route keeps REST alone.
      const apiPlugin = api({
        autoScan: false,
        client: { service: { id: 'blobs', audience: 'api://blobs' }, credentials: {} },
      });
      apiPlugin.register(
        '/blobs/[id]',
        endpoint()
          .params({ id: String })
          .returns(Binary({ mediaType: 'application/octet-stream', maxBytes: 64 }))
          .handle(() => new Uint8Array([0x00, 0xff])),
        'GET',
      );
      apiPlugin.register(
        '/blobs/[id]/name',
        endpoint()
          .params({ id: String })
          .returns({ name: String })
          .handle(() => ({ name: 'blob' })),
        'GET',
      );
      const openapiPlugin = openapi({ title: 'Blobs', version: '1.0.0' });
      const app = application()
        .use(http({ port: 0 }))
        .use(apiPlugin)
        .use(proto({ packageName: 'acme.blobs.v1' }))
        .use(grpc())
        .use(openapiPlugin);

      await app.start();
      try {
        const protocols = (path: string) =>
          openapiPlugin.spec()?.paths[path]?.get?.['x-putnami-client']?.transports.map((entry) => entry.protocol);
        expect(protocols('/blobs/{id}')).toEqual(['rest-json']);
        // The JSON sibling on the same provider still announces Connect first.
        expect(protocols('/blobs/{id}/name')).toEqual(['connect', 'connect', 'rest-json']);
      } finally {
        await app.stop();
      }
    });

    it('should generate spec from registered endpoints at warmup', async () => {
      const httpPlugin = http({ port: 0 });
      const apiPlugin = api({ autoScan: false });
      const openapiPlugin = openapi({ title: 'Test API', version: '1.0.0' });

      apiPlugin.register('/users', {
        GET: endpoint()
          .query({ page: Optional(Number) })
          .returns({ id: String, name: String })
          .handle(() => ({ id: '1', name: 'Test' })),
        POST: endpoint()
          .body({ name: String, email: Email })
          .returns({ id: String, name: String, email: String })
          .handle(async (ctx) => {
            const b = await ctx.body();
            return { id: '2', name: b.name, email: b.email };
          }),
      });

      apiPlugin.register(
        '/users/[id]',
        {
          default: endpoint()
            .params({ id: Uuid })
            .returns({ id: String, name: String })
            .handle((ctx) => ({ id: ctx.params.id, name: 'Test' })),
        },
        'GET',
      );

      const app = application().use(httpPlugin).use(apiPlugin).use(openapiPlugin);
      await app.start();

      const spec = openapiPlugin.spec();
      expect(spec).toBeDefined();
      expect(spec?.openapi).toBe('3.0.3');
      expect(spec?.info.title).toBe('Test API');
      expect(Object.keys(spec?.paths ?? {})).toHaveLength(2);
      expect(spec?.paths['/users'].get).toBeDefined();
      expect(spec?.paths['/users'].post).toBeDefined();
      expect(spec?.paths['/users/{id}'].get).toBeDefined();

      await app.stop();
    });

    it('should return undefined spec when no ApiPlugin registered', async () => {
      const httpPlugin = http({ port: 0 });
      const openapiPlugin = openapi({ title: 'Test', version: '1.0.0' });

      const app = application().use(httpPlugin).use(openapiPlugin);
      await app.start();

      expect(openapiPlugin.spec()).toBeUndefined();

      await app.stop();
    });

    it('should apply prefix to paths in spec', async () => {
      const httpPlugin = http({ port: 0 });
      const apiPlugin = api({ autoScan: false, prefix: '/v1' });
      const openapiPlugin = openapi({ title: 'API', version: '1.0.0' });

      apiPlugin.register('/users', { default: endpoint(() => ({ users: [] })) }, 'GET');

      const app = application().use(httpPlugin).use(apiPlugin).use(openapiPlugin);
      await app.start();

      const spec = openapiPlugin.spec();
      expect(spec?.paths['/v1/users']).toBeDefined();
      expect(spec?.paths['/users']).toBeUndefined();

      await app.stop();
    });
  });

  describe('build-time spec generation', () => {
    afterAll(() => {
      try {
        rmSync(`${import.meta.dir}/../."./.gen`, { recursive: true, force: true });
      } catch {}
    });

    it('should generate openapi.json during build', async () => {
      const httpPlugin = http({ port: 0 });
      const apiPlugin = api({ scanPath: FIXTURES_API_PATH, autoScan: false });
      const openapiPlugin = openapi({
        title: 'Build API',
        version: '1.0.0',
        description: 'Build-time generated spec',
        output: false,
      });

      const app = application().use(httpPlugin).use(apiPlugin).use(openapiPlugin);
      const result = await app.build();

      expect(result.assets?.['schema/openapi.json']).toBeDefined();
      expect(fileExists(result.assets?.['schema/openapi.json'] as string)).toBe(true);

      const specJson = readFileContent(result.assets?.['schema/openapi.json'] as string, 'utf-8');
      const spec = JSON.parse(specJson);

      expect(spec.openapi).toBe('3.0.3');
      expect(spec.info.title).toBe('Build API');
      expect(spec.info.description).toBe('Build-time generated spec');

      const paths = Object.keys(spec.paths);
      expect(paths.length).toBeGreaterThanOrEqual(2);

      // Check root GET
      const rootGet = spec.paths['/'];
      expect(rootGet?.get).toBeDefined();

      // Check POST
      const rootPost = spec.paths['/'];
      expect(rootPost?.post).toBeDefined();

      // Check /users/{id} GET
      const userGet = spec.paths['/users/{id}'];
      expect(userGet?.get).toBeDefined();
      expect(userGet?.get.parameters).toBeDefined();
      const idParam = userGet.get.parameters.find((p: { name: string }) => p.name === 'id');
      expect(idParam).toBeDefined();
      expect(idParam?.in).toBe('path');
      expect(idParam?.schema?.format).toBe('uuid');

      await app.stop();
    });

    it('should include servers in generated spec', async () => {
      const httpPlugin = http({ port: 0 });
      const apiPlugin = api({ scanPath: FIXTURES_API_PATH, autoScan: false });
      const openapiPlugin = openapi({
        title: 'API',
        version: '2.0.0',
        servers: [
          { url: 'https://api.example.com', description: 'Production' },
          { url: 'http://localhost:3000', description: 'Local' },
        ],
        output: false,
      });

      const app = application().use(httpPlugin).use(apiPlugin).use(openapiPlugin);
      const result = await app.build();

      const spec = JSON.parse(readFileContent(result.assets?.['schema/openapi.json'] as string, 'utf-8'));
      expect(spec.servers).toHaveLength(2);
      expect(spec.servers[0].url).toBe('https://api.example.com');
      expect(spec.servers[1].description).toBe('Local');

      await app.stop();
    });

    it('should apply prefix to generated spec paths', async () => {
      const httpPlugin = http({ port: 0 });
      const apiPlugin = api({ scanPath: FIXTURES_API_PATH, autoScan: false, prefix: '/v1' });
      const openapiPlugin = openapi({ title: 'Prefixed API', version: '1.0.0', output: false });

      const app = application().use(httpPlugin).use(apiPlugin).use(openapiPlugin);
      const result = await app.build();

      const spec = JSON.parse(readFileContent(result.assets?.['schema/openapi.json'] as string, 'utf-8'));

      for (const path of Object.keys(spec.paths)) {
        expect(path.startsWith('/v1')).toBe(true);
      }

      await app.stop();
    });

    it('should write to <project>/schema/openapi.json by default and keep .gz in .gen/', async () => {
      const projectRoot = getProjectRoot();
      const committedPath = joinPath(projectRoot, 'schema', 'openapi.json');
      const gzPath = joinPath(projectRoot, '.gen', 'schema', 'openapi.json.gz');
      // Sanity: clear any stale artifacts.
      try {
        rmSync(committedPath, { force: true });
        rmSync(gzPath, { force: true });
      } catch {}

      const httpPlugin = http({ port: 0 });
      const apiPlugin = api({ scanPath: FIXTURES_API_PATH, autoScan: false });
      const openapiPlugin = openapi({ title: 'Default Path API', version: '1.0.0' });
      const app = application().use(httpPlugin).use(apiPlugin).use(openapiPlugin);

      try {
        const result = await app.build();

        expect(result.assets?.['schema/openapi.json']).toBe(committedPath);
        expect(result.assets?.['schema/openapi.json.gz']).toBe(gzPath);
        expect(fileExists(committedPath)).toBe(true);
        expect(fileExists(gzPath)).toBe(true);
      } finally {
        await app.stop();
        try {
          rmSync(committedPath, { force: true });
          rmSync(joinPath(projectRoot, 'schema'), { recursive: true, force: true });
        } catch {}
      }
    });

    it('should write to a custom output path when provided', async () => {
      const projectRoot = getProjectRoot();
      const customPath = joinPath(projectRoot, '.gen', 'custom-openapi.json');
      try {
        rmSync(customPath, { force: true });
      } catch {}

      const httpPlugin = http({ port: 0 });
      const apiPlugin = api({ scanPath: FIXTURES_API_PATH, autoScan: false });
      const openapiPlugin = openapi({
        title: 'Custom Path API',
        version: '1.0.0',
        output: '.gen/custom-openapi.json',
      });
      const app = application().use(httpPlugin).use(apiPlugin).use(openapiPlugin);

      try {
        const result = await app.build();
        expect(result.assets?.['schema/openapi.json']).toBe(customPath);
        expect(fileExists(customPath)).toBe(true);
      } finally {
        await app.stop();
      }
    });

    it('should generate compressed .gz file at build time', async () => {
      const httpPlugin = http({ port: 0 });
      const apiPlugin = api({ scanPath: FIXTURES_API_PATH, autoScan: false });
      const openapiPlugin = openapi({ title: 'Compressed API', version: '1.0.0', output: false });

      const app = application().use(httpPlugin).use(apiPlugin).use(openapiPlugin);
      const result = await app.build();

      expect(result.assets?.['schema/openapi.json.gz']).toBeDefined();
      expect(fileExists(result.assets?.['schema/openapi.json.gz'] as string)).toBe(true);

      const gzipPath = result.assets?.['schema/openapi.json.gz'] as string;
      const gzipStat = readFileContent(gzipPath);
      expect(gzipStat.length).toBeGreaterThan(0);
      expect(gzipStat.length).toBeLessThan(readFileContent(result.assets?.['schema/openapi.json'] as string).length);
      expect(gunzipSync(gzipStat).toString('utf8')).toBe(
        readFileContent(result.assets?.['schema/openapi.json'] as string, 'utf8'),
      );

      await app.stop();
    });
  });

  describe('endpoint exposure', () => {
    afterAll(() => {
      try {
        rmSync(`${import.meta.dir}/../."./.gen`, { recursive: true, force: true });
      } catch {}
    });

    it('should NOT expose endpoint by default', async () => {
      const httpPlugin = http({ port: 3456 });
      const apiPlugin = api({ autoScan: false });
      const openapiPlugin = openapi({ title: 'Private API', version: '1.0.0' });

      apiPlugin.register('/test', { default: endpoint(() => ({ ok: true })) }, 'GET');

      const app = application().use(httpPlugin).use(apiPlugin).use(openapiPlugin);
      await app.start();

      const response = await fetch('http://localhost:3456/_/openapi.json');
      expect(response.status).toBe(404);

      await app.stop();
    });

    it('should expose endpoint when exposeRoute is true', async () => {
      const httpPlugin = http({ port: 3457 });
      const apiPlugin = api({ scanPath: FIXTURES_API_PATH, autoScan: false });
      const openapiPlugin = openapi({
        title: 'Public API',
        version: '1.0.0',
        exposeRoute: true,
        output: false,
      });

      const app = application().use(httpPlugin).use(apiPlugin).use(openapiPlugin);
      await app.build();
      await app.start();

      const response = await fetch('http://localhost:3457/_/openapi.json');
      expect(response.status).toBe(200);
      expect(response.headers.get('content-type')).toBe('application/json');
      expect(response.headers.get('content-encoding')).toBe('gzip');

      const spec = await response.json();
      expect(spec.openapi).toBe('3.0.3');
      expect(spec.info.title).toBe('Public API');

      await app.stop();
    });

    it('should expose endpoint at custom path when publicRoute is set', async () => {
      const httpPlugin = http({ port: 3458 });
      const apiPlugin = api({ scanPath: FIXTURES_API_PATH, autoScan: false });
      const openapiPlugin = openapi({
        title: 'Custom Route API',
        version: '1.0.0',
        exposeRoute: true,
        publicRoute: '/api/docs.json',
        output: false,
      });

      const app = application().use(httpPlugin).use(apiPlugin).use(openapiPlugin);
      await app.build();
      await app.start();

      const response = await fetch('http://localhost:3458/api/docs.json');
      expect(response.status).toBe(200);

      const spec = await response.json();
      expect(spec.info.title).toBe('Custom Route API');

      await app.stop();
    });

    it('should return 404 when exposeRoute is true but file does not exist', async () => {
      // Clean up generated files from workspace root (where getProjectRoot() points)
      try {
        const { getProjectRoot } = await import('@putnami/utils');
        const { joinPath } = await import('@putnami/utils');
        rmSync(joinPath(getProjectRoot(), '.gen', 'schema'), { recursive: true, force: true });
        rmSync(joinPath(getProjectRoot(), 'schema', 'openapi.json'), { force: true });
      } catch {}

      const httpPlugin = http({ port: 3459 });
      const apiPlugin = api({ autoScan: false });
      const openapiPlugin = openapi({
        title: 'No File API',
        version: '1.0.0',
        exposeRoute: true,
        output: false,
      });

      apiPlugin.register('/test', { default: endpoint(() => ({ ok: true })) }, 'GET');

      const app = application().use(httpPlugin).use(apiPlugin).use(openapiPlugin);
      // Don't run build() - so no file is generated
      await app.start();

      const response = await fetch('http://localhost:3459/_/openapi.json');
      expect(response.status).toBe(404);

      await app.stop();
    });
  });
});

function specTransport(spec: ReturnType<OpenApiPlugin['spec']>, path: string, protocol: string) {
  return spec?.paths[path]?.get?.['x-putnami-client']?.transports.find((entry) => entry.protocol === protocol);
}
