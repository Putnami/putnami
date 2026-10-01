import { describe, expect, test } from 'bun:test';
import type { ProtoDocument } from '@putnami/application';
import { readProtoSpec } from '../../src/generator/proto-reader';

function makeProtoDoc(overrides?: Partial<ProtoDocument>): ProtoDocument {
  return {
    content: '',
    packageName: 'myapp.v1',
    services: { UsersService: ['ListUsers', 'GetUsersById'] },
    messages: ['ListUsersRequest', 'ListUsersResponse', 'GetUsersByIdRequest', 'GetUsersByIdResponse'],
    messageMeta: {
      ListUsersRequest: [
        { name: 'page', number: 1, type: 'int32', optional: true, repeated: false },
        { name: 'limit', number: 2, type: 'int32', optional: true, repeated: false },
      ],
      ListUsersResponse: [
        { name: 'id', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'name', number: 2, type: 'string', optional: false, repeated: false },
      ],
      GetUsersByIdRequest: [{ name: 'id', number: 1, type: 'string', optional: false, repeated: false }],
      GetUsersByIdResponse: [
        { name: 'id', number: 1, type: 'string', optional: false, repeated: false },
        { name: 'name', number: 2, type: 'string', optional: false, repeated: false },
        { name: 'email', number: 3, type: 'string', optional: true, repeated: false },
      ],
    },
    enumTypes: [],
    enumMeta: {},
    serviceMeta: {
      UsersService: [
        {
          name: 'ListUsers',
          requestMessage: 'ListUsersRequest',
          responseMessage: 'ListUsersResponse',
          clientStreaming: false,
          serverStreaming: false,
        },
        {
          name: 'GetUsersById',
          requestMessage: 'GetUsersByIdRequest',
          responseMessage: 'GetUsersByIdResponse',
          clientStreaming: false,
          serverStreaming: false,
        },
      ],
    },
    ...overrides,
  };
}

