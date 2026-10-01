import type { ProtoDocument, RpcMethodMeta } from '@putnami/application';
import { CLIENT_IR_VERSION, type FieldIR, type MethodIR, type ServiceIR, type SpecIR } from './ir.type';
import { computeSpecHash, snakeToCamel, toCamelCase } from './string-utils';

/**
 * Converts a ProtoDocument into the intermediate representation.
 *
 * Each proto service becomes a ServiceIR, each RPC method becomes a MethodIR.
 * Field types are mapped from proto scalar types to TypeScript types.
 */
export function readProtoSpec(doc: ProtoDocument): SpecIR {
  // Build RPC→service lookup map once (O(n) instead of O(n*m) per lookup)
  const rpcToService = new Map<string, string>();
  for (const [serviceName, methods] of Object.entries(doc.serviceMeta)) {
    for (const m of methods) {
      rpcToService.set(m.name, serviceName);
    }
  }

  const services: ServiceIR[] = [];

  for (const [serviceName, methods] of Object.entries(doc.serviceMeta)) {
    const methodIRs: MethodIR[] = methods.map((rpc) => rpcToMethod(rpc, doc, rpcToService));

    services.push({
      name: serviceName,
      className: serviceName.replace(/Service$/, 'Client'),
      methods: methodIRs,
    });
  }

  // Compute spec hash for drift detection
  const specHash = computeSpecHash(doc.content);

  return {
    irVersion: CLIENT_IR_VERSION,
    transport: 'connect',
    packageName: doc.packageName,
    services,
    specHash,
    protoMeta: {
      messageMeta: doc.messageMeta,
      enumTypes: doc.enumTypes ?? [],
    },
  };
}

/**
 * Convert a single RPC method to a MethodIR.
 */
function rpcToMethod(rpc: RpcMethodMeta, doc: ProtoDocument, rpcToService: Map<string, string>): MethodIR {
  const connectPath = `/${doc.packageName}.${rpcToService.get(rpc.name) ?? 'UnknownService'}/${rpc.name}`;

  const method: MethodIR = {
    name: toCamelCase(rpc.name),
    operationId: rpc.name,
    httpMethod: 'POST',
    path: connectPath,
  };

  // Parse request message fields into body (Connect sends everything as body)
  const requestFields = doc.messageMeta[rpc.requestMessage];
  if (requestFields && requestFields.length > 0) {
    method.body = requestFields.map(protoFieldToFieldIR);
  }

  // Parse response message fields
  const responseFields = doc.messageMeta[rpc.responseMessage];
  if (responseFields && responseFields.length > 0) {
    method.response = responseFields.map(protoFieldToFieldIR);
  }

  // Streaming
  if (rpc.clientStreaming && rpc.serverStreaming) {
    method.streaming = 'bidirectional';
  } else if (rpc.serverStreaming) {
    method.streaming = 'server';
  } else if (rpc.clientStreaming) {
    method.streaming = 'client';
  }

  return method;
}

/**
 * Convert a proto field metadata to a FieldIR.
 */
function protoFieldToFieldIR(field: { name: string; type: string; optional: boolean; repeated: boolean }): FieldIR {
  return {
    name: toCamelCase(snakeToCamel(field.name)),
    tsType: protoTypeToTs(field.type),
    optional: field.optional,
    array: field.repeated,
  };
}

/**
 * Map proto scalar types to TypeScript types.
 */
function protoTypeToTs(protoType: string): string {
  switch (protoType) {
    case 'string':
      return 'string';
    case 'double':
    case 'float':
    case 'int32':
    case 'int64':
    case 'uint32':
    case 'uint64':
    case 'sint32':
    case 'sint64':
    case 'fixed32':
    case 'fixed64':
    case 'sfixed32':
    case 'sfixed64':
      return 'number';
    case 'bool':
      return 'boolean';
    case 'bytes':
      return 'Uint8Array';
    default:
      // Could be a message type or enum — treat as Record for now
      return 'Record<string, unknown>';
  }
}
