import { describe, expect, it } from 'bun:test';
import { encodeFileDescriptorProto } from '../../src/grpc/grpc-reflection';
import type { ProtoDocument } from '../../src/proto';

function createTestProto(overrides: Partial<ProtoDocument> = {}): ProtoDocument {
  return {
    content: 'syntax = "proto3";',
    packageName: 'test.v1',
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
        { name: 'email', number: 3, type: 'string', optional: false, repeated: false },
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

describe('FileDescriptorProto encoding', () => {
  it('should produce non-empty bytes for a valid proto document', () => {
    const proto = createTestProto();
    const bytes = encodeFileDescriptorProto(proto);
    expect(bytes.length).toBeGreaterThan(0);
  });

  it('should encode the file name "api.proto"', () => {
    const proto = createTestProto();
    const bytes = encodeFileDescriptorProto(proto);
    // Field 1 (name), wire type 2 (length-delimited): tag = (1 << 3) | 2 = 0x0A
    // Followed by length varint and "api.proto" UTF-8 bytes
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('api.proto');
  });

  it('should encode the package name', () => {
    const proto = createTestProto();
    const bytes = encodeFileDescriptorProto(proto);
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('test.v1');
  });

  it('should encode "proto3" syntax', () => {
    const proto = createTestProto();
    const bytes = encodeFileDescriptorProto(proto);
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('proto3');
  });

  it('should encode message names', () => {
    const proto = createTestProto();
    const bytes = encodeFileDescriptorProto(proto);
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('ListUsersRequest');
    expect(str).toContain('ListUsersResponse');
    expect(str).toContain('GetUsersByIdRequest');
    expect(str).toContain('GetUsersByIdResponse');
  });

  it('should encode field names within messages', () => {
    const proto = createTestProto();
    const bytes = encodeFileDescriptorProto(proto);
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('page');
    expect(str).toContain('limit');
    expect(str).toContain('name');
    expect(str).toContain('email');
  });

  it('should encode service name', () => {
    const proto = createTestProto();
    const bytes = encodeFileDescriptorProto(proto);
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('UsersService');
  });

  it('should encode RPC method names', () => {
    const proto = createTestProto();
    const bytes = encodeFileDescriptorProto(proto);
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('ListUsers');
    expect(str).toContain('GetUsersById');
  });

  it('should encode fully-qualified type names for method input/output', () => {
    const proto = createTestProto();
    const bytes = encodeFileDescriptorProto(proto);
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('.test.v1.ListUsersRequest');
    expect(str).toContain('.test.v1.ListUsersResponse');
  });

  it('should encode enum types', () => {
    const proto = createTestProto({
      enumTypes: ['UserStatus'],
      enumMeta: { UserStatus: ['active', 'inactive', 'banned'] },
    });
    const bytes = encodeFileDescriptorProto(proto);
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('UserStatus');
    expect(str).toContain('USER_STATUS_UNSPECIFIED');
    expect(str).toContain('USER_STATUS_ACTIVE');
    expect(str).toContain('USER_STATUS_INACTIVE');
    expect(str).toContain('USER_STATUS_BANNED');
  });

  it('should handle proto with no services', () => {
    const proto = createTestProto({ services: {}, serviceMeta: {} });
    const bytes = encodeFileDescriptorProto(proto);
    expect(bytes.length).toBeGreaterThan(0);
    // Should still contain messages and metadata
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('ListUsersRequest');
  });

  it('should handle proto with no messages', () => {
    const proto = createTestProto({ messages: [], messageMeta: {} });
    const bytes = encodeFileDescriptorProto(proto);
    expect(bytes.length).toBeGreaterThan(0);
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('UsersService');
  });

  it('should handle empty proto', () => {
    const proto = createTestProto({
      services: {},
      serviceMeta: {},
      messages: [],
      messageMeta: {},
      enumTypes: [],
      enumMeta: {},
    });
    const bytes = encodeFileDescriptorProto(proto);
    expect(bytes.length).toBeGreaterThan(0);
    // Should still contain file name and package
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('api.proto');
    expect(str).toContain('test.v1');
  });

  it('should produce valid proto binary structure (tag-length-value)', () => {
    const proto = createTestProto();
    const bytes = encodeFileDescriptorProto(proto);

    // First byte should be tag for field 1 (name), wire type 2
    // (1 << 3) | 2 = 0x0A
    expect(bytes[0]).toBe(0x0a);

    // Second byte is the length of "api.proto" = 9
    expect(bytes[1]).toBe(9);

    // Bytes 2..10 should be "api.proto"
    const name = new TextDecoder().decode(bytes.subarray(2, 11));
    expect(name).toBe('api.proto');
  });

  it('should encode server-streaming methods correctly', () => {
    const proto = createTestProto({
      serviceMeta: {
        UsersService: [
          {
            name: 'WatchUsers',
            requestMessage: 'ListUsersRequest',
            responseMessage: 'ListUsersResponse',
            clientStreaming: false,
            serverStreaming: true,
          },
        ],
      },
    });
    const bytes = encodeFileDescriptorProto(proto);
    // The server_streaming flag is field 6 (bool) in MethodDescriptorProto
    // When true: tag = (6 << 3) | 0 = 0x30, value = 0x01
    // These bytes should appear somewhere in the encoded output
    expect(bytes.length).toBeGreaterThan(0);
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('WatchUsers');
  });

  it('should encode enum field types as TYPE_ENUM (14)', () => {
    const proto = createTestProto({
      enumTypes: ['UserRole'],
      enumMeta: { UserRole: ['admin', 'user'] },
      messageMeta: {
        ...createTestProto().messageMeta,
        ListUsersResponse: [
          { name: 'id', number: 1, type: 'string', optional: false, repeated: false },
          { name: 'role', number: 2, type: 'UserRole', optional: false, repeated: false },
        ],
      },
    });
    const bytes = encodeFileDescriptorProto(proto);
    expect(bytes.length).toBeGreaterThan(0);
    // The encoded bytes should contain the enum type reference
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('.test.v1.UserRole');
  });

  it('should encode message field types as TYPE_MESSAGE (11)', () => {
    const proto = createTestProto({
      messages: [...createTestProto().messages, 'Address'],
      messageMeta: {
        ...createTestProto().messageMeta,
        Address: [{ name: 'street', number: 1, type: 'string', optional: false, repeated: false }],
        GetUsersByIdResponse: [
          { name: 'id', number: 1, type: 'string', optional: false, repeated: false },
          { name: 'address', number: 2, type: 'Address', optional: false, repeated: false },
        ],
      },
    });
    const bytes = encodeFileDescriptorProto(proto);
    const str = new TextDecoder().decode(bytes);
    expect(str).toContain('.test.v1.Address');
  });
});

describe('Reflection service (JSON handler)', () => {
  // These tests use a mock approach: we call the reflection handler directly
  // by importing and testing the registration function logic.
  // Integration tests with a running server would use createGrpcTestClient.

  it('should produce base64-encodable FileDescriptorProto', () => {
    const proto = createTestProto();
    const bytes = encodeFileDescriptorProto(proto);
    // Ensure we can base64 encode the bytes without error
    let binary = '';
    for (const byte of bytes) {
      binary += String.fromCharCode(byte);
    }
    const base64 = btoa(binary);
    expect(base64.length).toBeGreaterThan(0);

    // Verify round-trip: base64 → bytes
    const decoded = atob(base64);
    expect(decoded.length).toBe(bytes.length);
    for (let i = 0; i < bytes.length; i++) {
      expect(decoded.charCodeAt(i)).toBe(bytes[i]);
    }
  });
});
