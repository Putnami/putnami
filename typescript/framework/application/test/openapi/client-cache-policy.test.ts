import { describe, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { api, application, endpoint, http, type ClientCachePolicy } from '@putnami/application';
import { type OpenApiDocument, validateCacheInvalidationFields } from '../../src/openapi/openapi';
import { openapi } from '../../src/openapi/openapi.plugin';

const INVALIDATION_VECTORS = join(
  import.meta.dir,
  '../../../../../protocols/clientcontract/fixtures/cache/invalidation.json',
);

/** A published document whose one cached operation declares `field` as its invalidation field. */
function cachedDocument(
  responses: Record<string, { content?: Record<string, { schema: unknown }> }>,
  components: Record<string, unknown> = {},
): OpenApiDocument {
  return {
    openapi: '3.0.3',
    info: { title: 'Identity', version: '1.0.0' },
    paths: {
      '/access/{subject}': {
        get: {
          responses: Object.fromEntries(
            Object.entries(responses).map(([status, response]) => [status, { description: status, ...response }]),
          ),
          'x-putnami-client': {
            stream: 'unary',
            transports: [{ protocol: 'rest-json', path: '/access/{subject}', encoding: 'json' }],
            security: { alternatives: [{ allOf: [] }] },
            errors: [],
            idempotency: { kind: 'safe' },
            resilience: { cache: { freshMs: 5000, invalidationFields: ['field'] } },
          },
        },
      },
    },
    components: { schemas: components },
  } as unknown as OpenApiDocument;
}

function jsonBody(properties: Record<string, unknown>, mediaType = 'application/json') {
  return { content: { [mediaType]: { schema: { type: 'object', properties } } } };
}

/**
 * A provider declares a response cache beside the endpoint (ADR 0007 of
 * protocols/clientcontract). These tests build real providers, publish their
 * OpenAPI document through the real plugin, and read what the contract says,
 * mirroring go/framework/openapi/client_cache_policy_test.go.
 */

const FEATURE = 'typescript/api-contracts';
const REQUIREMENT = 'declared-response-cache';

function provider(
  path: string,
  method: 'GET' | 'POST',
  definition: ReturnType<typeof endpoint>,
  defaults?: { resilience: { cache: ClientCachePolicy } },
) {
  const apiPlugin = api({
    autoScan: false,
    client: {
      service: { id: 'identity', audience: 'urn:identity' },
      credentials: {},
      ...(defaults ? { defaults } : {}),
    },
  });
  apiPlugin.register(
    path,
    definition.handle(() => ({ id: 'a1' })),
    method,
  );
  const openapiPlugin = openapi({ title: 'Identity', version: '1.0.0' });
  const app = application()
    .use(http({ port: 0 }))
    .use(apiPlugin)
    .use(openapiPlugin);
  return { app, openapiPlugin };
}

describe('declared response cache', () => {
  specTest(
    'publishes the declared cache policy in the operation contract',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-declared-cache-policy-is-published-in-the-operation-contract',
    },
    async () => {
      const cache: ClientCachePolicy = { freshMs: 5000, staleMs: 300_000, maxEntries: 64, keyFields: ['path.id'] };
      const { app, openapiPlugin } = provider(
        '/accounts/[id]',
        'GET',
        endpoint()
          .params({ id: String })
          .returns({ id: String })
          .client({
            security: { alternatives: [{ allOf: [] }] },
            idempotency: { kind: 'safe' },
            resilience: { cache },
          }),
      );
      await app.start();
      try {
        const operation = openapiPlugin.spec()?.paths['/accounts/{id}'].get['x-putnami-client'];
        expect(operation.resilience.cache).toEqual(cache);
      } finally {
        await app.stop();
      }
    },
  );

  specTest(
    'refuses a cache that would replay an effect or key on nothing',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-cache-on-a-non-idempotent-operation-or-an-undeclared-key-field-is-refused-at-publication',
    },
    async () => {
      const refusals: [ReturnType<typeof provider>, string][] = [
        [
          provider(
            '/orders',
            'POST',
            endpoint()
              .body({ item: String })
              .client({
                security: { alternatives: [{ allOf: [] }] },
                idempotency: { kind: 'non-idempotent' },
                resilience: { cache: { freshMs: 5000 } },
              }),
          ),
          'resilience.cache requires a safe or idempotent operation',
        ],
        [
          provider(
            '/accounts/[id]',
            'GET',
            endpoint()
              .params({ id: String })
              .client({
                security: { alternatives: [{ allOf: [] }] },
                idempotency: { kind: 'safe' },
                resilience: { cache: { freshMs: 5000, keyFields: ['path.accountId'] } },
              }),
          ),
          "keyFields entry 'path.accountId' names no request input",
        ],
        [
          provider(
            '/accounts/[id]',
            'GET',
            endpoint()
              .params({ id: String })
              .client({
                security: { alternatives: [{ allOf: [] }] },
                idempotency: { kind: 'safe' },
                resilience: { cache: { freshMs: 5000, staleMs: 5000 } },
              }),
          ),
          'staleMs must exceed freshMs',
        ],
        [
          provider(
            '/accounts',
            'GET',
            endpoint().client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'safe' } }),
            { resilience: { cache: { freshMs: 5000 } } },
          ),
          'a response cache is declared per operation',
        ],
      ];
      for (const [{ app }, message] of refusals) {
        await expect(app.start()).rejects.toThrow(message);
      }
    },
  );

  specTest(
    'publishes invalidation fields that name a string, integer or boolean property of the answer',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-declared-cache-policy-is-published-in-the-operation-contract',
    },
    async () => {
      const cache: ClientCachePolicy = {
        freshMs: 5000,
        keyFields: ['path.id'],
        invalidationFields: ['principalId', 'active'],
      };
      const { app, openapiPlugin } = provider(
        '/accounts/[id]',
        'GET',
        endpoint()
          .params({ id: String })
          .returns({ id: String, principalId: String, active: Boolean })
          .client({
            security: { alternatives: [{ allOf: [] }] },
            idempotency: { kind: 'safe' },
            resilience: { cache },
          }),
      );
      await app.start();
      try {
        const operation = openapiPlugin.spec()?.paths['/accounts/{id}'].get['x-putnami-client'];
        expect(operation.resilience.cache).toEqual(cache);
      } finally {
        await app.stop();
      }
    },
  );

  specTest(
    'refuses an invalidation field that would tag no answer',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'an-invalidation-field-naming-no-scalar-response-property-is-refused-at-publication',
    },
    async () => {
      const declaring = (invalidationFields: readonly string[]) =>
        provider(
          '/accounts/[id]',
          'GET',
          endpoint()
            .params({ id: String })
            .returns({ id: String, principalId: String, score: Number })
            .client({
              security: { alternatives: [{ allOf: [] }] },
              idempotency: { kind: 'safe' },
              resilience: { cache: { freshMs: 5000, invalidationFields } },
            }),
        );
      const refusals: [ReturnType<typeof provider>, string][] = [
        [declaring(['userId']), "invalidationFields entry 'userId' names no top-level property"],
        [declaring(['score']), "invalidationFields entry 'score' must name a string, integer or boolean property"],
        [declaring([]), 'invalidationFields must name at least one field'],
        [declaring(['principalId', 'principalId']), 'invalidationFields must not contain duplicates'],
        [declaring([' principalId']), 'no leading or trailing space'],
      ];
      for (const [{ app }, message] of refusals) {
        await expect(app.start()).rejects.toThrow(message);
      }
    },
  );

  specTest(
    'judges an invalidation field by the shared vectors, octets included, over the responses the Go reader reads',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'an-invalidation-field-naming-no-scalar-response-property-is-refused-at-publication',
    },
    () => {
      const vectors = JSON.parse(readFileSync(INVALIDATION_VECTORS, 'utf8')) as {
        propertySchemas: { name: string; schema: Record<string, unknown>; comparable: boolean }[];
      };
      expect(vectors.propertySchemas.length).toBeGreaterThan(0);
      for (const vector of vectors.propertySchemas) {
        const inline = () =>
          validateCacheInvalidationFields(cachedDocument({ '200': jsonBody({ field: vector.schema }) }));
        const referenced = () =>
          validateCacheInvalidationFields(
            cachedDocument(
              { '200': jsonBody({ field: { $ref: '#/components/schemas/Field' } }) },
              { Field: vector.schema },
            ),
          );
        for (const check of [inline, referenced]) {
          if (vector.comparable) expect(check, vector.name).not.toThrow();
          else expect(check, vector.name).toThrow('must name a string, integer or boolean property');
        }
      }

      const text = { type: 'string' };
      // Every three-digit 2xx status counts, and every media type equal to
      // application/json ignoring case.
      expect(() =>
        validateCacheInvalidationFields(cachedDocument({ '200': jsonBody({ field: text }, 'Application/JSON') })),
      ).not.toThrow();
      expect(() =>
        validateCacheInvalidationFields(
          cachedDocument({ '200': jsonBody({ other: text }), '203': jsonBody({ field: text }, 'APPLICATION/JSON') }),
        ),
      ).not.toThrow();
      // A property every declaring body must compare: a second success that
      // declares it as a number refuses it, whatever the first one says.
      expect(() =>
        validateCacheInvalidationFields(
          cachedDocument({ '200': jsonBody({ field: text }), '201': jsonBody({ field: { type: 'number' } }) }),
        ),
      ).toThrow('must name a string, integer or boolean property');
      // An error status, a status range and a media type with parameters are
      // no success body the Go reader reads.
      for (const responses of [
        { '404': jsonBody({ field: text }) },
        { '2XX': jsonBody({ field: text }) },
        { '200': jsonBody({ field: text }, 'application/json; charset=utf-8') },
      ]) {
        expect(() => validateCacheInvalidationFields(cachedDocument(responses))).toThrow(
          'names no top-level property of a JSON success body',
        );
      }
    },
  );
});
