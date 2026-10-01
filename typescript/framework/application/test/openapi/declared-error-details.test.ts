import { describe, expect } from 'bun:test';
import { ArrayOf, ConflictException, Int, OneOf, Stream } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { api, application, endpoint, http, projectStreamError, type DiscoveredRoute } from '@putnami/application';
import { isStreamEndpointDefinition } from '../../src/api/route/stream-endpoint';
import { generateOpenApiSpec, type OpenApiDocument, type OpenApiSchema } from '../../src/openapi/openapi';
import { openapi } from '../../src/openapi/openapi.plugin';

/**
 * `.mayThrowDetails(code, schema)` declares the schema of one error code's
 * `details` member (ADR 0006 of protocols/clientcontract). These tests read
 * what the provider publishes and what it answers, mirroring
 * go/framework/openapi/error_details_test.go.
 */

const FEATURE = 'typescript/api-contracts';
const REQUIREMENT = 'declared-error-details';

const deployRejected = {
  rejections: ArrayOf({ index: Int, project: String, error: String }),
  gcp_response_body: String,
  retryable: Boolean,
};

const sampleRejection = {
  rejections: [{ index: 0, project: 'a', error: 'image not found' }],
  gcp_response_body: '{"error":{"code":409}}',
  retryable: false,
};

const clientOptions = {
  info: { title: 'Deploys', version: '1.0.0' },
  client: { service: { id: 'deploys', audience: 'api://deploys' }, credentials: {} },
};

type PublishedError = { status: number; code: string; schema?: OpenApiSchema; retryable?: boolean };

function publishedErrors(spec: OpenApiDocument): Record<string, PublishedError> {
  const errors = (spec.paths['/deploys'].post['x-putnami-client']?.errors ?? []) as PublishedError[];
  return Object.fromEntries(errors.map((error) => [error.code, error]));
}

function documentedDetails(spec: OpenApiDocument, status: string): OpenApiSchema | undefined {
  return spec.paths['/deploys'].post.responses[status].content?.['application/json'].schema.properties?.['details'];
}

/**
 * The component a declared details schema names. A `.mayThrowDetails()` body is
 * always a component, the way Go publishes a details type, so a reference is the
 * only accepted form.
 */
function component(spec: OpenApiDocument, schema: OpenApiSchema | undefined): OpenApiSchema {
  const prefix = '#/components/schemas/';
  const ref = schema?.$ref ?? '';
  if (!ref.startsWith(prefix)) throw new Error(`details schema ${JSON.stringify(schema)} is not a component`);
  const resolved = spec.components?.schemas?.[ref.slice(prefix.length)];
  if (!resolved) throw new Error(`details schema ${ref} names no component`);
  return resolved;
}

function deployRoute(errorCodes: NonNullable<DiscoveredRoute['responses']>['errorCodes']): DiscoveredRoute {
  return {
    method: 'POST',
    path: '/deploys',
    schemas: { body: { projects: ArrayOf(String) }, returns: { id: String } },
    responses: {
      errorCodes,
      errorOptions: { Conflict: { retryable: false } },
      errorDetails: {
        Conflict: deployRejected,
        AlreadyExists: deployRejected,
        Validation: { field: String },
        InvalidArgument: { limit: Int },
      },
    },
    meta: { client: { security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'non-idempotent' } } },
  };
}

