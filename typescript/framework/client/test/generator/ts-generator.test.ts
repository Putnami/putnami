import { describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import type { MethodIR, ServiceIR, SpecIR } from '../../src/generator/ir.type';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';

function makeSpec(overrides?: Partial<SpecIR>): SpecIR {
  return {
    transport: 'http',
    services: [
      {
        name: 'UsersService',
        className: 'UsersClient',
        methods: [
          {
            name: 'listUsers',
            operationId: 'listUsers',
            httpMethod: 'GET',
            path: '/users',
            query: [
              { name: 'page', tsType: 'number', optional: true, array: false },
              { name: 'limit', tsType: 'number', optional: true, array: false },
            ],
            response: [
              { name: 'id', tsType: 'string', optional: false, array: false },
              { name: 'name', tsType: 'string', optional: false, array: false },
            ],
          },
          {
            name: 'getUser',
            operationId: 'getUser',
            httpMethod: 'GET',
            path: '/users/{id}',
            params: [{ name: 'id', tsType: 'string', optional: false, array: false }],
            response: [
              { name: 'id', tsType: 'string', optional: false, array: false },
              { name: 'name', tsType: 'string', optional: false, array: false },
              { name: 'email', tsType: 'string', optional: true, array: false },
            ],
          },
          {
            name: 'createUser',
            operationId: 'createUser',
            httpMethod: 'POST',
            path: '/users',
            body: [
              { name: 'name', tsType: 'string', optional: false, array: false },
              { name: 'email', tsType: 'string', optional: false, array: false },
            ],
            response: [{ name: 'id', tsType: 'string', optional: false, array: false }],
          },
        ],
      },
    ],
    ...overrides,
  };
}

describe('generateTypeScriptClient', () => {
  specTest(
    'emits a strict first-party REST client, typed binding, and lossless neutral types',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-error-safety',
      check: 'generated-guards-bind-the-complete-operation-error-identity',
    },
    () => {
      const strictSpec: SpecIR = {
        irVersion: 1,
        transport: 'http',
        contract: {
          protocolVersion: 1,
          service: { id: 'widgets', audience: 'api://widgets' },
          credentials: { service: { kind: 'service-token', scopes: ['read'] } },
        },
        schemas: {
          Widget: {
            type: 'object',
            properties: {
              id: { type: 'integer', format: 'uint64' },
              payload: { type: 'string', format: 'byte' },
              name: { type: 'string' },
            },
            required: ['id', 'payload', 'name'],
          },
          ConflictDetails: {
            type: 'object',
            properties: { reason: { type: 'string' } },
            required: ['reason'],
          },
        },
        services: [
          {
            name: 'WidgetsService',
            className: 'WidgetsClient',
            methods: [
              {
                name: 'putWidget',
                operationId: 'putWidget',
                httpMethod: 'PUT',
                path: '/widgets/{id}',
                parameters: [
                  { name: 'id', location: 'path', required: true, schema: { type: 'integer', format: 'uint64' } },
                  { name: 'view', location: 'query', required: false, schema: { type: 'string' } },
                  { name: 'x-request-mode', location: 'header', required: true, schema: { type: 'string' } },
                ],
                request: {
                  required: true,
                  content: [{ mediaType: 'application/json', schema: { $ref: '#/components/schemas/Widget' } }],
                },
                successes: [
                  {
                    status: 200,
                    description: 'updated',
                    content: [{ mediaType: 'application/json', schema: { $ref: '#/components/schemas/Widget' } }],
                  },
                  { status: 204, description: 'unchanged', content: [] },
                ],
                client: {
                  stream: 'unary',
                  transports: [{ protocol: 'rest-json', path: '/widgets/{id}', encoding: 'json' }],
                  security: { alternatives: [{ allOf: [{ profile: 'service', scopes: ['write'] }] }] },
                  errors: [
                    { status: 400, code: 'http.bad_request' },
                    { status: 409, code: 'Conflict', schema: { $ref: '#/components/schemas/ConflictDetails' } },
                    { status: 500, code: 'internal' },
                  ],
                  idempotency: { kind: 'idempotent', keyHeader: 'Idempotency-Key' },
                },
              },
            ],
          },
        ],
      };

      const files = generateTypeScriptClient(strictSpec, { packageName: '@test/widgets-client' });
      const types = files.find((file) => file.path === 'src/types.ts')?.content ?? '';
      const client = files.find((file) => file.path === 'src/widgets-client.ts')?.content ?? '';
      const index = files.find((file) => file.path === 'src/index.ts')?.content ?? '';

      expect(types).toContain('id: bigint;');
      expect(types).toContain('payload: Uint8Array;');
      expect(types).toContain('"x-request-mode": string;');
      expect(types).toContain('export type PutWidgetResult =');
      expect(types).not.toContain('Record<string, unknown>');
      expect(types).toContain(
        'export type PutWidgetConflictError = import(\'@putnami/client\').ClientFrameworkError<"Conflict", ConflictDetails, "widgets", "putWidget", 409>;',
      );
      expect(types).toContain(
        'export function isPutWidgetConflictError(error: unknown): error is PutWidgetConflictError',
      );
      expect(types).toContain('service: "widgets", method: "putWidget", status: 409, code: "Conflict"');
      expect(types).toContain('detailsSchema: {"$ref":"#/components/schemas/ConflictDetails"}');
      expect(types).toContain('schemas: CLIENT_ERROR_SCHEMAS');
      expect(client).toContain('const SERVICE_DESCRIPTOR =');
      expect(client).toContain(
        'async putWidget(input: PutWidgetInput, options?: ClientCallOptions): Promise<PutWidgetResult>',
      );
      expect(client).toContain('operationId: "putWidget"');
      expect(client).toContain('return this.requestResult');
      expect(client).toContain(
        'encodeHttpParameter(input.path["id"], {"type":"integer","format":"uint64"}, { location: "path" }) as string',
      );
      expect(client).toContain('export function registerWidgetsClient(');
      expect(client).toContain('registerServiceClient(target, WidgetsClient, SERVICE_DESCRIPTOR, binding)');
      expect(index).toContain('registerWidgetsClient');

      const collision = structuredClone(strictSpec);
      const collisionMethod = collision.services[0].methods[0];
      if (!collisionMethod.client) throw new Error('strict test operation has no client contract');
      collisionMethod.client = {
        ...collisionMethod.client,
        errors: [...collisionMethod.client.errors, { status: 422, code: 'http-bad-request' }],
      };
      expect(() => generateTypeScriptClient(collision, { packageName: '@test/widgets-client' })).toThrow(
        /clientgen_unsupported_semantic.*generated symbol.*collision/,
      );

      const schemaCollision = structuredClone(strictSpec);
      schemaCollision.schemas = {
        ...schemaCollision.schemas,
        PutWidgetConflictError: { type: 'string' },
      };
      expect(() => generateTypeScriptClient(schemaCollision, { packageName: '@test/widgets-client' })).toThrow(
        /clientgen_unsupported_semantic.*PutWidgetConflictError.*collision/,
      );

      for (const reserved of ['isClientFrameworkError', 'CLIENT_ERROR_SCHEMAS']) {
        const reservedCollision = structuredClone(strictSpec);
        reservedCollision.schemas = { ...reservedCollision.schemas, [reserved]: { type: 'string' } };
        expect(() => generateTypeScriptClient(reservedCollision, { packageName: '@test/widgets-client' })).toThrow(
          new RegExp(`clientgen_unsupported_semantic.*${reserved}.*collision`),
        );
      }
    },
  );

  test('imports named types referenced inside an inline success schema', () => {
    const strictSpec: SpecIR = {
      irVersion: 1,
      transport: 'http',
      contract: {
        protocolVersion: 1,
        service: { id: 'catalog.items', audience: 'api://catalog.items' },
        credentials: {},
      },
      schemas: {
        Item: {
          type: 'object',
          properties: { id: { type: 'string' } },
          required: ['id'],
          additionalProperties: false,
        },
      },
      services: [
        {
          name: 'ItemsService',
          className: 'ItemsClient',
          methods: [
            {
              name: 'listItems',
              operationId: 'listItems',
              httpMethod: 'GET',
              path: '/items',
              successes: [
                {
                  status: 200,
                  content: [
                    {
                      mediaType: 'application/json',
                      schema: {
                        type: 'object',
                        properties: { items: { type: 'array', items: { $ref: '#/components/schemas/Item' } } },
                        required: ['items'],
                        additionalProperties: false,
                      },
                    },
                  ],
                },
              ],
              client: {
                stream: 'unary',
                transports: [{ protocol: 'rest-json', path: '/items', encoding: 'json' }],
                security: { alternatives: [{ allOf: [] }] },
                errors: [
                  { status: 400, code: 'http.bad_request' },
                  { status: 500, code: 'internal' },
                ],
                idempotency: { kind: 'safe' },
              },
            },
          ],
        },
      ],
    };

    const client =
      generateTypeScriptClient(strictSpec, { packageName: '@test/items-client' }).find(
        (file) => file.path === 'src/items-client.ts',
      )?.content ?? '';
    expect(client).toContain("import type { ClientCallOptions, Item } from './types';");
  });

  test('fails explicitly before emitting a stream that asks a provider with no resume transport to continue', () => {
    const spec = makeSpec({
      irVersion: 1,
      contract: {
        protocolVersion: 1,
        service: { id: 'events', audience: 'api://events' },
        credentials: {},
      },
    });
    spec.services[0].methods[0] = {
      ...spec.services[0].methods[0],
      client: {
        stream: 'server',
        messages: { output: { type: 'string' } },
        transports: [{ protocol: 'sse', path: '/events', encoding: 'json' }],
        security: { alternatives: [{ allOf: [] }] },
        errors: [
          { status: 400, code: 'http.bad_request' },
          { status: 500, code: 'internal' },
        ],
        idempotency: { kind: 'safe' },
        resilience: { stream: { reconnect: true } },
      },
    };
    expect(() => generateTypeScriptClient(spec, { packageName: '@test/events-client' })).toThrow(
      /clientgen_unsupported_semantic.*stream reconnect without a provider-declared resume transport/,
    );
  });

  test('fails explicitly before emitting an SSE stream whose request body would be ignored', () => {
    const spec = makeSpec({
      irVersion: 1,
      contract: {
        protocolVersion: 1,
        service: { id: 'events', audience: 'api://events' },
        credentials: {},
      },
    });
    spec.services[0].methods[0] = {
      ...spec.services[0].methods[0],
      httpMethod: 'POST',
      request: { required: true, content: [{ mediaType: 'application/json', schema: { type: 'string' } }] },
      client: {
        stream: 'server',
        messages: { output: { type: 'string' } },
        transports: [{ protocol: 'sse', path: '/events', encoding: 'json' }],
        security: { alternatives: [{ allOf: [] }] },
        errors: [
          { status: 400, code: 'http.bad_request' },
          { status: 500, code: 'internal' },
        ],
        idempotency: { kind: 'safe' },
      },
    };
    expect(() => generateTypeScriptClient(spec, { packageName: '@test/events-client' })).toThrow(
      /clientgen_unsupported_semantic.*SSE request body is not supported/,
    );
  });
  test('generates expected files', () => {
    const files = generateTypeScriptClient(makeSpec(), { packageName: '@test/users-client' });
    const paths = files.map((f) => f.path).sort((left, right) => left.localeCompare(right));

    expect(paths).toEqual(['package.json', 'src/index.ts', 'src/types.ts', 'src/users-client.ts', 'tsconfig.json']);
  });

  test('generates types.ts with interfaces', () => {
    const files = generateTypeScriptClient(makeSpec(), { packageName: '@test/users-client' });
    const types = files.find((f) => f.path === 'src/types.ts')?.content;

    // Should have query, params, body, and response interfaces
    expect(types).toContain('export interface ListUsersQuery');
    expect(types).toContain('page?: number');
    expect(types).toContain('limit?: number');

    expect(types).toContain('export interface ListUsersResponse');
    expect(types).toContain('id: string');
    expect(types).toContain('name: string');

    expect(types).toContain('export interface GetUserParams');
    expect(types).toContain('id: string');

    expect(types).toContain('export interface GetUserResponse');
    expect(types).toContain('email?: string');

    expect(types).toContain('export interface CreateUserBody');
    expect(types).toContain('export interface CreateUserResponse');
  });

  test('generates client class extending BaseClient', () => {
    const files = generateTypeScriptClient(makeSpec(), { packageName: '@test/users-client' });
    const client = files.find((f) => f.path === 'src/users-client.ts')?.content;

    expect(client).toContain("import { BaseClient, type ClientConfig } from '@putnami/client'");
    expect(client).toContain('export class UsersClient extends BaseClient');
    expect(client).toContain("readonly serviceName = 'users'");
    expect(client).toContain('constructor(config: ClientConfig)');
  });

  test('embeds exact producer feature and operation descriptors when declared', () => {
    const files = generateTypeScriptClient(makeSpec({ specHash: 'spec-123' }), {
      packageName: '@test/users-client',
      design: {
        operations: [
          { method: 'GET', path: '/users/{id}', producerProject: '@test/users', producerFeature: 'users/manage' },
        ],
      },
    });
    const client = files.find((file) => file.path === 'src/users-client.ts')?.content;

    expect(client).toContain('type GeneratedClientDesign');
    expect(client).toContain(
      '"operationId":"getUser","method":"GET","path":"/users/{id}","producerProject":"@test/users","producerFeature":"users/manage"',
    );
    expect(client).toContain('super({ ...config, design: UsersClient.design })');
    // The canonical operation id travels with the call so the runtime resolves
    // the trace from the invoked operation, not by reverse-matching the URL.
    expect(client).toContain("operationId: 'getUser',");
  });

  // The regression fixture the per-operation contract exists for: one generated
  // client whose operations belong to two producer features plus one that
  // belongs to none.
  test('attributes each operation to its own producer and leaves the rest unattributed', () => {
    const files = generateTypeScriptClient(makeSpec({ specHash: 'spec-123' }), {
      packageName: '@test/users-client',
      design: {
        operations: [
          { method: 'GET', path: '/users/{id}', producerProject: '@test/users', producerFeature: 'users/read' },
          { method: 'POST', path: '/users', producerProject: '@test/users', producerFeature: 'users/manage' },
        ],
      },
    });
    const client = files.find((file) => file.path === 'src/users-client.ts')?.content ?? '';

    expect(client).toContain('"operationId":"getUser","method":"GET","path":"/users/{id}"');
    expect(client).toContain('"producerFeature":"users/read"');
    expect(client).toContain('"producerFeature":"users/manage"');
    // listUsers (GET /users) is in neither table entry: it must stay bare.
    expect(client).toContain('{"operationId":"listUsers","method":"GET","path":"/users"}');
  });

  // Cloud's case: the TypeScript symbol normalizer rewrites the punctuation the
  // canonical operation id keeps, so the descriptor and the call must carry the
  // canonical id, never the symbol.
  test('preserves a canonical operation id the TypeScript symbol cannot keep', () => {
    const spec: SpecIR = {
      transport: 'http',
      specHash: 'spec-123',
      services: [
        {
          name: 'OperatorService',
          className: 'OperatorClient',
          methods: [
            {
              name: 'getV1_Operator_Cli_usage',
              operationId: 'getV1_Operator_Cli-usage',
              httpMethod: 'GET',
              path: '/v1/operator/cli-usage',
              response: [{ name: 'total', tsType: 'number', optional: false, array: false }],
            },
          ],
        },
      ],
    };
    const files = generateTypeScriptClient(spec, {
      packageName: '@test/operator-client',
      design: {
        operations: [
          {
            method: 'GET',
            path: '/v1/operator/cli-usage',
            producerProject: 'acme-platform',
            producerFeature: 'platform/operator-cli-usage',
          },
        ],
      },
    });
    const client = files.find((file) => file.path === 'src/operator-client.ts')?.content ?? '';

    expect(client).toContain('async getV1_Operator_Cli_usage(');
    expect(client).toContain('"operationId":"getV1_Operator_Cli-usage"');
    expect(client).toContain("operationId: 'getV1_Operator_Cli-usage',");
    expect(client).toContain('"producerFeature":"platform/operator-cli-usage"');
  });

  test('generates HTTP methods with correct arguments', () => {
    const files = generateTypeScriptClient(makeSpec(), { packageName: '@test/users-client' });
    const client = files.find((f) => f.path === 'src/users-client.ts')?.content;

    // GET with optional query
    expect(client).toContain('async listUsers(query?: ListUsersQuery): Promise<ListUsersResponse>');

    // GET with path params
    expect(client).toContain('async getUser(params: GetUserParams): Promise<GetUserResponse>');

    // POST with body
    expect(client).toContain('async createUser(body: CreateUserBody): Promise<CreateUserResponse>');
  });

  test('generates Connect method calls for proto transport', () => {
    const spec = makeSpec({
      transport: 'connect',
      packageName: 'myapp.v1',
      services: [
        {
          name: 'UsersService',
          className: 'UsersClient',
          methods: [
            {
              name: 'listUsers',
              operationId: 'ListUsers',
              httpMethod: 'POST',
              path: '/myapp.v1.UsersService/ListUsers',
              body: [{ name: 'page', tsType: 'number', optional: true, array: false }],
              response: [{ name: 'id', tsType: 'string', optional: false, array: false }],
            },
          ],
        },
      ],
    });

    const files = generateTypeScriptClient(spec, { packageName: '@test/users-client' });
    const client = files.find((f) => f.path === 'src/users-client.ts')?.content;

    // Connect uses POST for all methods
    expect(client).toContain("this.request('POST', '/myapp.v1.UsersService/ListUsers'");
    expect(client).toContain('body: body');
  });

  test('generates index.ts with re-exports', () => {
    const files = generateTypeScriptClient(makeSpec(), { packageName: '@test/users-client' });
    const index = files.find((f) => f.path === 'src/index.ts')?.content;

    expect(index).toContain("export * from './types'");
    expect(index).toContain("export { UsersClient } from './users-client'");
  });

  test('generates package.json', () => {
    const files = generateTypeScriptClient(makeSpec(), {
      packageName: '@test/users-client',
      version: '1.2.3',
    });
    const pkg = JSON.parse(files.find((f) => f.path === 'package.json')?.content);

    expect(pkg.name).toBe('@test/users-client');
    expect(pkg.version).toBe('1.2.3');
    expect(pkg.main).toBe('src/index.ts');
    expect(pkg.dependencies['@putnami/client']).toBe('workspace:*');
  });

  test('pins @putnami/client with the caller-resolved specifier', () => {
    const files = generateTypeScriptClient(makeSpec(), {
      packageName: '@test/users-client',
      clientDependency: 'catalog:',
    });
    const pkg = JSON.parse(files.find((f) => f.path === 'package.json')?.content);
    expect(pkg.dependencies['@putnami/client']).toBe('catalog:');
  });

  test('generates auto-generated header', () => {
    const files = generateTypeScriptClient(makeSpec(), { packageName: '@test/users-client' });

    for (const file of files) {
      if (file.path.endsWith('.ts')) {
        expect(file.content).toContain('Auto-generated by @putnami/client');
      }
    }
  });

  test('handles multiple services', () => {
    const spec = makeSpec({
      services: [
        {
          name: 'UsersService',
          className: 'UsersClient',
          methods: [
            {
              name: 'listUsers',
              operationId: 'listUsers',
              httpMethod: 'GET',
              path: '/users',
              response: [{ name: 'id', tsType: 'string', optional: false, array: false }],
            },
          ],
        },
        {
          name: 'OrdersService',
          className: 'OrdersClient',
          methods: [
            {
              name: 'listOrders',
              operationId: 'listOrders',
              httpMethod: 'GET',
              path: '/orders',
              response: [{ name: 'id', tsType: 'string', optional: false, array: false }],
            },
          ],
        },
      ],
    });

    const files = generateTypeScriptClient(spec, { packageName: '@test/api-client' });
    const paths = files.map((f) => f.path);

    expect(paths).toContain('src/users-client.ts');
    expect(paths).toContain('src/orders-client.ts');

    const index = files.find((f) => f.path === 'src/index.ts')?.content;
    expect(index).toContain('export { UsersClient }');
    expect(index).toContain('export { OrdersClient }');
  });

  test('emits closed enums and discriminated unions from contract components', () => {
    const files = generateTypeScriptClient(
      makeSpec({
        enums: { Status: ['pending', 'applied'] },
        unions: {
          Change: {
            discriminator: 'kind',
            variants: [
              {
                tag: 'rename',
                fields: [{ name: 'name', tsType: 'string', optional: false, array: false }],
              },
              {
                tag: 'archive',
                fields: [{ name: 'reason', tsType: 'string', optional: true, array: false }],
              },
            ],
          },
        },
      }),
      { packageName: '@test/users-client' },
    );
    const types = files.find((file) => file.path === 'src/types.ts')?.content ?? '';
    expect(types).toContain("export type Status = 'pending' | 'applied';");
    expect(types).toContain('export type Change =');
    expect(types).toContain("kind: 'rename';");
    expect(types).toContain('reason?: string;');
  });

  describe('shared named types ($ref)', () => {
    const namedSpec: SpecIR = {
      transport: 'http',
      namedTypes: {
        User: [
          { name: 'id', tsType: 'string', optional: false, array: false },
          { name: 'name', tsType: 'string', optional: false, array: false },
        ],
      },
      services: [
        {
          name: 'UsersService',
          className: 'UsersClient',
          methods: [
            {
              name: 'getUser',
              operationId: 'getUser',
              httpMethod: 'GET',
              path: '/users/{id}',
              params: [{ name: 'id', tsType: 'string', optional: false, array: false }],
              responseType: 'User',
            },
            {
              name: 'createUser',
              operationId: 'createUser',
              httpMethod: 'POST',
              path: '/users',
              bodyType: 'User',
              responseType: 'User',
            },
          ],
        },
      ],
    };

    test('emits named-type interfaces in types.ts', () => {
      const files = generateTypeScriptClient(namedSpec, { packageName: '@test/users-client' });
      const types = files.find((f) => f.path === 'src/types.ts')?.content ?? '';
      expect(types).toContain('export interface User {');
      expect(types).toContain('id: string;');
      expect(types).toContain('name: string;');
      // No per-operation interfaces synthesized for $ref body/response types.
      expect(types).not.toContain('CreateUserBody');
      expect(types).not.toContain('GetUserResponse');
    });

    test('method signatures reference the named type directly', () => {
      const files = generateTypeScriptClient(namedSpec, { packageName: '@test/users-client' });
      const client = files.find((f) => f.path === 'src/users-client.ts')?.content ?? '';
      expect(client).toContain('async getUser(params: GetUserParams): Promise<User>');
      expect(client).toContain('async createUser(body: User): Promise<User>');
    });

    test('imports a reused named type exactly once', () => {
      const files = generateTypeScriptClient(namedSpec, { packageName: '@test/users-client' });
      const client = files.find((f) => f.path === 'src/users-client.ts')?.content ?? '';
      const importLine = client.split('\n').find((l) => l.includes("from './types'")) ?? '';
      // `User` is referenced by two methods but must be imported once (\b excludes GetUserParams).
      expect(importLine.match(/\bUser\b/g)?.length).toBe(1);
      expect(importLine).toContain('GetUserParams');
    });
  });

  describe('spec-derived identifier validation (injection guard)', () => {
    // A name that, if interpolated raw into `{ <name>: ... }` inside
    // `this.request(...)`, closes the object + call and runs arbitrary code.
    const INJECTION = 'a }); sideEffect(); ({b';
    const OPTS = { packageName: '@test/users-client' };

    /** Minimal valid single-method spec with hostile overrides applied. */
    function specWith(method: Partial<MethodIR>, service?: Partial<ServiceIR>): SpecIR {
      return {
        transport: 'http',
        services: [
          {
            name: 'UsersService',
            className: 'UsersClient',
            methods: [
              {
                name: 'listUsers',
                operationId: 'listUsers',
                httpMethod: 'GET',
                path: '/users',
                ...method,
              },
            ],
            ...service,
          },
        ],
      };
    }

    const field = (name: string, tsType = 'string') => ({ name, tsType, optional: false, array: false });

    test('rejects a hostile query parameter name and names the offender', () => {
      const spec = specWith({ query: [field(INJECTION)] });
      expect(() => generateTypeScriptClient(spec, OPTS)).toThrow('not a valid JavaScript identifier');
      expect(() => generateTypeScriptClient(spec, OPTS)).toThrow(INJECTION);
      expect(() => generateTypeScriptClient(spec, OPTS)).toThrow('query parameter of operation "listUsers"');
    });

    test('rejects a hostile path parameter name', () => {
      const spec = specWith({ path: '/users/{id}', params: [field(INJECTION)] });
      expect(() => generateTypeScriptClient(spec, OPTS)).toThrow('not a valid JavaScript identifier');
    });

    test('rejects a hostile body field name', () => {
      const spec = specWith({ httpMethod: 'POST', body: [field(INJECTION)] });
      expect(() => generateTypeScriptClient(spec, OPTS)).toThrow('not a valid JavaScript identifier');
    });

    test('rejects a hostile response field name', () => {
      const spec = specWith({ response: [field(INJECTION)] });
      expect(() => generateTypeScriptClient(spec, OPTS)).toThrow('not a valid JavaScript identifier');
    });

    // The type name is derived, not interpolated raw, so punctuation an
    // operation id legally carries ("get.well-known_…") no longer aborts
    // generation. Every character outside the identifier alphabet is a word
    // break; the id itself stays verbatim in the emitted literals.
    test('derives a legal type name from an operationId containing spaces and parens', () => {
      const spec = specWith({ operationId: 'list users (all)', response: [field('id')] });
      const files = generateTypeScriptClient(spec, OPTS);
      const types = files.find((file) => file.path === 'src/types.ts')?.content ?? '';
      const client = files.find((file) => file.path === 'src/users-client.ts')?.content ?? '';

      expect(types).toContain('export interface ListUsersAllResponse');
      expect(client).toContain('async listUsers(): Promise<ListUsersAllResponse>');
      expect(client).not.toContain('list users (all)');
    });

    // The derived type base folds punctuation, so two ids that differ only in
    // it would declare the same interface twice in types.ts.
    test('rejects two operations whose ids derive the same type name', () => {
      for (const [first, second] of [
        ['get_Widgets_Id', 'get.widgets_Id'],
        ['list$Users', 'listUsers'],
      ]) {
        const spec = specWith({ operationId: first, response: [field('id')] });
        spec.services[0].methods.push({
          name: 'other',
          operationId: second,
          httpMethod: 'GET',
          path: '/other',
          response: [field('id')],
        });
        expect(() => generateTypeScriptClient(spec, OPTS)).toThrow(
          `Operations ${JSON.stringify(first)} and ${JSON.stringify(second)} both generate the type name`,
        );
      }
    });

    test('rejects a hostile method name', () => {
      const spec = specWith({ name: 'evil() { hack(); } async x' });
      expect(() => generateTypeScriptClient(spec, OPTS)).toThrow('not a valid JavaScript identifier');
    });

    test('rejects a hostile class name', () => {
      const spec = specWith({}, { className: 'Evil {}; class Hack' });
      expect(() => generateTypeScriptClient(spec, OPTS)).toThrow('not a valid JavaScript identifier');
    });

    test('rejects a path-traversal class name', () => {
      const spec = specWith({}, { className: '../../escape' });
      expect(() => generateTypeScriptClient(spec, OPTS)).toThrow('not a valid JavaScript identifier');
    });

    test('rejects prototype-polluting names even though they are valid identifiers', () => {
      for (const name of ['__proto__', 'constructor', 'prototype']) {
        const spec = specWith({ query: [{ ...field(name), optional: true }] });
        expect(() => generateTypeScriptClient(spec, OPTS)).toThrow('forbidden');
      }
    });

    test('rejects a hostile field type token', () => {
      const spec = specWith({ response: [field('id', 'string; } evil(); ({')] });
      expect(() => generateTypeScriptClient(spec, OPTS)).toThrow('not a valid JavaScript identifier');
    });

    test('rejects hostile bodyType and responseType tokens', () => {
      expect(() => generateTypeScriptClient(specWith({ httpMethod: 'POST', bodyType: 'User; evil()' }), OPTS)).toThrow(
        'not a valid JavaScript identifier',
      );
      expect(() => generateTypeScriptClient(specWith({ responseType: 'User; evil()' }), OPTS)).toThrow(
        'not a valid JavaScript identifier',
      );
    });

    test('rejects hostile named-type names and fields', () => {
      const badName = specWith({});
      badName.namedTypes = { [INJECTION]: [field('id')] };
      expect(() => generateTypeScriptClient(badName, OPTS)).toThrow('not a valid JavaScript identifier');

      const badField = specWith({});
      badField.namedTypes = { User: [field(INJECTION)] };
      expect(() => generateTypeScriptClient(badField, OPTS)).toThrow('not a valid JavaScript identifier');
    });

    test('still accepts a normal spec', () => {
      expect(() => generateTypeScriptClient(makeSpec(), OPTS)).not.toThrow();
    });

    test('accepts $- and _-prefixed names and the Record<string, unknown> degrade type', () => {
      const spec = specWith({
        query: [
          { ...field('$top', 'number'), optional: true },
          { ...field('_id'), optional: true },
        ],
        response: [field('meta', 'Record<string, unknown>')],
      });
      const files = generateTypeScriptClient(spec, OPTS);
      const types = files.find((f) => f.path === 'src/types.ts')?.content ?? '';
      expect(types).toContain('$top?: number');
      expect(types).toContain('meta: Record<string, unknown>;');
    });
  });
});
