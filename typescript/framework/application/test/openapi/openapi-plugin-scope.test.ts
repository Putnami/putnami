import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { api, application, endpoint, http, module } from '@putnami/application';
import { readFileContent } from '@putnami/utils';
import type { GenerateResult } from '../../src/application';
import { openapi } from '../../src/openapi/openapi.plugin';

/**
 * What one openapi() document describes: every api() plugin registered on the
 * module it resolves, whether a route was registered or scanned, under the one
 * client contract those plugins share.
 */

const FIXTURES_API_PATH = `${import.meta.dir}/fixtures/api`;
// Its own scan root: the generated loader bakes the prefix in, so sharing a
// folder with a test that scans it under another prefix would race that loader.
const MODULE_PATH_API_PATH = `${import.meta.dir}/fixtures/module-path-api`;
const BUILD_OPTIONS = { publishCapabilityManifest: false, publishDesignGraph: false };

const authServer = {
  service: { id: 'auth-server', audience: 'auth-server' },
  credentials: { service: { kind: 'service-token' as const } },
};

// biome-ignore lint/suspicious/noExplicitAny: the written spec is read back as plain JSON
function writtenSpec(result: GenerateResult, key = 'schema/openapi.json'): any {
  const path = result.assets?.[key];
  if (!path) throw new Error(`openapi() wrote no spec under ${key}`);
  return JSON.parse(readFileContent(path, 'utf-8'));
}

function publicSurface() {
  return api({ autoScan: false }).register(
    '/public',
    endpoint(() => ({ ok: true })),
    'GET',
  );
}

function revocation() {
  return endpoint()
    .csrfExempt()
    .body({ reference: String })
    .response(204, 'Revoked')
    .client({ security: { alternatives: [{ allOf: [{ profile: 'service' }] }] }, idempotency: { kind: 'idempotent' } })
    .handle(() => undefined);
}

