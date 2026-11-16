import { describe, expect } from 'bun:test';
import type { OpenApiDocument } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { readOpenApiSpecWithOptions } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';

const FEATURE = 'typescript/service-clients';
const REQUIREMENT = 'conditional-read';

type Operation = Record<string, unknown>;

/** A first-party summary read: 200 with a body, and the 304 a matching If-None-Match answers. */
function summaryOperation(overrides: Operation = {}): Operation {
  return {
    operationId: 'getSummary',
    responses: {
      '200': {
        description: 'Current',
        content: {
          'application/json': {
            schema: {
              type: 'object',
              properties: { count: { type: 'integer', format: 'int32' } },
              required: ['count'],
              additionalProperties: false,
            },
          },
        },
      },
      '304': { description: 'If-None-Match names the current ETag: no body.' },
    },
    'x-putnami-client': {
      stream: 'unary',
      transports: [{ protocol: 'rest-json', path: '/summary', encoding: 'json' }],
      security: { alternatives: [{ allOf: [] }] },
      errors: [],
      idempotency: { kind: 'safe' },
    },
    ...overrides,
  };
}

function document(operations: Record<string, Operation>, path = '/summary'): OpenApiDocument {
  return {
    openapi: '3.0.3',
    info: { title: 'Missions', version: '1.0.0' },
    'x-putnami-client': {
      protocolVersion: 1,
      service: { id: 'missions', audience: 'api://missions' },
      credentials: {},
    },
    paths: { [path]: operations },
  } as unknown as OpenApiDocument;
}

function read(doc: OpenApiDocument) {
  return readOpenApiSpecWithOptions(doc, { mode: 'firstParty' });
}

function emit(doc: OpenApiDocument): { types: string; client: string } {
  const files = generateTypeScriptClient(read(doc), { packageName: '@test/missions-client' });
  return {
    types: files.find((file) => file.path === 'src/types.ts')?.content ?? '',
    client: files.find((file) => file.content.includes('class '))?.content ?? '',
  };
}

function withClient(operation: Operation, client: Operation): Operation {
  return { ...operation, 'x-putnami-client': { ...(operation['x-putnami-client'] as Operation), ...client } };
}

describe('conditional read', () => {
  specTest(
    'emits the If-None-Match option and a status-discriminated result for an operation that declares a 304',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'a-declared-304-is-a-typed-result-of-a-conditional-read' },
    () => {
      const ir = read(document({ get: summaryOperation() }));
      const method = ir.services[0]?.methods[0];
      expect(method?.notModified).toEqual({ description: 'If-None-Match names the current ETag: no body.' });
      // The 304 stays out of the 2xx variants the Go reader shares.
      expect(method?.successes?.map((success) => success.status)).toEqual([200]);

      const { types, client } = emit(document({ get: summaryOperation() }));
      expect(types).toContain('export interface ClientConditionalCallOptions extends ClientCallOptions {');
      expect(types).toContain('ifNoneMatch?: string;');
      expect(types).toContain('| { status: 200; data: { count: number }; headers: Headers }');
      expect(types).toContain('| { status: 304; data: void; headers: Headers };');
      expect(client).toContain('async getSummary(options?: ClientConditionalCallOptions): Promise<GetSummaryResult>');
      expect(client).toContain("headers: { 'If-None-Match': options?.ifNoneMatch },");
      expect(client).toContain('return this.requestResult<GetSummaryResult>(');
      expect(client).toContain('"status":304');
    },
  );

  specTest(
    'sends declared header parameters beside If-None-Match',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'a-declared-304-is-a-typed-result-of-a-conditional-read' },
    () => {
      const { client } = emit(
        document({
          get: summaryOperation({
            parameters: [{ name: 'X-Region', in: 'header', required: false, schema: { type: 'string' } }],
          }),
        }),
      );
      expect(client).toContain("'If-None-Match': options?.ifNoneMatch");
      expect(client).toContain('"X-Region": encodeHttpParameter(input.headers?.["X-Region"]');
    },
  );

  specTest(
    'leaves an operation that declares no 304 unchanged',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'an-operation-without-a-304-generates-as-before' },
    () => {
      const plain = summaryOperation();
      (plain['responses'] as Record<string, unknown>)['304'] = undefined;
      const { types, client } = emit(document({ get: plain }));
      expect(types).not.toContain('ifNoneMatch');
      expect(types).not.toContain('ClientConditionalCallOptions');
      expect(types).not.toContain('GetSummaryResult');
      expect(client).toContain('async getSummary(options?: ClientCallOptions): Promise<{ count: number }>');
      expect(client).not.toContain('If-None-Match');
    },
  );

  specTest(
    'refuses a 304 the generated client could not honour',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'an-unrepresentable-304-fails-generation' },
    () => {
      const withBody = summaryOperation();
      (withBody['responses'] as Record<string, unknown>)['304'] = {
        description: 'Not modified',
        content: { 'application/json': { schema: { type: 'string' } } },
      };
      expect(() => read(document({ get: withBody }))).toThrow(/a 304 response carries no body operation getSummary/);

      expect(() =>
        read(document({ post: withClient(summaryOperation(), { idempotency: { kind: 'idempotent' } }) })),
      ).toThrow(/only a GET operation can declare a 304/);
      expect(() => read(document({ head: summaryOperation() }))).toThrow(/only a GET operation can declare a 304/);

      expect(() =>
        read(
          document({
            get: withClient(summaryOperation(), {
              stream: 'server',
              messages: { output: { type: 'string' } },
              transports: [{ protocol: 'sse', path: '/summary', encoding: 'json' }],
            }),
          }),
        ),
      ).toThrow(/only a unary operation can declare a 304/);

      expect(() =>
        read(document({ get: withClient(summaryOperation(), { resilience: { cache: { freshMs: 5000 } } }) })),
      ).toThrow(/a conditional read cannot declare a response cache/);

      const octets = summaryOperation();
      (octets['responses'] as Record<string, unknown>)['200'] = {
        description: 'Current',
        content: {
          'application/octet-stream': {
            schema: { type: 'string', format: 'binary' },
            'x-putnami-max-bytes': 1024,
          },
        },
      };
      expect(() => read(document({ get: octets }))).toThrow(/a raw octet operation cannot declare a 304/);

      const declaredHeader = summaryOperation({
        parameters: [{ name: 'If-None-Match', in: 'header', required: false, schema: { type: 'string' } }],
      });
      expect(() => emit(document({ get: declaredHeader }))).toThrow(/an If-None-Match header parameter beside/);

      // A 304 has no Connect equivalent: the emitter refuses one beside a Connect transport.
      const overConnect = read(document({ get: summaryOperation() }));
      const conditional = overConnect.services[0]?.methods[0];
      if (!conditional?.client) throw new Error('the reader produced no operation');
      conditional.client = {
        ...conditional.client,
        transports: [
          { protocol: 'connect', path: '/missions.v1.MissionsService/GetSummary', encoding: 'json' } as never,
          ...conditional.client.transports,
        ],
      };
      expect(() => generateTypeScriptClient(overConnect, { packageName: '@test/missions-client' })).toThrow(
        /a declared 304 on a transport other than rest-json/,
      );
    },
  );
});
