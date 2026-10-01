import {
  baseTypeName,
  getHttpStatusText,
  isNestedSchema,
  isSchemaDescriptor,
  type NestedSchema,
  type SchemaConstraint,
  type SchemaDefinition,
  type SchemaDescriptor,
  type SchemaPrimitive,
} from '@putnami/runtime';
import type { DiscoveredRoute, ErrorResponseCode } from '../api';
import { clientOperationId } from '../api/client-operation-id';
import { type BinaryMeta, validateBinaryMeta } from '../api/route/binary';
import type { ProviderWire } from '../api/route/byte-stream';
import {
  errorCodeToStableCode,
  IMPLICIT_BAD_REQUEST_CODE,
  IMPLICIT_INTERNAL_CODE,
  PAYLOAD_TOO_LARGE_CODE,
  UNSUPPORTED_MEDIA_TYPE_CODE,
} from '../api/route/error-codes';
import {
  DEFAULT_INTEGER_WIDTH,
  INTEGER_WIDTH_CONSTRAINT,
  type IntegerWidth,
  NULLABLE_CONSTRAINT,
} from '../api/route/schema-wire';
import {
  getContractTypeReference,
  type ContractField,
  type ContractManifest,
  type ContractTypeReference,
} from '../contracts';
import type { SecurityOptions } from '../security/security.types';
import {
  CLIENT_CONTRACT_PROTOCOL_VERSION,
  EXTERNAL_CONTRACT_KEY,
  externalClientPolicyError,
  type ClientContractDocument,
  type ClientContractOperation,
  type ClientAuthorizationPolicy,
  type ClientDeclaredError,
  type ClientOperationPolicy,
  type ClientProtobufDescriptor,
  type ClientSchema,
  type ClientSecurityPolicy,
  type ClientServiceContract,
  type ClientTransportContract,
} from '../api/client-contract';
import {
  generateProto,
  type ProtoDocument,
  protoRpcName,
  protoServiceName,
  toScreamingSnakeCase,
} from '../proto/proto';

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

export interface OpenApiInfo {
  title: string;
  version: string;
  description?: string;
}

export interface OpenApiOptions {
  info: OpenApiInfo;
  servers?: { url: string; description?: string }[];
  /** First-party service contract sourced from `api({ client })`. */
  client?: ClientServiceContract;
  /** Exact Proto package exposed by the provider's Proto + Connect plugins. */
  connect?: {
    packageName: string;
    unaryEncodings?: readonly ('proto' | 'json')[];
    serverStreamEncodings?: readonly ('proto' | 'json')[];
  };
}

// ---------------------------------------------------------------------------
// OpenAPI 3.x types (subset we produce)
// ---------------------------------------------------------------------------

export interface OpenApiDocument {
  openapi: '3.0.3';
  info: OpenApiInfo;
  servers?: { url: string; description?: string }[];
  paths: Record<string, Record<string, OpenApiOperation>>;
  components?: {
    /** Reusable named schemas — object models reused across operations are emitted here once and referenced via `$ref`. */
    schemas?: Record<string, OpenApiSchema>;
    securitySchemes?: Record<string, OpenApiSecurityScheme>;
  };
  /** Lossless first-party client contract marker. */
  'x-putnami-client'?: ClientContractDocument;
}

interface OpenApiSecurityScheme {
  type: string;
  scheme?: string;
  bearerFormat?: string;
  /** For `apiKey` schemes: where the key travels (`header`/`query`/`cookie`). */
  in?: string;
  /** For `apiKey` schemes: the header/query/cookie name carrying the key. */
  name?: string;
}

type OpenApiSecurityRequirement = Record<string, string[]>;

export interface OpenApiOperation {
  operationId?: string;
  summary?: string;
  description?: string;
  parameters?: OpenApiParameter[];
  requestBody?: OpenApiRequestBody;
  responses: Record<string, OpenApiResponse>;
  security?: OpenApiSecurityRequirement[];
  /** Lossless first-party operation semantics. */
  'x-putnami-client'?: ClientContractOperation;
  /**
   * The external authority that owns this operation's wire contract. An
   * operation of a first-party document carries it instead of
   * `x-putnami-client`, and every first-party reader skips the operation.
   */
  'x-putnami-external-contract'?: string;
}

interface OpenApiParameter {
  name: string;
  in: 'path' | 'query' | 'header';
  required: boolean;
  schema: OpenApiSchema;
}

/**
 * One media representation. `x-putnami-max-bytes` carries the declared byte
 * bound of a raw octet payload: it is the one fact such a representation has
 * that no JSON Schema keyword can express — `format: binary` says the payload
 * is octets, not how many a peer may send.
 */
interface OpenApiMediaType {
  schema: OpenApiSchema;
  'x-putnami-max-bytes'?: number;
  'x-putnami-streamed'?: true;
}

interface OpenApiRequestBody {
  required: boolean;
  content: Record<string, OpenApiMediaType>;
}

interface OpenApiResponse {
  description: string;
  content?: Record<string, OpenApiMediaType>;
}

export interface OpenApiSchema {
  type?: string;
  format?: string;
  description?: string;
  items?: OpenApiSchema;
  properties?: Record<string, OpenApiSchema>;
  required?: string[];
  enum?: string[];
  oneOf?: OpenApiSchema[];
  /**
   * Schemas a value may match several of at once. Only an error envelope's
   * `details` uses it, when codes sharing a status declare different details.
   */
  anyOf?: OpenApiSchema[];
  discriminator?: { propertyName: string; mapping?: Record<string, string> };
  additionalProperties?: boolean | OpenApiSchema;
  nullable?: boolean;
  default?: unknown;
  minimum?: number;
  maximum?: number;
  minLength?: number;
  maxLength?: number;
  pattern?: string;
  /** Reference to a named schema in `components.schemas` (e.g. `#/components/schemas/Model1`). */
  $ref?: string;
}

const standardErrorSchemaMarker = Symbol('putnami.standardErrorSchema');
type StandardErrorSchema = OpenApiSchema & { readonly [standardErrorSchemaMarker]: true };
/** Marks the schema of a `.mayThrowDetails()` body, so shared-schema promotion can name it. */
const declaredDetailsSchemaMarker = Symbol('putnami.declaredDetailsSchema');

// ---------------------------------------------------------------------------
// Generator
// ---------------------------------------------------------------------------

/**
 * Generate an OpenAPI 3.0.3 specification from discovered routes.
 *
 * Traverses the route definitions and their schemas to produce a complete
 * OpenAPI document — no extra annotations required from the user.
 */
export function generateOpenApiSpec(routes: DiscoveredRoute[], options: OpenApiOptions): OpenApiDocument {
  const paths: OpenApiDocument['paths'] = {};
  const contractProjection = new ContractProjection(options.client !== undefined);
  let needsBearer = false;
  let needsApiKey = false;

  if (options.client) validateClientServiceContract(options.client);

  // Route discovery ultimately walks the filesystem, whose iteration order is
  // platform-dependent. Sort a copy before populating the JSON object so the
  // committed spec is byte-identical on macOS and Linux without mutating the
  // caller's route inventory.
  const orderedRoutes = [...routes].sort((a, b) => {
    const aPath = toOpenApiPath(a.path);
    const bPath = toOpenApiPath(b.path);
    if (aPath !== bPath) return aPath < bPath ? -1 : 1;
    if (a.method === b.method) return 0;
    return a.method < b.method ? -1 : 1;
  });

  for (const route of orderedRoutes) {
    const openApiPath = toOpenApiPath(route.path);
    paths[openApiPath] ??= {};

    const method = route.method.toLowerCase();
    paths[openApiPath][method] = buildOperation(route, contractProjection, options.client, options.connect);

    if (route.meta?.security) {
      const schemes = requiredSchemes(route.meta.security);
      needsBearer ||= schemes.bearer;
      needsApiKey ||= schemes.apiKey;
    }
  }

  // Promote object models reused across operations into components/$ref, so a
  // shape that appears more than once becomes a single named type instead of N
  // inlined copies. Single-use schemas stay inline. Runs after all operations
  // are built so reuse is visible across the whole document.
  const contractSchemas = contractProjection.schemas();
  const sharedSchemas = dedupeSharedSchemas(paths, new Set(Object.keys(contractSchemas)));

  const protobuf =
    options.client && options.connect
      ? projectProtobufDescriptor(generateProto(orderedRoutes, { packageName: options.connect.packageName }))
      : undefined;
  const doc: OpenApiDocument = {
    openapi: '3.0.3',
    info: options.info,
    ...(options.servers?.length ? { servers: options.servers } : {}),
    paths,
    ...(options.client
      ? {
          'x-putnami-client': {
            protocolVersion: CLIENT_CONTRACT_PROTOCOL_VERSION,
            ...options.client,
            ...(protobuf ? { protobuf } : {}),
          },
        }
      : {}),
  };

  const components: NonNullable<OpenApiDocument['components']> = {};
  if (Object.keys(sharedSchemas).length > 0 || Object.keys(contractSchemas).length > 0) {
    for (const name of Object.keys(contractSchemas)) {
      if (sharedSchemas[name]) {
        throw new Error(`contract component ${JSON.stringify(name)} collides with a generated OpenAPI model`);
      }
    }
    components.schemas = { ...contractSchemas, ...sharedSchemas };
  }
  if (needsBearer || needsApiKey) {
    const securitySchemes: Record<string, OpenApiSecurityScheme> = {};
    if (needsBearer) {
      securitySchemes['bearerAuth'] = { type: 'http', scheme: 'bearer', bearerFormat: 'JWT' };
    }
    if (needsApiKey) {
      // Matches the Go framework's default API-key header (security/apikey.go).
      securitySchemes['apiKey'] = { type: 'apiKey', in: 'header', name: 'X-Api-Key' };
    }
    components.securitySchemes = securitySchemes;
  }
  if (Object.keys(components).length > 0) {
    doc.components = components;
  }
  if (options.client) {
    validateCacheInvalidationFields(doc);
    validateSseContinuationReferences(doc);
  }

  return doc;
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

/**
 * Convert framework path syntax (`/users/[id]`) to OpenAPI syntax (`/users/{id}`).
 */
function toOpenApiPath(path: string): string {
  return path.replace(/\[([^\]]+)]/g, '{$1}');
}

/**
 * Build an OpenAPI operation for a single route.
 */
