import type { HttpRequestContext } from '../http/http-context.type';
import { HttpResponse } from '../http/http-response';
import type { HttpPlugin } from '../http/http.plugin';
import type { ProtoDocument, ProtoFieldMeta, RpcMethodMeta } from '../proto';
import { encodeVarint } from './proto-codec';

// ---------------------------------------------------------------------------
// gRPC Server Reflection (v1 and v1alpha)
//
// Implements the ServerReflection service as a Connect JSON endpoint.
// Supports:
//   - list_services   → discover all available gRPC services
//   - file_by_filename → get the FileDescriptorProto for a proto file
//   - file_containing_symbol → get the FileDescriptorProto for a symbol
//
// The FileDescriptorProto is encoded as binary protobuf and base64-encoded
// in the JSON response, matching the standard gRPC reflection wire format.
//
// @see https://grpc.io/docs/guides/reflection/
// ---------------------------------------------------------------------------

const REFLECTION_V1_PATH = '/grpc.reflection.v1.ServerReflection/ServerReflectionInfo';
const REFLECTION_V1ALPHA_PATH = '/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo';

/**
 * Register the gRPC Server Reflection service on the HTTP server.
 *
 * Registers both v1 and v1alpha endpoints for maximum tool compatibility.
 * The reflection service responds with Connect JSON (no binary proto for
 * reflection requests — tools use JSON by default for reflection).
 */
export function registerReflection(httpPlugin: HttpPlugin, proto: ProtoDocument, acceptContentTypes: string[]): void {
  // Pre-compute the FileDescriptorProto once at startup
  const fdProtoBytes = encodeFileDescriptorProto(proto);
  const fdProtoBase64 = uint8ToBase64(fdProtoBytes);

  // Collect all fully-qualified service names
  const fqServiceNames = Object.keys(proto.services).map((svc) => `${proto.packageName}.${svc}`);

  // Collect all known symbols (services + messages + enums)
  const allSymbols = new Set<string>([
    ...fqServiceNames,
    ...proto.messages.map((m) => `${proto.packageName}.${m}`),
    ...proto.enumTypes.map((e) => `${proto.packageName}.${e}`),
  ]);

  const handler = async (ctx: HttpRequestContext): Promise<HttpResponse> => {
    let body: Record<string, unknown>;
    try {
      body = (await ctx.body()) as Record<string, unknown>;
      if (!body || typeof body !== 'object') body = {};
    } catch {
      body = {};
    }

    // Determine which request type was sent (Connect JSON uses camelCase)
    if ('listServices' in body || 'list_services' in body) {
      return HttpResponse.json({
        listServicesResponse: {
          service: fqServiceNames.map((name) => ({ name })),
        },
      });
    }

    if ('fileContainingSymbol' in body || 'file_containing_symbol' in body) {
      const symbol = (body['fileContainingSymbol'] ?? body['file_containing_symbol']) as string;
      if (allSymbols.has(symbol)) {
        return HttpResponse.json({
          fileDescriptorResponse: {
            fileDescriptorProto: [fdProtoBase64],
          },
        });
      }
      return HttpResponse.json({
        errorResponse: {
          errorCode: 5, // NOT_FOUND
          errorMessage: `Symbol not found: ${symbol}`,
        },
      });
    }

    if ('fileByFilename' in body || 'file_by_filename' in body) {
      const filename = (body['fileByFilename'] ?? body['file_by_filename']) as string;
      if (filename === 'api.proto') {
        return HttpResponse.json({
          fileDescriptorResponse: {
            fileDescriptorProto: [fdProtoBase64],
          },
        });
      }
      return HttpResponse.json({
        errorResponse: {
          errorCode: 5, // NOT_FOUND
          errorMessage: `File not found: ${filename}`,
        },
      });
    }

    return HttpResponse.json({
      errorResponse: {
        errorCode: 12, // UNIMPLEMENTED
        errorMessage: 'Unsupported reflection request type',
      },
    });
  };

  httpPlugin.route('POST', REFLECTION_V1_PATH, handler, { accept: acceptContentTypes });
  httpPlugin.route('POST', REFLECTION_V1ALPHA_PATH, handler, { accept: acceptContentTypes });
}

