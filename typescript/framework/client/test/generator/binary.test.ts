import { specTest } from '@putnami/runtime/spectest';
import { describe, expect } from 'bun:test';
import { readOpenApiSpec } from '../../src/generator/openapi-reader';
import { generateStrictTypeScriptClient } from '../../src/generator/ts/strict-ts-generator';

// Reading and emitting raw octets: the reader accepts `format: binary` only
// where the HTTP body itself is the payload, and the emitter renders a call
// that never encodes JSON or base64.

const BINARY_DOCUMENT = {
  openapi: '3.0.3',
  info: { title: 'Blobs', version: '1.0.0' },
  'x-putnami-client': {
    protocolVersion: 1,
    service: { id: 'blobs', audience: 'urn:blobs' },
    credentials: {},
  },
  paths: {
    '/blobs/{id}': {
      put: {
        operationId: 'putBlob',
        parameters: [{ name: 'id', in: 'path', required: true, schema: { type: 'string' } }],
        'x-putnami-client': {
          stream: 'unary',
          transports: [{ protocol: 'rest-json', path: '/blobs/{id}', encoding: 'json' }],
          security: { alternatives: [{ allOf: [] }] },
          errors: [],
          idempotency: { kind: 'idempotent' },
        },
        requestBody: {
          required: true,
          content: {
            'application/octet-stream': { schema: { type: 'string', format: 'binary' }, 'x-putnami-max-bytes': 4096 },
          },
        },
        responses: {
          '200': {
            description: 'ok',
            content: {
              'image/png': { schema: { type: 'string', format: 'binary' }, 'x-putnami-max-bytes': 8192 },
            },
          },
        },
      },
    },
  },
  // biome-ignore lint/suspicious/noExplicitAny: the fixture is an OpenAPI document, mutated one fact at a time
} as any;

function mutate(edit: (document: Record<string, unknown>) => void): Record<string, unknown> {
  const copy = JSON.parse(JSON.stringify(BINARY_DOCUMENT));
  edit(copy);
  return copy;
}

describe('raw octet generation', () => {
  specTest(
    'reads the declared media type and byte bound of a raw octet representation',
    {
      feature: 'typescript/service-clients',
      requirement: 'binary-payloads',
      check: 'the-reader-preserves-the-declared-octet-media-type-and-bound',
    },
    () => {
      const spec = readOpenApiSpec(BINARY_DOCUMENT);
      const method = spec.services[0].methods[0];
      expect(method.request?.content).toEqual([
        { mediaType: 'application/octet-stream', schema: { type: 'string', format: 'binary' }, maxBytes: 4096 },
      ]);
      expect(method.successes?.[0].content).toEqual([
        { mediaType: 'image/png', schema: { type: 'string', format: 'binary' }, maxBytes: 8192 },
      ]);
    },
  );

  specTest(
    'refuses raw octets everywhere a JSON document would have to carry them',
    {
      feature: 'typescript/service-clients',
      requirement: 'binary-payloads',
      check: 'raw-octets-are-refused-inside-a-json-document',
    },
    () => {
      // Inside a JSON document there is no octet literal; base64 bytes stay `format: byte`.
      expect(() =>
        readOpenApiSpec(
          mutate((document) => {
            // biome-ignore lint/suspicious/noExplicitAny: navigating the fixture
            const put = (document as any).paths['/blobs/{id}'].put;
            put.requestBody.content = {
              'application/json': {
                schema: {
                  type: 'object',
                  properties: { blob: { type: 'string', format: 'binary' } },
                  required: ['blob'],
                  additionalProperties: false,
                },
              },
            };
            // biome-ignore lint/suspicious/noExplicitAny: navigating the fixture
          }) as any,
        ),
      ).toThrow(/raw octets are not representable inside a JSON document/);

      // A JSON media type cannot carry octets either.
      expect(() =>
        readOpenApiSpec(
          mutate((document) => {
            // biome-ignore lint/suspicious/noExplicitAny: navigating the fixture
            const put = (document as any).paths['/blobs/{id}'].put;
            put.requestBody.content = {
              'application/json': { schema: { type: 'string', format: 'binary' }, 'x-putnami-max-bytes': 8 },
            };
            // biome-ignore lint/suspicious/noExplicitAny: navigating the fixture
          }) as any,
        ),
      ).toThrow(/raw octets are not representable under a JSON media type/);

      // An unbounded octet payload is refused rather than defaulted.
      expect(() =>
        readOpenApiSpec(
          mutate((document) => {
            // biome-ignore lint/suspicious/noExplicitAny: navigating the fixture
            const put = (document as any).paths['/blobs/{id}'].put;
            put.requestBody.content['application/octet-stream']['x-putnami-max-bytes'] = undefined;
            // biome-ignore lint/suspicious/noExplicitAny: navigating the fixture
          }) as any,
        ),
      ).toThrow(/require a positive x-putnami-max-bytes bound/);
    },
  );

  specTest(
    'emits a bounded reader in and a typed payload out, with no JSON or base64 hop',
    {
      feature: 'typescript/service-clients',
      requirement: 'binary-payloads',
      check: 'the-emitted-client-carries-octets-without-json-or-base64',
    },
    () => {
      const files = generateStrictTypeScriptClient(readOpenApiSpec(BINARY_DOCUMENT), {
        packageName: '@example/blobs-client',
        outputDir: 'clients/ts',
      });
      const source = files.map((file) => file.content).join('\n');
      expect(source).toContain("export type PutBlobBody = import('@putnami/client').BinarySource;");
      expect(source).toContain('readonly body: Uint8Array;');
      expect(source).toContain('await readBoundedBody(input.body, 4096)');
      expect(source).toContain('requestMediaType: "application/octet-stream"');
      expect(source).toContain('responseMediaType: "image/png"');
      expect(source).toContain('maxPayloadBytes: 8192');
      expect(source).toContain('Promise<PutBlobPayload>');
      expect(source).not.toContain('base64');
      expect(source).not.toContain('requestSchema');
    },
  );

  specTest(
    'refuses a raw octet payload on a transport that cannot carry it unchanged',
    {
      feature: 'typescript/service-clients',
      requirement: 'binary-payloads',
      check: 'raw-octets-are-refused-on-a-transport-that-cannot-carry-them',
    },
    () => {
      // The reader refuses a Connect transport with no published descriptor
      // first, so the emitter guard is reached by rewriting the dispatch order
      // on a spec the reader already accepted. That is the exact state a
      // foreign contract could hand the emitter.
      const spec = readOpenApiSpec(BINARY_DOCUMENT);
      const client = spec.services[0].methods[0].client;
      if (!client) throw new Error('the fixture lost its operation contract');
      spec.services[0].methods[0] = {
        ...spec.services[0].methods[0],
        client: { ...client, transports: [{ protocol: 'connect', path: '/blobs.v1.Blobs/Put', encoding: 'json' }] },
      };
      expect(() =>
        generateStrictTypeScriptClient(spec, { packageName: '@example/blobs-client', outputDir: 'clients/ts' }),
      ).toThrow(/only rest-json carries octets unchanged/);
    },
  );
});
