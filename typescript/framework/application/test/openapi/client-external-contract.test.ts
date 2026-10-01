import { afterEach, describe, expect } from 'bun:test';
import { NotFoundException } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import {
  api,
  application,
  type ClientOperationPolicy,
  type DiscoveredRoute,
  endpoint,
  Env,
  grpc,
  http,
  MapOf,
} from '@putnami/application';
import { generateOpenApiSpec } from '../../src/openapi/openapi';
import { openapi } from '../../src/openapi/openapi.plugin';
import { generateProto } from '../../src/proto/proto';
import { proto } from '../../src/proto/proto.plugin';
import { createTestApp } from '../../src/testing';
import type { TestApp } from '../../src/testing/test-app';

/**
 * A provider that serves a standard protocol beside its own routes marks the
 * standard legs with `.client({ external })`: they stay served and documented,
 * and they leave the first-party contract (clientcontract ADR 0011).
 */

const OCI = 'OCI Distribution Specification v1.1';
const anonymous = { alternatives: [{ allOf: [] }] } as const;
const clientService = {
  service: { id: 'oci-server', audience: 'oci-server' },
  credentials: { user: { kind: 'forwarded-user-token' as const } },
};

function registryApi(
  external: ClientOperationPolicy = { external: OCI },
  client: typeof clientService | null = clientService,
) {
  const apiPlugin = api({ autoScan: false, ...(client ? { client } : {}) });
  apiPlugin.register(
    '/v2/[name]/manifests/[reference]',
    endpoint()
      .params({ name: String, reference: String })
      .returns({ schemaVersion: Number, mediaType: String })
      .client(external)
      .handle(() => ({ schemaVersion: 2, mediaType: 'application/vnd.oci.image.manifest.v1+json' })),
    'GET',
  );
  apiPlugin.register(
    '/v2/_putnami/capabilities',
    endpoint()
      .returns({ copy: Boolean, revert: Boolean })
      .client({ security: anonymous, idempotency: { kind: 'safe' } })
      .handle(() => ({ copy: true, revert: false })),
    'GET',
  );
  return apiPlugin;
}

/** One route as the api plugin records it, for generation without a plugin. */
function externalRoute(policy: ClientOperationPolicy): DiscoveredRoute {
  return {
    method: 'GET',
    path: '/v2/[name]/manifests/[reference]',
    schemas: { params: { name: String, reference: String } },
    meta: { client: policy },
  };
}

