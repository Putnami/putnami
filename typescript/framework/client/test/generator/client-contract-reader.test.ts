import { describe, expect, it } from 'bun:test';
import { readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { Constrained, Default, MapOf, Max, Min, type OpenApiDocument, generateOpenApiSpec } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import {
  CLIENT_CONTRACT_DIAGNOSTIC_CODES,
  cacheResponseProperties,
  ClientGenerationError,
  ExactJsonNumber,
  readOpenApiSource,
  readOpenApiSpec,
  readOpenApiSpecWithOptions,
  serializeClientIR,
  serializeNeutralClientIR,
} from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';

const SHARED_FIXTURE_ROOT = join(
  import.meta.dir,
  '..',
  '..',
  '..',
  '..',
  '..',
  'protocols',
  'clientcontract',
  'fixtures',
  'openapi',
);

const contract = {
  protocolVersion: 1 as const,
  service: { id: 'documents', audience: 'api://documents' },
  credentials: {
    service: { kind: 'service-token' as const, scopes: ['documents:read'] },
    tenant: { kind: 'named-header' as const, header: 'X-Tenant-Token' },
  },
  protobuf: {
    syntax: 'proto3' as const,
    package: 'documents.v1',
    services: [
      {
        name: 'DocumentsService',
        methods: [
          {
            name: 'GetDocument',
            input: 'GetDocumentRequest',
            output: 'GetDocumentResponse',
            clientStreaming: false,
            serverStreaming: false,
          },
        ],
      },
    ],
    messages: [
      { name: 'GetDocumentRequest', fields: [] },
      { name: 'GetDocumentResponse', fields: [] },
    ],
    enums: [],
  },
};

function fullDocument(): OpenApiDocument {
  return {
    openapi: '3.0.3',
    info: { title: 'Documents', version: '1.0.0' },
    'x-putnami-client': structuredClone(contract),
    paths: {
      '/documents/{id}': {
        get: {
          operationId: 'getDocument',
          parameters: [
            { name: 'id', in: 'path', required: true, schema: { type: 'string', format: 'uuid' } },
            { name: 'X-Version', in: 'header', required: false, schema: { type: 'integer', format: 'int64' } },
          ],
          requestBody: {
            required: false,
            content: {
              'application/json': { schema: { type: 'array', items: { type: 'string', format: 'byte' } } },
            },
          },
          responses: {
            '200': {
              description: 'Current',
              content: {
                'application/json': {
                  schema: {
                    type: 'object',
                    properties: {
                      version: { type: 'integer', format: 'int64' },
                      bytes: { type: 'string', format: 'byte', nullable: true },
                    },
                    required: ['version', 'bytes'],
                  },
                },
              },
            },
            '206': {
              description: 'Partial',
              content: {
                // A raw octet representation carries its bound: `format: binary`
                // says the payload is octets, not how many a peer may send.
                'application/octet-stream': {
                  schema: { type: 'string', format: 'binary' },
                  'x-putnami-max-bytes': 65_536,
                },
              },
            },
            '404': { description: 'Missing' },
          },
          'x-putnami-client': {
            stream: 'unary',
            transports: [
              {
                protocol: 'connect',
                path: '/documents.v1.DocumentsService/GetDocument',
                encoding: 'proto',
                protobufMethod: '/documents.v1.DocumentsService/GetDocument',
              },
              { protocol: 'rest-json', path: '/documents/{id}', encoding: 'json' },
            ],
            security: {
              alternatives: [{ allOf: [{ profile: 'service', scopes: ['documents:read'] }, { profile: 'tenant' }] }],
            },
            errors: [{ status: 404, code: 'NotFound', grpcCode: 5, retryable: false }],
            idempotency: { kind: 'safe' },
            resilience: { timeoutMs: 5000, attemptTimeoutMs: 2000, maxResponseBytes: 2_097_152 },
          },
        },
      },
    },
  };
}

describe('first-party OpenAPI client contract reader', () => {
  it('consumes the shared Go-TypeScript full contract fixture', () => {
    const spec = readOpenApiSource(readFileSync(join(SHARED_FIXTURE_ROOT, 'valid', 'full.openapi.json'), 'utf8'), {
      mode: 'firstParty',
    });
    const goldenPath = join(SHARED_FIXTURE_ROOT, '..', 'ir', 'full.ir.json');
    if (process.env.PUTNAMI_UPDATE_GOLDEN === '1') {
      writeFileSync(goldenPath, `${serializeNeutralClientIR(spec)}\n`);
    }
    expect(`${serializeNeutralClientIR(spec)}\n`).toBe(readFileSync(goldenPath, 'utf8'));
    const getWidget = spec.services
      .flatMap((service) => service.methods)
      .find((method) => method.operationId === 'getWidget');

    expect(spec.contract?.protobuf?.messages.find((message) => message.name === 'Widget')?.fields).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ name: 'payload', number: 2, typeKind: 'scalar', type: 'bytes' }),
        expect.objectContaining({ name: 'labels', number: 4, typeKind: 'map' }),
        expect.objectContaining({ name: 'user_owner', oneof: 'owner' }),
      ]),
    );
    expect(spec.schemas?.WidgetEvent.properties?.sequence).toMatchObject({
      type: 'integer',
      format: 'uint64',
      maximum: new ExactJsonNumber('18446744073709551615'),
    });
    expect(getWidget?.client?.security.authorization?.rolesAny).toEqual(['widget-reader', 'widget-admin']);
    expect(getWidget?.client?.transports[0].protobufMethod).toBe('/fixtures.widgets.v1.WidgetsService/GetWidget');
  });

  it('retains native provider descriptor semantics through OpenAPI and strict IR', () => {
    const document = generateOpenApiSpec(
      [
        {
          method: 'POST',
          path: '/scores',
          schemas: {
            body: {
              scores: MapOf(String, Constrained(Min(0), Max(100))),
              pageSize: Default(Constrained(Min(1), Max(50)), 20),
            },
            returns: { accepted: Boolean },
          },
        },
      ],
      {
        info: { title: 'Scores', version: '1.0.0' },
        client: { service: { id: 'scores', audience: 'api://scores' }, credentials: {} },
      },
    );

    const method = readOpenApiSpecWithOptions(document, { mode: 'firstParty' }).services[0].methods[0];
    expect(method.request).toEqual({
      required: true,
      content: [
        {
          mediaType: 'application/json',
          schema: {
            type: 'object',
            properties: {
              scores: {
                type: 'object',
                additionalProperties: { type: 'number', minimum: 0, maximum: 100 },
              },
              pageSize: { type: 'number', minimum: 1, maximum: 50, default: 20 },
            },
            required: ['scores'],
            additionalProperties: false,
          },
        },
      ],
    });
  });

  it('reads every shared valid contract fixture the Go reader reads', () => {
    const expectations = JSON.parse(readFileSync(join(SHARED_FIXTURE_ROOT, 'expectations.json'), 'utf8')) as {
      valid: string[];
    };
    const fixtures = readdirSync(join(SHARED_FIXTURE_ROOT, 'valid')).filter((entry) => entry.endsWith('.openapi.json'));
    expect(fixtures.sort()).toEqual([...expectations.valid].sort());
    for (const name of fixtures) {
      const spec = readOpenApiSource(readFileSync(join(SHARED_FIXTURE_ROOT, 'valid', name), 'utf8'), {
        mode: 'firstParty',
      });
      expect(spec.contract, name).toBeDefined();
    }
  });

  it('keeps an optional credential as the credential then the anonymous alternative', () => {
    // A credentialed alternative followed by an empty allOf is the declared
    // optional credential (ADR 0002 of go/framework/security): the reader keeps
    // both, in order, so every emitter embeds what the provider declared.
    const spec = readOpenApiSource(
      readFileSync(join(SHARED_FIXTURE_ROOT, 'valid', 'optional-credential.openapi.json'), 'utf8'),
      { mode: 'firstParty' },
    );
    const method = spec.services
      .flatMap((service) => service.methods)
      .find((entry) => entry.operationId === 'resolvePackage');
    expect(method?.client?.security.alternatives).toEqual([
      { allOf: [{ profile: 'user', scopes: ['packages:read'] }] },
      { allOf: [] },
    ]);
  });

  it('rejects every shared invalid contract fixture with the diagnostic the Go reader names', () => {
    const expectations = JSON.parse(readFileSync(join(SHARED_FIXTURE_ROOT, 'expectations.json'), 'utf8')) as {
      valid: string[];
      invalid: Record<string, string>;
    };
    const fixtures = readdirSync(join(SHARED_FIXTURE_ROOT, 'invalid')).filter((entry) =>
      entry.endsWith('.openapi.json'),
    );
    // Non-vacuity: every invalid fixture on disk is covered by an expectation and
    // every expectation names a fixture, so adding a fixture cannot silently
    // widen the corpus.
    expect(fixtures.sort()).toEqual(Object.keys(expectations.invalid).sort());
    expect(fixtures.length).toBeGreaterThan(0);

    for (const name of fixtures) {
      const source = readFileSync(join(SHARED_FIXTURE_ROOT, 'invalid', name), 'utf8');
      let thrown: unknown;
      try {
        readOpenApiSource(source, { mode: 'firstParty' });
      } catch (error) {
        thrown = error;
      }
      expect(thrown, name).toBeInstanceOf(ClientGenerationError);
      expect((thrown as ClientGenerationError).contractCode, name).toBe(expectations.invalid[name]);
    }
  });

  it('refuses a reconnect no transport can resume, applying the document default only to server streams', () => {
    const fixture = (kind: 'valid' | 'invalid', name: string) =>
      JSON.parse(readFileSync(join(SHARED_FIXTURE_ROOT, kind, name), 'utf8')) as {
        'x-putnami-client': Record<string, unknown>;
        paths: Record<string, { get: { 'x-putnami-client': Record<string, unknown> } }>;
      };
    const refusal = (document: unknown): ClientGenerationError => {
      try {
        readOpenApiSource(JSON.stringify(document), { mode: 'firstParty' });
      } catch (error) {
        expect(error).toBeInstanceOf(ClientGenerationError);
        return error as ClientGenerationError;
      }
      throw new Error('expected the reader to refuse the document');
    };
    const expectUnresumable = (error: ClientGenerationError, operationId: string) => {
      expect(error).toMatchObject({
        code: 'clientgen_unsupported_semantic',
        contractCode: 'client_contract.invalid_resilience',
        operationId,
        field: 'x-putnami-client.resilience.stream.reconnect',
      });
      expect(error.message.startsWith('stream reconnect requires a websocket transport with resume support ')).toBe(
        true,
      );
    };

    // An SSE-only server stream that asks for reconnect.
    const sseOnly = fixture('invalid', 'reconnect-without-resumable-transport.openapi.json');
    expectUnresumable(refusal(sseOnly), 'invalid.stream');

    // The same stream relying on the document default is refused the same way.
    const inherited = fixture('invalid', 'reconnect-without-resumable-transport.openapi.json');
    inherited.paths['/stream'].get['x-putnami-client'].resilience = undefined;
    inherited['x-putnami-client'].defaults = { resilience: { stream: { reconnect: true } } };
    expectUnresumable(refusal(inherited), 'invalid.stream');

    // The document default leaves a unary operation valid.
    const spec = readOpenApiSource(
      readFileSync(join(SHARED_FIXTURE_ROOT, 'valid', 'reconnect-default-with-unary-operation.openapi.json'), 'utf8'),
      { mode: 'firstParty' },
    );
    expect(spec.services.flatMap((service) => service.methods.map((method) => method.operationId)).sort()).toEqual([
      'readClock',
      'watchClock',
    ]);

    // An operation's own reconnect always counts, on a unary operation too.
    const unary = fixture('valid', 'reconnect-default-with-unary-operation.openapi.json');
    unary.paths['/clock'].get['x-putnami-client'].resilience = { stream: { reconnect: true } };
    expectUnresumable(refusal(unary), 'readClock');
  });

  it('reads an sse continuation in either mode, and refuses one the Go reader refuses with the same code and field', () => {
    // clientcontract ADR 0013: the shared corpus names each refusal's code; this
    // pins the IR the reader keeps and the fields it names, like the Go tests.
    const source = readFileSync(join(SHARED_FIXTURE_ROOT, 'valid', 'sse-continuation.openapi.json'), 'utf8');
    const methods = new Map(
      readOpenApiSource(source, { mode: 'firstParty' })
        .services.flatMap((service) => service.methods)
        .map((method) => [method.operationId, method]),
    );
    expect(methods.get('tailRunLogs')?.client?.transports.map((transport) => transport.sse)).toEqual([
      { continuation: { mode: 'cursor', cursor: { outputField: 'cursor', queryParameter: 'cursor' } } },
      undefined,
    ]);
    expect(methods.get('followRunLogs')?.client?.transports[0]?.sse).toEqual({
      continuation: { mode: 'cursor', cursor: { outputField: 'position', queryParameter: 'after' } },
    });
    expect(methods.get('tailLogs')?.client?.transports[0]?.sse).toEqual({ continuation: { mode: 'best-effort' } });

    type Operation = {
      transports: Record<string, unknown>[];
      idempotency: { kind: string };
      resilience?: unknown;
    };
    const variant = (mutate: (document: { paths: Record<string, { get: Record<string, unknown> }> }) => void) => {
      const document = JSON.parse(source);
      mutate(document);
      try {
        readOpenApiSource(JSON.stringify(document), { mode: 'firstParty' });
      } catch (error) {
        expect(error).toBeInstanceOf(ClientGenerationError);
        return error as ClientGenerationError;
      }
      throw new Error('expected the reader to refuse the document');
    };
    const operation = (document: { paths: Record<string, { get: Record<string, unknown> }> }, path: string) =>
      document.paths[path].get['x-putnami-client'] as Operation;

    // A best-effort transport without its continuation no longer carries the reconnect.
    expect(
      variant((document) => {
        operation(document, '/logs/tail').transports[0] = { protocol: 'sse', path: '/logs/tail', encoding: 'json' };
      }),
    ).toMatchObject({
      contractCode: 'client_contract.invalid_resilience',
      operationId: 'tailLogs',
      field: 'x-putnami-client.resilience.stream.reconnect',
    });
    // A continuation belongs to a safe server stream.
    expect(
      variant((document) => {
        operation(document, '/logs/tail').idempotency.kind = 'idempotent';
      }),
    ).toMatchObject({
      contractCode: 'client_contract.invalid_resilience',
      field: 'x-putnami-client.transports.0.sse.continuation',
    });
    // sse metadata on another transport is refused where it stands.
    expect(
      variant((document) => {
        operation(document, '/runs/{run}/logs/tail').transports[1].sse = { continuation: { mode: 'best-effort' } };
      }),
    ).toMatchObject({ contractCode: 'client_contract.invalid_transport', field: 'x-putnami-client.transports.1.sse' });
    // A blank mode is missing, as in Go; an unknown one is outside the closed set.
    expect(
      variant((document) => {
        operation(document, '/logs/tail').transports[0].sse = { continuation: { mode: ' ' } };
      }),
    ).toMatchObject({
      contractCode: 'client_contract.required',
      field: 'x-putnami-client.transports.0.sse.continuation.mode',
    });
    expect(
      variant((document) => {
        operation(document, '/logs/tail').transports[0].sse = { continuation: { mode: 5 } };
      }),
    ).toMatchObject({ contractCode: 'client_contract.parse_error' });
    // References are judged after one local component reference, like Go.
    expect(
      variant((document) => {
        const position = (document as unknown as { components: { schemas: Record<string, Record<string, unknown>> } })
          .components.schemas.Position;
        position.format = 'uuid';
      }),
    ).toMatchObject({
      contractCode: 'client_contract.invalid_resilience',
      operationId: 'tailRunLogs',
      field: 'x-putnami-client.transports.0.sse.continuation.cursor.outputField',
    });
    expect(
      variant((document) => {
        const follow = document.paths['/runs/{run}/logs/follow'].get as { parameters: { schema: unknown }[] };
        follow.parameters[1].schema = { type: 'string', nullable: true };
      }),
    ).toMatchObject({
      contractCode: 'client_contract.invalid_resilience',
      operationId: 'followRunLogs',
      field: 'x-putnami-client.transports.0.sse.continuation.cursor.queryParameter',
    });
  });

  specTest(
    'reads every shared valid fixture and skips an operation an external authority owns',
    {
      feature: 'typescript/service-clients',
      requirement: 'external-contract-operations',
      check: 'the-typescript-reader-skips-an-external-operation-whole',
    },
    () => {
      const expectations = JSON.parse(readFileSync(join(SHARED_FIXTURE_ROOT, 'expectations.json'), 'utf8')) as {
        valid: string[];
      };
      const fixtures = readdirSync(join(SHARED_FIXTURE_ROOT, 'valid')).filter((entry) =>
        entry.endsWith('.openapi.json'),
      );
      expect(fixtures.sort()).toEqual([...expectations.valid].sort());
      expect(fixtures).toContain('external-operation.openapi.json');
      for (const name of fixtures) {
        const spec = readOpenApiSource(readFileSync(join(SHARED_FIXTURE_ROOT, 'valid', name), 'utf8'), {
          mode: 'firstParty',
        });
        expect(spec.contract, name).toBeDefined();
      }

      // The external operation carries a schema the first-party subset refuses,
      // so reading it would throw: the fixture reads only because it is skipped.
      const source = readFileSync(join(SHARED_FIXTURE_ROOT, 'valid', 'external-operation.openapi.json'), 'utf8');
      const spec = readOpenApiSource(source, { mode: 'firstParty' });
      expect(spec.services.flatMap((service) => service.methods.map((method) => method.operationId))).toEqual([
        'getV2PutnamiCapabilities',
      ]);

      // The marker never excuses an unmarked operation.
      const unmarked = source.replace(
        '"x-putnami-external-contract": "OCI Distribution Specification v1.1"',
        '"deprecated": false',
      );
      let thrown: unknown;
      try {
        readOpenApiSource(unmarked, { mode: 'firstParty' });
      } catch (error) {
        thrown = error;
      }
      expect(thrown).toBeInstanceOf(ClientGenerationError);
      expect((thrown as ClientGenerationError).contractCode).toBe('client_contract.required');
    },
  );

  it('names every contract diagnostic with the closed Go vocabulary', () => {
    // The reader must not invent a code the contract package does not publish.
    expect(new Set(CLIENT_CONTRACT_DIAGNOSTIC_CODES).size).toBe(CLIENT_CONTRACT_DIAGNOSTIC_CODES.length);
    for (const code of CLIENT_CONTRACT_DIAGNOSTIC_CODES) expect(code.startsWith('client_contract.')).toBe(true);
    const expectations = JSON.parse(readFileSync(join(SHARED_FIXTURE_ROOT, 'expectations.json'), 'utf8')) as {
      invalid: Record<string, string>;
    };
    for (const code of Object.values(expectations.invalid)) expect(CLIENT_CONTRACT_DIAGNOSTIC_CODES).toContain(code);
  });

  it('rejects duplicate source keys and serializes wide integers as exact JSON numeric tokens', () => {
    const source = readFileSync(join(SHARED_FIXTURE_ROOT, 'valid', 'full.openapi.json'), 'utf8');
    const duplicate = source.replace('"protocolVersion": 1,', '"protocolVersion": 1, "protocolVersion": 1,');
    expect(() => readOpenApiSource(duplicate, { mode: 'firstParty' })).toThrow(/duplicate JSON object key/);

    const serialized = serializeClientIR(readOpenApiSource(source, { mode: 'firstParty' }));
    expect(serialized).toContain('"maximum":18446744073709551615');
    expect(serialized).not.toContain('"maximum":"18446744073709551615"');
  });

  it('rejects an already-parsed unsafe integer because its source precision cannot be recovered', () => {
    const document = JSON.parse(
      readFileSync(join(SHARED_FIXTURE_ROOT, 'valid', 'full.openapi.json'), 'utf8'),
    ) as OpenApiDocument;
    expect(() => readOpenApiSpecWithOptions(document, { mode: 'firstParty' })).toThrow(/unsafe integer lost precision/);
  });

  it.each([
    [
      'operation header parameter',
      (document: OpenApiDocument) => {
        document.paths['/documents/{id}'].get.parameters?.push({
          name: 'X-Client-Id',
          in: 'header',
          schema: { type: 'string' },
        });
      },
    ],
    [
      'credential header',
      (document: OpenApiDocument) => {
        (document['x-putnami-client']?.credentials as Record<string, unknown>)['identity'] = {
          kind: 'named-header',
          header: 'X-Client-Id',
        };
      },
    ],
    [
      'idempotency header',
      (document: OpenApiDocument) => {
        const operationContract = document.paths['/documents/{id}'].get['x-putnami-client'];
        if (!operationContract) throw new Error('missing client operation contract');
        operationContract.idempotency = {
          kind: 'idempotent',
          keyHeader: 'X-Client-Id',
        };
      },
    ],
  ])('rejects a framework-owned client identity used as %s', (_name, mutate) => {
    const document = fullDocument();
    mutate(document);
    expect(() => readOpenApiSpecWithOptions(document, { mode: 'firstParty' })).toThrow(/framework-owned|reserved/);
  });

  it('preserves parameters, root bodies, every success variant, schemas, and operation policy', () => {
    const spec = readOpenApiSpecWithOptions(fullDocument(), { mode: 'firstParty' });
    const method = spec.services[0].methods[0];

    expect(spec.irVersion).toBe(1);
    expect(spec.contract).toEqual(contract);
    expect(method.parameters).toEqual([
      {
        name: 'id',
        location: 'path',
        required: true,
        schema: { type: 'string', format: 'uuid' },
      },
      {
        name: 'X-Version',
        location: 'header',
        required: false,
        schema: { type: 'integer', format: 'int64' },
      },
    ]);
    expect(method.request).toEqual({
      required: false,
      content: [
        { mediaType: 'application/json', schema: { type: 'array', items: { type: 'string', format: 'byte' } } },
      ],
    });
    expect(method.successes).toEqual([
      {
        status: 200,
        description: 'Current',
        content: [
          {
            mediaType: 'application/json',
            schema: {
              type: 'object',
              properties: {
                version: { type: 'integer', format: 'int64' },
                bytes: { type: 'string', format: 'byte', nullable: true },
              },
              required: ['version', 'bytes'],
            },
          },
        ],
      },
      {
        status: 206,
        description: 'Partial',
        content: [
          { mediaType: 'application/octet-stream', schema: { type: 'string', format: 'binary' }, maxBytes: 65_536 },
        ],
      },
    ]);
    expect(method.client).toEqual(fullDocument().paths['/documents/{id}'].get['x-putnami-client']);
  });

  it('requires the first-party marker when the provider flow requests strict generation', () => {
    const document = fullDocument();
    document['x-putnami-client'] = undefined;
    expect(() => readOpenApiSpecWithOptions(document, { mode: 'firstParty' })).toThrow(
      expect.objectContaining({ code: 'clientgen_first_party_required' }),
    );
  });

  it('automatically reads marked documents strictly and names the operation and field on failure', () => {
    const document = fullDocument() as OpenApiDocument & Record<string, unknown>;
    const operation = document.paths['/documents/{id}'].get as unknown as Record<string, unknown>;
    operation['x-putnami-client'] = {
      ...(operation['x-putnami-client'] as object),
      unexpectedFallback: true,
    };

    try {
      readOpenApiSpec(document);
      throw new Error('expected strict reader to fail');
    } catch (error) {
      expect(error).toBeInstanceOf(ClientGenerationError);
      expect(error).toMatchObject({ code: 'clientgen_unsupported_semantic' });
      expect((error as Error).message).toContain('getDocument');
      expect((error as Error).message).toContain('x-putnami-client.unexpectedFallback');
    }
  });

  it('merges path parameters with operation overrides using OpenAPI defaults', () => {
    const document = fullDocument();
    const pathItem = document.paths['/documents/{id}'] as unknown as Record<string, unknown>;
    pathItem.parameters = [
      { name: 'X-Version', in: 'header', schema: { type: 'integer', format: 'int32' } },
      { name: 'trace', in: 'query', schema: { type: 'string' } },
    ];

    expect(readOpenApiSpec(document).services[0].methods[0].parameters).toEqual([
      { name: 'id', location: 'path', required: true, schema: { type: 'string', format: 'uuid' } },
      { name: 'trace', location: 'query', required: false, schema: { type: 'string' } },
      { name: 'X-Version', location: 'header', required: false, schema: { type: 'integer', format: 'int64' } },
    ]);
  });

  it('rejects path references and operation callbacks instead of skipping their semantics', () => {
    const referenced = fullDocument();
    (referenced.paths['/documents/{id}'] as unknown as Record<string, unknown>).$ref =
      '#/components/pathItems/Document';
    expect(() => readOpenApiSpec(referenced)).toThrow(/paths\.\/documents\/\{id\}.*\$ref/);

    const callback = fullDocument();
    (callback.paths['/documents/{id}'].get as unknown as Record<string, unknown>).callbacks = {};
    expect(() => readOpenApiSpec(callback)).toThrow(/getDocument.*callbacks/);
  });

  it('rejects duplicate operation identity and generated method name collisions', () => {
    const duplicate = fullDocument();
    duplicate.paths['/documents'] = {
      post: {
        ...duplicate.paths['/documents/{id}'].get,
        operationId: 'getDocument',
      },
    };
    expect(() => readOpenApiSpec(duplicate)).toThrow(/duplicate operationId/);

    const collision = fullDocument();
    collision.paths['/documents/{id}'].get.operationId = 'get-document';
    collision.paths['/documents'] = {
      post: {
        ...collision.paths['/documents/{id}'].get,
        operationId: 'get_document',
      },
    };
    expect(() => readOpenApiSpec(collision)).toThrow(/collide after TypeScript identifier normalization/);
  });

  it('rejects unknown protobuf references while allowing the closed well-known message set', () => {
    const unknown = fullDocument();
    const unknownMethod = unknown['x-putnami-client']?.protobuf?.services[0].methods[0] as
      | { input: string }
      | undefined;
    if (!unknownMethod) throw new Error('missing protobuf method');
    unknownMethod.input = 'MissingRequest';
    expect(() => readOpenApiSpec(unknown)).toThrow(/input message is not declared/);

    const wellKnown = fullDocument();
    const wellKnownMethod = wellKnown['x-putnami-client']?.protobuf?.services[0].methods[0] as
      | { input: string }
      | undefined;
    if (!wellKnownMethod) throw new Error('missing protobuf method');
    wellKnownMethod.input = 'google.protobuf.Empty';
    expect(() => readOpenApiSpec(wellKnown)).not.toThrow();
  });

  it('rejects schema keywords that cannot be represented instead of degrading them', () => {
    const document = fullDocument();
    const schema = document.paths['/documents/{id}'].get.responses['200'].content?.['application/json']
      .schema as unknown as Record<string, unknown>;
    schema['allOf'] = [{ type: 'string' }];

    expect(() => readOpenApiSpec(document)).toThrow(/getDocument.*responses\.200.*allOf/);
  });

  it('judges invalidation field schemas by the shared vectors and reads the responses the Go reader reads', () => {
    const vectors = JSON.parse(readFileSync(join(SHARED_FIXTURE_ROOT, '..', 'cache', 'invalidation.json'), 'utf8')) as {
      propertySchemas: { name: string; schema: Record<string, unknown>; comparable: boolean }[];
    };
    expect(vectors.propertySchemas.length).toBeGreaterThan(0);
    const success = (mediaType: string, schema: Record<string, unknown>) => ({
      status: 200,
      description: 'ok',
      content: [{ mediaType, schema: schema as never }],
    });
    for (const vector of vectors.propertySchemas) {
      const inline = cacheResponseProperties(
        [success('application/json', { type: 'object', properties: { field: vector.schema } })],
        {},
      );
      const referenced = cacheResponseProperties(
        [
          success('application/json', {
            type: 'object',
            properties: { field: { $ref: '#/components/schemas/Field' } },
          }),
        ],
        { Field: vector.schema },
      );
      expect(inline.get('field'), vector.name).toBe(vector.comparable);
      expect(referenced.get('field'), vector.name).toBe(vector.comparable);
    }

    // Every JSON body counts, the media type compared ignoring case; a media
    // type with parameters is another media type, as it is for the Go reader.
    const properties = cacheResponseProperties(
      [
        success('Application/JSON', { type: 'object', properties: { principalId: { type: 'string' } } }),
        success('application/json; charset=utf-8', { type: 'object', properties: { tenant: { type: 'string' } } }),
        success('application/json', {
          type: 'object',
          properties: { principalId: { type: 'integer', format: 'int64' } },
        }),
      ],
      {},
    );
    expect(properties.get('principalId')).toBe(true);
    expect(properties.has('tenant')).toBe(false);
  });
});

describe('a declared sse continuation in the emitted TypeScript client', () => {
  it('pins the sse-continuation runtime capability the way the Go emitter does', () => {
    const source = readFileSync(join(SHARED_FIXTURE_ROOT, 'valid', 'sse-continuation.openapi.json'), 'utf8');
    const files = generateTypeScriptClient(readOpenApiSource(source, { mode: 'firstParty' }), {
      packageName: '@test/sse-continuation-client',
    });
    const clients = files.filter((file) => file.path.endsWith('-client.ts')).map((file) => file.content);
    expect(clients.length).toBeGreaterThan(0);
    for (const client of clients) {
      expect(client).toContain("import { requireClientRuntimeCapabilities } from '@putnami/client';");
      expect(client).toContain("requireClientRuntimeCapabilities(['sse-continuation']);");
    }
  });
});
