import { describe, expect, it } from 'bun:test';
import type { DiscoveredRoute } from '@putnami/application';
import { ArrayOf, Email, Int, MapOf, OneOf, Optional, Uuid } from '@putnami/application';
import { generateProto } from '../../src/proto/proto';

describe('generateProto', () => {
  const defaultOptions = { packageName: 'test.v1' };

  it('should generate valid proto3 syntax header', () => {
    const proto = generateProto([], defaultOptions);
    expect(proto.content).toContain('syntax = "proto3";');
    expect(proto.content).toContain('package test.v1;');
    expect(proto.packageName).toBe('test.v1');
  });

  it('should include go_package option when provided', () => {
    const proto = generateProto([], { ...defaultOptions, goPackage: 'github.com/org/repo/pb' });
    expect(proto.content).toContain('option go_package = "github.com/org/repo/pb";');
  });

  it('should omit go_package when not provided', () => {
    const proto = generateProto([], defaultOptions);
    expect(proto.content).not.toContain('go_package');
  });

  it('should return empty services and messages for no routes', () => {
    const proto = generateProto([], defaultOptions);
    expect(proto.services).toEqual({});
    expect(proto.messages).toEqual([]);
  });

  describe('service grouping', () => {
    it('should group routes by first path segment into services', () => {
      const routes: DiscoveredRoute[] = [
        { method: 'GET', path: '/users' },
        { method: 'GET', path: '/orders' },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.services.UsersService).toBeDefined();
      expect(proto.services.OrdersService).toBeDefined();
    });

    it('should group nested routes under the same service', () => {
      const routes: DiscoveredRoute[] = [
        { method: 'GET', path: '/users' },
        { method: 'GET', path: '/users/[id]' },
        { method: 'POST', path: '/users' },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.services.UsersService).toHaveLength(3);
    });

    it('emits the same document whatever order the routes are discovered in', () => {
      const routes: DiscoveredRoute[] = [
        { method: 'POST', path: '/users' },
        { method: 'GET', path: '/users/[id]' },
        { method: 'GET', path: '/orders' },
        { method: 'GET', path: '/users' },
      ];
      const forward = generateProto(routes, defaultOptions);
      const reversed = generateProto([...routes].reverse(), defaultOptions);
      expect(reversed.content).toBe(forward.content);
      expect(Object.keys(forward.services)).toEqual(['OrdersService', 'UsersService']);
      expect(forward.services.UsersService).toEqual(['ListUsers', 'CreateUsers', 'GetUsersById']);
    });
  });

  describe('RPC naming', () => {
    it('should use List prefix for GET on collection', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/users' }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.services.UsersService).toContain('ListUsers');
    });

    it('should use Get prefix for GET with path param', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/users/[id]' }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.services.UsersService).toContain('GetUsersById');
    });

    it('should use Create prefix for POST', () => {
      const routes: DiscoveredRoute[] = [{ method: 'POST', path: '/users' }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.services.UsersService).toContain('CreateUsers');
    });

    it('should use Update prefix for PUT', () => {
      const routes: DiscoveredRoute[] = [{ method: 'PUT', path: '/users/[id]' }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.services.UsersService).toContain('UpdateUsersById');
    });

    it('should use Delete prefix for DELETE', () => {
      const routes: DiscoveredRoute[] = [{ method: 'DELETE', path: '/users/[id]' }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.services.UsersService).toContain('DeleteUsersById');
    });

    it('should use Patch prefix for PATCH', () => {
      const routes: DiscoveredRoute[] = [{ method: 'PATCH', path: '/users/[id]' }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.services.UsersService).toContain('PatchUsersById');
    });
  });

  describe('message generation', () => {
    it('should create request and response messages for each RPC', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/users' }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.messages).toContain('ListUsersRequest');
      expect(proto.messages).toContain('ListUsersResponse');
    });

    it('should generate empty message for route without schemas', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/health' }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('message ListHealthRequest {');
      expect(proto.content).toContain('}');
    });
  });

  describe('type mapping', () => {
    it('should map String to string', () => {
      const routes: DiscoveredRoute[] = [{ method: 'POST', path: '/items', schemas: { body: { name: String } } }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('string name = ');
    });

    it('should map Number to double', () => {
      const routes: DiscoveredRoute[] = [{ method: 'POST', path: '/items', schemas: { body: { price: Number } } }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('double price = ');
    });

    it('should map Boolean to bool', () => {
      const routes: DiscoveredRoute[] = [{ method: 'POST', path: '/items', schemas: { body: { active: Boolean } } }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('bool active = ');
    });

    it('should map Uuid to string with UUID comment', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/users/[id]', schemas: { params: { id: Uuid } } }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('string id = 1; // UUID format');
    });

    it('should map Email to string with Email comment', () => {
      const routes: DiscoveredRoute[] = [{ method: 'POST', path: '/users', schemas: { body: { email: Email } } }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('string email = 1; // Email format');
    });

    it('maps the contract type Int to int64 on every transport (D0.2)', () => {
      const routes: DiscoveredRoute[] = [{ method: 'POST', path: '/items', schemas: { body: { count: Int } } }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('int64 count = 1;');
    });

    it('should map Optional to optional field', () => {
      const routes: DiscoveredRoute[] = [
        { method: 'POST', path: '/items', schemas: { body: { name: Optional(String) } } },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('optional string name = ');
    });

    it('should map ArrayOf to repeated field', () => {
      const routes: DiscoveredRoute[] = [
        { method: 'POST', path: '/items', schemas: { body: { tags: ArrayOf(String) } } },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('repeated string tags = ');
    });

    it('should map ArrayOf with constrained items', () => {
      const routes: DiscoveredRoute[] = [{ method: 'POST', path: '/items', schemas: { body: { ids: ArrayOf(Uuid) } } }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('repeated string ids = ');
    });

    it('should map OneOf to a proto enum type', () => {
      const routes: DiscoveredRoute[] = [
        { method: 'POST', path: '/items', schemas: { body: { status: OneOf('active', 'inactive', 'archived') } } },
      ];
      const proto = generateProto(routes, defaultOptions);
      // Should generate enum block, scoped to the body section of the envelope
      expect(proto.content).toContain('enum CreateItemsRequestBodyStatus {');
      expect(proto.content).toContain('CREATE_ITEMS_REQUEST_BODY_STATUS_UNSPECIFIED = 0;');
      expect(proto.content).toContain('CREATE_ITEMS_REQUEST_BODY_STATUS_ACTIVE = 1;');
      expect(proto.content).toContain('CREATE_ITEMS_REQUEST_BODY_STATUS_INACTIVE = 2;');
      expect(proto.content).toContain('CREATE_ITEMS_REQUEST_BODY_STATUS_ARCHIVED = 3;');
      // Field should reference the enum type
      expect(proto.content).toContain('CreateItemsRequestBodyStatus status = 1;');
    });

    it('should include OneOf enum types in enumTypes array', () => {
      const routes: DiscoveredRoute[] = [
        { method: 'POST', path: '/items', schemas: { body: { status: OneOf('active', 'inactive') } } },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.enumTypes).toContain('CreateItemsRequestBodyStatus');
    });

    it('sets the Int field type to int64 in messageMeta (D0.2)', () => {
      const routes: DiscoveredRoute[] = [{ method: 'POST', path: '/items', schemas: { body: { count: Int } } }];
      const proto = generateProto(routes, defaultOptions);
      const field = proto.messageMeta.CreateItemsRequestBody.find((f) => f.name === 'count');
      expect(field?.type).toBe('int64');
    });
  });

  describe('the request envelope', () => {
    it('should carry params and query as named sections of the request message', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/users/[id]',
          schemas: {
            params: { id: Uuid },
            query: { include: Optional(String) },
          },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('GetUsersByIdRequestParams params = 1;');
      expect(proto.content).toContain('GetUsersByIdRequestQuery query = 2;');
      expect(proto.messageMeta.GetUsersByIdRequestParams?.map((field) => `${field.name}=${field.number}`)).toEqual([
        'id=1',
      ]);
      expect(proto.messageMeta.GetUsersByIdRequestQuery?.map((field) => `${field.name}=${field.number}`)).toEqual([
        'include=1',
      ]);
    });

    it('should number the sections in params, query, body order, skipping an absent one', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'PUT',
          path: '/users/[id]',
          schemas: {
            params: { id: Uuid },
            body: { id: String, name: String, email: Email },
          },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      const request = Object.entries(proto.messageMeta).find(
        ([name, fields]) => name.endsWith('Request') && fields.some((field) => field.name === 'params'),
      );
      expect(request?.[1].map((field) => `${field.name}=${field.number}`)).toEqual(['params=1', 'body=2']);
      // A body member named like the path parameter stays apart from it.
      const body = proto.messageMeta[`${request?.[0]}Body`];
      expect(body?.map((field) => `${field.name}=${field.number}`)).toEqual(['id=1', 'name=2', 'email=3']);
    });
  });

  describe('response messages', () => {
    it('should generate response fields from returns schema', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/users',
          schemas: { returns: { id: String, name: String, email: Email } },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('message ListUsersResponse {');
      expect(proto.content).toContain('string id = 1;');
      expect(proto.content).toContain('string name = 2;');
      expect(proto.content).toContain('string email = 3; // Email format');
    });

    it('should generate empty response for route without returns', () => {
      const routes: DiscoveredRoute[] = [{ method: 'DELETE', path: '/users/[id]' }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('message DeleteUsersByIdResponse {');
      expect(proto.content).toMatch(/message DeleteUsersByIdResponse \{\n\}/);
    });
  });

  describe('nested object schemas', () => {
    it('should create sub-messages for nested objects in body', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/users',
          schemas: {
            body: {
              name: String,
              address: { street: String, city: String },
            },
          },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      // Should create a nested message
      expect(proto.content).toContain('message CreateUsersRequestBodyAddress {');
      expect(proto.content).toContain('string street = 1;');
      expect(proto.content).toContain('string city = 2;');
      // The body section references the nested type
      expect(proto.content).toContain('CreateUsersRequestBodyAddress address = 2;');
    });
  });

  describe('streaming', () => {
    it('should generate server streaming RPC for server stream mode', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/events',
          streamMode: 'server',
          schemas: { returns: { type: String, data: String } },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('rpc ListEvents(ListEventsRequest) returns (stream ListEventsResponse);');
    });

    it('should generate client streaming RPC for client stream mode', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/upload',
          streamMode: 'client',
          schemas: { body: { chunk: String } },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('rpc ListUpload(stream ListUploadRequest) returns (ListUploadResponse);');
    });

    it('should generate bidirectional streaming RPC', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/chat',
          streamMode: 'bidirectional',
          schemas: {
            body: { message: String },
            returns: { reply: String },
          },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('rpc ListChat(stream ListChatRequest) returns (stream ListChatResponse);');
    });
  });

  describe('service rendering', () => {
    it('should render service block with RPCs', () => {
      const routes: DiscoveredRoute[] = [
        { method: 'GET', path: '/users' },
        { method: 'POST', path: '/users' },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('service UsersService {');
      expect(proto.content).toContain('rpc ListUsers(ListUsersRequest) returns (ListUsersResponse);');
      expect(proto.content).toContain('rpc CreateUsers(CreateUsersRequest) returns (CreateUsersResponse);');
    });
  });

  describe('full proto generation', () => {
    it('should generate a complete proto for a typical CRUD API', () => {
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

      const proto = generateProto(routes, {
        packageName: 'myapp.v1',
        goPackage: 'github.com/myorg/myapp/pb',
      });

      expect(proto.content).toContain('syntax = "proto3";');
      expect(proto.content).toContain('package myapp.v1;');
      expect(proto.content).toContain('option go_package = "github.com/myorg/myapp/pb";');

      // Messages
      expect(proto.messages).toContain('ListUsersRequest');
      expect(proto.messages).toContain('ListUsersResponse');
      expect(proto.messages).toContain('CreateUsersRequest');
      expect(proto.messages).toContain('CreateUsersResponse');
      expect(proto.messages).toContain('GetUsersByIdRequest');
      expect(proto.messages).toContain('GetUsersByIdResponse');
      expect(proto.messages).toContain('DeleteUsersByIdRequest');
      expect(proto.messages).toContain('DeleteUsersByIdResponse');

      // Service
      expect(proto.services.UsersService).toHaveLength(4);
      expect(proto.content).toContain('service UsersService {');
    });
  });

  describe('camelCase to snake_case field names', () => {
    it('should convert camelCase field names to snake_case', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/items',
          schemas: { body: { firstName: String, lastName: String } },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('string first_name = 1;');
      expect(proto.content).toContain('string last_name = 2;');
    });
  });

  describe('messageMeta', () => {
    it('should include field metadata for each message', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/users/[id]',
          schemas: {
            params: { id: Uuid },
            returns: { id: String, name: String },
          },
        },
      ];
      const proto = generateProto(routes, defaultOptions);

      expect(proto.messageMeta.GetUsersByIdRequest).toBeDefined();
      expect(proto.messageMeta.GetUsersByIdRequest).toHaveLength(1);
      expect(proto.messageMeta.GetUsersByIdRequest[0].name).toBe('params');
      expect(proto.messageMeta.GetUsersByIdRequest[0].type).toBe('GetUsersByIdRequestParams');
      expect(proto.messageMeta.GetUsersByIdRequestParams[0].name).toBe('id');
      expect(proto.messageMeta.GetUsersByIdRequestParams[0].number).toBe(1);
      expect(proto.messageMeta.GetUsersByIdRequestParams[0].type).toBe('string');

      expect(proto.messageMeta.GetUsersByIdResponse).toBeDefined();
      expect(proto.messageMeta.GetUsersByIdResponse).toHaveLength(2);
    });

    it('should include repeated and optional flags in messageMeta', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/items',
          schemas: {
            body: { name: String, tags: ArrayOf(String), note: Optional(String) },
          },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      const fields = proto.messageMeta.CreateItemsRequestBody;
      expect(fields).toBeDefined();

      const nameField = fields.find((f) => f.name === 'name');
      expect(nameField?.repeated).toBe(false);
      expect(nameField?.optional).toBe(false);

      const tagsField = fields.find((f) => f.name === 'tags');
      expect(tagsField?.repeated).toBe(true);

      const noteField = fields.find((f) => f.name === 'note');
      expect(noteField?.optional).toBe(true);
    });
  });

  describe('enumMeta and serviceMeta', () => {
    it('should populate enumMeta with enum values', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/items',
          schemas: { body: { status: OneOf('active', 'inactive', 'archived') } },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.enumMeta).toBeDefined();
      expect(proto.enumMeta.CreateItemsRequestBodyStatus).toEqual(['active', 'inactive', 'archived']);
    });

    it('should return empty enumMeta when no enums exist', () => {
      const routes: DiscoveredRoute[] = [{ method: 'GET', path: '/users' }];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.enumMeta).toEqual({});
    });

    it('should populate serviceMeta with method details', () => {
      const routes: DiscoveredRoute[] = [
        { method: 'GET', path: '/users' },
        { method: 'POST', path: '/users', schemas: { body: { name: String } } },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.serviceMeta).toBeDefined();
      expect(proto.serviceMeta.UsersService).toHaveLength(2);

      const listRpc = proto.serviceMeta.UsersService.find((m) => m.name === 'ListUsers');
      expect(listRpc).toBeDefined();
      expect(listRpc?.requestMessage).toBe('ListUsersRequest');
      expect(listRpc?.responseMessage).toBe('ListUsersResponse');
      expect(listRpc?.clientStreaming).toBe(false);
      expect(listRpc?.serverStreaming).toBe(false);
    });

    it('should include streaming flags in serviceMeta', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'GET',
          path: '/events',
          streamMode: 'server',
          schemas: { returns: { type: String } },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      const rpc = proto.serviceMeta.EventsService?.[0];
      expect(rpc?.serverStreaming).toBe(true);
      expect(rpc?.clientStreaming).toBe(false);
    });
  });

  describe('map fields', () => {
    it('should generate map<string, string> for MapOf(String, String)', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/items',
          schemas: {
            body: { labels: MapOf(String, String) },
          },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('map<string, string> labels = 1;');
    });

    it('should generate map<string, double> for MapOf(String, Number)', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/items',
          schemas: {
            body: { prices: MapOf(String, Number) },
          },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('map<string, double> prices = 1;');
    });

    it('generates map<string, int64> for MapOf(String, Int) (D0.2)', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/items',
          schemas: {
            body: { counts: MapOf(String, Int) },
          },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('map<string, int64> counts = 1;');
    });

    it('generates map<int64, string> for MapOf(Int, String) (D0.2)', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/items',
          schemas: {
            body: { lookup: MapOf(Int, String) },
          },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('map<int64, string> lookup = 1;');
    });

    it('should generate map<string, bool> for MapOf(String, Boolean)', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/items',
          schemas: {
            body: { flags: MapOf(String, Boolean) },
          },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('map<string, bool> flags = 1;');
    });

    it('should include mapKeyType and mapValueType in messageMeta', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/items',
          schemas: {
            body: { labels: MapOf(String, String), scores: MapOf(String, Number) },
          },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      const fields = proto.messageMeta.CreateItemsRequestBody;

      const labelsField = fields.find((f) => f.name === 'labels');
      expect(labelsField?.mapKeyType).toBe('string');
      expect(labelsField?.mapValueType).toBe('string');

      const scoresField = fields.find((f) => f.name === 'scores');
      expect(scoresField?.mapKeyType).toBe('string');
      expect(scoresField?.mapValueType).toBe('double');
    });

    it('should work alongside other field types', () => {
      const routes: DiscoveredRoute[] = [
        {
          method: 'POST',
          path: '/items',
          schemas: {
            body: { name: String, labels: MapOf(String, String), tags: ArrayOf(String) },
          },
        },
      ];
      const proto = generateProto(routes, defaultOptions);
      expect(proto.content).toContain('string name = 1;');
      expect(proto.content).toContain('map<string, string> labels = 2;');
      expect(proto.content).toContain('repeated string tags = 3;');
    });
  });
});
