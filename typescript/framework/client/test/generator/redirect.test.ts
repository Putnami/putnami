import { describe, expect } from 'bun:test';
import type { OpenApiDocument } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { readOpenApiSpecWithOptions } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';

const FEATURE = 'typescript/service-clients';
const REQUIREMENT = 'redirect-success';

type Operation = Record<string, unknown>;

const ARTIFACT = {
  type: 'object',
  properties: { id: { type: 'string' } },
  required: ['id'],
  additionalProperties: false,
};

/** A first-party read answered by a redirect, the shape a Go redirecting route publishes. */
function redirectOperation(responses: Record<string, unknown>, client: Operation = {}): Operation {
  return {
    operationId: 'getArtifact',
    parameters: [{ name: 'id', in: 'path', required: true, schema: { type: 'string' } }],
    responses,
    'x-putnami-client': {
      stream: 'unary',
      transports: [{ protocol: 'rest-json', path: '/artifacts/{id}', encoding: 'json' }],
      security: { alternatives: [{ allOf: [] }] },
      errors: [],
      idempotency: { kind: 'safe' },
      ...client,
    },
  };
}

function document(operation: Operation, method = 'get'): OpenApiDocument {
  return {
    openapi: '3.0.3',
    info: { title: 'Artifacts', version: '1.0.0' },
    'x-putnami-client': {
      protocolVersion: 1,
      service: { id: 'artifacts', audience: 'api://artifacts' },
      credentials: {},
    },
    paths: { '/artifacts/{id}': { [method]: operation } },
  } as unknown as OpenApiDocument;
}

function read(doc: OpenApiDocument) {
  return readOpenApiSpecWithOptions(doc, { mode: 'firstParty' });
}

function emit(doc: OpenApiDocument): { types: string; client: string } {
  const files = generateTypeScriptClient(read(doc), { packageName: '@test/artifacts-client' });
  return {
    types: files.find((file) => file.path === 'src/types.ts')?.content ?? '',
    client: files.find((file) => file.content.includes('class '))?.content ?? '',
  };
}

describe('redirect success', () => {
  specTest(
    'generates an operation whose only success is a redirect, with its Location in the result',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'a-redirect-only-operation-is-generated' },
    () => {
      const doc = document(redirectOperation({ '302': { description: "The artifact's location" } }));
      const method = read(doc).services[0]?.methods[0];
      expect(method?.successes?.map((success) => success.status)).toEqual([302]);

      const { types, client } = emit(doc);
      expect(types).toContain('| { status: 302; data: void; headers: Headers; location: string };');
      expect(client).toContain('Promise<GetArtifactResult>');
      expect(client).toContain('return this.requestResult<GetArtifactResult>(');
      expect(client).toContain('"status":302');
    },
  );

  specTest(
    'keeps a body beside a redirect as one status-discriminated result',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'a-redirect-only-operation-is-generated' },
    () => {
      const { types } = emit(
        document(
          redirectOperation({
            '200': { description: 'Held here', content: { 'application/json': { schema: ARTIFACT } } },
            '307': { description: 'Where the artifact lives' },
          }),
        ),
      );
      expect(types).toContain('| { status: 200; data: { id: string }; headers: Headers }');
      expect(types).toContain('| { status: 307; data: void; headers: Headers; location: string };');
    },
  );

  specTest(
    'leaves a redirect beside raw octets as documentation, as the Go client does',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'a-redirect-only-operation-is-generated' },
    () => {
      const method = read(
        document(
          redirectOperation({
            '200': {
              description: 'The blob',
              content: {
                'application/octet-stream': {
                  schema: { type: 'string', format: 'binary' },
                  'x-putnami-max-bytes': 1024,
                },
              },
            },
            '302': { description: 'Stored elsewhere' },
          }),
        ),
      ).services[0]?.methods[0];
      expect(method?.successes?.map((success) => success.status)).toEqual([200]);
    },
  );

  specTest(
    'still refuses an operation that declares neither a 2xx nor a redirect',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'an-unrepresentable-redirect-fails-generation' },
    () => {
      expect(() => read(document(redirectOperation({ '304': { description: 'Not modified' } })))).toThrow(
        /at least one concrete 2xx or redirect response/,
      );
    },
  );

  specTest(
    'refuses a redirect dispatched on Connect, which answers every success with HTTP 200',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'an-unrepresentable-redirect-fails-generation' },
    () => {
      // The reader needs a protobuf descriptor for Connect; the emitter's own
      // refusal is what this proves, so the transport is swapped on the IR.
      const ir = read(document(redirectOperation({ '302': { description: "The artifact's location" } })));
      const method = ir.services[0]?.methods[0];
      if (!method?.client) throw new Error('the reader left the operation out');
      method.client = {
        ...method.client,
        transports: [
          {
            protocol: 'connect',
            path: '/artifacts.v1.Artifacts/GetArtifact',
            encoding: 'json',
            protobufMethod: 'artifacts.v1.Artifacts.GetArtifact',
          },
        ],
      } as typeof method.client;
      expect(() => generateTypeScriptClient(ir, { packageName: '@test/artifacts-client' })).toThrow(
        /a declared redirect on a connect transport/,
      );
    },
  );
});