function buildOperation(
  route: DiscoveredRoute,
  contractProjection: ContractProjection,
  client?: ClientServiceContract,
  connect?: OpenApiOptions['connect'],
): OpenApiOperation {
  const operation: OpenApiOperation = {
    operationId: clientOperationId(route.method, route.path),
    responses: {},
  };

  // An operation an external authority owns publishes the schemas that
  // standard owns, so its own parameters, body and responses are projected the
  // way a document without a client contract projects them. Contract
  // components stay shared and first-party: the whole document reads them.
  const external = externalAuthority(route, client !== undefined);
  const projection = external === undefined ? contractProjection : contractProjection.lenient();

  // Endpoint description
  if (route.meta?.description) {
    operation.description = route.meta.description;
  }

  const parameters: OpenApiParameter[] = [];

  // Path parameters
  if (route.schemas?.params) {
    for (const [name, prop] of Object.entries(route.schemas.params)) {
      parameters.push({
        name,
        in: 'path',
        required: !isOptionalProp(prop),
        schema: primitiveToSchema(prop, projection),
      });
    }
  }

  // Query parameters
  if (route.schemas?.query) {
    for (const [name, prop] of Object.entries(route.schemas.query)) {
      parameters.push({
        name,
        in: 'query',
        required: !isOptionalProp(prop),
        schema: primitiveToSchema(prop, projection),
      });
    }
  }

  // Header parameters
  if (route.schemas?.headers) {
    for (const [name, prop] of Object.entries(route.schemas.headers)) {
      parameters.push({
        name,
        in: 'header',
        required: !isOptionalProp(prop),
        schema: primitiveToSchema(prop, projection),
      });
    }
  }

  if (parameters.length > 0) {
    operation.parameters = parameters;
  }

  // Request body.
  //
  // A client or bidirectional stream declares its body with `Stream(...)`: that
  // schema is the message shape carried in frames, published under
  // `x-putnami-client` `messages.input`. Publishing it as an HTTP request body
  // as well would make the document claim the upgrade GET carries a payload,
  // which no first-party emitter can honor and no intermediary would forward.
  const streamsItsBody = route.streamMode === 'client' || route.streamMode === 'bidirectional';
  if (route.schemas?.bodyBinary) {
    operation.requestBody = { required: true, content: binaryContent(route.schemas.bodyBinary) };
  } else if (route.schemas?.body && !streamsItsBody) {
    const bodyContentType = route.schemas.bodyContentType ?? 'application/json';
    operation.requestBody = {
      required: true,
      content: {
        [bodyContentType]: {
          schema: definitionToSchema(route.schemas.body, projection),
        },
      },
    };
  }

  // Response
  buildResponses(route, operation, projection);

  // Security — derive the required scheme(s) from the route's `.secure()` meta.
  // Bearer routes carry their required scopes in the requirement; api-key routes
  // add an apiKey scheme. Roles (and any-of scopes) remain description text.
  if (route.meta?.security) {
    operation.security = buildSecurityRequirements(route.meta.security);
    const notes = collectSecurityNotes(route.meta.security);
    if (notes) {
      operation.description = operation.description ? `${operation.description}\n\n${notes}` : notes;
    }
  }

  if (external !== undefined) {
    operation[EXTERNAL_CONTRACT_KEY] = external;
  } else if (client) {
    operation['x-putnami-client'] = buildClientOperation(route, client, contractProjection, connect);
  }

  return operation;
}

/**
 * The authority an endpoint names with `.client({ external })`, or undefined
 * for a first-party operation. `external` stands alone and only means something
 * inside a first-party contract, so every other combination is refused.
 */
function externalAuthority(route: DiscoveredRoute, firstPartyDocument: boolean): string | undefined {
  const authority = route.meta?.client?.external;
  if (authority === undefined) return undefined;
  const contradiction = externalClientPolicyError(route.meta?.client, firstPartyDocument);
  if (contradiction) throw clientContractError(route, contradiction);
  return authority;
}

/** Project endpoint declarations into the operation extension without a second operation inventory. */
function buildClientOperation(
  route: DiscoveredRoute,
  service: ClientServiceContract,
  contractProjection: ContractProjection,
  connect?: OpenApiOptions['connect'],
): ClientContractOperation {
  if (route.meta?.customSecurityGuard || route.meta?.security?.verify) {
    throw clientContractError(
      route,
      'custom security guards and token verifiers cannot be represented in a first-party client contract',
    );
  }
  const policy = route.meta?.client;
  const stream = route.streamMode ?? 'unary';
  const operation: ClientContractOperation = {
    stream,
    ...clientMessageShapes(route, stream, contractProjection),
    transports: clientTransports(route, connect, policy),
    security: clientSecurity(route, policy),
    errors: clientErrors(route, contractProjection),
    idempotency: policy?.idempotency ?? defaultIdempotency(route.method),
    ...(policy?.resilience ? { resilience: policy.resilience } : {}),
  };
  validateClientOperation(route, service, operation);
  return operation;
}

function clientMessageShapes(
  route: DiscoveredRoute,
  stream: ClientContractOperation['stream'],
  contractProjection: ContractProjection,
): Pick<ClientContractOperation, 'messages'> {
  if (stream === 'unary') return {};
  // Raw octets have no schema: a byte stream declares no messages.
  if (route.wire?.bytes) return {};
  const input = route.schemas?.body
    ? (definitionToSchema(route.schemas.body, contractProjection) as ClientSchema)
    : undefined;
  const output = route.schemas?.returns
    ? (definitionToSchema(route.schemas.returns, contractProjection) as ClientSchema)
    : undefined;
  if ((stream === 'client' || stream === 'bidirectional') && !input) {
    throw clientContractError(route, `${stream} stream must declare its input body schema`);
  }
  if ((stream === 'server' || stream === 'bidirectional') && !output) {
    throw clientContractError(route, `${stream} stream must declare its output schema`);
  }
  return { messages: { ...(input ? { input } : {}), ...(output ? { output } : {}) } };
}

function clientTransports(
  route: DiscoveredRoute,
  connect?: OpenApiOptions['connect'],
  policy?: ClientOperationPolicy,
): ClientTransportContract[] {
  const path = toOpenApiPath(route.path);
  const stream = route.streamMode ?? 'unary';
  if (route.wire) {
    // A provider-owned wire is the route's only wire: the bound server
    // negotiates its declared token and nothing else, so no other transport can
    // be published beside it. The declared order may still name it, and
    // nothing else.
    const wire: ClientTransportContract = {
      protocol: 'websocket',
      path,
      encoding: route.wire.bytes ? 'binary' : 'json',
      websocket: {
        ...(route.wire.subprotocol ? { subprotocol: route.wire.subprotocol } : {}),
        resume: false,
        wire: 'provider',
      },
    };
    return applyDeclaredSseContinuation(
      route,
      applyDeclaredResume(route, orderDeclaredTransports(route, [wire], policy), policy),
      policy,
    );
  }
  const transports: ClientTransportContract[] = [];
  // A raw octet payload travels on REST alone: a Connect envelope carries an
  // encoded message, so the octets would cross re-encoded — the re-wrapping a
  // binary declaration refuses. The Go projection announces no Connect for such
  // a route either.
  const binary = route.schemas?.bodyBinary !== undefined || route.schemas?.returnsBinary !== undefined;
  if (connect && !binary && (stream === 'unary' || stream === 'server')) {
    const service = protoServiceName(route.path);
    const rpc = protoRpcName(route.method, route.path);
    const protobufMethod = `/${connect.packageName}.${service}/${rpc}`;
    const encodings = (stream === 'server' ? connect.serverStreamEncodings : connect.unaryEncodings) ?? [
      'proto',
      'json',
    ];
    for (const encoding of encodings) {
      transports.push({
        protocol: 'connect',
        path: protobufMethod,
        encoding,
        protobufMethod,
      });
    }
  }
  if (stream === 'unary') {
    transports.push({ protocol: 'rest-json', path, encoding: 'json' });
  } else if (stream === 'server') {
    transports.push({ protocol: 'sse', path, encoding: 'json' });
    transports.push({
      protocol: 'websocket',
      path,
      encoding: 'json',
      websocket: { subprotocol: 'putnami.service.v1', resume: false },
    });
  } else {
    transports.push({
      protocol: 'websocket',
      path,
      encoding: 'json',
      websocket: { subprotocol: 'putnami.service.v1', resume: false },
    });
  }
  return applyDeclaredSseContinuation(
    route,
    applyDeclaredResume(
      route,
      orderDeclaredConnectEncodings(route, orderDeclaredTransports(route, transports, policy), policy),
      policy,
    ),
    policy,
  );
}

/**
 * Narrow and reorder the transports this provider can carry to the order the
 * operation declared.
 *
 * The declaration cannot add a transport: the derived list is what the bound
 * server actually serves, and publishing anything else would hand a generated
 * client a wire nobody answers. It can drop one, because narrowing is a
 * deliberate provider choice, and it can reorder, because the declared order is
 * the dispatch order every generated client follows.
 */
function orderDeclaredTransports(
  route: DiscoveredRoute,
  available: ClientTransportContract[],
  policy?: ClientOperationPolicy,
): ClientTransportContract[] {
  const declared = policy?.transports;
  if (!declared || declared.length === 0) return available;
  const ordered: ClientTransportContract[] = [];
  const seen = new Set<string>();
  for (const protocol of declared) {
    if (seen.has(protocol)) {
      throw clientContractError(route, `transport '${protocol}' is declared twice in the client transport order`);
    }
    seen.add(protocol);
    const matched = available.filter((transport) => transport.protocol === protocol);
    if (matched.length === 0) {
      throw clientContractError(
        route,
        `transport '${protocol}' is declared but this provider does not serve it for a ${route.streamMode ?? 'unary'} shape`,
      );
    }
    ordered.push(...matched);
  }
  return ordered;
}

/**
 * Narrow and reorder the Connect entries of an operation's transports to the
 * encoding order it declared.
 *
 * The contract carries one Connect entry per encoding, and a generated client
 * dispatches the first one it can carry, so without this declaration the gRPC
 * plugin's own order decides and the second encoding is never dispatched. The
 * declared entries take the place of the first Connect entry; the transports
 * around them keep the order the operation already declared. As with the
 * transport order, a declaration can drop an encoding and can never add one the
 * plugin does not serve.
 */
function orderDeclaredConnectEncodings(
  route: DiscoveredRoute,
  transports: ClientTransportContract[],
  policy?: ClientOperationPolicy,
): ClientTransportContract[] {
  const declared = policy?.connectEncodings;
  if (!declared || declared.length === 0) return transports;
  const served = new Map<string, ClientTransportContract>();
  for (const transport of transports) {
    if (transport.protocol === 'connect') served.set(transport.encoding, transport);
  }
  if (served.size === 0) {
    throw clientContractError(route, 'a Connect encoding order is declared and no Connect transport is published');
  }
  const seen = new Set<string>();
  const ordered: ClientTransportContract[] = [];
  for (const encoding of declared) {
    if (seen.has(encoding)) {
      throw clientContractError(route, `Connect encoding '${encoding}' is declared twice in the client encoding order`);
    }
    seen.add(encoding);
    const transport = served.get(encoding);
    if (!transport) {
      throw clientContractError(
        route,
        `Connect encoding '${encoding}' is declared but the gRPC plugin does not serve it for this route`,
      );
    }
    ordered.push(transport);
  }
  let placed = false;
  return transports.flatMap((transport) => {
    if (transport.protocol !== 'connect') return [transport];
    if (placed) return [];
    placed = true;
    return ordered;
  });
}

/**
 * Mark the operation's WebSocket transport resume-capable.
 *
 * Resume continues a stream after the last sequence the consumer completely
 * delivered, so it is only sound where re-reading the same position has no
 * effect: a server stream, declared safe, carried by the first-party WebSocket
 * wire. Every other shape is refused here rather than published as a capability
 * no provider could honor without a gap or a duplicate.
 */
function applyDeclaredResume(
  route: DiscoveredRoute,
  transports: ClientTransportContract[],
  policy?: ClientOperationPolicy,
): ClientTransportContract[] {
  if (!policy?.resume) return transports;
  if (route.streamMode !== 'server') {
    throw clientContractError(
      route,
      `stream resume is declared on a ${route.streamMode ?? 'unary'} shape; only a server stream can be resumed`,
    );
  }
  const kind = policy.idempotency?.kind ?? defaultIdempotency(route.method).kind;
  if (kind !== 'safe') {
    throw clientContractError(
      route,
      `stream resume is declared with idempotency '${kind}'; only a safe stream can be resumed`,
    );
  }
  let marked = false;
  const resumable = transports.map((transport) => {
    if (transport.protocol !== 'websocket' || !transport.websocket) return transport;
    marked = true;
    return { ...transport, websocket: { ...transport.websocket, resume: true } };
  });
  if (!marked) {
    throw clientContractError(route, 'stream resume is declared and no websocket transport is published to carry it');
  }
  return resumable;
}

