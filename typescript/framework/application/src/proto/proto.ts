import { isNestedSchema } from '@putnami/runtime';
import type { DiscoveredRoute } from '../api';
import { isExternalClientPolicy } from '../api/client-contract';
import { pascalCase, renderProto } from './proto-render';
import {
  type MessageField,
  type ProtoOptions,
  GenerationContext,
  registerNestedMessage,
  schemaFieldToProto,
} from './proto-types';

// Re-export types from split modules so consumers don't need to know about the split
export type { MessageField, ProtoOptions, RpcEntry } from './proto-types';
export { GenerationContext } from './proto-types';
export { pascalCase, protoJsonName, toScreamingSnakeCase, toSnakeCase } from './proto-render';

/** Field metadata for binary protobuf encoding/decoding. */
export interface ProtoFieldMeta {
  /** Field name in snake_case (matches proto field names) */
  name: string;
  /**
   * The `json_name` protobuf derives from {@link name}, and the key a Connect
   * JSON payload uses. Emitted by the proto emitter so no consumer re-derives
   * it. Optional only so a hand-written descriptor stays valid; the emitter
   * always sets it.
   */
  jsonName?: string;
  /** Proto field number */
  number: number;
  /** Wire type: 'string' | 'double' | 'bool' (or a message name for nested types) */
  type: string;
  /**
   * Explicit presence. A field with explicit presence keeps its zero value on
   * the wire and reads back as absent when the peer omitted it; an
   * implicit-presence field omits its zero value and reads back as the zero
   * value.
   */
  optional: boolean;
  /** Whether this field is repeated */
  repeated: boolean;
  /** The `oneof` this field belongs to, when the message declares one. */
  oneof?: string;
  /** Map key type (only for map fields) — e.g. 'string', 'int64', 'bool' */
  mapKeyType?: string;
  /** Map value type (only for map fields) — e.g. 'string', 'double', or a message name */
  mapValueType?: string;
}

/** Detailed metadata for an RPC method (used by reflection and testing). */
export interface RpcMethodMeta {
  name: string;
  requestMessage: string;
  responseMessage: string;
  clientStreaming: boolean;
  serverStreaming: boolean;
}

export interface ProtoDocument {
  /** The proto3 syntax source text */
  content: string;
  /** Package name used in the proto */
  packageName: string;
  /** Map of service name → RPC method names */
  services: Record<string, string[]>;
  /** List of top-level message names */
  messages: string[];
  /** Map of message name → field metadata (for binary protobuf encoding) */
  messageMeta: Record<string, ProtoFieldMeta[]>;
  /** Set of enum type names (used by codec to select varint wire type) */
  enumTypes: string[];
  /** Map of enum name → enum values (for reflection descriptor encoding) */
  enumMeta: Record<string, string[]>;
  /** Map of service name → method details (for reflection descriptor encoding) */
  serviceMeta: Record<string, RpcMethodMeta[]>;
}

// ---------------------------------------------------------------------------
// Generator
// ---------------------------------------------------------------------------

/**
 * Generate a proto3 definition from discovered routes.
 *
 * Traverses route definitions and their schemas to produce a complete
 * `.proto` file — no extra annotations required from the user.
 *
 * Routes are grouped into services by their first path segment.
 * HTTP methods map to RPC names (e.g. GET /users → ListUsers, POST /users → CreateUser).
 * Stream endpoints map to gRPC streaming RPCs.
 */
export function generateProto(routes: DiscoveredRoute[], options: ProtoOptions): ProtoDocument {
  const ctx = new GenerationContext(options);

  // Group routes into services. A route an external authority owns speaks the
  // standard's wire format, not a proto message: it has no RPC, so neither the
  // descriptor nor the gRPC plugin offers it.
  // Route discovery walks the filesystem, whose iteration order differs between
  // macOS and Linux. Sort a copy by path, then method, so the committed
  // schema/api.proto is byte-identical on every machine.
  const orderedRoutes = routes
    .filter((route) => !isExternalClientPolicy(route.meta?.client))
    .sort((a, b) => compareCodeUnits(a.path, b.path) || compareCodeUnits(a.method, b.method));
  const serviceMap = groupRoutesIntoServices(orderedRoutes);

  // Generate messages and service RPCs
  for (const [serviceName, serviceRoutes] of serviceMap) {
    const rpcs: {
      name: string;
      requestMessage: string;
      responseMessage: string;
      clientStreaming: boolean;
      serverStreaming: boolean;
    }[] = [];

    for (const route of serviceRoutes) {
      const rpcName = protoRpcName(route.method, route.path);
      const requestMsg = buildRequestMessage(ctx, rpcName, route);
      const responseMsg = buildResponseMessage(ctx, rpcName, route);
      rpcs.push({
        name: rpcName,
        requestMessage: requestMsg,
        responseMessage: responseMsg,
        clientStreaming: route.streamMode === 'client' || route.streamMode === 'bidirectional',
        serverStreaming: route.streamMode === 'server' || route.streamMode === 'bidirectional',
      });
    }

    ctx.services.set(serviceName, rpcs);
  }

  return {
    content: renderProto(ctx),
    packageName: options.packageName,
    services: Object.fromEntries([...ctx.services.entries()].map(([name, rpcs]) => [name, rpcs.map((r) => r.name)])),
    messages: [...ctx.messages.keys()],
    messageMeta: Object.fromEntries(
      [...ctx.messages.entries()].map(([name, fields]) => [
        name,
        fields.map((f) => {
          const meta: ProtoFieldMeta = {
            name: f.name,
            jsonName: f.jsonName,
            number: f.number,
            type: f.type,
            optional: f.optional,
            repeated: f.repeated,
          };
          if (f.oneof) meta.oneof = f.oneof;
          if (f.mapKeyType) meta.mapKeyType = f.mapKeyType;
          if (f.mapValueType) meta.mapValueType = f.mapValueType;
          return meta;
        }),
      ]),
    ),
    enumTypes: [...ctx.enums.keys()],
    enumMeta: Object.fromEntries([...ctx.enums.entries()]),
    serviceMeta: Object.fromEntries(
      [...ctx.services.entries()].map(([name, rpcs]) => [
        name,
        rpcs.map((r) => ({
          name: r.name,
          requestMessage: r.requestMessage,
          responseMessage: r.responseMessage,
          clientStreaming: r.clientStreaming,
          serverStreaming: r.serverStreaming,
        })),
      ]),
    ),
  };
}