// ---------------------------------------------------------------------------
// FileDescriptorProto binary encoder
//
// Encodes a ProtoDocument into the google.protobuf.FileDescriptorProto
// binary format. This is the self-describing proto format used by reflection
// services and proto tooling.
//
// Field numbers reference:
//   FileDescriptorProto:     1=name, 2=package, 4=message_type, 5=enum_type, 6=service, 12=syntax
//   DescriptorProto:         1=name, 2=field
//   FieldDescriptorProto:    1=name, 3=number, 4=label, 5=type, 6=type_name
//   EnumDescriptorProto:     1=name, 2=value
//   EnumValueDescriptorProto: 1=name, 2=number
//   ServiceDescriptorProto:  1=name, 2=method
//   MethodDescriptorProto:   1=name, 2=input_type, 3=output_type, 5=client_streaming, 6=server_streaming
// ---------------------------------------------------------------------------

const textEncoder = new TextEncoder();

/** @internal Exported for testing */
export function encodeFileDescriptorProto(proto: ProtoDocument): Uint8Array {
  const parts: Uint8Array[] = [];

  // field 1: name = "api.proto"
  writeString(parts, 1, 'api.proto');
  // field 2: package
  writeString(parts, 2, proto.packageName);

  // field 4: message_type (repeated DescriptorProto)
  for (const msgName of proto.messages) {
    const fields = proto.messageMeta[msgName] ?? [];
    const msgParts = encodeDescriptorProto(msgName, fields, proto);
    writeBytes(parts, 4, msgParts);
  }

  // field 5: enum_type (repeated EnumDescriptorProto)
  for (const enumName of proto.enumTypes) {
    const values = proto.enumMeta[enumName] ?? [];
    const enumParts = encodeEnumDescriptorProto(enumName, values);
    writeBytes(parts, 5, enumParts);
  }

  // field 6: service (repeated ServiceDescriptorProto)
  for (const [svcName, methods] of Object.entries(proto.serviceMeta)) {
    const svcParts = encodeServiceDescriptorProto(svcName, methods, proto.packageName);
    writeBytes(parts, 6, svcParts);
  }

  // field 12: syntax = "proto3"
  writeString(parts, 12, 'proto3');

  return concatParts(parts);
}

function encodeDescriptorProto(name: string, fields: ProtoFieldMeta[], proto: ProtoDocument): Uint8Array {
  const parts: Uint8Array[] = [];
  const enumSet = new Set(proto.enumTypes);

  // field 1: name
  writeString(parts, 1, name);

  // field 2: field (repeated FieldDescriptorProto)
  for (const f of fields) {
    const fieldParts = encodeFieldDescriptorProto(f, proto.packageName, enumSet, proto.messages);
    writeBytes(parts, 2, fieldParts);
  }

  return concatParts(parts);
}

function encodeFieldDescriptorProto(
  field: ProtoFieldMeta,
  packageName: string,
  enumSet: Set<string>,
  messageNames: string[],
): Uint8Array {
  const parts: Uint8Array[] = [];

  // field 1: name
  writeString(parts, 1, field.name);
  // field 3: number
  writeVarint(parts, 3, field.number);

  // field 4: label (1=optional, 3=repeated)
  writeVarint(parts, 4, field.repeated ? 3 : 1);

  // field 5: type (FieldDescriptorProto.Type enum)
  const fdType = fieldDescriptorType(field.type, enumSet, messageNames);
  writeVarint(parts, 5, fdType);

  // field 6: type_name (for message and enum types — fully qualified)
  if (fdType === 11 || fdType === 14) {
    writeString(parts, 6, `.${packageName}.${field.type}`);
  }

  return concatParts(parts);
}

function encodeEnumDescriptorProto(name: string, values: string[]): Uint8Array {
  const parts: Uint8Array[] = [];

  // field 1: name
  writeString(parts, 1, name);

  // field 2: value — first is UNSPECIFIED=0 (proto3 convention)
  const prefix = toScreamingSnake(name);
  const unspecifiedParts: Uint8Array[] = [];
  writeString(unspecifiedParts, 1, `${prefix}_UNSPECIFIED`);
  writeVarint(unspecifiedParts, 2, 0);
  writeBytes(parts, 2, concatParts(unspecifiedParts));

  for (let i = 0; i < values.length; i++) {
    const valueParts: Uint8Array[] = [];
    writeString(valueParts, 1, `${prefix}_${toScreamingSnake(values[i])}`);
    writeVarint(valueParts, 2, i + 1);
    writeBytes(parts, 2, concatParts(valueParts));
  }

  return concatParts(parts);
}

