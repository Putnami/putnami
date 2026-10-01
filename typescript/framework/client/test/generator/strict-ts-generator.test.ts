import { describe, expect, test } from 'bun:test';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import type { ClientContractOperation } from '@putnami/application';
import type { MethodIR, SpecIR } from '../../src/generator/ir.type';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';

const WORKSPACE_ROOT = resolve(import.meta.dir, '..', '..', '..', '..', '..');
const CLIENT_ROOT = resolve(import.meta.dir, '..', '..');

function operation(overrides: Partial<ClientContractOperation> = {}): ClientContractOperation {
  return {
    stream: 'unary',
    transports: [{ protocol: 'rest-json', path: '/items', encoding: 'json' }],
    security: { alternatives: [{ allOf: [] }] },
    errors: [],
    idempotency: { kind: 'safe' },
    ...overrides,
  } as ClientContractOperation;
}

function method(overrides: Partial<MethodIR> = {}): MethodIR {
  return {
    name: 'listItems',
    operationId: 'listItems',
    httpMethod: 'GET',
    path: '/items',
    successes: [
      { status: 200, description: 'ok', content: [{ mediaType: 'application/json', schema: { type: 'string' } }] },
    ],
    client: operation(),
    ...overrides,
  } as MethodIR;
}

function spec(methods: MethodIR[]): SpecIR {
  return {
    irVersion: 1,
    transport: 'http',
    contract: {
      protocolVersion: 1,
      service: { id: 'catalog.items', audience: 'api://items' },
      credentials: {},
    },
    services: [{ name: 'ItemsService', className: 'ItemsClient', methods }],
  } as SpecIR;
}

function emit(methods: MethodIR[]) {
  return generateTypeScriptClient(spec(methods), { packageName: '@test/items-client' });
}

/** The provider descriptor a contract that declares Connect publishes. */
const CONNECT_DESCRIPTOR = {
  syntax: 'proto3' as const,
  package: 'catalog.items.v1',
  services: [
    {
      name: 'ItemsService',
      methods: [
        {
          name: 'ListItems',
          input: 'ListItemsRequest',
          output: 'ListItemsResponse',
          clientStreaming: false,
          serverStreaming: false,
        },
      ],
    },
  ],
  messages: [
    { name: 'ListItemsRequest', fields: [] },
    {
      name: 'ListItemsResponse',
      fields: [{ name: 'value', jsonName: 'value', number: 1, typeKind: 'scalar' as const, type: 'string' }],
    },
  ],
  enums: [],
};

function connectMethod(overrides: Partial<MethodIR> = {}): MethodIR {
  return method({
    client: operation({
      transports: [
        {
          protocol: 'connect',
          path: '/catalog.items.v1.ItemsService/ListItems',
          encoding: 'proto',
          protobufMethod: '/catalog.items.v1.ItemsService/ListItems',
        },
        { protocol: 'rest-json', path: '/items', encoding: 'json' },
      ],
    }),
    ...overrides,
  });
}

function emitConnect(methods: MethodIR[]) {
  const base = spec(methods);
  return generateTypeScriptClient(
    { ...base, contract: { ...base.contract!, protobuf: CONNECT_DESCRIPTOR } } as SpecIR,
    { packageName: '@test/items-client' },
  );
}