/**
 * Publish the operation's declared continuation on its SSE transport (ADR 0013
 * of protocols/clientcontract).
 *
 * A reopened connection reads the stream again from a position, so only a safe
 * server stream carried by SSE can declare one. Every other shape, and a
 * declaration the strict readers would refuse, fails here with the route
 * named. What a cursor names is checked against the finished document by
 * `validateSseContinuationReferences`. Mirrors the Go projection's
 * `applyDeclaredSSEContinuation`.
 */
function applyDeclaredSseContinuation(
  route: DiscoveredRoute,
  transports: ClientTransportContract[],
  policy?: ClientOperationPolicy,
): ClientTransportContract[] {
  const continuation = policy?.sseContinuation;
  if (continuation === undefined) return transports;
  if (route.streamMode !== 'server') {
    throw clientContractError(
      route,
      `an sse continuation is declared on a ${route.streamMode ?? 'unary'} shape; only a server stream can be continued`,
    );
  }
  const kind = policy?.idempotency?.kind ?? defaultIdempotency(route.method).kind;
  if (kind !== 'safe') {
    throw clientContractError(
      route,
      `an sse continuation is declared with idempotency '${kind}'; only a safe stream can be continued`,
    );
  }
  const mode: unknown = continuation.mode;
  const cursor = (continuation as { cursor?: { outputField?: unknown; queryParameter?: unknown } }).cursor;
  let published: NonNullable<ClientOperationPolicy['sseContinuation']>;
  if (mode === 'cursor') {
    if (!cursor || !isNonBlankString(cursor.outputField) || !isNonBlankString(cursor.queryParameter)) {
      throw clientContractError(
        route,
        'a cursor continuation does not name both its output field and its query parameter',
      );
    }
    published = { mode: 'cursor', cursor: { outputField: cursor.outputField, queryParameter: cursor.queryParameter } };
  } else if (mode === 'best-effort') {
    if (cursor !== undefined) {
      throw clientContractError(
        route,
        'a best-effort continuation declares a cursor; best-effort reopens the original selector and carries no position',
      );
    }
    published = { mode: 'best-effort' };
  } else {
    throw clientContractError(
      route,
      `sse continuation mode ${JSON.stringify(mode)} is unsupported; the modes are 'cursor' and 'best-effort'`,
    );
  }
  let marked = false;
  const continued = transports.map((transport) => {
    if (transport.protocol !== 'sse') return transport;
    marked = true;
    return { ...transport, sse: { continuation: published } };
  });
  if (!marked) {
    throw clientContractError(route, 'an sse continuation is declared and no sse transport is published to carry it');
  }
  return continued;
}

function isNonBlankString(value: unknown): value is string {
  return typeof value === 'string' && value.trim().length > 0;
}

function clientSecurity(route: DiscoveredRoute, policy?: ClientOperationPolicy): ClientSecurityPolicy {
  if (policy?.security) {
    return {
      ...policy.security,
      ...(route.meta?.security ? { authorization: clientAuthorization(route.meta.security) } : {}),
    };
  }
  if (route.meta?.security) {
    throw clientContractError(
      route,
      'secure endpoint has no client security policy; add endpoint().client({ security: ... }) naming an allowed credential profile',
    );
  }
  return { alternatives: [{ allOf: [] }] };
}

function clientAuthorization(security: SecurityOptions): ClientAuthorizationPolicy {
  return {
    ...stringList('issuers', security.issuer),
    ...stringList('audiences', security.audience),
    ...stringList('principalKinds', security.principalKind),
    ...stringList('clients', security.client),
    ...stringList('scopesAll', security.scopes),
    ...stringList('scopesAny', security.scopesAny),
    ...stringList('rolesAll', security.roles),
    ...stringList('rolesAny', security.rolesAny),
    ...stringList('scopeClaims', security.scopeClaim),
    ...stringList('roleClaims', security.roleClaim),
  };
}

function stringList(name: keyof ClientAuthorizationPolicy, value: string | readonly string[] | undefined) {
  if (value === undefined) return {};
  return { [name]: Array.isArray(value) ? [...value] : [value] };
}

function projectProtobufDescriptor(doc: ProtoDocument): ClientProtobufDescriptor {
  const scalarTypes = new Set([
    'double',
    'float',
    'int32',
    'int64',
    'uint32',
    'uint64',
    'sint32',
    'sint64',
    'fixed32',
    'fixed64',
    'sfixed32',
    'sfixed64',
    'bool',
    'string',
    'bytes',
  ]);
  const enums = new Set(doc.enumTypes);
  const kind = (type: string): 'scalar' | 'message' | 'enum' =>
    enums.has(type) ? 'enum' : scalarTypes.has(type) ? 'scalar' : 'message';
  return {
    syntax: 'proto3',
    package: doc.packageName,
    services: Object.entries(doc.serviceMeta).map(([name, methods]) => ({
      name,
      methods: methods.map((method) => ({
        name: method.name,
        input: method.requestMessage,
        output: method.responseMessage,
        clientStreaming: method.clientStreaming,
        serverStreaming: method.serverStreaming,
      })),
    })),
    messages: Object.entries(doc.messageMeta).map(([name, fields]) => ({
      name,
      fields: fields.map((field) => {
        if (field.mapKeyType && field.mapValueType) {
          return {
            name: field.name,
            jsonName: protoJsonName(field.name),
            number: field.number,
            typeKind: 'map' as const,
            type: 'map',
            map: {
              keyType: field.mapKeyType,
              valueKind: kind(field.mapValueType),
              valueType: field.mapValueType,
            },
          };
        }
        return {
          name: field.name,
          jsonName: protoJsonName(field.name),
          number: field.number,
          typeKind: kind(field.type),
          type: field.type,
          ...(field.repeated ? { repeated: true } : {}),
          ...(field.optional ? { optional: true } : {}),
        };
      }),
    })),
    enums: Object.entries(doc.enumMeta).map(([name, values]) => {
      const prefix = toScreamingSnakeCase(name);
      return {
        name,
        values: [
          { name: `${prefix}_UNSPECIFIED`, number: 0 },
          ...values.map((value, index) => ({
            name: `${prefix}_${toScreamingSnakeCase(value)}`,
            number: index + 1,
          })),
        ],
      };
    }),
  };
}

function protoJsonName(name: string): string {
  return name.replace(/_([a-z0-9])/g, (_, character: string) => character.toUpperCase());
}

function clientErrors(route: DiscoveredRoute, contractProjection: ContractProjection): ClientDeclaredError[] {
  const errors: ClientDeclaredError[] = [
    { status: 400, code: IMPLICIT_BAD_REQUEST_CODE },
    { status: 500, code: IMPLICIT_INTERNAL_CODE },
  ];
  if (route.schemas?.bodyBinary) {
    // A refusal the provider raises before it reads the body is never
    // retryable: the same request would be refused again.
    errors.push({ status: 413, code: PAYLOAD_TOO_LARGE_CODE, retryable: false });
    errors.push({ status: 415, code: UNSUPPORTED_MEDIA_TYPE_CODE, retryable: false });
  }
  // `.mayThrowDetails()` names the schema of one code's `details` member; it is
  // that error's schema and nothing else's (ADR 0006). A code declared twice —
  // `.mayThrowDetails()` beside `.mayThrowWith()` — still projects one error.
  const declaredDetails = new Set<ClientDeclaredError>();
  for (const code of new Set(route.responses?.errorCodes ?? [])) {
    const status = errorCodeToStatus[code] ?? 500;
    const retryable = route.responses?.errorOptions?.[code]?.retryable;
    const details = route.responses?.errorDetails?.[code];
    const declared: ClientDeclaredError = {
      status,
      code: errorCodeToStableCode[code],
      ...(retryable !== undefined ? { retryable } : {}),
      ...(details ? { schema: declaredDetailsSchema(details, contractProjection) as unknown as ClientSchema } : {}),
    };
    errors.push(declared);
    if (details) declaredDetails.add(declared);
  }
  for (const declaration of route.responses?.throws ?? []) {
    // One declared code at this status or several, the implicit 400 and 500
    // pair included: the codes discriminate the error a client receives
    // there, and .throws() documents the status. Only a status no declared
    // code shares is undiscriminated. The Go projection applies the same rule.
    const matches = errors.filter((error) => error.status === declaration.status);
    if (matches.length === 0) {
      throw clientContractError(
        route,
        `.throws(${declaration.status}) has no stable .mayThrow() error code at that status`,
      );
    }
    // The .throws() schema describes the details of every error at its status
    // (ADR 0006), so it applies to each matching code. The stream error
    // projection reads it the same way: by status, whatever the declared code.
    // The implicit pair carries the envelope and never details, so it never
    // takes a details schema. A code that declares its own details with
    // .mayThrowDetails() keeps them: the declaration for one code is more
    // specific than the one for its status, and the stream projection applies
    // the same precedence.
    if (declaration.schema) {
      for (const match of matches) {
        if (match.code === IMPLICIT_BAD_REQUEST_CODE || match.code === IMPLICIT_INTERNAL_CODE) continue;
        if (declaredDetails.has(match)) continue;
        const index = errors.indexOf(match);
        errors[index] = {
          ...match,
          schema: definitionToSchema(declaration.schema, contractProjection) as unknown as ClientSchema,
        };
      }
    }
  }
  errors.sort((left, right) => left.status - right.status || compareCodeUnits(left.code, right.code));
  return errors;
}

function compareCodeUnits(left: string, right: string): number {
  if (left < right) return -1;
  return left > right ? 1 : 0;
}

function defaultIdempotency(method: string): ClientContractOperation['idempotency'] {
  switch (method.toUpperCase()) {
    case 'GET':
      return { kind: 'safe' };
    case 'PUT':
    case 'DELETE':
      return { kind: 'idempotent' };
    default:
      return { kind: 'non-idempotent' };
  }
}

function validateClientServiceContract(contract: ClientServiceContract): void {
  assertNonEmpty(contract.service.id, 'api.client.service.id');
  assertNonEmpty(contract.service.audience, 'api.client.service.audience');
  if (contract.defaults?.resilience?.cache !== undefined) {
    throw new Error(
      'api.client.defaults.resilience.cache: a response cache is declared per operation; a document default would cache operations that never asked for it',
    );
  }
  for (const [name, profile] of Object.entries(contract.credentials)) {
    assertNonEmpty(name, 'api.client.credentials profile name');
    if ((profile.kind === 'api-key' || profile.kind === 'named-header') && !isHeaderName(profile.header)) {
      throw new Error(`api.client.credentials.${name}.header must be a valid non-empty HTTP header name`);
    }
    if (
      (profile.kind === 'api-key' || profile.kind === 'named-header') &&
      isFrameworkOwnedClientHeader(profile.header)
    ) {
      throw new Error(`api.client.credentials.${name}.header is framework-owned`);
    }
    assertUnique(
      profile.kind === 'service-token' ? profile.scopes : undefined,
      `api.client.credentials.${name}.scopes`,
    );
  }
}