describe('an operation an external authority owns', () => {
  specTest(
    'is published with its authority, without client metadata or a protobuf method',
    {
      feature: 'typescript/api-contracts',
      requirement: 'external-contract-operations',
      check: 'an-external-route-is-published-with-its-authority-and-no-client-metadata',
    },
    async () => {
      const openapiPlugin = openapi({ title: 'Registry', version: '1.0.0' });
      const app = application()
        .use(http({ port: 0 }))
        .use(registryApi())
        .use(proto({ packageName: 'registry.v1' }))
        .use(grpc())
        .use(openapiPlugin);
      await app.start();
      try {
        const spec = openapiPlugin.spec();
        const external = spec?.paths['/v2/{name}/manifests/{reference}']?.get;
        expect(external?.['x-putnami-external-contract']).toBe(OCI);
        expect(external?.['x-putnami-client']).toBeUndefined();
        expect(external?.parameters?.map((parameter) => parameter.name)).toEqual(['name', 'reference']);

        const firstParty = spec?.paths['/v2/_putnami/capabilities']?.get;
        expect(firstParty?.['x-putnami-client']).toBeDefined();
        expect(firstParty?.['x-putnami-external-contract']).toBeUndefined();

        // The published descriptor binds the first-party route alone.
        const methods = spec?.['x-putnami-client']?.protobuf?.services.flatMap((service) =>
          service.methods.map((method) => method.name),
        );
        expect(methods?.length).toBe(1);
        expect(methods?.some((name) => name.includes('Manifests'))).toBe(false);
      } finally {
        await app.stop();
      }
    },
  );

  specTest(
    'has no RPC in the generated proto document',
    {
      feature: 'typescript/api-contracts',
      requirement: 'external-contract-operations',
      check: 'an-external-route-is-absent-from-the-protobuf-service',
    },
    () => {
      const document = generateProto([...registryApi().routes], { packageName: 'registry.v1' });
      const rpcs = Object.values(document.services).flat();
      expect(rpcs).toHaveLength(1);
      expect(rpcs.some((name) => name.includes('Manifests'))).toBe(false);
      expect(document.content).not.toContain('Manifests');
    },
  );

  specTest(
    'publishes the schemas its standard owns, and never as a shared component',
    {
      feature: 'typescript/api-contracts',
      requirement: 'external-contract-operations',
      check: 'an-external-route-publishes-the-schemas-its-standard-owns',
    },
    () => {
      // Two declarations the first-party subset refuses: a resolver descriptor,
      // and a JSON map keyed by anything but a string.
      const standardSchemas = {
        params: { name: String },
        query: { upstream: Env('REGISTRY_UPSTREAM', String) },
        returns: { layers: MapOf(Number, String) },
      };
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/v2/[name]/manifests/latest',
          schemas: standardSchemas,
          meta: { client: { external: OCI } },
        },
        {
          method: 'GET',
          path: '/v2/[name]/blobs/latest',
          schemas: standardSchemas,
          meta: { client: { external: OCI } },
        },
        {
          method: 'GET',
          path: '/v2/_putnami/capabilities',
          schemas: { returns: { copy: Boolean } },
          meta: { client: { security: anonymous, idempotency: { kind: 'safe' } } },
        },
      ];
      const document = generateOpenApiSpec(routes, {
        info: { title: 'Registry', version: '1' },
        client: clientService,
      });

      const external = document.paths['/v2/{name}/manifests/latest'].get;
      expect(external['x-putnami-external-contract']).toBe(OCI);
      expect(external.responses['200'].content?.['application/json']?.schema).toEqual({
        type: 'object',
        properties: { layers: { type: 'object', additionalProperties: { type: 'string' } } },
        required: ['layers'],
        additionalProperties: false,
      });
      // The standard's shape is shared by both external routes and still does
      // not become a component: a shared component is first-party, and every
      // reader validates it.
      expect(Object.keys(document.components?.schemas ?? {})).toEqual([]);
      expect(document.paths['/v2/_putnami/capabilities'].get['x-putnami-client']).toBeDefined();

      // Non-vacuity: the same declarations on a first-party route still fail.
      expect(() =>
        generateOpenApiSpec(
          [
            {
              method: 'GET',
              path: '/v2/_putnami/upstream',
              schemas: standardSchemas,
              meta: { client: { security: anonymous, idempotency: { kind: 'safe' } } },
            },
          ],
          { info: { title: 'Registry', version: '1' }, client: clientService },
        ),
      ).toThrow('api.client schema');
    },
  );

  specTest(
    'refuses every contradictory declaration before the route is bound',
    {
      feature: 'typescript/api-contracts',
      requirement: 'external-contract-operations',
      check: 'a-contradictory-external-declaration-is-refused-at-registration',
    },
    () => {
      const cases: { policy: ClientOperationPolicy; client?: typeof clientService | null; message: string }[] = [
        { policy: { external: '  ' }, message: 'external names a blank authority' },
        {
          policy: { external: OCI, idempotency: { kind: 'safe' } },
          message: 'is declared together with idempotency',
        },
        {
          policy: { external: OCI, security: anonymous, resume: true },
          message: 'is declared together with security, resume',
        },
        { policy: { external: OCI }, client: null, message: 'the API publishes no first-party client contract' },
      ];
      for (const { policy, message, ...rest } of cases) {
        const client = rest.client === undefined ? clientService : rest.client;
        expect(() => registryApi(policy, client), message).toThrow(message);
        expect(() => registryApi(policy, client), message).toThrow('GET /v2/[name]/manifests/[reference]');
      }
    },
  );

  specTest(
    'is refused at generation too, and a zero-valued option beside it is not a declaration',
    {
      feature: 'typescript/api-contracts',
      requirement: 'external-contract-operations',
      check: 'a-contradictory-external-declaration-is-refused-at-generation',
    },
    () => {
      const generate = (route: DiscoveredRoute, client: typeof clientService | null = clientService) =>
        generateOpenApiSpec([route], { info: { title: 'Registry', version: '1' }, ...(client ? { client } : {}) });

      expect(() => generate(externalRoute({ external: ' ' }))).toThrow('external names a blank authority');
      expect(() => generate(externalRoute({ external: OCI, resilience: { timeoutMs: 1 } }))).toThrow(
        'is declared together with resilience',
      );
      expect(() => generate(externalRoute({ external: OCI }), null)).toThrow(
        'the API publishes no first-party client contract',
      );

      // A zero value is not a declaration, exactly as the Go framework reads
      // one: these publish the marker instead of failing.
      for (const policy of [
        { external: OCI, resume: false },
        { external: OCI, security: { alternatives: [] } },
        { external: OCI, transports: [], connectEncodings: [] },
      ] satisfies ClientOperationPolicy[]) {
        const document = generate(externalRoute(policy));
        expect(document.paths['/v2/{name}/manifests/{reference}'].get['x-putnami-external-contract']).toBe(OCI);
      }
    },
  );

  describe('at runtime', () => {
    let testApp: TestApp | undefined;

    afterEach(async () => {
      await testApp?.stop();
      testApp = undefined;
    });

    specTest(
      'answers errors with the standard body while its first-party neighbor keeps the envelope',
      {
        feature: 'typescript/api-contracts',
        requirement: 'external-contract-operations',
        check: 'an-external-route-is-served-by-the-standard-pipeline',
      },
      async () => {
        const apiPlugin = api({ autoScan: false, client: clientService });
        const missing = () => {
          throw new NotFoundException('nope');
        };
        apiPlugin.register(
          '/v2/[name]/blobs/[digest]',
          endpoint().params({ name: String, digest: String }).client({ external: OCI }).handle(missing),
          'GET',
        );
        apiPlugin.register(
          '/v2/_putnami/blobs/[digest]',
          endpoint()
            .params({ digest: String })
            .client({ security: anonymous, idempotency: { kind: 'safe' } })
            .handle(missing),
          'GET',
        );
        testApp = await createTestApp({ plugins: [apiPlugin] });

        const external = await testApp.fetch('/v2/library/blobs/sha256:0');
        expect(external.status).toBe(404);
        expect(await external.json()).toEqual({ statusCode: 404, message: 'nope', error: 'Not Found' });

        const firstParty = await testApp.fetch('/v2/_putnami/blobs/sha256:0');
        expect(firstParty.status).toBe(404);
        expect(await firstParty.json()).toEqual({ code: 'not_found', error: 'Not Found', message: 'nope' });
      },
    );
  });
});
