import { describe, expect, it } from 'bun:test';
import type { DiscoveredRoute } from '@putnami/application';
import {
  ArrayOf,
  Constrained,
  DateIso,
  Default,
  Desc,
  Email,
  Int,
  IntWidth,
  MapOf,
  Max,
  MaxLength,
  Min,
  MinLength,
  Nullable,
  Optional,
  Pattern,
  Url,
  Uuid,
  type SchemaDescriptor,
} from '@putnami/application';
import { contractStruct, type ContractManifest } from '../../src/contracts';
import { generateOpenApiSpec } from '../../src/openapi/openapi';

describe('generateOpenApiSpec', () => {
  const defaultOptions = {
    info: { title: 'Test API', version: '1.0.0' },
  };

  it('projects canonical contract enums, unions, and DTOs as referenced components', () => {
    const manifest: ContractManifest = {
      protocolVersion: 1,
      name: 'example/projection',
      enums: [
        {
          name: 'ProjectionStatus',
          values: [
            { name: 'Pending', value: 'pending' },
            { name: 'Applied', value: 'applied' },
          ],
        },
        { name: 'UnusedProjectionType', values: [{ name: 'Unused', value: 'unused' }] },
      ],
      unions: [
        {
          name: 'ProjectionChange',
          discriminator: 'kind',
          variants: [
            {
              tag: 'rename',
              fields: [
                { name: 'kind', type: 'string' },
                { name: 'name', type: 'string' },
              ],
            },
            { tag: 'archive', fields: [{ name: 'reason', type: 'string', optional: true }] },
          ],
        },
      ],
      structs: [
        {
          name: 'ProjectionRequest',
          fields: [
            { name: 'status', type: 'ProjectionStatus' },
            { name: 'change', type: 'ProjectionChange' },
          ],
        },
      ],
    };
    const request = contractStruct(manifest, 'ProjectionRequest');
    const spec = generateOpenApiSpec(
      [{ method: 'POST', path: '/projection', schemas: { body: request, returns: request } }],
      defaultOptions,
    );

    expect(spec.paths['/projection'].post.requestBody?.content['application/json'].schema).toEqual({
      $ref: '#/components/schemas/ProjectionRequest',
    });
    expect(spec.components?.schemas?.ProjectionStatus.enum).toEqual(['pending', 'applied']);
    expect(spec.components?.schemas?.ProjectionChange.discriminator).toEqual({ propertyName: 'kind' });
    expect(spec.components?.schemas?.ProjectionChange.oneOf).toHaveLength(2);
    expect(spec.components?.schemas?.ProjectionChange.oneOf?.[0].properties?.kind).toEqual({
      type: 'string',
      enum: ['rename'],
    });
    expect(spec.components?.schemas?.ProjectionRequest.properties?.change).toEqual({
      $ref: '#/components/schemas/ProjectionChange',
    });
    expect(spec.components?.schemas?.UnusedProjectionType).toBeUndefined();
  });

  it('reserves contract names when assigning generated shared-model names', () => {
    const manifest: ContractManifest = {
      protocolVersion: 1,
      name: 'example/model-collision',
      structs: [{ name: 'Model1', fields: [{ name: 'contractId', type: 'string' }] }],
    };
    const contract = contractStruct(manifest, 'Model1');
    const shared = { name: String };
    const spec = generateOpenApiSpec(
      [
        { method: 'POST', path: '/contract', schemas: { body: contract } },
        { method: 'GET', path: '/first', schemas: { returns: shared } },
        { method: 'GET', path: '/second', schemas: { returns: shared } },
      ],
      defaultOptions,
    );

    expect(spec.components?.schemas?.Model1.properties?.contractId).toEqual({ type: 'string' });
    expect(spec.components?.schemas?.Model2.properties?.name).toEqual({ type: 'string' });
    expect(spec.paths['/first'].get.responses['200'].content?.['application/json'].schema).toEqual({
      $ref: '#/components/schemas/Model2',
    });
  });

  it('should generate a valid OpenAPI 3.0.3 document', () => {
    const spec = generateOpenApiSpec([], defaultOptions);
    expect(spec.openapi).toBe('3.0.3');
    expect(spec.info.title).toBe('Test API');
    expect(spec.info.version).toBe('1.0.0');
    expect(spec.paths).toEqual({});
  });

  it('should serialize routes deterministically regardless of discovery order', () => {
    const routes: DiscoveredRoute[] = [
      { method: 'POST', path: '/zeta', schemas: { body: { name: String } } },
      { method: 'GET', path: '/alpha' },
      { method: 'GET', path: '/zeta' },
    ];

    const forward = generateOpenApiSpec(routes, defaultOptions);
    const reversed = generateOpenApiSpec([...routes].reverse(), defaultOptions);

    expect(Object.keys(forward.paths)).toEqual(['/alpha', '/zeta']);
    expect(Object.keys(forward.paths['/zeta'])).toEqual(['get', 'post']);
    expect(JSON.stringify(forward)).toBe(JSON.stringify(reversed));
    expect(routes.map(({ method, path }) => `${method} ${path}`)).toEqual(['POST /zeta', 'GET /alpha', 'GET /zeta']);
  });

  it('should include servers when provided', () => {
    const spec = generateOpenApiSpec([], {
      ...defaultOptions,
      servers: [{ url: 'https://api.example.com', description: 'Production' }],
    });
    expect(spec.servers).toEqual([{ url: 'https://api.example.com', description: 'Production' }]);
  });

  it('should omit servers when empty', () => {
    const spec = generateOpenApiSpec([], defaultOptions);
    expect(spec.servers).toBeUndefined();
  });

  it('projects the first-party client contract without losing operation semantics', () => {
    const spec = generateOpenApiSpec(
      [
        {
          method: 'GET',
          path: '/events/[topic]',
          streamMode: 'server',
          schemas: {
            params: { topic: String },
            returns: {
              sequence: Int,
              payload: Uuid,
            },
          },
          responses: {
            errorCodes: ['Conflict', 'Unavailable'],
            errorOptions: { Conflict: { retryable: false }, Unavailable: { retryable: true } },
            throws: [{ status: 409, description: 'Cursor conflict', schema: { cursor: String } }],
          },
          meta: {
            security: { scopes: ['events:read'] },
            client: {
              security: {
                alternatives: [{ allOf: [{ profile: 'service', scopes: ['events:read'] }] }],
              },
              idempotency: { kind: 'safe' },
              resilience: {
                timeoutMs: 5000,
                stream: { heartbeatMs: 1000, maxBufferedMessages: 64 },
              },
            },
          },
        },
      ],
      {
        ...defaultOptions,
        client: {
          service: { id: 'events', audience: 'api://events' },
          credentials: {
            service: { kind: 'service-token', scopes: ['events:read'] },
          },
          defaults: { resilience: { maxResponseBytes: 1_048_576 } },
        },
        connect: { packageName: 'example.events.v1' },
      },
    );

    expect(spec['x-putnami-client']).toEqual({
      protocolVersion: 1,
      service: { id: 'events', audience: 'api://events' },
      credentials: { service: { kind: 'service-token', scopes: ['events:read'] } },
      defaults: { resilience: { maxResponseBytes: 1_048_576 } },
      protobuf: expect.objectContaining({
        syntax: 'proto3',
        package: 'example.events.v1',
        services: [
          {
            name: 'EventsService',
            methods: [
              expect.objectContaining({
                name: 'GetEventsByTopic',
                serverStreaming: true,
              }),
            ],
          },
        ],
      }),
    });
    expect(spec.paths['/events/{topic}'].get['x-putnami-client']).toEqual({
      stream: 'server',
      messages: { output: expect.any(Object) },
      transports: [
        {
          protocol: 'connect',
          path: '/example.events.v1.EventsService/GetEventsByTopic',
          encoding: 'proto',
          protobufMethod: '/example.events.v1.EventsService/GetEventsByTopic',
        },
        {
          protocol: 'connect',
          path: '/example.events.v1.EventsService/GetEventsByTopic',
          encoding: 'json',
          protobufMethod: '/example.events.v1.EventsService/GetEventsByTopic',
        },
        { protocol: 'sse', path: '/events/{topic}', encoding: 'json' },
        {
          protocol: 'websocket',
          path: '/events/{topic}',
          encoding: 'json',
          websocket: { subprotocol: 'putnami.service.v1', resume: false },
        },
      ],
      security: {
        alternatives: [{ allOf: [{ profile: 'service', scopes: ['events:read'] }] }],
        authorization: { scopesAll: ['events:read'] },
      },
      errors: [
        { status: 400, code: 'http.bad_request' },
        { status: 409, code: 'conflict', retryable: false, schema: expect.any(Object) },
        { status: 500, code: 'http.internal_server' },
        { status: 503, code: 'http.service_unavailable', retryable: true },
      ],
      idempotency: { kind: 'safe' },
      resilience: {
        timeoutMs: 5000,
        stream: { heartbeatMs: 1000, maxBufferedMessages: 64 },
      },
    });
  });

  it('rejects a client security alternative weaker than the served endpoint', () => {
    expect(() =>
      generateOpenApiSpec(
        [
          {
            method: 'GET',
            path: '/admin',
            meta: {
              security: { scopes: ['admin:read'] },
              client: {
                security: { alternatives: [{ allOf: [] }] },
                idempotency: { kind: 'safe' },
              },
            },
          },
        ],
        {
          ...defaultOptions,
          client: {
            service: { id: 'admin', audience: 'api://admin' },
            credentials: { user: { kind: 'forwarded-user-token' } },
          },
        },
      ),
    ).toThrow('an authenticated endpoint cannot advertise an anonymous client alternative');
  });

  it('rejects a typed throw without one stable declared framework error code', () => {
    expect(() =>
      generateOpenApiSpec(
        [
          {
            method: 'POST',
            path: '/documents',
            schemas: { returns: { id: String } },
            responses: { throws: [{ status: 409, description: 'Conflict', schema: { reason: String } }] },
          },
        ],
        {
          ...defaultOptions,
          client: { service: { id: 'documents', audience: 'api://documents' }, credentials: {} },
        },
      ),
    ).toThrow('.throws(409) has no stable .mayThrow() error code at that status');
  });

  // Several codes on one status discriminate the error; .throws() documents the status.
  it('keeps every .mayThrow() code that shares a status documented by .throws()', () => {
    const spec = generateOpenApiSpec(
      [
        {
          method: 'POST',
          path: '/documents',
          schemas: { returns: { id: String } },
          responses: {
            errorCodes: ['Conflict', 'AlreadyExists'],
            throws: [{ status: 409, description: 'Document conflict' }],
          },
        },
      ],
      {
        ...defaultOptions,
        client: { service: { id: 'documents', audience: 'api://documents' }, credentials: {} },
      },
    );

    const operation = spec.paths['/documents'].post;
    expect(operation['x-putnami-client']?.errors).toEqual([
      { status: 400, code: 'http.bad_request' },
      { status: 409, code: 'already_exists' },
      { status: 409, code: 'conflict' },
      { status: 500, code: 'http.internal_server' },
    ]);
    expect(operation.responses['409'].description).toBe('Document conflict');
  });

  it('applies a .throws() details schema to every .mayThrow() code at its status', () => {
    const spec = generateOpenApiSpec(
      [
        {
          method: 'POST',
          path: '/documents',
          schemas: { returns: { id: String } },
          responses: {
            errorCodes: ['Conflict', 'AlreadyExists', 'NotFound'],
            throws: [{ status: 409, description: 'Document conflict', schema: { reason: String } }],
          },
        },
      ],
      {
        ...defaultOptions,
        client: { service: { id: 'documents', audience: 'api://documents' }, credentials: {} },
      },
    );

    const details = {
      type: 'object',
      properties: { reason: { type: 'string' } },
      additionalProperties: false,
      required: ['reason'],
    };
    expect(spec.paths['/documents'].post['x-putnami-client']?.errors).toEqual([
      { status: 400, code: 'http.bad_request' },
      { status: 404, code: 'not_found' },
      { status: 409, code: 'already_exists', schema: details },
      { status: 409, code: 'conflict', schema: details },
      { status: 500, code: 'http.internal_server' },
    ]);
  });

  // Parity with the Go projection: the implicit 400 and 500 codes are declared
  // wire codes, so a .throws() at those statuses has a stable code. They carry
  // the envelope and never details (ADR 0006), so they never take its schema.
  it('accepts a .throws(500) with no .mayThrow() and keeps the implicit code schema-free', () => {
    const spec = generateOpenApiSpec(
      [
        {
          method: 'POST',
          path: '/documents',
          schemas: { returns: { id: String } },
          responses: {
            throws: [{ status: 500, description: 'Storage failure', schema: { reason: String } }],
          },
        },
      ],
      {
        ...defaultOptions,
        client: { service: { id: 'documents', audience: 'api://documents' }, credentials: {} },
      },
    );

    const operation = spec.paths['/documents'].post;
    expect(operation['x-putnami-client']?.errors).toEqual([
      { status: 400, code: 'http.bad_request' },
      { status: 500, code: 'http.internal_server' },
    ]);
    expect(operation.responses['500'].description).toBe('Storage failure');
  });

  it('attaches a .throws(400) details schema to the declared code and never to the implicit one', () => {
    const spec = generateOpenApiSpec(
      [
        {
          method: 'POST',
          path: '/documents',
          schemas: { returns: { id: String } },
          responses: {
            errorCodes: ['Validation'],
            throws: [{ status: 400, description: 'Validation failed', schema: { field: String } }],
          },
        },
      ],
      {
        ...defaultOptions,
        client: { service: { id: 'documents', audience: 'api://documents' }, credentials: {} },
      },
    );

    expect(spec.paths['/documents'].post['x-putnami-client']?.errors).toEqual([
      { status: 400, code: 'http.bad_request' },
      {
        status: 400,
        code: 'validation',
        schema: {
          type: 'object',
          properties: { field: { type: 'string' } },
          additionalProperties: false,
          required: ['field'],
        },
      },
      { status: 500, code: 'http.internal_server' },
    ]);
  });

  it('projects client and bidirectional stream message schemas without inventing HTTP 200 responses', () => {
    const spec = generateOpenApiSpec(
      [
        {
          method: 'POST',
          path: '/uploads',
          streamMode: 'client',
          schemas: { body: { chunk: String }, returns: { accepted: Number } },
        },
        {
          method: 'POST',
          path: '/chat',
          streamMode: 'bidirectional',
          schemas: { body: { text: String }, returns: { text: String } },
        },
      ],
      {
        ...defaultOptions,
        client: {
          service: { id: 'streams', audience: 'api://streams' },
          credentials: {},
        },
      },
    );

    expect(Object.keys(spec.paths['/uploads'].post.responses).sort()).toEqual(['101', '400', '500']);
    expect(spec.paths['/uploads'].post.responses['101']).toEqual({ description: 'WebSocket upgrade' });
    expect(spec.paths['/uploads'].post.responses['200']).toBeUndefined();
    expect(spec.paths['/uploads'].post['x-putnami-client']?.messages).toEqual({
      input: expect.objectContaining({ type: 'object' }),
      output: expect.objectContaining({ type: 'object' }),
    });
    expect(spec.paths['/chat'].post['x-putnami-client']?.messages).toEqual({
      input: expect.objectContaining({ type: 'object' }),
      output: expect.objectContaining({ type: 'object' }),
    });
  });

  it('rejects a first-party bidirectional stream with an untyped message direction', () => {
    expect(() =>
      generateOpenApiSpec(
        [{ method: 'POST', path: '/chat', streamMode: 'bidirectional', schemas: { body: { text: String } } }],
        {
          ...defaultOptions,
          client: {
            service: { id: 'streams', audience: 'api://streams' },
            credentials: {},
          },
        },
      ),
    ).toThrow('bidirectional stream must declare its output schema');
  });

  describe('path conversion', () => {
    it('should convert [param] to {param} in paths', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/users/[id]' }];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.paths['/users/{id}']).toBeDefined();
    });

    it('should handle multiple params', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/orgs/[orgId]/users/[userId]' }];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.paths['/orgs/{orgId}/users/{userId}']).toBeDefined();
    });

    it('should handle paths without params', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/health' }];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.paths['/health']).toBeDefined();
    });
  });

  describe('operations', () => {
    it('should map HTTP method to lowercase', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/items' }];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.paths['/items'].get).toBeDefined();
      expect(spec.paths['/items'].GET).toBeUndefined();
    });

    it('should group multiple methods under the same path', () => {
      const routes: DiscoveredRoute[] = [
        { method: 'GET', path: '/items' },
        { method: 'POST', path: '/items' },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.paths['/items'].get).toBeDefined();
      expect(spec.paths['/items'].post).toBeDefined();
    });

    it('should generate operationId', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/users/[id]' }];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.paths['/users/{id}'].get.operationId).toBe('getUsers_id');
    });
  });

  describe('path parameters', () => {
    it('should generate path parameters from params schema', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/users/[id]',
          schemas: { params: { id: Uuid } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const params = spec.paths['/users/{id}'].get.parameters;
      expect(params).toHaveLength(1);
      expect(params?.[0]).toEqual({
        name: 'id',
        in: 'path',
        required: true,
        schema: { type: 'string', format: 'uuid' },
      });
    });

    it('should mark Optional params as not required', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/users/[id]',
          schemas: { params: { id: Optional(String) } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.paths['/users/{id}'].get.parameters?.[0].required).toBe(false);
    });
  });

  describe('query parameters', () => {
    it('should generate query parameters from query schema', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/users',
          schemas: { query: { page: Number, search: Optional(String) } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const params = spec.paths['/users'].get.parameters;
      expect(params).toHaveLength(2);
      expect(params?.[0]).toEqual({ name: 'page', in: 'query', required: true, schema: { type: 'number' } });
      expect(params?.[1]).toEqual({ name: 'search', in: 'query', required: false, schema: { type: 'string' } });
    });
  });

  describe('header parameters', () => {
    it('should generate header parameters from headers schema', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/traced',
          schemas: { headers: { 'x-request-id': Uuid, 'x-tenant': Optional(String) } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const params = spec.paths['/traced'].get.parameters;
      expect(params).toHaveLength(2);
      expect(params?.[0]).toEqual({
        name: 'x-request-id',
        in: 'header',
        required: true,
        schema: { type: 'string', format: 'uuid' },
      });
      expect(params?.[1]).toEqual({
        name: 'x-tenant',
        in: 'header',
        required: false,
        schema: { type: 'string' },
      });
    });
  });

  describe('request body', () => {
    it('should generate request body from body schema', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/users',
          schemas: { body: { name: String, email: Email } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const body = spec.paths['/users'].post.requestBody;
      expect(body).toBeDefined();
      expect(body?.required).toBe(true);
      expect(body?.content['application/json'].schema).toEqual({
        type: 'object',
        properties: {
          name: { type: 'string' },
          email: { type: 'string', format: 'email' },
        },
        required: ['name', 'email'],
        additionalProperties: false,
      });
    });

    it('should handle optional body fields', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'PUT',
          path: '/users/[id]',
          schemas: { body: { name: Optional(String), email: Optional(Email) } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const schema = spec.paths['/users/{id}'].put.requestBody?.content['application/json'].schema;
      expect(schema.required).toBeUndefined();
    });

    it('should use application/x-www-form-urlencoded when bodyContentType is set', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/token',
          schemas: {
            body: { grant_type: String, code: Optional(String) },
            bodyContentType: 'application/x-www-form-urlencoded',
          },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const body = spec.paths['/token'].post.requestBody;
      expect(body).toBeDefined();
      expect(body?.required).toBe(true);
      expect(body?.content['application/x-www-form-urlencoded']).toBeDefined();
      expect(body?.content['application/json']).toBeUndefined();
      expect(body?.content['application/x-www-form-urlencoded'].schema).toEqual({
        type: 'object',
        properties: {
          grant_type: { type: 'string' },
          code: { type: 'string' },
        },
        required: ['grant_type'],
        additionalProperties: false,
      });
    });
  });

  describe('responses', () => {
    it('should generate response schema from returns', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/users',
          schemas: { returns: { id: String, name: String, email: Email } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const response = spec.paths['/users'].get.responses['200'];
      expect(response.description).toBe('Successful response');
      expect(response.content?.['application/json'].schema).toEqual({
        type: 'object',
        properties: {
          id: { type: 'string' },
          name: { type: 'string' },
          email: { type: 'string', format: 'email' },
        },
        required: ['id', 'name', 'email'],
        additionalProperties: false,
      });
    });

    it('should generate generic 200 response when no returns schema', () => {
      const routes: DiscoveredRoute[] = [{ method: 'DELETE', path: '/users/[id]' }];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const response = spec.paths['/users/{id}'].delete.responses['200'];
      expect(response).toEqual({ description: 'Successful response' });
      expect(response.content).toBeUndefined();
    });
  });

  describe('schema type mapping', () => {
    it('should map JS constructors to OpenAPI types', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/test',
          schemas: {
            body: { str: String, num: Number, bool: Boolean },
          },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const props = spec.paths['/test'].post.requestBody?.content['application/json'].schema.properties;
      expect(props?.str).toEqual({ type: 'string' });
      expect(props?.num).toEqual({ type: 'number' });
      expect(props?.bool).toEqual({ type: 'boolean' });
    });

    it('should map constrained types with format', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/test',
          schemas: { body: { id: Uuid, email: Email, count: Int } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const props = spec.paths['/test'].post.requestBody?.content['application/json'].schema.properties;
      expect(props?.id).toEqual({ type: 'string', format: 'uuid' });
      expect(props?.email).toEqual({ type: 'string', format: 'email' });
      // ADR 0004: an integer carries its declared width, not the JavaScript
      // runtime's safe-integer range.
      expect(props?.count).toEqual({ type: 'integer', format: 'int64' });
    });

    it('declares an integer width and its natural range, and projects nullability', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/test',
          schemas: {
            body: {
              wide: Constrained(Int, IntWidth('uint64')),
              narrow: Constrained(Int, IntWidth('int32')),
              unsigned: Constrained(Int, IntWidth('uint32')),
              bounded: Constrained(Int, IntWidth('int32'), Min(1), Max(100)),
              nickname: Nullable(String),
              score: Nullable(Constrained(Int, IntWidth('int32'))),
            },
          },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const props = spec.paths['/test'].post.requestBody?.content['application/json'].schema.properties;
      // A 64-bit range cannot survive `JSON.stringify`, so the width carries it
      // and no rounded bound is written.
      expect(props?.wide).toEqual({ type: 'integer', format: 'uint64' });
      expect(props?.narrow).toEqual({
        type: 'integer',
        format: 'int32',
        minimum: -2_147_483_648,
        maximum: 2_147_483_647,
      });
      expect(props?.unsigned).toEqual({ type: 'integer', format: 'uint32', minimum: 0, maximum: 4_294_967_295 });
      // An author bound is narrower than the natural range and wins.
      expect(props?.bounded).toEqual({ type: 'integer', format: 'int32', minimum: 1, maximum: 100 });
      expect(props?.nickname).toEqual({ type: 'string', nullable: true });
      expect(props?.score).toEqual({
        type: 'integer',
        format: 'int32',
        minimum: -2_147_483_648,
        maximum: 2_147_483_647,
        nullable: true,
      });
    });

    it('rejects an unknown integer width at the declaration site', () => {
      expect(() => IntWidth('int16' as never)).toThrow('is not one of int32, int64, uint32, uint64');
    });

    it('should map ArrayOf to array type', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/test',
          schemas: { body: { tags: ArrayOf(String), ids: ArrayOf(Uuid) } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const props = spec.paths['/test'].post.requestBody?.content['application/json'].schema.properties;
      expect(props?.tags).toEqual({ type: 'array', items: { type: 'string' } });
      expect(props?.ids).toEqual({ type: 'array', items: { type: 'string', format: 'uuid' } });
    });
  });

  describe('full spec generation', () => {
    it('should generate a complete spec for a typical CRUD API', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/users',
          schemas: {
            query: { page: Optional(Number), limit: Optional(Number) },
            returns: { id: String, name: String },
          },
        },
        {
          method: 'POST',
          path: '/users',
          schemas: {
            body: { name: String, email: Email },
            returns: { id: String, name: String, email: String },
          },
        },
        {
          method: 'GET',
          path: '/users/[id]',
          schemas: {
            params: { id: Uuid },
            returns: { id: String, name: String, email: String },
          },
        },
        {
          method: 'DELETE',
          path: '/users/[id]',
          schemas: {
            params: { id: Uuid },
          },
        },
      ];

      const spec = generateOpenApiSpec(routes, {
        info: { title: 'User API', version: '2.0.0', description: 'User management' },
        servers: [{ url: 'http://localhost:3000' }],
      });

      expect(spec.openapi).toBe('3.0.3');
      expect(spec.info.description).toBe('User management');
      expect(Object.keys(spec.paths)).toHaveLength(2); // /users and /users/{id}
      expect(spec.paths['/users'].get).toBeDefined();
      expect(spec.paths['/users'].post).toBeDefined();
      expect(spec.paths['/users/{id}'].get).toBeDefined();
      expect(spec.paths['/users/{id}'].delete).toBeDefined();
    });
  });

  describe('multi-status responses', () => {
    it('should generate multiple response codes from responses.returns', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/users',
          schemas: { body: { name: String } },
          responses: {
            returns: [
              { status: 200, description: 'Already exists', schema: { id: String } },
              { status: 201, description: 'Created', schema: { id: String, name: String } },
            ],
          },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const responses = spec.paths['/users'].post.responses;
      expect(responses['200']).toBeDefined();
      expect(responses['200'].description).toBe('Already exists');
      expect(responses['200'].content?.['application/json'].schema).toEqual({
        type: 'object',
        properties: { id: { type: 'string' } },
        required: ['id'],
        additionalProperties: false,
      });
      expect(responses['201']).toBeDefined();
      expect(responses['201'].description).toBe('Created');
    });

    it('should use schemas.returns as fallback 200 when responses.returns has no 200', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/users',
          schemas: { returns: { id: String, name: String } },
          responses: {
            returns: [{ status: 201, description: 'Created', schema: { id: String } }],
          },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const responses = spec.paths['/users'].post.responses;
      expect(responses['200']).toBeDefined();
      expect(responses['200'].description).toBe('Successful response');
      expect(responses['201']).toBeDefined();
      expect(responses['201'].description).toBe('Created');
    });

    it('should generate description-only response without schema', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'DELETE',
          path: '/users/[id]',
          responses: {
            returns: [{ status: 204, description: 'No content' }],
          },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const response = spec.paths['/users/{id}'].delete.responses['204'];
      expect(response.description).toBe('No content');
      expect(response.content).toBeUndefined();
    });

    it('should fallback to legacy single 200 when no responses.returns', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/health',
          schemas: { returns: { status: String } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.paths['/health'].get.responses['200']).toBeDefined();
      expect(spec.paths['/health'].get.responses['200'].description).toBe('Successful response');
    });
  });

  describe('throws responses', () => {
    it('should add default framework error responses to every operation', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/health' }];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const responses = spec.paths['/health'].get.responses;

      expect(responses['400'].description).toBe('Bad Request');
      expect(responses['400'].content?.['application/json'].schema.properties?.message).toEqual({ type: 'string' });
      expect(responses['500'].description).toBe('Internal Server Error');
      expect(responses['500'].content?.['application/json'].schema.properties?.error).toEqual({ type: 'string' });
    });

    it('should map response error codes to standard error responses', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/users/[id]',
          responses: {
            errorCodes: ['NotFound', 'Conflict'],
          },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const responses = spec.paths['/users/{id}'].get.responses;

      expect(responses['404'].description).toBe('Not Found');
      expect(responses['404'].content?.['application/json'].schema.properties?.message).toEqual({ type: 'string' });
      expect(responses['409'].description).toBe('Conflict');
      expect(responses['409'].content?.['application/json'].schema.properties?.error).toEqual({ type: 'string' });
    });

    it('should let explicit throws override generated error responses for the same status', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/users/[id]',
          responses: {
            errorCodes: ['NotFound'],
            throws: [{ status: 404, description: 'User not found', schema: { message: String } }],
          },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const response = spec.paths['/users/{id}'].get.responses['404'];

      expect(response.description).toBe('User not found');
      expect(response.content?.['application/json'].schema).toEqual({
        type: 'object',
        properties: { message: { type: 'string' } },
        required: ['message'],
        additionalProperties: false,
      });
    });

    it('should generate error responses from responses.throws', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/users',
          responses: {
            throws: [
              { status: 401, description: 'Unauthorized', schema: { message: String } },
              { status: 500, description: 'Internal error' },
            ],
          },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const responses = spec.paths['/users'].get.responses;
      expect(responses['401'].description).toBe('Unauthorized');
      expect(responses['401'].content?.['application/json'].schema).toEqual({
        type: 'object',
        properties: { message: { type: 'string' } },
        required: ['message'],
        additionalProperties: false,
      });
      expect(responses['500'].description).toBe('Internal error');
      expect(responses['500'].content).toBeUndefined();
    });

    it('should combine success and error responses', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/users',
          schemas: { body: { name: String }, returns: { id: String } },
          responses: {
            returns: [
              { status: 200, description: 'Success', schema: { id: String } },
              { status: 201, description: 'Created', schema: { id: String } },
            ],
            throws: [
              { status: 400, description: 'Validation failed', schema: { message: String } },
              { status: 401, description: 'Unauthorized' },
            ],
          },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const responses = spec.paths['/users'].post.responses;
      expect(responses['200']).toBeDefined();
      expect(responses['201']).toBeDefined();
      expect(responses['400']).toBeDefined();
      expect(responses['401']).toBeDefined();
    });

    it('should add throws to stream endpoints', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/events',
          streamMode: 'server',
          schemas: { returns: { event: String } },
          responses: {
            throws: [{ status: 401, description: 'Unauthorized' }],
          },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const responses = spec.paths['/events'].get.responses;
      expect(responses['200']).toBeDefined(); // SSE stream
      expect(responses['401']).toBeDefined(); // throws
      expect(responses['401'].description).toBe('Unauthorized');
    });
  });

  describe('security metadata', () => {
    it('should add securitySchemes when any route has security', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/users', meta: { security: {} } }];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.components?.securitySchemes?.bearerAuth).toEqual({
        type: 'http',
        scheme: 'bearer',
        bearerFormat: 'JWT',
      });
    });

    it('should add per-operation security with empty scopes for basic auth', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/users', meta: { security: {} } }];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.paths['/users'].get.security).toEqual([{ bearerAuth: [] }]);
    });

    it('keeps the bearerAuth requirement scope-array empty (OpenAPI 3.0.3: http schemes carry no scopes)', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'DELETE',
          path: '/users/[id]',
          meta: { security: { roles: ['admin'], scopes: ['users:delete'] } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const security = spec.paths['/users/{id}'].delete.security;
      expect(security).toHaveLength(1);
      // The bearer scheme is type `http`; OpenAPI 3.0.3 requires its scope array
      // to be empty. Required scopes are surfaced in the operation description
      // (see the "document roles/scopes in operation description" test).
      expect(security?.[0].bearerAuth).toEqual([]);
    });

    it('should derive an apiKey scheme and requirement for api-key-only routes', () => {
      const routes: DiscoveredRoute[] = [
        { method: 'POST', path: '/ingest', meta: { security: { principalKind: 'apikey' } } },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.components?.securitySchemes?.apiKey).toEqual({
        type: 'apiKey',
        in: 'header',
        name: 'X-Api-Key',
      });
      // No bearer scheme when the route only accepts api keys.
      expect(spec.components?.securitySchemes?.bearerAuth).toBeUndefined();
      expect(spec.paths['/ingest'].post.security).toEqual([{ apiKey: [] }]);
    });

    it('should emit both schemes as alternatives when either kind is accepted', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/ingest',
          meta: { security: { principalKind: ['user', 'apikey'], scopes: ['ingest:write'] } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.components?.securitySchemes?.bearerAuth).toBeDefined();
      expect(spec.components?.securitySchemes?.apiKey).toBeDefined();
      // Alternatives (logical OR). Both schemes carry an empty scope array —
      // OpenAPI 3.0.3 forbids scopes on non-oauth2 schemes — so required scopes
      // live in the operation description, not the requirement.
      expect(spec.paths['/ingest'].post.security).toEqual([{ bearerAuth: [] }, { apiKey: [] }]);
    });

    it('should document roles/scopes in operation description', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'DELETE',
          path: '/users/[id]',
          meta: { security: { roles: ['admin'], scopes: ['users:delete'] } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const description = spec.paths['/users/{id}'].delete.description;
      expect(description).toContain('Required roles: admin');
      expect(description).toContain('Required scopes: users:delete');
    });

    it('should append security notes to existing description', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'DELETE',
          path: '/users/[id]',
          meta: { description: 'Delete a user', security: { roles: ['admin'] } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const description = spec.paths['/users/{id}'].delete.description;
      expect(description).toContain('Delete a user');
      expect(description).toContain('Required roles: admin');
    });

    it('should not add notes when security has no specific roles/scopes', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/me', meta: { security: {} } }];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.paths['/me'].get.description).toBeUndefined();
    });

    it('should not add security to operations without meta.security', () => {
      const routes: DiscoveredRoute[] = [
        { method: 'GET', path: '/health' },
        { method: 'GET', path: '/users', meta: { security: {} } },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.paths['/health'].get.security).toBeUndefined();
      expect(spec.paths['/users'].get.security).toBeDefined();
    });

    it('should not add security schemes when no routes have security', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/health' }];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.components?.securitySchemes).toBeUndefined();
    });
  });

  describe('operation description', () => {
    it('should add description from meta.description', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/users', meta: { description: 'List all users' } }];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.paths['/users'].get.description).toBe('List all users');
    });

    it('should not set description when meta has no description', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/users' }];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.paths['/users'].get.description).toBeUndefined();
    });

    it('should use user description for stream endpoints instead of auto-generated', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/events',
          streamMode: 'server',
          meta: { description: 'Real-time event feed' },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.paths['/events'].get.description).toBe('Real-time event feed');
    });
  });

  describe('schema-level descriptions', () => {
    it('should include description from Desc() in body schema properties', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/users',
          schemas: {
            body: {
              name: Desc('Full name of the user', String),
              email: Desc('Primary email address', Email),
            },
          },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const props = spec.paths['/users'].post.requestBody?.content['application/json'].schema.properties;
      expect(props?.name.description).toBe('Full name of the user');
      expect(props?.email.description).toBe('Primary email address');
      expect(props?.email.format).toBe('email');
    });

    it('should include description from Desc() in response schema properties', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/users',
          schemas: {
            returns: {
              id: Desc('Unique identifier', Uuid),
              name: Desc('Display name', String),
            },
          },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const props = spec.paths['/users'].get.responses['200'].content?.['application/json'].schema.properties;
      expect(props?.id.description).toBe('Unique identifier');
      expect(props?.id.format).toBe('uuid');
      expect(props?.name.description).toBe('Display name');
    });

    it('should not add description when Desc() is not used', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/test',
          schemas: { body: { name: String, count: Number } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const props = spec.paths['/test'].post.requestBody?.content['application/json'].schema.properties;
      expect(props?.name.description).toBeUndefined();
      expect(props?.count.description).toBeUndefined();
    });
  });

  describe('nested object schemas', () => {
    it('should emit a real object schema for a plain nested object (not the string fallback)', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/things',
          schemas: { body: { meta: { a: String, b: Number } } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const props = spec.paths['/things'].post.requestBody?.content['application/json'].schema.properties;
      expect(props?.meta).toEqual({
        type: 'object',
        properties: { a: { type: 'string' }, b: { type: 'number' } },
        required: ['a', 'b'],
        additionalProperties: false,
      });
    });

    it('should emit a nested object schema for Desc(description, { ... }) with the description attached', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/things',
          schemas: { body: { address: Desc('Postal address', { street: String, zip: Optional(String) }) } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const address = spec.paths['/things'].post.requestBody?.content['application/json'].schema.properties?.address;
      expect(address).toEqual({
        type: 'object',
        properties: { street: { type: 'string' }, zip: { type: 'string' } },
        required: ['street'],
        description: 'Postal address',
        additionalProperties: false,
      });
    });

    it('should preserve the MapOf value schema', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/things',
          schemas: { body: { labels: MapOf(String, String) } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const props = spec.paths['/things'].post.requestBody?.content['application/json'].schema.properties;
      expect(props?.labels).toEqual({ type: 'object', additionalProperties: { type: 'string' } });
    });

    it('preserves supported validation constraints and defaults for first-party clients', () => {
      const constrained = Constrained(Min(1), Max(10));
      const name = Constrained(MinLength(2), MaxLength(20), Pattern(/^[a-z]+$/));
      const spec = generateOpenApiSpec(
        [
          {
            method: 'POST',
            path: '/things',
            schemas: {
              body: {
                count: Default(constrained, 3),
                name,
                website: Url,
                createdAt: DateIso,
              },
            },
          },
        ],
        {
          ...defaultOptions,
          client: { service: { id: 'things', audience: 'api://things' }, credentials: {} },
        },
      );

      expect(spec.paths['/things'].post.requestBody?.content['application/json'].schema).toEqual({
        type: 'object',
        properties: {
          count: { type: 'number', minimum: 1, maximum: 10, default: 3 },
          name: { type: 'string', minLength: 2, maxLength: 20, pattern: '^[a-z]+$' },
          website: { type: 'string', format: 'uri' },
          createdAt: { type: 'string', format: 'date-time' },
        },
        required: ['name', 'website', 'createdAt'],
        additionalProperties: false,
      });
    });

    it('fails first-party generation on an unsupported validation constraint', () => {
      const custom = {
        __schema: 'putnami:schema',
        baseType: 'string',
        constraints: [{ name: 'custom', validate: () => true, message: 'custom' }],
      } as unknown as SchemaDescriptor<string>;

      expect(() =>
        generateOpenApiSpec([{ method: 'POST', path: '/things', schemas: { body: { value: custom } } }], {
          ...defaultOptions,
          client: { service: { id: 'things', audience: 'api://things' }, credentials: {} },
        }),
      ).toThrow('unsupported validation constraint');
    });

    it('gives every first-party integer a declared width, so a strict emitter accepts it', () => {
      const spec = generateOpenApiSpec(
        [
          {
            method: 'POST',
            path: '/events',
            schemas: { body: { sequence: Constrained(Int, IntWidth('uint64')), retries: Int } },
          },
        ],
        {
          ...defaultOptions,
          client: { service: { id: 'events', audience: 'api://events' }, credentials: {} },
        },
      );
      const props = spec.paths['/events'].post.requestBody?.content['application/json'].schema.properties;
      // Both strict emitters refuse `{"type":"integer"}` without a format, so a
      // first-party provider that exposes an integer used to be ungeneratable.
      expect(props?.sequence).toEqual({ type: 'integer', format: 'uint64' });
      expect(props?.retries).toEqual({ type: 'integer', format: 'int64' });
    });

    it('refuses a first-party integer bound the projection cannot write exactly', () => {
      expect(() =>
        generateOpenApiSpec(
          [
            {
              method: 'POST',
              path: '/events',
              schemas: { body: { sequence: Constrained(Int, IntWidth('uint64'), Max(2 ** 64)) } },
            },
          ],
          {
            ...defaultOptions,
            client: { service: { id: 'events', audience: 'api://events' }, credentials: {} },
          },
        ),
      ).toThrow('is not an exact integer');
    });
  });

  describe('shared schema promotion (components/$ref)', () => {
    it('should leave single-use object schemas inline and not emit components.schemas', () => {
      const routes: DiscoveredRoute[] = [{ method: 'POST', path: '/things', schemas: { body: { unique: String } } }];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.components?.schemas).toBeUndefined();
      expect(spec.paths['/things'].post.requestBody?.content['application/json'].schema).toEqual({
        type: 'object',
        properties: { unique: { type: 'string' } },
        required: ['unique'],
        additionalProperties: false,
      });
    });

    it('should promote an object reused across operations to a single $ref component', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/users/[id]',
          schemas: { params: { id: Uuid }, returns: { id: String, name: String } },
        },
        {
          method: 'GET',
          path: '/accounts/[id]',
          schemas: { params: { id: Uuid }, returns: { id: String, name: String } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);

      expect(spec.components?.schemas?.Model1).toEqual({
        type: 'object',
        properties: { id: { type: 'string' }, name: { type: 'string' } },
        required: ['id', 'name'],
        additionalProperties: false,
      });

      const ref = { $ref: '#/components/schemas/Model1' };
      expect(spec.paths['/users/{id}'].get.responses['200'].content?.['application/json'].schema).toEqual(ref);
      expect(spec.paths['/accounts/{id}'].get.responses['200'].content?.['application/json'].schema).toEqual(ref);
    });

    it('should assign component names deterministically by sorted signature', () => {
      const routes: DiscoveredRoute[] = [
        { method: 'GET', path: '/a', schemas: { returns: { x: String } } },
        { method: 'GET', path: '/b', schemas: { returns: { x: String } } },
        { method: 'GET', path: '/c', schemas: { returns: { y: Number } } },
        { method: 'GET', path: '/d', schemas: { returns: { y: Number } } },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      // Signature for {x} sorts before {y}, so it is Model1 regardless of route order.
      expect(spec.components?.schemas?.Model1).toEqual({
        type: 'object',
        properties: { x: { type: 'string' } },
        required: ['x'],
        additionalProperties: false,
      });
      expect(spec.components?.schemas?.Model2).toEqual({
        type: 'object',
        properties: { y: { type: 'number' } },
        required: ['y'],
        additionalProperties: false,
      });
    });

    it('should promote a nested object reused within a single body and $ref it in place', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/orgs',
          schemas: { body: { home: { city: String }, work: { city: String } } },
        },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.components?.schemas?.Model1).toEqual({
        type: 'object',
        properties: { city: { type: 'string' } },
        required: ['city'],
        additionalProperties: false,
      });
      const bodySchema = spec.paths['/orgs'].post.requestBody?.content['application/json'].schema;
      expect(bodySchema?.properties?.home).toEqual({ $ref: '#/components/schemas/Model1' });
      expect(bodySchema?.properties?.work).toEqual({ $ref: '#/components/schemas/Model1' });
    });

    it('should keep objects that differ only in a constraint or a map value as separate components', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/a',
          schemas: { returns: { name: Constrained(MinLength(3)), tags: MapOf(String, String) } },
        },
        {
          method: 'GET',
          path: '/b',
          schemas: { returns: { name: Constrained(MinLength(3)), tags: MapOf(String, String) } },
        },
        { method: 'GET', path: '/c', schemas: { returns: { name: String, tags: MapOf(String, Int) } } },
        { method: 'GET', path: '/d', schemas: { returns: { name: String, tags: MapOf(String, Int) } } },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      const returned = (path: string) => spec.paths[path].get.responses['200'].content?.['application/json'].schema;
      // Each pair shares one component; the two pairs never share one, so neither
      // route publishes the other's constraint or map values.
      expect(returned('/a')).toEqual(returned('/b') as never);
      expect(returned('/c')).toEqual(returned('/d') as never);
      expect(returned('/a')).not.toEqual(returned('/c') as never);
      const named = (path: string) => spec.components?.schemas?.[(returned(path)?.$ref ?? '').split('/').pop() ?? ''];
      expect(named('/a')?.properties?.name).toEqual({ type: 'string', minLength: 3 });
      expect(named('/c')?.properties?.name).toEqual({ type: 'string' });
      expect(named('/a')?.properties?.tags).toEqual({ type: 'object', additionalProperties: { type: 'string' } });
      expect(named('/c')?.properties?.tags).toMatchObject({
        type: 'object',
        additionalProperties: { type: 'integer' },
      });
    });

    it('should keep securitySchemes alongside promoted schemas', () => {
      const routes: DiscoveredRoute[] = [
        { method: 'GET', path: '/a', meta: { security: {} }, schemas: { returns: { id: String } } },
        { method: 'GET', path: '/b', schemas: { returns: { id: String } } },
      ];
      const spec = generateOpenApiSpec(routes, defaultOptions);
      expect(spec.components?.schemas?.Model1).toBeDefined();
      expect(spec.components?.securitySchemes?.bearerAuth).toBeDefined();
    });
  });
});