function compareCodeUnits(a: string, b: string): number {
  if (a === b) return 0;
  return a < b ? -1 : 1;
}

// ---------------------------------------------------------------------------
// Route → Service grouping
// ---------------------------------------------------------------------------

/**
 * Group routes by their first non-parameter path segment into services.
 * e.g. /users/[id] → UsersService, /orders/[id]/items → OrdersService
 */
function groupRoutesIntoServices(routes: DiscoveredRoute[]): Map<string, DiscoveredRoute[]> {
  const groups = new Map<string, DiscoveredRoute[]>();

  for (const route of routes) {
    const serviceName = protoServiceName(route.path);

    const existing = groups.get(serviceName) ?? [];
    existing.push(route);
    groups.set(serviceName, existing);
  }

  return groups;
}

/** Resolve the exact service name used by the Proto emitter for a route. */
export function protoServiceName(path: string): string {
  const segments = path.split('/').filter(Boolean);
  const firstSegment = segments.find((segment) => !segment.startsWith('[') && !segment.startsWith('{')) ?? 'api';
  return `${pascalCase(firstSegment)}Service`;
}

// ---------------------------------------------------------------------------
// RPC naming
// ---------------------------------------------------------------------------

/**
 * Build an RPC method name from the HTTP method and path.
 * e.g. GET /users → ListUsers, POST /users → CreateUsers
 *      GET /users/[id] → GetUsersById, DELETE /users/[id] → DeleteUsersById
 */
export function protoRpcName(method: string, path: string): string {
  const segments = path.split('/').filter(Boolean);
  const parts: string[] = [];

  for (const segment of segments) {
    const paramMatch = segment.match(/^(?:\[|\{)(.+?)(?:]|})$/);
    if (paramMatch) {
      parts.push(`By${pascalCase(paramMatch[1])}`);
    } else {
      parts.push(pascalCase(segment));
    }
  }

  const prefix = rpcMethodPrefix(method, segments);
  return `${prefix}${parts.join('')}`;
}

/**
 * Map HTTP method to a gRPC-style verb prefix.
 * Uses heuristics: GET on collection paths → List, GET with params → Get, etc.
 */
function rpcMethodPrefix(method: string, segments: string[]): string {
  const hasParamSuffix = segments.length > 0 && segments[segments.length - 1].startsWith('[');

  switch (method.toUpperCase()) {
    case 'GET':
      return hasParamSuffix ? 'Get' : 'List';
    case 'POST':
      return 'Create';
    case 'PUT':
      return 'Update';
    case 'PATCH':
      return 'Patch';
    case 'DELETE':
      return 'Delete';
    default:
      return pascalCase(method);
  }
}

// ---------------------------------------------------------------------------
// Message generation
// ---------------------------------------------------------------------------

/**
 * Build the request message: the `{params, query, body}` envelope, one nested
 * message per section the route declares, numbered in that order.
 *
 * The sections keep a path parameter, a query parameter and a body member of
 * the same name apart, and they are the shape the Go provider publishes and the
 * Go client sends (go/framework/grpc ADR 0003), so either provider answers
 * either generated client on the same wire.
 */
function buildRequestMessage(ctx: GenerationContext, rpcName: string, route: DiscoveredRoute): string {
  const messageName = `${rpcName}Request`;
  const fields: MessageField[] = [];
  const sections = [
    ['params', route.schemas?.params],
    ['query', route.schemas?.query],
    ['body', route.schemas?.body],
  ] as const;
  for (const [section, schema] of sections) {
    if (!schema || Object.keys(schema).length === 0) continue;
    const sectionMessage = `${messageName}${pascalCase(section)}`;
    registerNestedMessage(ctx, sectionMessage, schema);
    fields.push({
      name: section,
      jsonName: section,
      type: sectionMessage,
      number: fields.length + 1,
      optional: false,
      repeated: false,
    });
  }
  return ctx.addMessage(messageName, fields);
}

/**
 * Build a response message from the returns schema.
 */
function buildResponseMessage(ctx: GenerationContext, rpcName: string, route: DiscoveredRoute): string {
  const messageName = `${rpcName}Response`;

  if (!route.schemas?.returns) {
    // Empty response
    return ctx.addMessage(messageName, []);
  }

  const fields: MessageField[] = [];
  let fieldNumber = 1;

  for (const [key, prop] of Object.entries(route.schemas.returns)) {
    const field = schemaFieldToProto(key, prop, fieldNumber++, ctx, messageName);
    if (isNestedSchema(prop)) {
      const nestedName = `${messageName}${pascalCase(key)}`;
      registerNestedMessage(ctx, nestedName, prop);
      field.type = nestedName;
    }
    fields.push(field);
  }

  return ctx.addMessage(messageName, fields);
}