function validateClientOperation(
  route: DiscoveredRoute,
  service: ClientServiceContract,
  operation: ClientContractOperation,
): void {
  if (operation.security.alternatives.length === 0) {
    throw clientContractError(route, 'client security alternatives must not be empty');
  }
  for (const [alternativeIndex, alternative] of operation.security.alternatives.entries()) {
    const profiles = new Set<string>();
    for (const requirement of alternative.allOf) {
      assertNonEmpty(requirement.profile, `client security alternative ${alternativeIndex} profile`, route);
      if (profiles.has(requirement.profile)) {
        throw clientContractError(
          route,
          `client security alternative ${alternativeIndex} repeats profile '${requirement.profile}'`,
        );
      }
      profiles.add(requirement.profile);
      if (!service.credentials[requirement.profile]) {
        throw clientContractError(
          route,
          `client security references unknown credential profile '${requirement.profile}'`,
        );
      }
      assertUnique(requirement.scopes, `client security profile '${requirement.profile}' scopes`, route);
      assertUnique(requirement.roles, `client security profile '${requirement.profile}' roles`, route);
    }
  }

  validateSecurityIsNotWeaker(route, service, operation.security);

  if (operation.idempotency.kind !== 'idempotent' && operation.idempotency.keyHeader !== undefined) {
    throw clientContractError(route, 'idempotency.keyHeader is valid only when kind is idempotent');
  }
  if (operation.idempotency.keyHeader !== undefined && !isHeaderName(operation.idempotency.keyHeader)) {
    throw clientContractError(route, 'idempotency.keyHeader must be a valid HTTP header name');
  }
  if (operation.idempotency.keyHeader && isFrameworkOwnedClientHeader(operation.idempotency.keyHeader)) {
    throw clientContractError(route, 'idempotency.keyHeader is framework-owned');
  }
  for (const header of Object.keys(route.schemas?.headers ?? {})) {
    if (isFrameworkOwnedClientHeader(header)) {
      throw clientContractError(route, `header parameter '${header}' is framework-owned`);
    }
  }
  // The operation's own value always counts; the document default reaches only
  // server streams, the one mode a reconnect can continue. This is the Go
  // reader's `streamReconnect`.
  const reconnect =
    operation.resilience?.stream?.reconnect ??
    (operation.stream === 'server' ? service.defaults?.resilience?.stream?.reconnect : undefined);
  if (
    reconnect === true &&
    !operation.transports.some((transport) => transport.websocket?.resume || transport.sse?.continuation)
  ) {
    throw clientContractError(
      route,
      'stream reconnect requires provider continuation support; declare .client({ resume: true }) or .client({ sseContinuation }) on a safe server stream',
    );
  }
  if (operation.resilience?.cache) validateClientCachePolicy(route, operation, operation.resilience.cache);
}

/**
 * A response cache stands in for a call only where replaying the answer is the
 * same as calling again, and a key field must name an input the endpoint
 * declares: one that names nothing keys every request on the same absent value
 * and hands one caller another caller's answer. Mirrors the Go provider, which
 * re-reads its published contract through the strict reader (ADR 0007 of
 * protocols/clientcontract).
 */
// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: each declared cache field has its own refusal
function validateClientCachePolicy(
  route: DiscoveredRoute,
  operation: ClientContractOperation,
  cache: NonNullable<NonNullable<ClientContractOperation['resilience']>['cache']>,
): void {
  if (route.schemas?.bodyBinary?.streamed || route.schemas?.returnsBinary?.streamed) {
    throw clientContractError(route, 'resilience.cache cannot store or replay a raw HTTP stream');
  }
  if (operation.stream !== 'unary') {
    throw clientContractError(
      route,
      'resilience.cache requires a unary operation; a stream has no single answer to store',
    );
  }
  if (operation.idempotency.kind !== 'safe' && operation.idempotency.kind !== 'idempotent') {
    throw clientContractError(
      route,
      'resilience.cache requires a safe or idempotent operation; a repeated effect cannot be answered from memory',
    );
  }
  const positive = (value: number | undefined): boolean =>
    value === undefined || (Number.isSafeInteger(value) && value > 0);
  if (!positive(cache.freshMs) || cache.freshMs === undefined) {
    throw clientContractError(route, 'resilience.cache.freshMs must be a positive integer');
  }
  if (!positive(cache.staleMs) || !positive(cache.maxEntries)) {
    throw clientContractError(route, 'resilience.cache.staleMs and maxEntries must be positive integers');
  }
  if (cache.staleMs !== undefined && cache.staleMs <= cache.freshMs) {
    throw clientContractError(
      route,
      'resilience.cache.staleMs must exceed freshMs; omit it to never serve a stale answer',
    );
  }
  if (cache.invalidationFields !== undefined) {
    if (cache.invalidationFields.length === 0) {
      throw clientContractError(
        route,
        'resilience.cache.invalidationFields must name at least one field; omit it to drop answers by key prefix only',
      );
    }
    assertUnique(cache.invalidationFields, 'resilience.cache.invalidationFields', route);
    for (const field of cache.invalidationFields) {
      if (!isCacheInvalidationField(field)) {
        throw clientContractError(
          route,
          `resilience.cache.invalidationFields entry ${JSON.stringify(field)} must name a top-level response property, with no leading or trailing space and no control character`,
        );
      }
    }
  }
  if (cache.keyFields === undefined) return;
  if (cache.keyFields.length === 0) {
    throw clientContractError(
      route,
      'resilience.cache.keyFields must name at least one field; omit it to key on the whole request',
    );
  }
  assertUnique(cache.keyFields, 'resilience.cache.keyFields', route);
  const schemas = route.schemas;
  for (const keyField of cache.keyFields) {
    const dot = keyField.indexOf('.');
    const section = keyField === 'body' ? 'body' : dot > 0 ? keyField.slice(0, dot) : '';
    const name = keyField === 'body' ? '' : dot > 0 ? keyField.slice(dot + 1) : '';
    const declared =
      section === 'path'
        ? name in (schemas?.params ?? {})
        : section === 'query'
          ? name in (schemas?.query ?? {})
          : section === 'header'
            ? Object.keys(schemas?.headers ?? {}).some((header) => header.toLowerCase() === name.toLowerCase())
            : section === 'body'
              ? (schemas?.body !== undefined || schemas?.bodyBinary !== undefined) &&
                (name === '' || (schemas?.body !== undefined && name in schemas.body))
              : false;
    if (!declared) {
      throw clientContractError(
        route,
        `resilience.cache.keyFields entry '${keyField}' names no request input this endpoint declares`,
      );
    }
  }
}

/**
 * One declared invalidation field: a property name as it appears on the wire,
 * with no leading or trailing space and no control character — the byte rule
 * `ParseCacheInvalidationField` in protocols/clientcontract applies.
 */
function isCacheInvalidationField(field: string): boolean {
  if (field === '' || field.startsWith(' ') || field.endsWith(' ')) return false;
  for (const char of field) {
    const code = char.codePointAt(0) ?? 0;
    if (code < 0x20 || code === 0x7f) return false;
  }
  return true;
}

/**
 * Refuse an invalidation field that names no comparable property of its
 * operation's JSON success body: it would tag no answer, and every
 * invalidation by it would silently drop nothing. It runs on the finished
 * document, whose components hold every schema a body can reference. The Go
 * provider re-reads its published contract through the strict Go reader, which
 * applies the same rule.
 *
 * @internal Exported for tests; the package entry does not re-export it.
 */
export function validateCacheInvalidationFields(doc: OpenApiDocument): void {
  const components = doc.components?.schemas ?? {};
  for (const [path, item] of Object.entries(doc.paths)) {
    for (const [method, operation] of Object.entries(item)) {
      const fields = operation['x-putnami-client']?.resilience?.cache?.invalidationFields;
      if (!fields) continue;
      const properties = cacheResponseProperties(operation, components);
      for (const field of fields) {
        const comparable = properties.get(field);
        if (comparable === true) continue;
        throw new Error(
          `api.client operation ${method.toUpperCase()} ${path}: resilience.cache.invalidationFields entry '${field}' ${
            comparable === undefined
              ? 'names no top-level property of a JSON success body this endpoint declares'
              : 'must name a string, integer or boolean property; the runtimes compare no other value'
          }`,
        );
      }
    }
  }
}

/**
 * Refuse a cursor continuation whose output field or query parameter is not a
 * plain string the operation declares (ADR 0013 of protocols/clientcontract):
 * the runtime copies a position verbatim from one to the other and never
 * interprets it. It runs on the finished document, whose components hold
 * every schema a message can reference. The rule is the strict readers'
 * `ValidateSSEContinuationReferences`: one local component reference is
 * followed for the message and for each schema.
 *
 * @internal Exported for tests; the package entry does not re-export it.
 */
export function validateSseContinuationReferences(doc: OpenApiDocument): void {
  const components = doc.components?.schemas ?? {};
  for (const [path, item] of Object.entries(doc.paths)) {
    for (const [method, operation] of Object.entries(item)) {
      const client = operation['x-putnami-client'];
      if (!client) continue;
      const fail = (message: string): never => {
        throw new Error(`api.client operation ${method.toUpperCase()} ${path}: ${message}`);
      };
      for (const transport of client.transports) {
        const continuation = transport.protocol === 'sse' ? transport.sse?.continuation : undefined;
        if (continuation?.mode !== 'cursor') continue;
        const { outputField, queryParameter } = continuation.cursor;
        const output = resolveComponent(client.messages?.output as OpenApiSchema | undefined, components);
        const properties = output?.type === 'object' ? output.properties : undefined;
        const property = properties && Object.hasOwn(properties, outputField) ? properties[outputField] : undefined;
        if (property === undefined) {
          fail(`sseContinuation output field '${outputField}' names no property of the declared output message`);
        }
        if (!(output?.required ?? []).includes(outputField)) {
          fail(
            `sseContinuation output field '${outputField}' must be required: every message carries the position after it`,
          );
        }
        if (!isCursorText(resolveComponent(property, components))) {
          fail(`sseContinuation output field '${outputField}' must be a plain string: a position is opaque text`);
        }
        const parameter = (operation.parameters ?? []).find(
          (entry) => entry.in === 'query' && entry.name === queryParameter,
        );
        if (parameter === undefined) {
          fail(`sseContinuation query parameter '${queryParameter}' is not declared by this endpoint`);
        }
        if (!isCursorText(resolveComponent(parameter?.schema, components))) {
          fail(`sseContinuation query parameter '${queryParameter}' must be a plain string: a position is opaque text`);
        }
      }
    }
  }
}

/**
 * A schema whose values a runtime carries verbatim as a position: a string
 * with no format, no enum, no union and no null. Mirrors `sseCursorText` in
 * protocols/clientcontract.
 */
function isCursorText(schema: OpenApiSchema | undefined): boolean {
  return (
    schema !== undefined &&
    schema.$ref === undefined &&
    schema.type === 'string' &&
    !schema.format &&
    schema.nullable !== true &&
    !schema.enum?.length &&
    !schema.oneOf?.length &&
    (schema as Record<string, unknown>)['x-putnami-json'] === undefined
  );
}

/**
 * The top-level properties of an operation's JSON success bodies, each mapped
 * to whether every body that declares it declares a comparable value. The
 * responses read are the Go reader's (`neutralSuccesses` and
 * `validateMethodCacheKeyFields` in go/framework/api): every three-digit 2xx
 * status, and in each every media type equal to `application/json` ignoring
 * case. The comparable values are `CacheResponseProperties` in
 * protocols/clientcontract (ADR 0007): one local component reference is
 * followed for a body and for each property.
 */
function cacheResponseProperties(
  operation: OpenApiOperation,
  components: Record<string, OpenApiSchema>,
): Map<string, boolean> {
  const properties = new Map<string, boolean>();
  for (const [status, response] of Object.entries(operation.responses)) {
    if (!/^2\d\d$/.test(status)) continue;
    for (const [mediaType, media] of Object.entries(response.content ?? {})) {
      if (mediaType.toLowerCase() !== 'application/json') continue;
      const body = resolveComponent(media.schema, components);
      for (const [name, property] of Object.entries(body?.properties ?? {})) {
        const comparable = isComparableScalar(resolveComponent(property, components));
        properties.set(name, comparable && (properties.get(name) ?? true));
      }
    }
  }
  return properties;
}