function encodeServiceDescriptorProto(name: string, methods: RpcMethodMeta[], packageName: string): Uint8Array {
  const parts: Uint8Array[] = [];

  // field 1: name
  writeString(parts, 1, name);

  // field 2: method (repeated MethodDescriptorProto)
  for (const m of methods) {
    const methodParts: Uint8Array[] = [];
    writeString(methodParts, 1, m.name);
    writeString(methodParts, 2, `.${packageName}.${m.requestMessage}`);
    writeString(methodParts, 3, `.${packageName}.${m.responseMessage}`);
    if (m.clientStreaming) writeBool(methodParts, 5, true);
    if (m.serverStreaming) writeBool(methodParts, 6, true);
    writeBytes(parts, 2, concatParts(methodParts));
  }

  return concatParts(parts);
}

// ---------------------------------------------------------------------------
// FieldDescriptorProto.Type mapping
// ---------------------------------------------------------------------------

/** Map our proto type names to FieldDescriptorProto.Type enum values. */
function fieldDescriptorType(type: string, enumSet: Set<string>, messageNames: string[]): number {
  switch (type) {
    case 'double':
      return 1; // TYPE_DOUBLE
    case 'float':
      return 2; // TYPE_FLOAT
    case 'int64':
      return 3; // TYPE_INT64
    case 'uint64':
      return 4; // TYPE_UINT64
    case 'int32':
      return 5; // TYPE_INT32
    case 'fixed64':
      return 6; // TYPE_FIXED64
    case 'fixed32':
      return 7; // TYPE_FIXED32
    case 'bool':
      return 8; // TYPE_BOOL
    case 'string':
      return 9; // TYPE_STRING
    case 'bytes':
      return 12; // TYPE_BYTES
    case 'uint32':
      return 13; // TYPE_UINT32
    case 'sfixed32':
      return 15; // TYPE_SFIXED32
    case 'sfixed64':
      return 16; // TYPE_SFIXED64
    case 'sint32':
      return 17; // TYPE_SINT32
    case 'sint64':
      return 18; // TYPE_SINT64
    default:
      if (enumSet.has(type)) return 14; // TYPE_ENUM
      if (messageNames.includes(type)) return 11; // TYPE_MESSAGE
      return 9; // fallback: TYPE_STRING
  }
}

// ---------------------------------------------------------------------------
// Low-level proto binary writing helpers
// ---------------------------------------------------------------------------

function writeString(parts: Uint8Array[], fieldNumber: number, value: string): void {
  const bytes = textEncoder.encode(value);
  parts.push(encodeVarint((fieldNumber << 3) | 2));
  parts.push(encodeVarint(bytes.length));
  parts.push(bytes);
}

function writeVarint(parts: Uint8Array[], fieldNumber: number, value: number): void {
  parts.push(encodeVarint((fieldNumber << 3) | 0));
  parts.push(encodeVarint(value));
}

function writeBool(parts: Uint8Array[], fieldNumber: number, value: boolean): void {
  parts.push(encodeVarint((fieldNumber << 3) | 0));
  parts.push(encodeVarint(value ? 1 : 0));
}

function writeBytes(parts: Uint8Array[], fieldNumber: number, value: Uint8Array): void {
  parts.push(encodeVarint((fieldNumber << 3) | 2));
  parts.push(encodeVarint(value.length));
  parts.push(value);
}

function concatParts(buffers: Uint8Array[]): Uint8Array {
  if (buffers.length === 0) return new Uint8Array(0);
  if (buffers.length === 1) return buffers[0];
  let total = 0;
  for (const b of buffers) total += b.length;
  const result = new Uint8Array(total);
  let offset = 0;
  for (const b of buffers) {
    result.set(b, offset);
    offset += b.length;
  }
  return result;
}

function uint8ToBase64(bytes: Uint8Array): string {
  let binary = '';
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary);
}

function toScreamingSnake(str: string): string {
  return str
    .replace(/([a-z])([A-Z])/g, '$1_$2')
    .replace(/[-\s]+/g, '_')
    .toUpperCase();
}