describe('readProtoSpec', () => {
  test('reads services and methods', () => {
    const doc = makeProtoDoc();
    const spec = readProtoSpec(doc);

    expect(spec.transport).toBe('connect');
    expect(spec.packageName).toBe('myapp.v1');
    expect(spec.services).toHaveLength(1);

    const service = spec.services[0];
    expect(service.name).toBe('UsersService');
    expect(service.className).toBe('UsersClient');
    expect(service.methods).toHaveLength(2);
  });

  test('maps RPC methods to MethodIR', () => {
    const doc = makeProtoDoc();
    const spec = readProtoSpec(doc);
    const method = spec.services[0].methods[0];

    expect(method.name).toBe('listUsers');
    expect(method.operationId).toBe('ListUsers');
    expect(method.httpMethod).toBe('POST');
    expect(method.path).toBe('/myapp.v1.UsersService/ListUsers');
  });

  test('parses request message fields as body', () => {
    const doc = makeProtoDoc();
    const spec = readProtoSpec(doc);
    const method = spec.services[0].methods[0]; // ListUsers

    expect(method.body).toHaveLength(2);
    expect(method.body?.[0]).toEqual({ name: 'page', tsType: 'number', optional: true, array: false });
    expect(method.body?.[1]).toEqual({ name: 'limit', tsType: 'number', optional: true, array: false });
  });

  test('parses response message fields', () => {
    const doc = makeProtoDoc();
    const spec = readProtoSpec(doc);
    const method = spec.services[0].methods[1]; // GetUsersById

    expect(method.response).toHaveLength(3);
    expect(method.response?.[0]).toEqual({ name: 'id', tsType: 'string', optional: false, array: false });
    expect(method.response?.[1]).toEqual({ name: 'name', tsType: 'string', optional: false, array: false });
    expect(method.response?.[2]).toEqual({ name: 'email', tsType: 'string', optional: true, array: false });
  });

  test('maps proto scalar types to TypeScript', () => {
    const doc = makeProtoDoc({
      messages: ['TestRequest', 'TestResponse'],
      messageMeta: {
        ...makeProtoDoc().messageMeta,
        TestRequest: [
          { name: 'str', number: 1, type: 'string', optional: false, repeated: false },
          { name: 'num', number: 2, type: 'double', optional: false, repeated: false },
          { name: 'int', number: 3, type: 'int32', optional: false, repeated: false },
          { name: 'flag', number: 4, type: 'bool', optional: false, repeated: false },
        ],
        TestResponse: [],
      },
      serviceMeta: {
        ...makeProtoDoc().serviceMeta,
        TestService: [
          {
            name: 'Test',
            requestMessage: 'TestRequest',
            responseMessage: 'TestResponse',
            clientStreaming: false,
            serverStreaming: false,
          },
        ],
      },
    });

    const spec = readProtoSpec(doc);
    const testService = spec.services.find((s) => s.name === 'TestService');
    if (!testService) throw new Error('missing TestService');
    const method = testService.methods[0];

    expect(method.body?.[0].tsType).toBe('string');
    expect(method.body?.[1].tsType).toBe('number');
    expect(method.body?.[2].tsType).toBe('number');
    expect(method.body?.[3].tsType).toBe('boolean');
  });

  test('detects streaming methods', () => {
    const doc = makeProtoDoc({
      serviceMeta: {
        EventsService: [
          {
            name: 'Watch',
            requestMessage: 'WatchRequest',
            responseMessage: 'WatchResponse',
            clientStreaming: false,
            serverStreaming: true,
          },
          {
            name: 'Chat',
            requestMessage: 'ChatRequest',
            responseMessage: 'ChatResponse',
            clientStreaming: true,
            serverStreaming: true,
          },
        ],
      },
      messageMeta: {
        ...makeProtoDoc().messageMeta,
        WatchRequest: [],
        WatchResponse: [],
        ChatRequest: [],
        ChatResponse: [],
      },
    });

    const spec = readProtoSpec(doc);
    const service = spec.services.find((s) => s.name === 'EventsService');
    if (!service) throw new Error('missing EventsService');

    expect(service.methods[0].streaming).toBe('server');
    expect(service.methods[1].streaming).toBe('bidirectional');
  });

  test('converts snake_case fields to camelCase', () => {
    const doc = makeProtoDoc({
      messageMeta: {
        ...makeProtoDoc().messageMeta,
        ListUsersRequest: [
          { name: 'page_size', number: 1, type: 'int32', optional: true, repeated: false },
          { name: 'next_token', number: 2, type: 'string', optional: true, repeated: false },
        ],
      },
    });

    const spec = readProtoSpec(doc);
    const method = spec.services[0].methods[0];

    expect(method.body?.[0].name).toBe('pageSize');
    expect(method.body?.[1].name).toBe('nextToken');
  });

  test('handles repeated fields as arrays', () => {
    const doc = makeProtoDoc({
      messageMeta: {
        ...makeProtoDoc().messageMeta,
        ListUsersResponse: [{ name: 'ids', number: 1, type: 'string', optional: false, repeated: true }],
      },
    });

    const spec = readProtoSpec(doc);
    const method = spec.services[0].methods[0];

    expect(method.response?.[0].array).toBe(true);
    expect(method.response?.[0].tsType).toBe('string');
  });

  test('maps client-streaming methods and non-scalar proto fields', () => {
    const doc = makeProtoDoc({
      serviceMeta: {
        UploadService: [
          {
            name: 'UploadChunk',
            requestMessage: 'UploadChunkRequest',
            responseMessage: 'UploadChunkResponse',
            clientStreaming: true,
            serverStreaming: false,
          },
        ],
      },
      messageMeta: {
        ...makeProtoDoc().messageMeta,
        UploadChunkRequest: [
          { name: 'payload', number: 1, type: 'bytes', optional: false, repeated: false },
          { name: 'metadata', number: 2, type: 'UploadMetadata', optional: true, repeated: false },
        ],
        UploadChunkResponse: [],
      },
    });

    const spec = readProtoSpec(doc);
    const method = spec.services[0].methods[0];

    expect(method.streaming).toBe('client');
    expect(method.body?.[0].tsType).toBe('Uint8Array');
    expect(method.body?.[1].tsType).toBe('Record<string, unknown>');
  });
});