function resolveComponent(
  schema: OpenApiSchema | undefined,
  components: Record<string, OpenApiSchema>,
): OpenApiSchema | undefined {
  if (!schema?.$ref) return schema;
  const prefix = '#/components/schemas/';
  return schema.$ref.startsWith(prefix) ? components[schema.$ref.slice(prefix.length)] : undefined;
}

/**
 * A value both runtimes render to the same text: a string, an integer or a
 * boolean. A string declared `format: byte` or `binary` is octets: its JSON
 * form is base64 text, which the Go runtime would tag, but the TypeScript
 * runtime decodes it to a byte array no invalidation value can equal.
 */
function isComparableScalar(schema: OpenApiSchema | undefined): boolean {
  if (
    !schema ||
    schema.$ref ||
    schema.oneOf?.length ||
    (schema as Record<string, unknown>)['x-putnami-json'] !== undefined
  )
    return false;
  if (schema.type === 'string') return schema.format !== 'byte' && schema.format !== 'binary';
  return schema.type === 'integer' || schema.type === 'boolean';
}

function validateSecurityIsNotWeaker(
  route: DiscoveredRoute,
  service: ClientServiceContract,
  policy: ClientSecurityPolicy,
): void {
  const secure = route.meta?.security;
  if (!secure) return;
  const last = policy.alternatives.length - 1;
  for (const [index, alternative] of policy.alternatives.entries()) {
    if (alternative.allOf.length === 0) {
      // Only a rule that serves a caller presenting no credential admits the
      // anonymous alternative, and only last: a client takes the first
      // alternative it satisfies and an empty one always is. The rule's claims
      // bind an authenticated caller only, so nothing below applies to it.
      if (secure.optional !== true) {
        throw clientContractError(
          route,
          'an authenticated endpoint cannot advertise an anonymous client alternative; declare .secure({ optional: true }) when it also serves anonymous callers',
        );
      }
      if (index !== last) {
        throw clientContractError(
          route,
          'the anonymous client alternative must come last; a client takes the first alternative it satisfies and an empty one always is',
        );
      }
      continue;
    }
    const kinds = alternative.allOf.map((requirement) => service.credentials[requirement.profile]?.kind);
    const principalKinds = Array.isArray(secure.principalKind)
      ? secure.principalKind
      : secure.principalKind
        ? [secure.principalKind]
        : [];
    if (principalKinds.length === 1 && principalKinds[0] === 'apikey' && !kinds.includes('api-key')) {
      throw clientContractError(
        route,
        'endpoint requires an API-key principal but a client alternative has no api-key profile',
      );
    }
    const scopes = new Set<string>();
    const roles = new Set<string>();
    for (const requirement of alternative.allOf) {
      for (const scope of requirement.scopes ?? []) scopes.add(scope);
      for (const role of requirement.roles ?? []) roles.add(role);
      const profile = service.credentials[requirement.profile];
      if (profile?.kind === 'service-token') {
        for (const scope of profile.scopes ?? []) scopes.add(scope);
      }
    }
    for (const required of secure.scopes ?? []) {
      if (!scopes.has(required)) {
        throw clientContractError(route, `client security alternative does not request required scope '${required}'`);
      }
    }
    if (secure.scopesAny?.length && !secure.scopesAny.some((scope) => scopes.has(scope))) {
      throw clientContractError(
        route,
        `client security alternative requests none of scopesAny [${secure.scopesAny.join(', ')}]`,
      );
    }
    for (const required of secure.roles ?? []) {
      if (!roles.has(required)) {
        throw clientContractError(route, `client security alternative does not request required role '${required}'`);
      }
    }
    if (secure.rolesAny?.length && !secure.rolesAny.some((role) => roles.has(role))) {
      throw clientContractError(
        route,
        `client security alternative requests none of rolesAny [${secure.rolesAny.join(', ')}]`,
      );
    }
  }
}

function isHeaderName(value: string): boolean {
  return value.length > 0 && /^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(value);
}

function isFrameworkOwnedClientHeader(value: string): boolean {
  return new Set([
    'authorization',
    'host',
    'content-length',
    'connection',
    'transfer-encoding',
    'upgrade',
    'cookie',
    'set-cookie',
    'traceparent',
    'tracestate',
    'baggage',
    'grpc-timeout',
    'connect-timeout-ms',
    'x-client-id',
    'x-request-id',
    'x-putnami-client-id',
    'x-putnami-service',
    'x-putnami-stream-wire',
  ]).has(value.toLowerCase());
}

function assertNonEmpty(value: string, field: string, route?: DiscoveredRoute): void {
  if (value.trim().length > 0) return;
  if (route) throw clientContractError(route, `${field} must not be empty`);
  throw new Error(`${field} must not be empty`);
}

function assertUnique(values: readonly string[] | undefined, field: string, route?: DiscoveredRoute): void {
  if (!values || new Set(values).size === values.length) return;
  if (route) throw clientContractError(route, `${field} must not contain duplicates`);
  throw new Error(`${field} must not contain duplicates`);
}

function clientContractError(route: DiscoveredRoute, message: string): Error {
  return new Error(`api.client operation ${route.method.toUpperCase()} ${toOpenApiPath(route.path)}: ${message}`);
}

/**
 * Convert a full `SchemaDefinition` to an OpenAPI object schema.
 */
function definitionToSchema(definition: SchemaDefinition, contractProjection: ContractProjection): OpenApiSchema {
  const contractRef = getContractTypeReference(definition);
  if (contractRef) return contractProjection.reference(contractRef);
  const properties: Record<string, OpenApiSchema> = {};
  const required: string[] = [];

  for (const [key, prop] of Object.entries(definition)) {
    properties[key] = primitiveToSchema(prop, contractProjection);
    if (!isOptionalProp(prop)) {
      required.push(key);
    }
  }

  const schema: OpenApiSchema = { type: 'object', properties, additionalProperties: false };
  if (required.length > 0) {
    schema.required = required;
  }
  return schema;
}

/**
 * Convert a single `SchemaPrimitive` to an OpenAPI schema.
 */
function primitiveToSchema(prop: SchemaPrimitive, contractProjection: ContractProjection): OpenApiSchema {
  const contractRef = getContractTypeReference(prop);
  if (contractRef) return contractProjection.reference(contractRef);
  if (isSchemaDescriptor(prop)) {
    return descriptorToSchema(prop, contractProjection);
  }

  // Nested object literal — a plain object whose values are schema primitives.
  // Recurse so the nested shape is emitted as a real `type: object`, not the
  // `string` fallback that `baseTypeName` would otherwise produce.
  if (isNestedSchema(prop)) {
    return definitionToSchema(prop as NestedSchema, contractProjection);
  }

  // JS constructors
  const type = baseTypeName(prop);
  return { type: openApiType(type, contractProjection.strict) };
}

function descriptorToSchema(desc: SchemaDescriptor, contractProjection: ContractProjection): OpenApiSchema {
  contractProjection.validateDescriptor(desc);

  // Array type
  if (desc.array && desc.items) {
    const schema: OpenApiSchema = {
      type: 'array',
      items: primitiveToSchema(desc.items, contractProjection),
    };
    return applyDescriptorMetadata(schema, desc, contractProjection);
  }

  // Nested object — `Desc(description, { ...nested })` carries the shape on
  // `schema` with baseType 'object'. Emit the full nested object schema.
  if (desc.baseType === 'object' && desc.schema) {
    const schema = definitionToSchema(desc.schema, contractProjection);
    return applyDescriptorMetadata(schema, desc, contractProjection);
  }

  // Map (`MapOf`) — arbitrary string-keyed object. Model it as a generic object
  // (mirrors the Go side's `reflect.Map → {type: "object"}`) without enumerating
  // keys, rather than falling through to the scalar `string` default.
  if (desc.map) {
    const schema: OpenApiSchema = {
      type: 'object',
      ...(desc.mapValue ? { additionalProperties: primitiveToSchema(desc.mapValue, contractProjection) } : {}),
    };
    return applyDescriptorMetadata(schema, desc, contractProjection);
  }

  const integer = desc.constraints?.some((constraint) => constraint.name === 'integer') ?? false;
  const schema: OpenApiSchema = { type: integer ? 'integer' : openApiType(desc.baseType, contractProjection.strict) };

  return applyDescriptorMetadata(schema, desc, contractProjection);
}

function applyDescriptorMetadata(
  schema: OpenApiSchema,
  desc: SchemaDescriptor,
  contractProjection: ContractProjection,
): OpenApiSchema {
  // Apply format hints from constraints
  if (desc.constraints) {
    const format = constraintFormat(desc.constraints);
    if (format) {
      schema.format = format;
    }
    const enumValues = constraintEnum(desc.constraints);
    if (enumValues) schema.enum = enumValues;
    for (const constraint of desc.constraints) {
      switch (constraint.name) {
        case 'min':
          schema.minimum = constraint.value as number;
          break;
        case 'max':
          schema.maximum = constraint.value as number;
          break;
        case 'minLength':
          schema.minLength = constraint.value as number;
          break;
        case 'maxLength':
          schema.maxLength = constraint.value as number;
          break;
        case 'pattern':
          schema.pattern = constraint.value as string;
          break;
      }
    }
    if (desc.constraints.some((constraint) => constraint.name === 'integer')) {
      applyIntegerWidth(schema, integerWidth(desc.constraints), contractProjection.strict);
    }
    if (desc.constraints.some((constraint) => constraint.name === NULLABLE_CONSTRAINT)) {
      schema.nullable = true;
    }
  }

  if (desc.default !== undefined) schema.default = cloneJsonValue(desc.default, contractProjection.strict, new Set());

  if (desc.description) {
    schema.description = desc.description;
  }

  return schema;
}

/** The declared width of an integer field, or the ADR 0004 default. */
function integerWidth(constraints: readonly SchemaConstraint[]): IntegerWidth {
  const declared = constraints.find((constraint) => constraint.name === INTEGER_WIDTH_CONSTRAINT);
  return declared ? (declared.value as IntegerWidth) : DEFAULT_INTEGER_WIDTH;
}

/**
 * Natural range of each declared width, as exact decimal text. `int64` and
 * `uint64` bounds are deliberately absent from the emitted document: this
 * projection is serialized with `JSON.stringify`, which cannot write
 * 9223372036854775807 without rounding it, and a rounded bound is worse than no
 * bound. The width itself already states the range, and both strict emitters
 * read the width, not the bounds.
 */
const INTEGER_WIDTH_BOUNDS: Readonly<Record<IntegerWidth, { min: number; max: number } | undefined>> = {
  int32: { min: -2_147_483_648, max: 2_147_483_647 },
  uint32: { min: 0, max: 4_294_967_295 },
  int64: undefined,
  uint64: undefined,
};

/**
 * Project the declared width of an integer (ADR 0004): the `format` is always
 * written, and the natural range is written when the author declared no
 * narrower bound.
 */
function applyIntegerWidth(schema: OpenApiSchema, width: IntegerWidth, strict: boolean): void {
  schema.format = width;
  for (const bound of ['minimum', 'maximum'] as const) {
    const declared = schema[bound];
    if (declared === undefined) continue;
    if (Number.isSafeInteger(declared)) continue;
    if (strict) {
      // The lexeme was already rounded by the JavaScript parser at the
      // declaration site, so there is no exact value left to emit.
      throw new Error(
        `api.client schema: ${bound} ${declared} on a ${width} field is not an exact integer; declare a bound within the safe-integer range`,
      );
    }
    delete schema[bound];
  }
  const natural = INTEGER_WIDTH_BOUNDS[width];
  if (!natural) return;
  schema.minimum ??= natural.min;
  schema.maximum ??= natural.max;
}