describe('strict emitter refusals', () => {
  test('endpoint selection cannot be shadowed by a provider method', () => {
    for (const name of ['forEndpoint', 'failedEndpointStream', 'endpointBinding', 'endpointClients', 'baseUrl']) {
      expect(() => emit([method({ name, operationId: name })])).toThrow(
        'method name conflicts with endpoint selection',
      );
    }
  });

  test('endpoint call options delegate through the registered binding', () => {
    const files = emit([method()]);
    expect(files.find((file) => file.path.endsWith('types.ts'))?.content).toContain('endpoint?: string;');
    expect(files.find((file) => file.content.includes('class ItemsClient'))?.content).toContain(
      'return this.forEndpoint(options.endpoint).listItems({ ...options, endpoint: undefined });',
    );

    const streamed = emit([
      method({
        name: 'watchItems',
        operationId: 'watchItems',
        client: operation({
          stream: 'server',
          messages: { output: { type: 'string' } },
          transports: [{ protocol: 'sse', path: '/items', encoding: 'json' }],
        }),
        successes: [{ status: 200, description: 'ok', content: [] }],
      }),
    ]).find((file) => file.content.includes('class ItemsClient'))?.content;
    expect(streamed).toContain('try {');
    expect(streamed).toContain('return this.failedEndpointStream<never, WatchItemsMessage>(error);');
  });

  test('the success body call option reaches every method shape so the runtime decides', () => {
    const unary = emit([method()]);
    expect(unary.find((file) => file.path.endsWith('types.ts'))?.content).toContain(
      "successBody?: import('@putnami/client').SuccessBody;",
    );
    expect(unary.find((file) => file.content.includes('class ItemsClient'))?.content).toContain(
      'successBody: options?.successBody,',
    );
    const streamed = emit([
      method({
        name: 'watchItems',
        operationId: 'watchItems',
        client: operation({
          stream: 'server',
          messages: { output: { type: 'string' } },
          transports: [{ protocol: 'sse', path: '/items', encoding: 'json' }],
        }),
        successes: [{ status: 200, description: 'ok', content: [] }],
      }),
    ]).find((file) => file.content.includes('class ItemsClient'))?.content;
    // A stream never delivers a body; the option travels so the runtime refuses
    // it before a socket opens instead of ignoring it.
    expect(streamed).toContain('successBody: options?.successBody,');
  });

  specTest(
    'refuses a request body on a method the framework declares safe',
    {
      feature: 'typescript/service-clients',
      requirement: 'strict-generation',
      check: 'a-get-or-head-request-body-is-refused-instead-of-dropped',
    },
    () => {
      for (const httpMethod of ['GET', 'HEAD']) {
        expect(
          () =>
            emit([
              method({
                httpMethod,
                request: { required: true, content: [{ mediaType: 'application/json', schema: { type: 'string' } }] },
              }),
            ]),
          httpMethod,
        ).toThrow(
          new RegExp(`clientgen_unsupported_semantic: operation listItems uses a request body on ${httpMethod}`),
        );
      }
      // The same body on POST is emitted, so the refusal is about the method, not
      // about bodies in general.
      expect(
        emit([
          method({
            httpMethod: 'POST',
            client: operation({ idempotency: { kind: 'idempotent' } }),
            request: { required: true, content: [{ mediaType: 'application/json', schema: { type: 'string' } }] },
          }),
        ]).length,
      ).toBeGreaterThan(0);
    },
  );

  specTest(
    'refuses a websocket transport whose declared encoding is proto',
    {
      feature: 'typescript/service-clients',
      requirement: 'strict-generation',
      check: 'a-websocket-proto-encoding-is-refused-at-generation-not-at-runtime',
    },
    () => {
      // D0.8: the wire contract keeps `encoding: "proto"`; until a codec exists the
      // emitter refuses it by name. A supported sibling transport must not mask it.
      const streamed = method({
        client: operation({
          stream: 'server',
          messages: { output: { type: 'string' } },
          transports: [
            { protocol: 'sse', path: '/items', encoding: 'json' },
            {
              protocol: 'websocket',
              path: '/items',
              encoding: 'proto',
              protobufMethod: '/catalog.items.v1.Items/Watch',
              websocket: { subprotocol: 'putnami.service.v1', resume: false },
            },
          ],
        }),
        successes: [{ status: 200, description: 'ok', content: [] }],
      });
      expect(() => emit([streamed])).toThrow(
        /clientgen_unsupported_semantic: operation listItems uses transports\[1\] websocket encoding "proto"/,
      );
    },
  );

  specTest(
    'refuses a contract reachable only over Connect or WebSocket without emitting a file',
    {
      feature: 'typescript/service-clients',
      requirement: 'strict-generation',
      check: 'a-connect-only-or-websocket-only-contract-emits-no-partial-output',
    },
    () => {
      // A Connect operation the runtime cannot carry: the binary codec needs the
      // provider's descriptor, and this contract publishes none.
      const connectOnly = method({
        client: operation({
          transports: [
            {
              protocol: 'connect',
              path: '/catalog.items.v1.Items/List',
              encoding: 'proto',
              protobufMethod: '/catalog.items.v1.Items/List',
            },
          ],
        }),
      });
      // A WebSocket-only operation is emitted since N9; what stays refused is a
      // WebSocket this runtime cannot open. Resume is provider-declared and no
      // first-party client asks for it, so a method that assumed it would
      // compile against a handshake that is never sent.
      const websocketOnly = method({
        client: operation({
          stream: 'bidirectional',
          messages: { input: { type: 'string' }, output: { type: 'string' } },
          transports: [
            {
              protocol: 'websocket',
              path: '/items',
              encoding: 'json',
              websocket: { subprotocol: 'putnami.service.v1', resume: true },
            },
          ],
        }),
        successes: [{ status: 200, description: 'ok', content: [] }],
      });
      for (const [name, unsupportedMethod] of [
        ['connect', connectOnly],
        ['websocket', websocketOnly],
      ] as const) {
        let files: unknown;
        expect(() => {
          files = emit([unsupportedMethod]);
        }, name).toThrow(/clientgen_unsupported_semantic/);
        // No partial output: the generator returns nothing at all, so a caller can
        // never write half a client.
        expect(files, name).toBeUndefined();
      }
      // A supported operation next to the unsupported one does not rescue it.
      expect(() => emit([method(), connectOnly])).toThrow(/clientgen_unsupported_semantic/);
    },
  );

  specTest(
    'emits a client for a Connect-only contract that publishes its descriptor',
    {
      feature: 'typescript/service-clients',
      requirement: 'connect-transport',
      check: 'a-connect-only-contract-emits-a-client-carrying-the-provider-descriptor',
    },
    () => {
      const files = emitConnect([connectMethod()]);
      const client = files.find((file) => file.path === 'src/items-client.ts')?.content ?? '';
      // The generated method names no transport: the contract's declared order
      // decides, and the descriptor travels so the binary codec needs no
      // consumer configuration.
      expect(client).toContain('clientProtobuf: SERVICE_DESCRIPTOR.contract.protobuf');
      expect(client).toContain('this.request(\'GET\', "/items"');
      expect(client).not.toContain('ConnectTransport');
    },
  );

  specTest(
    'refuses a Connect operation whose json_name would rename a declared property',
    {
      feature: 'typescript/service-clients',
      requirement: 'connect-transport',
      check: 'a-property-name-that-protobuf-json-name-would-rename-is-refused',
    },
    () => {
      // `user_id` becomes `userId` in protobuf's JSON mapping, so the property
      // would arrive under a different key than it left under.
      expect(() =>
        emitConnect([
          connectMethod({
            successes: [
              {
                status: 200,
                description: 'ok',
                content: [
                  {
                    mediaType: 'application/json',
                    schema: { type: 'object', properties: { user_id: { type: 'string' } }, required: ['user_id'] },
                  },
                ],
              },
            ],
          }),
        ]),
      ).toThrow(/clientgen_unsupported_semantic: operation listItems uses property "user_id"/);
    },
  );

  specTest(
    'refuses a Connect operation whose declared field may be null',
    {
      feature: 'typescript/service-clients',
      requirement: 'connect-transport',
      check: 'a-nullable-field-is-refused-because-protobuf-cannot-represent-null',
    },
    () => {
      // Protobuf writes nothing for an absent field and reads the zero back, so
      // an explicit `null` and a `0` would arrive identical.
      expect(() =>
        emitConnect([
          connectMethod({
            successes: [
              {
                status: 200,
                description: 'ok',
                content: [
                  {
                    mediaType: 'application/json',
                    schema: {
                      type: 'object',
                      properties: { note: { type: 'string', nullable: true } },
                      required: ['note'],
                    },
                  },
                ],
              },
            ],
          }),
        ]),
      ).toThrow(/clientgen_unsupported_semantic: operation listItems uses a nullable field/);

      // The same nullable field on a REST-only contract is emitted: the refusal
      // is about what protobuf can carry, not about nullability in general.
      expect(() =>
        emit([
          method({
            successes: [
              {
                status: 200,
                description: 'ok',
                content: [
                  {
                    mediaType: 'application/json',
                    schema: {
                      type: 'object',
                      properties: { note: { type: 'string', nullable: true } },
                      required: ['note'],
                    },
                  },
                ],
              },
            ],
          }),
        ]),
      ).not.toThrow();
    },
  );

  test('does not refuse a REST-first operation for a Connect reason, like the Go emitter', () => {
    // A Go provider with a mounted bridge publishes Connect behind REST on every
    // unary route. The declared order is the dispatch order, so the call never
    // travels over Connect and the Connect narrowings do not apply to it.
    const nullableNote = {
      status: 200,
      description: 'ok',
      content: [
        {
          mediaType: 'application/json',
          schema: {
            type: 'object',
            properties: { note: { type: 'string', nullable: true } },
            required: ['note'],
          },
        },
      ],
    };
    const restFirst = connectMethod({ successes: [nullableNote] });
    restFirst.client = operation({
      transports: [...(restFirst.client?.transports ?? [])].reverse(),
    });
    expect(restFirst.client?.transports[0]?.protocol).toBe('rest-json');
    expect(() => emitConnect([restFirst])).not.toThrow();
    // Declared Connect-first, the same operation is still refused.
    expect(() => emitConnect([connectMethod({ successes: [nullableNote] })])).toThrow(
      /clientgen_unsupported_semantic: operation listItems uses a nullable field/,
    );
  });

  specTest(
    'refuses a Connect operation declaring more than one success status',
    {
      feature: 'typescript/service-clients',
      requirement: 'connect-transport',
      check: 'a-connect-operation-with-several-declared-successes-is-refused',
    },
    () => {
      // Connect answers every successful RPC with HTTP 200; several declared
      // statuses have no representation on that wire.
      expect(() =>
        emitConnect([
          connectMethod({
            successes: [
              {
                status: 200,
                description: 'ok',
                content: [{ mediaType: 'application/json', schema: { type: 'string' } }],
              },
              { status: 204, description: 'no content', content: [] },
            ],
          }),
        ]),
      ).toThrow(/clientgen_unsupported_semantic: operation listItems uses a connect transport with more than one/);
    },
  );

  specTest(
    'keeps every declared success variant instead of collapsing to the first 2xx',
    {
      feature: 'typescript/service-clients',
      requirement: 'strict-generation',
      check: 'multiple-success-variants-are-emitted-as-a-status-discriminated-result',
    },
    () => {
      const files = emit([
        method({
          httpMethod: 'POST',
          client: operation({ idempotency: { kind: 'idempotent' } }),
          successes: [
            {
              status: 200,
              description: 'ok',
              content: [{ mediaType: 'application/json', schema: { type: 'string' } }],
            },
            {
              status: 202,
              description: 'accepted',
              content: [{ mediaType: 'application/json', schema: { type: 'boolean' } }],
            },
            { status: 204, description: 'no content', content: [] },
          ],
        }),
      ]);
      const types = files.find((file) => file.path === 'src/types.ts')?.content ?? '';
      expect(types).toContain('export type ListItemsResult =');
      expect(types).toContain('| { status: 200; data: string; headers: Headers }');
      expect(types).toContain('| { status: 202; data: boolean; headers: Headers }');
      expect(types).toContain('| { status: 204; data: void; headers: Headers };');
      const client = files.find((file) => file.path === 'src/items-client.ts')?.content ?? '';
      expect(client).toContain('Promise<ListItemsResult>');
      // The 202 and 204 variants are not reachable through a single-shape return.
      expect(client).not.toContain('Promise<string>');
    },
  );
});

