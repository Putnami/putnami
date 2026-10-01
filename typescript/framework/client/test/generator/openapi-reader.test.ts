import { describe, expect, test } from 'bun:test';
import type { OpenApiDocument } from '@putnami/application';
import { readOpenApiSpec } from '../../src/generator/openapi-reader';

function makeOpenApiDoc(paths: OpenApiDocument['paths']): OpenApiDocument {
  return {
    openapi: '3.0.3',
    info: { title: 'Test API', version: '1.0.0' },
    paths,
  };
}

describe('readOpenApiSpec', () => {
  // "/.well-known/putnami/events" must not produce ".wellKnownClient", which the
  // strict emitter refuses. go/framework/api clientir_test.go holds the same
  // table, so the two readers cannot drift on service names.
  test('derives a legal service identifier from any first path segment', () => {
    const cases: Record<string, string> = {
      '/users/{id}': 'UsersService',
      '/user-profiles': 'UserProfilesService',
      '/audit_log': 'AuditLogService',
      '/.well-known/putnami/events': 'WellKnownService',
      '/v1.2/items': 'V12Service',
      '/2fa/codes': '_2faService',
      '/.../x': 'ApiService',
      '/{id}': 'ApiService',
    };
    for (const [path, name] of Object.entries(cases)) {
      const spec = readOpenApiSpec(
        makeOpenApiDoc({ [path]: { get: { operationId: 'probe', responses: { '204': { description: 'ok' } } } } }),
      );
      expect({ path, name: spec.services[0].name }).toEqual({ path, name });
      expect(spec.services[0].className).toBe(name.replace(/Service$/, 'Client'));
    }
  });

  test('preserves contract enums and tagged unions', () => {
    const doc: OpenApiDocument = {
      openapi: '3.0.3',
      info: { title: 'Contract API', version: '1.0.0' },
      paths: {
        '/changes': {
          post: {
            operationId: 'createChange',
            requestBody: {
              required: true,
              content: { 'application/json': { schema: { $ref: '#/components/schemas/ChangeRequest' } } },
            },
            responses: {
              '200': {
                description: 'Success',
                content: { 'application/json': { schema: { $ref: '#/components/schemas/ChangeRequest' } } },
              },
            },
          },
        },
      },
      components: {
        schemas: {
          Status: { type: 'string', enum: ['pending', 'applied'] },
          RenameChange: {
            type: 'object',
            properties: { name: { type: 'string' } },
            required: ['name'],
          },
          ArchiveChange: {
            type: 'object',
            properties: { reason: { type: 'string' } },
          },
          Change: {
            oneOf: [{ $ref: '#/components/schemas/RenameChange' }, { $ref: '#/components/schemas/ArchiveChange' }],
            discriminator: {
              propertyName: 'kind',
              mapping: {
                rename: '#/components/schemas/RenameChange',
                archive: '#/components/schemas/ArchiveChange',
              },
            },
          },
          ChangeRequest: {
            type: 'object',
            properties: {
              status: { $ref: '#/components/schemas/Status' },
              change: { $ref: '#/components/schemas/Change' },
            },
            required: ['status', 'change'],
          },
        },
      },
    };

    const spec = readOpenApiSpec(doc);
    expect(spec.enums).toEqual({ Status: ['pending', 'applied'] });
    expect(spec.unions?.Change).toEqual({
      discriminator: 'kind',
      variants: [
        { tag: 'rename', fields: [{ name: 'name', tsType: 'string', optional: false, array: false }] },
        { tag: 'archive', fields: [{ name: 'reason', tsType: 'string', optional: true, array: false }] },
      ],
    });
    expect(spec.namedTypes?.ChangeRequest).toEqual([
      { name: 'status', tsType: 'Status', optional: false, array: false },
      { name: 'change', tsType: 'Change', optional: false, array: false },
    ]);
  });

  test('reads a simple GET endpoint', () => {
    const doc = makeOpenApiDoc({
      '/users': {
        get: {
          operationId: 'listUsers',
          responses: {
            '200': {
              description: 'Success',
              content: {
                'application/json': {
                  schema: {
                    type: 'object',
                    properties: {
                      id: { type: 'string' },
                      name: { type: 'string' },
                    },
                    required: ['id', 'name'],
                  },
                },
              },
            },
          },
        },
      },
    });

    const spec = readOpenApiSpec(doc);

    expect(spec.transport).toBe('http');
    expect(spec.services).toHaveLength(1);
    expect(spec.services[0].name).toBe('UsersService');
    expect(spec.services[0].className).toBe('UsersClient');
    expect(spec.services[0].methods).toHaveLength(1);

    const method = spec.services[0].methods[0];
    expect(method.name).toBe('listUsers');
    expect(method.httpMethod).toBe('GET');
    expect(method.path).toBe('/users');
    expect(method.response).toHaveLength(2);
    expect(method.response?.[0]).toEqual({ name: 'id', tsType: 'string', optional: false, array: false });
    expect(method.response?.[1]).toEqual({ name: 'name', tsType: 'string', optional: false, array: false });
  });

  test('reads path and query parameters', () => {
    const doc = makeOpenApiDoc({
      '/users/{id}': {
        get: {
          operationId: 'getUser',
          parameters: [
            { name: 'id', in: 'path', required: true, schema: { type: 'string', format: 'uuid' } },
            { name: 'fields', in: 'query', required: false, schema: { type: 'string' } },
          ],
          responses: { '200': { description: 'Success' } },
        },
      },
    });

    const spec = readOpenApiSpec(doc);
    const method = spec.services[0].methods[0];

    expect(method.params).toHaveLength(1);
    expect(method.params?.[0]).toEqual({ name: 'id', tsType: 'string', optional: false, array: false });

    expect(method.query).toHaveLength(1);
    expect(method.query?.[0]).toEqual({ name: 'fields', tsType: 'string', optional: true, array: false });
  });

  test('reads POST with request body', () => {
    const doc = makeOpenApiDoc({
      '/users': {
        post: {
          operationId: 'createUser',
          requestBody: {
            required: true,
            content: {
              'application/json': {
                schema: {
                  type: 'object',
                  properties: {
                    name: { type: 'string' },
                    age: { type: 'number' },
                    active: { type: 'boolean' },
                  },
                  required: ['name'],
                },
              },
            },
          },
          responses: { '200': { description: 'Created' } },
        },
      },
    });

    const spec = readOpenApiSpec(doc);
    const method = spec.services[0].methods[0];

    expect(method.body).toHaveLength(3);
    expect(method.body?.[0]).toEqual({ name: 'name', tsType: 'string', optional: false, array: false });
    expect(method.body?.[1]).toEqual({ name: 'age', tsType: 'number', optional: true, array: false });
    expect(method.body?.[2]).toEqual({ name: 'active', tsType: 'boolean', optional: true, array: false });
  });

  test('groups routes by first path segment', () => {
    const doc = makeOpenApiDoc({
      '/users': { get: { operationId: 'listUsers', responses: {} } },
      '/users/{id}': { get: { operationId: 'getUser', responses: {} } },
      '/orders': { get: { operationId: 'listOrders', responses: {} } },
      '/orders/{id}': { delete: { operationId: 'deleteOrder', responses: {} } },
    });

    const spec = readOpenApiSpec(doc);

    expect(spec.services).toHaveLength(2);
    const serviceNames = spec.services.map((s) => s.name).sort((left, right) => left.localeCompare(right));
    expect(serviceNames).toEqual(['OrdersService', 'UsersService']);

    const usersService = spec.services.find((s) => s.name === 'UsersService');
    if (!usersService) throw new Error('missing UsersService');
    expect(usersService.methods).toHaveLength(2);

    const ordersService = spec.services.find((s) => s.name === 'OrdersService');
    if (!ordersService) throw new Error('missing OrdersService');
    expect(ordersService.methods).toHaveLength(2);
  });

  test('handles array types', () => {
    const doc = makeOpenApiDoc({
      '/items': {
        get: {
          operationId: 'listItems',
          responses: {
            '200': {
              description: 'Success',
              content: {
                'application/json': {
                  schema: {
                    type: 'object',
                    properties: {
                      tags: { type: 'array', items: { type: 'string' } },
                    },
                    required: ['tags'],
                  },
                },
              },
            },
          },
        },
      },
    });

    const spec = readOpenApiSpec(doc);
    const field = spec.services[0].methods[0].response?.[0];

    expect(field.name).toBe('tags');
    expect(field.tsType).toBe('string');
    expect(field.array).toBe(true);
  });

  test('generates operationId if missing', () => {
    const doc = makeOpenApiDoc({
      '/users/{id}': {
        get: {
          responses: { '200': { description: 'OK' } },
        },
      },
    });

    const spec = readOpenApiSpec(doc);
    expect(spec.services[0].methods[0].name).toBe('getUsers_id');
  });
});