/**
 * Map internal type names to OpenAPI type strings.
 */
function openApiType(type: string, strict = false): string {
  if (type === 'string') return 'string';
  if (type === 'number') return 'number';
  if (type === 'boolean') return 'boolean';
  if (type === 'array') return 'array';
  if (type === 'object' || type === 'map') return 'object';
  if (strict) throw new Error(`api.client schema: unsupported schema base type ${JSON.stringify(type)}`);
  return 'string'; // default
}

/**
 * Derive an OpenAPI `format` from schema constraints.
 */
function constraintFormat(constraints: readonly SchemaConstraint[]): string | undefined {
  for (const c of constraints) {
    if (c.name === 'uuid') return 'uuid';
    if (c.name === 'email') return 'email';
    if (c.name === 'url') return 'uri';
    if (c.name === 'dateIso') return 'date-time';
  }
  return undefined;
}

function constraintEnum(constraints: readonly SchemaConstraint[]): string[] | undefined {
  const oneOf = constraints.find((constraint) => constraint.name === 'oneOf');
  return Array.isArray(oneOf?.value) && oneOf.value.every((value) => typeof value === 'string')
    ? [...oneOf.value]
    : undefined;
}

function cloneJsonValue(value: unknown, strict: boolean, ancestors: Set<object>): unknown {
  if (value === null || typeof value === 'string' || typeof value === 'boolean') return value;
  if (typeof value === 'number') {
    if (Number.isFinite(value)) return value;
    if (strict) throw new Error('api.client schema: default must contain only finite JSON numbers');
    return value;
  }
  if (Array.isArray(value)) {
    if (ancestors.has(value)) {
      if (strict) throw new Error('api.client schema: default must not contain cycles');
      return value;
    }
    ancestors.add(value);
    const copy = value.map((entry) => cloneJsonValue(entry, strict, ancestors));
    ancestors.delete(value);
    return copy;
  }
  if (value && typeof value === 'object') {
    if (ancestors.has(value)) {
      if (strict) throw new Error('api.client schema: default must not contain cycles');
      return value;
    }
    ancestors.add(value);
    const copy = Object.fromEntries(
      Object.entries(value).map(([key, entry]) => [key, cloneJsonValue(entry, strict, ancestors)]),
    );
    ancestors.delete(value);
    return copy;
  }
  if (strict) throw new Error('api.client schema: default must be a JSON value');
  return value;
}

function isOptionalProp(prop: SchemaPrimitive): boolean {
  return isSchemaDescriptor(prop) && (prop.optional === true || prop.default !== undefined);
}

// ---------------------------------------------------------------------------
// Canonical contract projection
// ---------------------------------------------------------------------------

/** Collect contract references encountered on routes and render their closure. */
class ContractProjection {
  private readonly references: Map<string, ContractTypeReference>;
  private lenientView?: ContractProjection;

  constructor(
    readonly strict: boolean,
    references: Map<string, ContractTypeReference> = new Map(),
  ) {
    this.references = references;
  }

  /**
   * The same projection with the first-party schema rules off, for an
   * operation an external authority owns: the standard owns its schemas, so
   * they are projected the way a document without a client contract projects
   * them. The contract references stay shared, so a component such an
   * operation reaches is still emitted once, under the document's own rules.
   */
  lenient(): ContractProjection {
    if (!this.strict) return this;
    this.lenientView ??= new ContractProjection(false, this.references);
    return this.lenientView;
  }

  validateDescriptor(desc: SchemaDescriptor): void {
    if (!this.strict) return;
    if (desc.env !== undefined || desc.resolve !== undefined) {
      throw new Error(
        'api.client schema: environment and resolver descriptors are not representable in a client contract',
      );
    }
    if (desc.array && !desc.items) throw new Error('api.client schema: array descriptor is missing items');
    if (desc.map) {
      if (!desc.mapValue) throw new Error('api.client schema: map descriptor is missing its value schema');
      if (desc.mapKey !== String) {
        throw new Error('api.client schema: JSON map keys must use String in a first-party contract');
      }
    }
    const supported = new Set([
      'uuid',
      'email',
      'url',
      'dateIso',
      'integer',
      'min',
      'max',
      'minLength',
      'maxLength',
      'pattern',
      'oneOf',
      INTEGER_WIDTH_CONSTRAINT,
      NULLABLE_CONSTRAINT,
    ]);
    for (const constraint of desc.constraints ?? []) {
      if (!supported.has(constraint.name)) {
        throw new Error(`api.client schema: unsupported validation constraint ${JSON.stringify(constraint.name)}`);
      }
      if (['min', 'max', 'minLength', 'maxLength'].includes(constraint.name)) {
        if (typeof constraint.value !== 'number' || !Number.isFinite(constraint.value)) {
          throw new Error(`api.client schema: ${constraint.name} requires a finite numeric value`);
        }
      }
      if (constraint.name === 'pattern' && typeof constraint.value !== 'string') {
        throw new Error('api.client schema: pattern requires a string value');
      }
      if (
        constraint.name === 'oneOf' &&
        (!Array.isArray(constraint.value) ||
          !constraint.value.length ||
          constraint.value.some((value) => typeof value !== 'string'))
      ) {
        throw new Error('api.client schema: oneOf requires a non-empty string value list');
      }
    }
  }

  reference(reference: ContractTypeReference): OpenApiSchema {
    const existing = this.references.get(reference.name);
    if (existing && existing.manifest !== reference.manifest) {
      throw new Error(`contract type ${JSON.stringify(reference.name)} is declared by multiple manifests`);
    }
    this.references.set(reference.name, reference);
    return { $ref: schemaRef(reference.name) };
  }

  schemas(): Record<string, OpenApiSchema> {
    const schemas: Record<string, OpenApiSchema> = {};
    // Projection may discover referenced field types while walking a node. Keep
    // consuming the growing insertion-ordered map until the dependency closure
    // is complete.
    const emitted = new Set<string>();
    while (emitted.size < this.references.size) {
      for (const [name, reference] of this.references) {
        if (emitted.has(name)) continue;
        schemas[name] = this.project(reference);
        emitted.add(name);
      }
    }
    return Object.fromEntries(Object.entries(schemas).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)));
  }

  private project(reference: ContractTypeReference): OpenApiSchema {
    const { manifest, name } = reference;
    const enumNode = manifest.enums?.find((node) => node.name === name);
    if (enumNode) {
      return {
        type: 'string',
        enum: enumNode.values.map((value) => value.value),
        ...(enumNode.description ? { description: enumNode.description } : {}),
      };
    }

    const structNode = manifest.structs?.find((node) => node.name === name);
    if (structNode) return this.projectFields(manifest, structNode.fields ?? [], structNode.description);

    const unionNode = manifest.unions?.find((node) => node.name === name);
    if (unionNode) {
      const oneOf = unionNode.variants.map((variant) => {
        const referencedStruct = variant.struct
          ? manifest.structs?.find((candidate) => candidate.name === variant.struct)
          : undefined;
        const fields = (variant.fields ?? referencedStruct?.fields ?? []).filter(
          (field) => field.name !== unionNode.discriminator,
        );
        const schema = this.projectFields(manifest, fields, variant.description);
        schema.properties = {
          ...(schema.properties ?? {}),
          [unionNode.discriminator]: { type: 'string', enum: [variant.tag] },
        };
        schema.required = [...new Set([unionNode.discriminator, ...(schema.required ?? [])])].sort();
        return schema;
      });
      return {
        oneOf,
        discriminator: { propertyName: unionNode.discriminator },
        ...(unionNode.description ? { description: unionNode.description } : {}),
      };
    }
    throw new Error(`contract type ${JSON.stringify(name)} is not declared by ${JSON.stringify(manifest.name)}`);
  }

  private projectFields(manifest: ContractManifest, fields: ContractField[], description?: string): OpenApiSchema {
    const properties: Record<string, OpenApiSchema> = {};
    const required: string[] = [];
    for (const field of fields) {
      properties[field.name] = this.projectField(manifest, field);
      if (!field.optional) required.push(field.name);
    }
    required.sort();
    return {
      type: 'object',
      properties,
      ...(required.length ? { required } : {}),
      additionalProperties: false,
      ...(description ? { description } : {}),
    };
  }

  private projectField(manifest: ContractManifest, field: ContractField): OpenApiSchema {
    let schema: OpenApiSchema;
    switch (field.type) {
      case 'string':
        schema = { type: 'string' };
        break;
      case 'int':
        // ADR 0004: a contract `int` is int64 on every transport. The width is
        // declared, never narrowed by the transport that carries it.
        schema = { type: 'integer' };
        applyIntegerWidth(schema, DEFAULT_INTEGER_WIDTH, true);
        break;
      case 'float':
        schema = { type: 'number', format: 'double' };
        break;
      case 'bool':
        schema = { type: 'boolean' };
        break;
      case 'duration':
        schema = { type: 'string', format: 'duration' };
        break;
      default:
        schema = this.reference({ manifest, name: field.type });
    }
    if (field.repeated) schema = { type: 'array', items: schema };
    if (field.description) schema.description = field.description;
    return schema;
  }
}

// ---------------------------------------------------------------------------
// Shared-schema promotion (components / $ref)
// ---------------------------------------------------------------------------

/**
 * Promote every object schema reused across the document into a single named
 * entry under `components.schemas`, replacing each reused occurrence with a
 * `$ref`. A shape that appears only once stays inline, except a
 * `.mayThrowDetails()` body: it is always a component, in the error contract
 * and in the documented response alike, the way Go publishes a details type.
 *
 * Reuse is structural: two object schemas are "the same" when their complete
 * identity matches — every keyword a reader validates, descriptions aside — so
 * a component never publishes one declaration's constraints for another's.
 * Component names (`Model1`, `Model2`, …) are assigned by sorting the shared
 * shapes, so the same set of models always yields the same names regardless
 * of route discovery order — the emitted spec stays byte-stable.
 *
 * Mutates the schemas reachable from `paths` (rewriting reused ones to `$ref`s)
 * and returns the `components.schemas` map.
 */
function dedupeSharedSchemas(
  paths: OpenApiDocument['paths'],
  reservedNames: ReadonlySet<string> = new Set(),
): Record<string, OpenApiSchema> {
  const holders = [...collectSchemaHolders(paths), ...collectDeclaredDetailsHolders(paths)];

  // Pass 1: count how often each object shape occurs across the document.
  const counts = new Map<string, number>();
  const signatures = new Map<string, string>();
  for (const holder of holders) {
    countObjectSignatures(holder.schema, counts, signatures);
  }

  // Shared = shapes seen at least twice, and every declared details body.
  const promoted = new Set([...counts.entries()].filter(([, n]) => n >= 2).map(([identity]) => identity));
  for (const holder of holders) {
    if (holder.declaredDetails && isObjectSchema(holder.schema)) promoted.add(schemaIdentity(holder.schema));
  }
  if (promoted.size === 0) {
    return {};
  }
  // Names follow the structural signature, then the identity: a document whose
  // shapes the signature already told apart keeps the names it always had.
  const shared = [...promoted].sort(
    (left, right) =>
      compareCodeUnits(signatures.get(left) ?? '', signatures.get(right) ?? '') || compareCodeUnits(left, right),
  );
  const nameByIdentity = new Map<string, string>();
  let nextModel = 1;
  for (const identity of shared) {
    while (reservedNames.has(`Model${nextModel}`)) nextModel++;
    nameByIdentity.set(identity, `Model${nextModel}`);
    nextModel++;
  }

  // Pass 2: rewrite reused occurrences to $refs and publish the component bodies.
  const components: Record<string, OpenApiSchema> = {};
  for (const holder of holders) {
    holder.schema = rewriteSchema(holder.schema, nameByIdentity, components);
  }
  return components;
}