describe('operation type names', () => {
  // The canonical id of "GET /.well-known/putnami/events" is
  // "get.well-known_Putnami_Events". The dot survived into every per-operation
  // type name ("export type Get.wellKnownPutnamiEventsResult"), so Biome
  // refused the whole generated client.
  test('derives a legal type name from an operation id carrying a dotted path segment', () => {
    const files = emit([
      method({
        name: 'get_well_known_Putnami_Events',
        operationId: 'get.well-known_Putnami_Events',
        path: '/.well-known/putnami/events',
        parameters: [{ name: 'since', location: 'query', required: true, schema: { type: 'string' } }],
        client: operation({
          transports: [{ protocol: 'rest-json', path: '/.well-known/putnami/events', encoding: 'json' }],
          errors: [{ code: 'not_found', status: 404, retryable: false }],
        }),
      }),
    ]);
    const types = files.find((file) => file.path === 'src/types.ts')?.content ?? '';
    const client = files.find((file) => file.path === 'src/items-client.ts')?.content ?? '';

    expect(types).toContain('export interface GetWellKnownPutnamiEventsQuery');
    expect(types).toContain('export interface GetWellKnownPutnamiEventsInput');
    expect(types).toContain('export type GetWellKnownPutnamiEventsNotFoundError');
    expect(client).toContain('input: GetWellKnownPutnamiEventsInput');
    for (const content of [types, client]) expect(content).not.toContain('Get.well');
    // The canonical id stays the wire and trace identity, verbatim.
    expect(client).toContain('operationId: "get.well-known_Putnami_Events"');
  });
});