describe('declared error details', () => {
  specTest(
    'publishes the declared details schema for that code only, whatever the declaration order',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'the-contract-publishes-the-declared-details-schema-for-that-code-only',
    },
    () => {
      const forward = generateOpenApiSpec(
        [deployRoute(['Conflict', 'AlreadyExists', 'Validation', 'InvalidArgument', 'NotFound', 'Conflict'])],
        clientOptions,
      );
      const reverse = generateOpenApiSpec(
        [deployRoute(['NotFound', 'Conflict', 'InvalidArgument', 'Conflict', 'Validation', 'AlreadyExists'])],
        clientOptions,
      );
      expect(JSON.stringify(reverse)).toBe(JSON.stringify(forward));

      const errors = forward.paths['/deploys'].post['x-putnami-client']?.errors ?? [];
      // A code declared twice — details beside a retry classification — is one error.
      expect(errors.filter((error) => error.code === 'conflict')).toHaveLength(1);
      const byCode = publishedErrors(forward);
      expect(byCode['conflict']).toMatchObject({ status: 409, retryable: false });
      // ADR 0006: the schema is the details body, never the envelope carrying it.
      expect(Object.keys(component(forward, byCode['conflict'].schema).properties ?? {}).sort()).toEqual([
        'gcp_response_body',
        'rejections',
        'retryable',
      ]);
      expect(byCode['validation'].status).toBe(400);
      expect(component(forward, byCode['validation'].schema)).toMatchObject({
        properties: { field: { type: 'string' } },
      });
      expect(byCode['invalid_argument']).toMatchObject({ status: 400 });
      for (const bare of ['not_found', 'http.bad_request', 'http.internal_server']) {
        expect(byCode[bare]).toBeDefined();
        expect(byCode[bare].schema).toBeUndefined();
      }

      // The contract and the documented response name the same component.
      // Two codes answering 409 with one schema document it once.
      expect(byCode['already_exists'].schema).toEqual(byCode['conflict'].schema as OpenApiSchema);
      expect(documentedDetails(forward, '409')).toEqual(byCode['conflict'].schema as OpenApiSchema);
      // Two codes answering 400 with different schemas document `anyOf` them —
      // `oneOf` would refuse a body two overlapping variants both admit — in
      // stable-code order, invalid_argument before validation, whatever their
      // shapes. Go orders and combines its details types the same way.
      const at400 = documentedDetails(forward, '400');
      expect(at400?.oneOf).toBeUndefined();
      expect(at400?.anyOf).toEqual([
        byCode['invalid_argument'].schema as OpenApiSchema,
        byCode['validation'].schema as OpenApiSchema,
      ]);
      expect(at400?.anyOf?.map((variant) => Object.keys(component(forward, variant).properties ?? {}))).toEqual([
        ['limit'],
        ['field'],
      ]);
      expect(documentedDetails(forward, '404')).toBeUndefined();
    },
  );

  specTest(
    'publishes two details schemas that differ only in a constraint as two components',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'the-contract-publishes-the-declared-details-schema-for-that-code-only',
    },
    () => {
      // Same member names and types, different allowed values: one shared
      // component would tell a client to validate one code's details against
      // the other code's values.
      const spec = generateOpenApiSpec(
        [
          {
            ...deployRoute(['Validation', 'InvalidArgument']),
            responses: {
              errorCodes: ['Validation', 'InvalidArgument'],
              errorDetails: {
                Validation: { reason: OneOf('missing', 'malformed') },
                InvalidArgument: { reason: OneOf('too_large') },
              },
            },
          },
        ],
        clientOptions,
      );
      const byCode = publishedErrors(spec);
      expect(byCode['validation'].schema).not.toEqual(byCode['invalid_argument'].schema as OpenApiSchema);
      expect(component(spec, byCode['validation'].schema).properties?.['reason']?.enum).toEqual([
        'missing',
        'malformed',
      ]);
      expect(component(spec, byCode['invalid_argument'].schema).properties?.['reason']?.enum).toEqual(['too_large']);
      expect(documentedDetails(spec, '400')?.anyOf).toEqual([
        byCode['invalid_argument'].schema as OpenApiSchema,
        byCode['validation'].schema as OpenApiSchema,
      ]);
    },
  );

  specTest(
    'refuses a details schema for the implicit http.bad_request and http.internal_server codes',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'the-implicit-codes-refuse-a-declared-details-body',
    },
    () => {
      // Every endpoint answers these two codes and the framework writes their
      // response, so a declared schema would describe bodies the endpoint never
      // sends — and publish a second http.bad_request error beside the implicit one.
      for (const code of ['BadRequest', 'InternalServerError'] as const) {
        const builder = endpoint();
        expect(() => builder.mayThrowDetails(code, { field: String })).toThrow('framework-owned implicit error');
        const definition = builder.handle(() => ({ ok: true }));
        expect(definition.responses?.errorCodes ?? []).toEqual([]);
        expect(definition.responses?.errorDetails ?? {}).toEqual({});
      }
      // `Internal` answers 500 too, under its own stable code `internal`.
      expect(() => endpoint().mayThrowDetails('Internal', { field: String })).not.toThrow();
      expect(() => endpoint().mayThrowDetails('Validation', { field: String })).not.toThrow();
    },
  );

  specTest(
    "keeps a code's declared details over a .throws() schema for its status, in the contract and on a stream",
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-codes-declared-details-take-precedence-over-a-throws-schema-for-its-status',
    },
    () => {
      // Conflict and AlreadyExists both answer 409. Conflict declares its own
      // details; the .throws() schema describes every other code at 409.
      const spec = generateOpenApiSpec(
        [
          {
            ...deployRoute(['Conflict', 'AlreadyExists']),
            responses: {
              errorCodes: ['Conflict', 'AlreadyExists'],
              errorDetails: { Conflict: deployRejected },
              throws: [{ status: 409, description: 'Rejected', schema: { code: String, reason: String } }],
            },
          },
        ],
        clientOptions,
      );
      const byCode = publishedErrors(spec);
      // The .throws() entry replaces the documented 409 response, so the
      // declared details appear once, in the contract, and stay a component.
      expect(Object.keys(component(spec, byCode['conflict'].schema).properties ?? {}).sort()).toEqual([
        'gcp_response_body',
        'rejections',
        'retryable',
      ]);
      expect(Object.keys(byCode['already_exists'].schema?.properties ?? {}).sort()).toEqual(['code', 'reason']);

      // The stream terminal applies the same precedence at run time.
      const stream = endpoint()
        .returns(Stream({ id: String }))
        .mayThrowDetails('Conflict', deployRejected)
        .mayThrow('AlreadyExists')
        .throws(409, 'Rejected', { code: String, reason: String })
        .handle(async () => {});
      if (!isStreamEndpointDefinition(stream)) throw new Error('expected a stream definition');
      expect(projectStreamError(new ConflictException(sampleRejection), stream).details).toEqual(sampleRejection);
      // A TypeScript exception names its declared code in its response object.
      const alreadyExists = new ConflictException({ code: 'AlreadyExists', reason: 'taken' });
      expect(projectStreamError(alreadyExists, stream)).toMatchObject({
        code: 'already_exists',
        details: { code: 'AlreadyExists', reason: 'taken' },
      });
    },
  );

  specTest(
    'answers the declared details in the first-party envelope, unary and stream',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'the-provider-answers-the-declared-details-in-the-first-party-envelope',
    },
    async () => {
      const providerHttp = http({ port: 0 });
      const apiPlugin = api({
        autoScan: false,
        client: { service: { id: 'deploys', audience: 'api://deploys' }, credentials: {} },
      });
      apiPlugin.register(
        '/deploys',
        endpoint()
          .body({ projects: ArrayOf(String) })
          .returns({ id: String })
          .mayThrowDetails('Conflict', deployRejected)
          .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'non-idempotent' } })
          .handle(() => {
            throw new ConflictException(sampleRejection);
          }),
        'POST',
      );
      const openapiPlugin = openapi({ title: 'Deploys', version: '1.0.0' });
      const app = application().use(providerHttp).use(apiPlugin).use(openapiPlugin);
      await app.start();
      try {
        const server = providerHttp.getServer();
        if (!server) throw new Error('provider HTTP server did not start');
        const response = await fetch(`http://localhost:${server.port}/deploys`, {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ projects: ['a'] }),
        });
        expect(response.status).toBe(409);
        const body = (await response.json()) as Record<string, unknown>;
        expect(Object.keys(body).sort()).toEqual(['code', 'details', 'error', 'message']);
        expect(body).toMatchObject({ code: 'conflict', error: 'Conflict' });
        expect(body['details']).toEqual(sampleRejection);
        const published = openapiPlugin.spec()?.paths['/deploys'].post['x-putnami-client'].errors;
        expect(published.find((error: { code: string }) => error.code === 'conflict').schema).toBeDefined();
      } finally {
        await app.stop();
      }

      // A stream terminal validates the details against the schema declared for
      // the code, not against a `.throws()` entry for the status.
      const stream = endpoint()
        .returns(Stream({ id: String }))
        .mayThrowDetails('Conflict', deployRejected)
        .handle(async () => {});
      if (!isStreamEndpointDefinition(stream)) throw new Error('expected a stream definition');
      const terminal = projectStreamError(new ConflictException(sampleRejection), stream);
      expect(terminal).toMatchObject({ status: 409, code: 'conflict', error: 'Conflict' });
      expect(terminal.details).toEqual(sampleRejection);
    },
  );
});