/** A mutable owner of a `schema` field (request/response media-type objects). */
interface SchemaHolder {
  schema: OpenApiSchema;
  /** The schema is a `.mayThrowDetails()` body, promoted even when used once. */
  readonly declaredDetails?: true;
}

/** Collect every request-body and response media-type schema holder in the document. */
function collectSchemaHolders(paths: OpenApiDocument['paths']): SchemaHolder[] {
  const holders: SchemaHolder[] = [];
  for (const pathItem of Object.values(paths)) {
    for (const operation of Object.values(pathItem)) {
      // A shared component is first-party, and every reader validates it. An
      // operation an external authority owns publishes schemas the standard
      // owns, so promoting one into a component would put a schema the
      // first-party subset never accepted into the shared space. It stays
      // inline instead.
      if (operation[EXTERNAL_CONTRACT_KEY] !== undefined) continue;
      if (operation.requestBody?.content) {
        holders.push(...Object.values(operation.requestBody.content));
      }
      for (const response of Object.values(operation.responses)) {
        if (response.content) {
          holders.push(...Object.values(response.content));
        }
      }
    }
  }
  return holders;
}

/**
 * Every `.mayThrowDetails()` body a first-party operation publishes: in its
 * error contract, and as the `details` of a documented error envelope — or one
 * `anyOf` variant of it. The envelope itself stays out of promotion.
 */
function collectDeclaredDetailsHolders(paths: OpenApiDocument['paths']): SchemaHolder[] {
  const holders: SchemaHolder[] = [];
  const hold = (read: () => OpenApiSchema, write: (schema: OpenApiSchema) => void): void => {
    holders.push({
      declaredDetails: true,
      get schema() {
        return read();
      },
      set schema(schema: OpenApiSchema) {
        write(schema);
      },
    });
  };
  for (const pathItem of Object.values(paths)) {
    for (const operation of Object.values(pathItem)) {
      if (operation[EXTERNAL_CONTRACT_KEY] !== undefined) continue;
      const errors = operation['x-putnami-client']?.errors as ClientDeclaredError[] | undefined;
      errors?.forEach((declared, index) => {
        const schema = declared.schema as unknown as OpenApiSchema | undefined;
        if (!isDeclaredDetailsSchema(schema)) return;
        hold(
          () => errors[index].schema as unknown as OpenApiSchema,
          (rewritten) => {
            errors[index] = { ...errors[index], schema: rewritten as unknown as ClientSchema };
          },
        );
      });
      for (const response of Object.values(operation.responses)) {
        for (const media of Object.values(response.content ?? {})) {
          const envelope = media.schema;
          if (!isStandardErrorSchema(envelope) || !envelope.properties) continue;
          const properties = envelope.properties;
          const details = properties['details'];
          if (isDeclaredDetailsSchema(details)) {
            hold(
              () => properties['details'],
              (rewritten) => {
                properties['details'] = rewritten;
              },
            );
          }
          const variants = details?.anyOf ?? [];
          variants.forEach((variant, index) => {
            if (!isDeclaredDetailsSchema(variant)) return;
            hold(
              () => variants[index],
              (rewritten) => {
                variants[index] = rewritten;
              },
            );
          });
        }
      }
    }
  }
  return holders;
}

/**
 * Recursively tally object-schema identities within a schema tree, recording
 * each identity's structural signature for naming.
 */
function countObjectSignatures(
  schema: OpenApiSchema,
  counts: Map<string, number>,
  signatures: Map<string, string>,
): void {
  if (isStandardErrorSchema(schema)) return;
  if (isObjectSchema(schema)) {
    const identity = schemaIdentity(schema);
    counts.set(identity, (counts.get(identity) ?? 0) + 1);
    signatures.set(identity, schemaSignature(schema));
    for (const prop of Object.values(schema.properties as Record<string, OpenApiSchema>)) {
      countObjectSignatures(prop, counts, signatures);
    }
  } else if (schema.type === 'array' && schema.items) {
    countObjectSignatures(schema.items, counts, signatures);
  }
}

/**
 * Return a rewritten copy of `schema`: a reused object node becomes a `$ref`
 * (and is published into `components`), a single-use object node keeps its
 * inline shape but has its children rewritten, and arrays recurse into items.
 */
function rewriteSchema(
  schema: OpenApiSchema,
  nameByIdentity: Map<string, string>,
  components: Record<string, OpenApiSchema>,
): OpenApiSchema {
  if (isStandardErrorSchema(schema)) return schema;
  if (isObjectSchema(schema)) {
    const name = nameByIdentity.get(schemaIdentity(schema));
    if (name) {
      if (!components[name]) {
        components[name] = rewriteObjectProperties(schema, nameByIdentity, components);
      }
      return { $ref: schemaRef(name) };
    }
    return rewriteObjectProperties(schema, nameByIdentity, components);
  }
  if (schema.type === 'array' && schema.items) {
    return { ...schema, items: rewriteSchema(schema.items, nameByIdentity, components) };
  }
  return schema;
}

/** Clone an object schema with each property recursively rewritten. */
function rewriteObjectProperties(
  schema: OpenApiSchema,
  nameByIdentity: Map<string, string>,
  components: Record<string, OpenApiSchema>,
): OpenApiSchema {
  const properties: Record<string, OpenApiSchema> = {};
  for (const [key, prop] of Object.entries(schema.properties as Record<string, OpenApiSchema>)) {
    properties[key] = rewriteSchema(prop, nameByIdentity, components);
  }
  return { ...schema, properties };
}

/** A schema is a structural object when it has `type: 'object'` and enumerated properties. */
function isObjectSchema(schema: OpenApiSchema): boolean {
  return schema.type === 'object' && schema.properties !== undefined;
}

function isStandardErrorSchema(schema: OpenApiSchema): schema is StandardErrorSchema {
  return (schema as Partial<StandardErrorSchema>)[standardErrorSchemaMarker] === true;
}

/**
 * A stable structural signature for a schema node: property names, `type`,
 * `format` and `required`. It orders component names and nothing else — it
 * does not tell apart two schemas that differ in a constraint, an enum, a
 * reference or a map's values; {@link schemaIdentity} does.
 */
function schemaSignature(schema: OpenApiSchema): string {
  if (isObjectSchema(schema)) {
    const props = schema.properties as Record<string, OpenApiSchema>;
    const entries = Object.keys(props)
      .sort()
      .map((key) => `${key}:${schemaSignature(props[key])}`)
      .join(',');
    const required = (schema.required ?? []).slice().sort().join(',');
    return `O{${entries}}!${required}`;
  }
  if (schema.type === 'array' && schema.items) {
    return `A[${schemaSignature(schema.items)}]`;
  }
  return `S:${schema.type ?? ''}:${schema.format ?? ''}`;
}

/**
 * The complete identity of a schema node: every keyword a reader validates, with
 * nested schemas, property order and `required` order normalized. Descriptions
 * are ignored so two otherwise identical models still collapse to one component.
 */
function schemaIdentity(schema: OpenApiSchema): string {
  const members: string[] = [];
  for (const key of Object.keys(schema).sort()) {
    const value = (schema as Record<string, unknown>)[key];
    if (key === 'description' || value === undefined) continue;
    members.push(`${JSON.stringify(key)}:${keywordIdentity(key, value)}`);
  }
  return `{${members.join(',')}}`;
}

function keywordIdentity(key: string, value: unknown): string {
  switch (key) {
    case 'properties': {
      const properties = value as Record<string, OpenApiSchema>;
      const entries = Object.keys(properties)
        .sort()
        .map((name) => `${JSON.stringify(name)}:${schemaIdentity(properties[name])}`);
      return `{${entries.join(',')}}`;
    }
    case 'items':
      return schemaIdentity(value as OpenApiSchema);
    case 'additionalProperties':
      return typeof value === 'object' && value !== null
        ? schemaIdentity(value as OpenApiSchema)
        : JSON.stringify(value);
    case 'oneOf':
    case 'anyOf':
      return `[${(value as OpenApiSchema[]).map(schemaIdentity).join(',')}]`;
    case 'required':
      return JSON.stringify([...(value as string[])].sort());
    default:
      return canonicalJson(value);
  }
}

/** JSON with object keys sorted, so equal values have one spelling. */
function canonicalJson(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(',')}]`;
  if (typeof value === 'object' && value !== null) {
    const record = value as Record<string, unknown>;
    const entries = Object.keys(record)
      .sort()
      .filter((key) => record[key] !== undefined)
      .map((key) => `${JSON.stringify(key)}:${canonicalJson(record[key])}`);
    return `{${entries.join(',')}}`;
  }
  return JSON.stringify(value) ?? 'null';
}

/** Build a `$ref` pointer into `components.schemas`. */
function schemaRef(name: string): string {
  return `#/components/schemas/${name}`;
}

// ---------------------------------------------------------------------------
// Response builders
// ---------------------------------------------------------------------------

const DEFAULT_ERROR_STATUSES = [400, 500] as const;

const errorCodeToStatus: Record<ErrorResponseCode, number> = {
  BadRequest: 400,
  Validation: 400,
  InvalidArgument: 400,
  Unauthorized: 401,
  Forbidden: 403,
  NotFound: 404,
  MethodNotAllowed: 405,
  NotAcceptable: 406,
  ProxyAuthenticationRequired: 407,
  RequestTimeout: 408,
  Conflict: 409,
  AlreadyExists: 409,
  Gone: 410,
  LengthRequired: 411,
  Precondition: 412,
  PreconditionFailed: 412,
  PayloadTooLarge: 413,
  UriTooLong: 414,
  UnsupportedMediaType: 415,
  RangeNotSatisfiable: 416,
  ExpectationFailed: 417,
  ImATeapot: 418,
  Misdirected: 421,
  UnprocessableEntity: 422,
  FailedDependency: 424,
  RateLimit: 429,
  TooManyRequests: 429,
  Internal: 500,
  InternalServerError: 500,
  NotImplemented: 501,
  BadGateway: 502,
  Unavailable: 503,
  ServiceUnavailable: 503,
  Timeout: 504,
  GatewayTimeout: 504,
  HttpVersionNotSupported: 505,
};

/**
 * Populate `operation.responses` (and `description` for stream routes).
 */