describe('optional parameter access', () => {
  test('renders an optional parameter with the optional-chaining member form', () => {
    const files = emit([
      method({
        parameters: [
          { name: 'limit', location: 'query', required: true, schema: { type: 'integer', format: 'int32' } },
          { name: 'prefix', location: 'query', required: false, schema: { type: 'string' } },
        ],
      }),
    ]);
    const client = files.find((file) => file.path === 'src/items-client.ts')?.content ?? '';
    expect(client).toContain('input.query["limit"]');
    expect(client).toContain('input.query?.["prefix"]');
    expect(client).not.toContain('input.query?["prefix"]');
  });
});

describe('response cache bypass option', () => {
  specTest(
    'offers the bypass only where a cache is declared, so an uncached client keeps its bytes',
    {
      feature: 'typescript/service-clients',
      requirement: 'response-cache-bypass-and-field-invalidation',
      check: 'a-bypassed-call-neither-reads-nor-stores-nor-joins-a-call-in-flight',
    },
    () => {
      const uncached = emit([method()]);
      for (const file of uncached) expect(file.content).not.toContain('withoutResponseCache');

      const cached = emit([
        method({ client: operation({ resilience: { cache: { freshMs: 5000 } } }) }),
        method({ name: 'getItem', operationId: 'getItem', path: '/items/{id}' }),
      ]);
      const types = cached.find((file) => file.path === 'src/types.ts')?.content ?? '';
      const client = cached.find((file) => file.path === 'src/items-client.ts')?.content ?? '';
      expect(types).toContain('withoutResponseCache?: boolean;');
      // Only the cached method forwards it.
      expect(client.split('withoutResponseCache: options?.withoutResponseCache,').length).toBe(2);
    },
  );
});