describe('openapi() scope', () => {
  specTest(
    'publishes the routes a manual api() registered in the build-time document',
    {
      feature: 'typescript/api-contracts',
      requirement: 'contract-scope',
      check: 'registered-routes-are-published-in-the-build-time-document',
    },
    async () => {
      // The subset the issue declared: a module that owns a first-party
      // contract for its routes only, beside a public surface elsewhere.
      const internal = module('auth-server-internal')
        .use(
          api({ autoScan: false, csrf: true, client: authServer }).register('/internal/_repro', revocation(), 'POST'),
        )
        .use(openapi({ title: 'x', version: '1.0.0', output: false }));
      const publicApi = api({ autoScan: false }).register(
        '/public',
        endpoint(() => ({ ok: true })),
        'GET',
      );
      const app = application()
        .use(http({ port: 0 }))
        .use(publicApi)
        .use(internal);

      const spec = writtenSpec(await app.build(BUILD_OPTIONS));

      expect(spec['x-putnami-client'].service).toEqual(authServer.service);
      expect(Object.keys(spec.paths)).toEqual(['/internal/_repro']);
      expect(spec.paths['/internal/_repro'].post['x-putnami-client']).toMatchObject({
        idempotency: { kind: 'idempotent' },
      });
    },
  );

  specTest(
    'publishes every api() of the module, registered and scanned, in one document',
    {
      feature: 'typescript/api-contracts',
      requirement: 'contract-scope',
      check: 'every-api-plugin-of-the-module-is-published-in-one-document',
    },
    async () => {
      // Two separately built but equal contracts are one contract.
      const reordered = {
        credentials: authServer.credentials,
        service: { audience: 'auth-server', id: 'auth-server' },
      };
      const scanned = api({ scanPath: FIXTURES_API_PATH, autoScan: false, client: authServer });
      const registered = api({ autoScan: false, client: reordered }).register('/revocations', revocation(), 'POST');
      const openapiPlugin = openapi({ title: 'Merged', version: '1.0.0', output: false });
      const app = application()
        .use(http({ port: 0 }))
        .use(scanned)
        .use(registered)
        .use(openapiPlugin)
        // A sibling module with a contract of its own is not in this document.
        .use(
          module('elsewhere').use(
            api({
              autoScan: false,
              client: { service: { id: 'other', audience: 'other' }, credentials: {} },
            }).register('/elsewhere', revocation(), 'POST'),
          ),
        );

      const spec = writtenSpec(await app.build(BUILD_OPTIONS));
      const paths = Object.keys(spec.paths);

      // The fixture's scanned routes and the registered one, nothing else.
      expect(paths).toContain('/');
      expect(paths).toContain('/users/{id}');
      expect(paths).toContain('/revocations');
      expect(paths).not.toContain('/elsewhere');

      // The runtime document is drawn from the same plugins.
      const runtime = application()
        .use(http({ port: 0 }))
        .use(
          api({ autoScan: false }).register(
            '/a',
            endpoint(() => ({ ok: true })),
            'GET',
          ),
        )
        .use(
          api({ autoScan: false }).register(
            '/b',
            endpoint(() => ({ ok: true })),
            'GET',
          ),
        );
      const runtimeOpenapi = openapi({ title: 'Runtime', version: '1.0.0' });
      runtime.use(runtimeOpenapi);
      await runtime.start();
      try {
        expect(Object.keys(runtimeOpenapi.spec()?.paths ?? {})).toEqual(['/a', '/b']);
      } finally {
        await runtime.stop();
      }
    },
  );

  specTest(
    'refuses a document whose api() plugins declare different client contracts',
    {
      feature: 'typescript/api-contracts',
      requirement: 'contract-scope',
      check: 'api-plugins-that-disagree-on-the-client-contract-are-refused-at-generation',
    },
    async () => {
      const mixed = () =>
        application()
          .use(http({ port: 0 }))
          .use(api({ autoScan: false, client: authServer }).register('/internal', revocation(), 'POST'))
          // No contract: publishing this route under auth-server's would claim
          // a first-party operation this plugin does not serve as one.
          .use(
            api({ autoScan: false }).register(
              '/public',
              endpoint(() => ({ ok: true })),
              'GET',
            ),
          )
          .use(openapi({ title: 'Mixed', version: '1.0.0', output: false }));

      await expect(mixed().build(BUILD_OPTIONS)).rejects.toThrow('declare different client contracts');
      await expect(mixed().start()).rejects.toThrow('declare different client contracts');
    },
  );

  specTest(
    'gives every openapi() document of an application its own output, asset key and route',
    {
      feature: 'typescript/api-contracts',
      requirement: 'document-slots',
      check: 'every-openapi-document-writes-its-own-output',
    },
    async () => {
      // The public document is registered first, yet the contract takes the
      // project's document: that is the one every client consumer reads.
      const server = http({ port: 0 });
      const app = application()
        .use(server)
        .use(publicSurface())
        .use(openapi({ title: 'Public', version: '1.0.0', output: false, exposeRoute: true }))
        .use(
          module('auth-server-internal')
            .use(
              api({ autoScan: false, csrf: true, client: authServer }).register(
                '/internal/_repro',
                revocation(),
                'POST',
              ),
            )
            .use(openapi({ title: 'Internal', version: '1.0.0', output: false, exposeRoute: true })),
        );

      const result = await app.build(BUILD_OPTIONS);
      const contract = writtenSpec(result);
      const extra = writtenSpec(result, 'schema/openapi-1.json');

      expect(contract.info.title).toBe('Internal');
      expect(contract['x-putnami-client'].service).toEqual(authServer.service);
      expect(Object.keys(contract.paths)).toEqual(['/internal/_repro']);
      expect(extra.info.title).toBe('Public');
      expect(extra['x-putnami-client']).toBeUndefined();
      expect(Object.keys(extra.paths)).toEqual(['/public']);
      expect(result.assets?.['schema/openapi-1.json']).toEndWith('.gen/schema/openapi-1.json');
      expect(result.assets?.['schema/openapi-1.json.gz']).toEndWith('.gen/schema/openapi-1.json.gz');
      expect(result.assets?.['schema/openapi.json.gz']).toEndWith('.gen/schema/openapi.json.gz');

      await app.start();
      try {
        const base = `http://localhost:${server.getServer()?.port}`;
        const served = await Promise.all(
          ['/_/openapi.json', '/_/openapi-1.json'].map(
            async (route) => (await (await fetch(base + route)).json()).info,
          ),
        );
        expect(served.map((info) => info.title)).toEqual(['Internal', 'Public']);
      } finally {
        await app.stop();
      }
    },
  );

  specTest(
    'refuses an application whose openapi() documents publish two client contracts',
    {
      feature: 'typescript/api-contracts',
      requirement: 'document-slots',
      check: 'an-application-publishes-at-most-one-client-contract',
    },
    async () => {
      const contractModule = (name: string, path: string) =>
        module(name)
          .use(api({ autoScan: false, client: authServer }).register(path, revocation(), 'POST'))
          .use(openapi({ title: name, version: '1.0.0', output: false }));
      const app = application()
        .use(http({ port: 0 }))
        .use(contractModule('first', '/first'))
        .use(contractModule('second', '/second'));

      await expect(app.build(BUILD_OPTIONS)).rejects.toThrow('2 openapi() documents of this application publish');
    },
  );

  specTest(
    "publishes scanned routes under the module's .path() at build time, as the runtime document does",
    {
      feature: 'typescript/api-contracts',
      requirement: 'contract-scope',
      check: 'build-time-and-runtime-documents-agree-on-module-paths',
    },
    async () => {
      const openapiPlugin = openapi({ title: 'Items', version: '1.0.0', output: false });
      const app = application()
        .use(http({ port: 0 }))
        .use(
          module('items')
            .path('/items-api')
            .use(api({ scanPath: MODULE_PATH_API_PATH, autoScan: false }))
            .use(openapiPlugin),
        );

      const buildPaths = Object.keys(writtenSpec(await app.build(BUILD_OPTIONS)).paths).sort();
      expect(buildPaths).toEqual(['/items-api', '/items-api/items/{id}']);

      await app.start();
      try {
        expect(Object.keys(openapiPlugin.spec()?.paths ?? {}).sort()).toEqual(buildPaths);
      } finally {
        await app.stop();
      }
    },
  );
});