function buildResponses(
  route: DiscoveredRoute,
  operation: OpenApiOperation,
  contractProjection: ContractProjection,
): void {
  if (route.streamMode) {
    buildStreamResponses(route, operation, contractProjection);
    addErrorResponses(route, operation, contractProjection);
    return;
  }

  // Multi-status returns
  if (route.responses?.returns && route.responses.returns.length > 0) {
    for (const ret of route.responses.returns) {
      const statusKey = String(ret.status);
      const response: OpenApiResponse = {
        description: ret.description ?? `${statusKey} response`,
      };
      if (ret.schema) {
        response.content = {
          'application/json': { schema: definitionToSchema(ret.schema, contractProjection) },
        };
      }
      operation.responses[statusKey] = response;
    }

    // If the endpoint also declares its primary schemas.returns but no explicit
    // 200 response, publish that primary response.
    if (route.schemas?.returnsBinary && !operation.responses['200']) {
      operation.responses['200'] = {
        description: 'Successful response',
        content: binaryContent(route.schemas.returnsBinary),
      };
    } else if (route.schemas?.returns && !operation.responses['200']) {
      operation.responses['200'] = {
        description: 'Successful response',
        content: {
          'application/json': { schema: definitionToSchema(route.schemas.returns, contractProjection) },
        },
      };
    }
  } else if (route.schemas?.returnsBinary) {
    operation.responses['200'] = {
      description: 'Successful response',
      content: binaryContent(route.schemas.returnsBinary),
    };
  } else if (route.schemas?.returns) {
    // Canonical primary response declared by endpoint .returns().
    operation.responses['200'] = {
      description: 'Successful response',
      content: {
        'application/json': { schema: definitionToSchema(route.schemas.returns, contractProjection) },
      },
    };
  } else {
    operation.responses['200'] = { description: 'Successful response' };
  }

  addErrorResponses(route, operation, contractProjection);
}

/**
 * Populate `operation.responses` with framework-guaranteed errors, `.mayThrow()`
 * declarations, and explicit `.throws()` overrides.
 */
function addErrorResponses(
  route: DiscoveredRoute,
  operation: OpenApiOperation,
  contractProjection: ContractProjection,
): void {
  const details = errorDetailsByStatus(route, contractProjection);
  for (const status of DEFAULT_ERROR_STATUSES) {
    addStandardErrorResponse(operation, status, details.get(status));
  }
  if (route.schemas?.bodyBinary) {
    addStandardErrorResponse(operation, 413, details.get(413));
    addStandardErrorResponse(operation, 415, details.get(415));
  }

  const declaredStatuses = [
    ...new Set(route.responses?.errorCodes?.map((code) => errorCodeToStatus[code] ?? 500) ?? []),
  ];
  declaredStatuses.sort((a, b) => a - b);
  for (const status of declaredStatuses) {
    addStandardErrorResponse(operation, status, details.get(status));
  }

  addExplicitThrowsResponses(route, operation, contractProjection);
}

/**
 * The `details` schema each status documents: the one `.mayThrowDetails()` schema
 * declared for a code answering with it, or `anyOf` them. The variants follow the
 * stable error codes in code-unit order and name a schema once, at its first code,
 * so the document does not depend on declaration order; Go orders its details
 * types the same way. It is `anyOf` and not `oneOf` because the variants may
 * overlap — two schemas of one shape, or schemas whose members are all optional —
 * and `oneOf` refuses a body more than one variant admits. The envelope's `code`
 * already tells the errors apart.
 */
function errorDetailsByStatus(
  route: DiscoveredRoute,
  contractProjection: ContractProjection,
): Map<number, OpenApiSchema> {
  const declared = [...new Set(route.responses?.errorCodes ?? [])]
    .map((code) => ({ code, stable: errorCodeToStableCode[code], details: route.responses?.errorDetails?.[code] }))
    // Two authoring names can share a stable code (`RateLimit`, `TooManyRequests`);
    // the name settles that tie so declaration order never does.
    .sort((left, right) => compareCodeUnits(left.stable, right.stable) || compareCodeUnits(left.code, right.code));
  const byStatus = new Map<number, Map<string, OpenApiSchema>>();
  for (const { code, details } of declared) {
    if (!details) continue;
    const status = errorCodeToStatus[code] ?? 500;
    const variants = byStatus.get(status) ?? new Map<string, OpenApiSchema>();
    const schema = declaredDetailsSchema(details, contractProjection);
    // Keyed by the identity shared-schema promotion names components by, so two
    // codes declaring one schema document one variant, as Go does for one type.
    const identity = schemaIdentity(schema);
    if (!variants.has(identity)) variants.set(identity, schema);
    byStatus.set(status, variants);
  }
  const documented = new Map<number, OpenApiSchema>();
  for (const [status, variants] of byStatus) {
    const schemas = [...variants.values()];
    documented.set(status, schemas.length === 1 ? schemas[0] : { anyOf: schemas });
  }
  return documented;
}

/**
 * The OpenAPI schema of a `.mayThrowDetails()` body. Shared-schema promotion
 * publishes every one as a component, the way Go publishes a details type, so
 * both providers reference the same shape from the contract and the response.
 */
function declaredDetailsSchema(details: SchemaDefinition, contractProjection: ContractProjection): OpenApiSchema {
  const schema = definitionToSchema(details, contractProjection);
  Object.defineProperty(schema, declaredDetailsSchemaMarker, { value: true });
  return schema;
}

function isDeclaredDetailsSchema(schema: OpenApiSchema | undefined): boolean {
  return (schema as { [declaredDetailsSchemaMarker]?: true } | undefined)?.[declaredDetailsSchemaMarker] === true;
}

function addStandardErrorResponse(operation: OpenApiOperation, status: number, details?: OpenApiSchema): void {
  const statusKey = String(status);
  if (operation.responses[statusKey]) return;
  const schema = standardErrorSchema();
  if (details && schema.properties) schema.properties['details'] = details;
  operation.responses[statusKey] = {
    description: getHttpStatusText(status) ?? `Error ${statusKey}`,
    content: {
      'application/json': { schema },
    },
  };
}

/**
 * Project one declared raw octet payload. The schema is the OpenAPI vocabulary
 * for "these are octets" and nothing more; the bound travels beside it because
 * no schema keyword can carry it.
 */
function binaryContent(declared: BinaryMeta): Record<string, OpenApiMediaType> {
  validateBinaryMeta(declared);
  return {
    [declared.mediaType]: {
      schema: { type: 'string', format: 'binary' },
      'x-putnami-max-bytes': declared.maxBytes,
      ...(declared.streamed ? { 'x-putnami-streamed': true as const } : {}),
    },
  };
}

function standardErrorSchema(): OpenApiSchema {
  const schema: OpenApiSchema = {
    type: 'object',
    properties: {
      statusCode: { type: 'number' },
      message: { type: 'string' },
      error: { type: 'string' },
      code: { type: 'string' },
      stack: { type: 'string' },
      errors: { type: 'array', items: { type: 'object' } },
    },
  };
  Object.defineProperty(schema, standardErrorSchemaMarker, { value: true });
  return schema;
}

/**
 * Populate `operation.responses` with explicit error/exception entries from `.throws()`.
 */
function addExplicitThrowsResponses(
  route: DiscoveredRoute,
  operation: OpenApiOperation,
  contractProjection: ContractProjection,
): void {
  if (!route.responses?.throws) return;

  for (const err of route.responses.throws) {
    const statusKey = String(err.status);
    const response: OpenApiResponse = {
      description: err.description ?? `Error ${statusKey}`,
    };
    if (err.schema) {
      response.content = {
        'application/json': { schema: definitionToSchema(err.schema, contractProjection) },
      };
    }
    operation.responses[statusKey] = response;
  }
}

function buildStreamResponses(
  route: DiscoveredRoute,
  operation: OpenApiOperation,
  contractProjection: ContractProjection,
): void {
  const protocols = route.streamMode === 'server' ? 'WebSocket, SSE' : 'WebSocket';
  // User-provided description takes precedence; append protocol info
  if (!operation.description) {
    operation.description = route.wire
      ? `Stream endpoint (WebSocket). ${providerWireDescription(route.wire)}`
      : `Stream endpoint (${protocols}). ${streamModeDescription(route.streamMode ?? 'bidirectional')}`;
  }

  if (route.streamMode === 'server') {
    if (route.schemas?.returns) {
      operation.responses['200'] = {
        description: 'Server-Sent Events stream',
        content: {
          'text/event-stream': { schema: definitionToSchema(route.schemas.returns, contractProjection) },
        },
      };
    } else {
      operation.responses['200'] = { description: 'Server-Sent Events stream' };
    }
  } else {
    operation.responses['101'] = { description: 'WebSocket upgrade' };
  }
}

/** Document a provider-owned wire for a reader who does not read `x-putnami-client`. */
function providerWireDescription(wire: ProviderWire): string {
  const negotiation = wire.subprotocol
    ? `Connect via WebSocket upgrade with subprotocol ${wire.subprotocol}`
    : 'Connect via WebSocket upgrade';
  return wire.bytes
    ? `${negotiation}. Raw octets in binary messages, both directions.`
    : `${negotiation}. One JSON value per text message, both directions.`;
}

function streamModeDescription(mode: string): string {
  if (mode === 'server') return 'Connect via WebSocket upgrade or Accept: text/event-stream.';
  if (mode === 'client')
    return 'Connect via WebSocket upgrade. Client sends messages, server returns a final response.';
  return 'Connect via WebSocket upgrade. Bidirectional message exchange.';
}

// ---------------------------------------------------------------------------
// Security helpers
// ---------------------------------------------------------------------------

/** Normalize a `principalKind` requirement (single, list, or unset) to an array. */
function toKindArray(value: SecurityOptions['principalKind']): string[] {
  if (!value) return [];
  return Array.isArray(value) ? value : [value];
}

/**
 * Which security schemes a route's `.secure()` options require.
 *
 * A route that declares no `principalKind` (the common case) accepts a bearer
 * user, so the default stays bearer-only and backward compatible. Declaring
 * `principalKind` narrows to — or widens across — the api-key scheme. A route
 * with security meta always documents at least the bearer scheme, so an
 * unrecognized principal-kind value can never yield a scheme-less operation.
 */
function requiredSchemes(security: SecurityOptions): { bearer: boolean; apiKey: boolean } {
  const kinds = toKindArray(security.principalKind);
  const bearer = kinds.length === 0 || kinds.includes('user');
  const apiKey = kinds.includes('apikey');
  if (!bearer && !apiKey) {
    return { bearer: true, apiKey: false };
  }
  return { bearer, apiKey };
}

/**
 * Build the per-operation OpenAPI security requirement list from route meta.
 *
 * Both the bearer (`http`/`bearer`) and api-key (`apiKey`) schemes carry an
 * empty scope array. Per the OpenAPI 3.0.3 Security Requirement Object, the
 * value array MUST be empty for every scheme type other than `oauth2` /
 * `openIdConnect`, so listing the route's required scopes on the `http` bearer
 * scheme would emit a spec-invalid document. Required scopes are surfaced in the
 * operation description instead (see {@link collectSecurityNotes}). Multiple
 * entries are alternatives (logical OR), so a route accepting either a bearer
 * user or an api key lists both.
 */
function buildSecurityRequirements(security: SecurityOptions): OpenApiSecurityRequirement[] {
  const schemes = requiredSchemes(security);
  const requirements: OpenApiSecurityRequirement[] = [];
  if (schemes.bearer) {
    requirements.push({ bearerAuth: [] });
  }
  if (schemes.apiKey) {
    requirements.push({ apiKey: [] });
  }
  if (security.optional === true) {
    // The empty requirement is OpenAPI's anonymous alternative.
    requirements.push({});
  }
  return requirements;
}

/**
 * Build a human-readable security note from roles/scopes for operation description.
 * Returns undefined if there are no specific requirements to document.
 */
function collectSecurityNotes(security: SecurityOptions): string | undefined {
  const parts: string[] = [];
  if (security.roles?.length) parts.push(`Required roles: ${security.roles.join(', ')}`);
  if (security.rolesAny?.length) parts.push(`Required roles (any): ${security.rolesAny.join(', ')}`);
  if (security.scopes?.length) parts.push(`Required scopes: ${security.scopes.join(', ')}`);
  if (security.scopesAny?.length) parts.push(`Required scopes (any): ${security.scopesAny.join(', ')}`);
  return parts.length > 0 ? parts.join('. ') : undefined;
}