describe('emitted sources compile', () => {
  specTest(
    'typechecks the emitted client against the real @putnami/client surface',
    {
      feature: 'typescript/service-clients',
      requirement: 'strict-generation',
      check: 'the-emitted-sources-typecheck-against-the-real-client-surface',
    },
    () => {
      const files = emit([
        method({
          // An optional query parameter is the shape the emitter used to render
          // as `input.query?["prefix"]`, which is not TypeScript. tsc is the
          // proof: the emitted file has to parse.
          parameters: [
            { name: 'limit', location: 'query', required: true, schema: { type: 'integer', format: 'int32' } },
            { name: 'prefix', location: 'query', required: false, schema: { type: 'string' } },
            { name: 'x-region', location: 'header', required: false, schema: { type: 'string' } },
          ],
          // A declared cache: the forwarded bypass has to type-check against
          // the real request options.
          client: operation({ resilience: { cache: { freshMs: 5000 } } }),
        }),
        method({
          name: 'createItem',
          operationId: 'createItem',
          httpMethod: 'POST',
          path: '/items',
          request: {
            required: true,
            content: [
              {
                mediaType: 'application/json',
                schema: {
                  type: 'object',
                  properties: { name: { type: 'string' }, size: { type: 'integer', format: 'int64' } },
                  required: ['name', 'size'],
                  additionalProperties: false,
                },
              },
            ],
          },
          successes: [
            {
              status: 201,
              description: 'created',
              content: [{ mediaType: 'application/json', schema: { type: 'string' } }],
            },
            { status: 202, description: 'accepted', content: [] },
          ],
          client: operation({
            idempotency: { kind: 'idempotent' },
            errors: [
              {
                status: 409,
                code: 'conflict',
                schema: {
                  type: 'object',
                  properties: { reason: { type: 'string' } },
                  required: ['reason'],
                  additionalProperties: false,
                },
              },
            ],
          }),
        }),
      ]);

      const root = mkdtempSync(join(tmpdir(), 'clientgen-compile-'));
      try {
        for (const file of files) {
          const destination = join(root, file.path);
          mkdirSync(dirname(destination), { recursive: true });
          writeFileSync(destination, file.content);
        }
        // Resolve `@putnami/client` to this working tree's sources, so the emitted
        // client is checked against the surface it will actually import.
        writeFileSync(
          join(root, 'tsconfig.json'),
          JSON.stringify({
            // Spelled out rather than extended from the workspace base: the emitted
            // client must stand on its own under strict settings, with no
            // runtime-specific type library and only the framework surface it
            // imports.
            compilerOptions: {
              noEmit: true,
              strict: true,
              noImplicitOverride: true,
              noPropertyAccessFromIndexSignature: true,
              noImplicitReturns: true,
              noFallthroughCasesInSwitch: true,
              module: 'esnext',
              moduleResolution: 'bundler',
              target: 'esnext',
              lib: ['ESNext', 'DOM'],
              types: [],
              skipLibCheck: true,
              paths: { '@putnami/client': [join(CLIENT_ROOT, 'src/index.ts')] },
            },
            include: ['src/**/*.ts'],
          }),
        );

        const tsc = join(WORKSPACE_ROOT, 'node_modules', '.bin', 'tsc');
        expect(existsSync(tsc), 'workspace TypeScript compiler').toBe(true);
        const result = spawnSync(tsc, ['--project', join(root, 'tsconfig.json')], { cwd: root, encoding: 'utf8' });
        expect(`${result.stdout ?? ''}${result.stderr ?? ''}`).toBe('');
        expect(result.status).toBe(0);
      } finally {
        rmSync(root, { force: true, recursive: true });
      }
    },
    120_000,
  );
});

describe('websocket stream emission', () => {
  const websocket = {
    protocol: 'websocket',
    path: '/items',
    encoding: 'json',
    websocket: { subprotocol: 'putnami.service.v1', resume: false },
  } as const;
  const sse = { protocol: 'sse', path: '/items', encoding: 'json' } as const;

  function source(methods: MethodIR[]): string {
    const files = emit(methods);
    const client = files?.find((file) => file.path.endsWith('items-client.ts'));
    expect(client, 'emitted service file').toBeDefined();
    return client?.content ?? '';
  }

  specTest(
    'emits a websocket server stream when the provider declares websocket first',
    {
      feature: 'typescript/service-clients',
      requirement: 'websocket-stream-emission',
      check: 'a-server-stream-follows-the-declared-transport-order',
    },
    () => {
      // One entrypoint whichever transport wins: `serviceStream` resolves the
      // declared order at call time, so moving an operation from SSE to
      // WebSocket never reaches the consuming application.
      for (const transports of [[sse, websocket], [websocket, sse], [websocket]]) {
        const emitted = source([
          method({
            name: 'watchItems',
            operationId: 'watchItems',
            client: operation({ stream: 'server', messages: { output: { type: 'string' } }, transports }),
            successes: [{ status: 200, description: 'ok', content: [] }],
          }),
        ]);
        expect(emitted).toContain('watchItems(options?: ClientCallOptions): StreamObserver<WatchItemsMessage>');
        expect(emitted).toContain(`return this.serviceStream('GET', "/items"`);
      }
    },
  );

  specTest(
    'emits a typed duplex handle for a client stream and a bidirectional stream',
    {
      feature: 'typescript/service-clients',
      requirement: 'websocket-stream-emission',
      check: 'a-duplex-stream-is-emitted-as-a-typed-duplex-handle',
    },
    () => {
      for (const [stream, entrypoint] of [
        ['client', 'serviceClientStream'],
        ['bidirectional', 'serviceBidiStream'],
      ] as const) {
        const emitted = source([
          method({
            name: 'adjustItems',
            operationId: 'adjustItems',
            client: operation({
              stream,
              messages: { input: { type: 'number', format: 'int64' }, output: { type: 'string' } },
              transports: [websocket],
            }),
            successes: [{ status: 200, description: 'ok', content: [] }],
          }),
        ]);
        expect(emitted).toContain(
          'adjustItems(options?: ClientCallOptions): DuplexStream<AdjustItemsSend, AdjustItemsMessage>',
        );
        expect(emitted).toContain(`return this.${entrypoint}('GET', "/items"`);
        // No phase rule in the emitted code: the runtime drives the published
        // conversation, and a generated method that numbered frames or tracked
        // a half-close would hold a second, divergent state machine.
        for (const forbidden of ['sequence', 'half-close', 'await-ready']) expect(emitted).not.toContain(forbidden);
      }
    },
  );

  specTest(
    'refuses a duplex stream the provider declared without both message schemas',
    {
      feature: 'typescript/service-clients',
      requirement: 'websocket-stream-emission',
      check: 'a-duplex-stream-without-both-message-schemas-is-refused',
    },
    () => {
      for (const messages of [{ output: { type: 'string' } }, { input: { type: 'string' } }]) {
        expect(() =>
          emit([
            method({
              client: operation({ stream: 'bidirectional', messages, transports: [websocket] }),
              successes: [{ status: 200, description: 'ok', content: [] }],
            }),
          ]),
        ).toThrow(/clientgen_unsupported_semantic: operation listItems uses bidirectional operation over websocket/);
      }
    },
  );
});
